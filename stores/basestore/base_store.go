package basestore

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	ipfslog "berty.tech/go-ipfs-log"
	logac "berty.tech/go-ipfs-log/accesscontroller"
	"berty.tech/go-ipfs-log/entry"
	"berty.tech/go-ipfs-log/identityprovider"
	"berty.tech/go-ipfs-log/io"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	files "github.com/ipfs/go-ipfs-files"
	coreapi "github.com/ipfs/interface-go-ipfs-core"
	"github.com/ipfs/interface-go-ipfs-core/path"
	"github.com/pkg/errors"
	"go.uber.org/zap"

	"berty.tech/go-orbit-db/accesscontroller"
	"berty.tech/go-orbit-db/accesscontroller/simple"
	"berty.tech/go-orbit-db/address"
	"berty.tech/go-orbit-db/events"
	"berty.tech/go-orbit-db/iface"
	"berty.tech/go-orbit-db/stores"
	"berty.tech/go-orbit-db/stores/operation"
	"berty.tech/go-orbit-db/stores/replicator"
)

// BaseStore The base of other stores
type BaseStore struct {
	events.EventEmitter

	id                string
	identity          *identityprovider.Identity
	address           address.Address
	dbName            string
	ipfs              coreapi.CoreAPI
	cache             datastore.Datastore
	access            accesscontroller.Interface
	oplog             ipfslog.Log
	replicator        replicator.Replicator
	index             iface.StoreIndex
	replicationStatus replicator.ReplicationInfo
	stats             struct {
		snapshot struct {
			bytesLoaded int
		}
		syncRequestsReceived int
	}
	referenceCount int
	replicate      bool
	directory      string
	options        *iface.NewStoreOptions
	cacheDestroy   func() error

	muCache   sync.RWMutex
	muIndex   sync.RWMutex
	muStats   sync.RWMutex
	muJoining sync.Mutex
	sortFn    ipfslog.SortFn
}

func (b *BaseStore) DBName() string {
	return b.dbName
}

func (b *BaseStore) IPFS() coreapi.CoreAPI {
	return b.ipfs
}

func (b *BaseStore) Identity() *identityprovider.Identity {
	return b.identity
}

func (b *BaseStore) OpLog() ipfslog.Log {
	b.muIndex.RLock()
	defer b.muIndex.RUnlock()

	return b.oplog
}

func (b *BaseStore) AccessController() accesscontroller.Interface {
	return b.access
}

func (b *BaseStore) Replicator() replicator.Replicator {
	return b.replicator
}

func (b *BaseStore) Cache() datastore.Datastore {
	b.muCache.RLock()
	defer b.muCache.RUnlock()

	return b.cache
}

// InitBaseStore Initializes the store base
func (b *BaseStore) InitBaseStore(ctx context.Context, ipfs coreapi.CoreAPI, identity *identityprovider.Identity, addr address.Address, options *iface.NewStoreOptions) error {
	var err error

	if identity == nil {
		return errors.New("identity required")
	}

	b.id = addr.String()
	b.identity = identity
	b.address = addr
	if options.AccessController != nil {
		b.access = options.AccessController
	} else {
		manifestParams := accesscontroller.NewManifestParams(cid.Cid{}, true, "simple")
		manifestParams.SetAccess("write", []string{identity.ID})
		b.access, err = simple.NewSimpleAccessController(ctx, nil, manifestParams)

		if err != nil {
			return errors.Wrap(err, "unable to create simple access controller")
		}
	}
	b.dbName = addr.GetPath()
	b.ipfs = ipfs
	b.replicationStatus = replicator.NewReplicationInfo()

	b.muCache.Lock()
	b.cache = options.Cache
	b.cacheDestroy = options.CacheDestroy
	b.sortFn = options.SortFn
	b.muCache.Unlock()

	b.muIndex.Lock()
	b.oplog, err = ipfslog.NewLog(ipfs, identity, &ipfslog.LogOptions{
		ID:               addr.String(),
		AccessController: b.AccessController(),
		SortFn:           b.sortFn,
	})

	if err != nil {
		return errors.New("unable to instantiate an IPFS log")
	}

	if options.Index == nil {
		options.Index = NewBaseIndex
	}

	b.index = options.Index(b.Identity().PublicKey)
	b.muIndex.Unlock()

	b.muStats.Lock()
	b.stats.snapshot.bytesLoaded = -1
	b.muStats.Unlock()

	b.replicator = replicator.NewReplicator(ctx, b, options.ReplicationConcurrency)

	b.referenceCount = 64
	if options.ReferenceCount != nil {
		b.referenceCount = *options.ReferenceCount
	}

	// TODO: Doesn't seem to be used
	b.directory = "./orbitdb"
	if options.Directory != "" {
		b.directory = options.Directory
	}

	// TODO: Doesn't seem to be used
	b.replicate = true
	if options.Replicate != nil {
		b.replicate = *options.Replicate
	}

	b.options = options

	go func() {
		for e := range b.Replicator().Subscribe(ctx) {
			switch evt := e.(type) {
			case *replicator.EventLoadAdded:
				b.ReplicationStatus().IncQueued()
				b.recalculateReplicationMax(0)
				b.Emit(ctx, stores.NewEventReplicate(b.Address(), evt.Hash))

			case *replicator.EventLoadEnd:
				b.replicationLoadComplete(ctx, evt.Logs)

			case *replicator.EventLoadProgress:
				if b.ReplicationStatus().GetBuffered() > evt.BufferLength {
					b.recalculateReplicationProgress(b.ReplicationStatus().GetProgress() + evt.BufferLength)
				} else {
					if _, ok := b.OpLog().GetEntries().Get(evt.Hash.String()); ok {
						continue
					}

					b.recalculateReplicationProgress(b.OpLog().GetEntries().Len() + evt.BufferLength)
				}

				b.ReplicationStatus().SetBuffered(evt.BufferLength)
				b.recalculateReplicationMax(b.ReplicationStatus().GetProgress())
				// logger.debug(`<replicate.progress>`)
				b.Emit(ctx, stores.NewEventReplicateProgress(b.Address(), evt.Hash, evt.Latest, b.ReplicationStatus()))
			}
		}
	}()

	return nil
}

func (b *BaseStore) Close() error {
	// Replicator teardown logic
	b.Replicator().Stop()

	// Reset replication statistics
	b.ReplicationStatus().Reset()

	b.muStats.Lock()
	// Reset database statistics
	b.stats.snapshot.bytesLoaded = -1
	b.stats.syncRequestsReceived = 0
	b.muStats.Unlock()

	b.UnsubscribeAll()

	err := b.Cache().Close()
	if err != nil {
		return errors.Wrap(err, "unable to close cache")
	}

	return nil
}

func (b *BaseStore) Address() address.Address {
	return b.address
}

func (b *BaseStore) Index() iface.StoreIndex {
	b.muIndex.RLock()
	defer b.muIndex.RUnlock()

	return b.index
}

func (b *BaseStore) Type() string {
	return "store"
}

func (b *BaseStore) ReplicationStatus() replicator.ReplicationInfo {
	return b.replicationStatus
}

func (b *BaseStore) Drop() error {
	var err error
	if err = b.Close(); err != nil {
		return errors.Wrap(err, "unable to close store")
	}

	err = b.cacheDestroy()
	if err != nil {
		return errors.Wrap(err, "unable to destroy cache")
	}

	// TODO: Destroy cache? b.cache.Delete()

	// Reset
	b.muIndex.Lock()
	b.index = b.options.Index(b.Identity().PublicKey)
	b.oplog, err = ipfslog.NewLog(b.IPFS(), b.Identity(), &ipfslog.LogOptions{
		ID:               b.id,
		AccessController: b.AccessController(),
		SortFn:           b.SortFn(),
	})
	b.muIndex.Unlock()

	if err != nil {
		return errors.Wrap(err, "unable to create log")
	}

	b.muCache.Lock()
	b.cache = b.options.Cache
	b.muCache.Unlock()

	return nil
}

func (b *BaseStore) Load(ctx context.Context, amount int) error {
	if amount <= 0 && b.options.MaxHistory != nil {
		amount = *b.options.MaxHistory
	}

	var localHeads, remoteHeads []*entry.Entry
	localHeadsBytes, err := b.Cache().Get(datastore.NewKey("_localHeads"))
	if err != nil {
		return errors.Wrap(err, "unable to get local heads from cache")
	}

	err = json.Unmarshal(localHeadsBytes, &localHeads)
	if err != nil {
		return errors.Wrap(err, "unable to unmarshal cached local heads")
	}

	remoteHeadsBytes, err := b.Cache().Get(datastore.NewKey("_remoteHeads"))
	if err != nil && err != datastore.ErrNotFound {
		return errors.Wrap(err, "unable to get data from cache")
	}

	err = nil

	if remoteHeadsBytes != nil {
		err = json.Unmarshal(remoteHeadsBytes, &remoteHeads)
		if err != nil {
			return errors.Wrap(err, "unable to unmarshal cached remote heads")
		}
	}

	heads := append(localHeads, remoteHeads...)

	if len(heads) > 0 {
		headsForEvent := make([]ipfslog.Entry, len(heads))
		for i := range heads {
			headsForEvent[i] = heads[i]
		}

		b.Emit(ctx, stores.NewEventLoad(b.Address(), headsForEvent))
	}

	wg := sync.WaitGroup{}
	wg.Add(len(heads))

	for _, h := range heads {
		go func() {
			b.muJoining.Lock()
			defer b.muJoining.Unlock()
			defer wg.Done()

			oplog := b.OpLog()

			b.recalculateReplicationMax(h.GetClock().GetTime())

			l, inErr := ipfslog.NewFromEntryHash(ctx, b.IPFS(), b.Identity(), h.GetHash(), &ipfslog.LogOptions{
				ID:               oplog.GetID(),
				AccessController: b.AccessController(),
				SortFn:           b.SortFn(),
			}, &ipfslog.FetchOptions{
				Length:  &amount,
				Exclude: oplog.GetEntries().Slice(),
				// TODO: ProgressChan:  this._onLoadProgress.bind(this),
			})

			if inErr != nil {
				err = errors.Wrap(err, "unable to create log from entry hash")
			}

			if _, inErr = oplog.Join(l, amount); inErr != nil {
				// err = errors.Wrap(err, "unable to join log")
				// TODO: log
			}
		}()
	}

	wg.Wait()

	if err != nil {
		return err
	}

	// Update the index
	if len(heads) > 0 {
		if err := b.updateIndex(); err != nil {
			return errors.Wrap(err, "unable to update index")
		}
	}

	b.Emit(ctx, stores.NewEventReady(b.Address(), b.OpLog().Heads().Slice()))
	return nil
}

func (b *BaseStore) Sync(ctx context.Context, heads []ipfslog.Entry) error {
	b.muStats.Lock()
	b.stats.syncRequestsReceived++
	b.muStats.Unlock()

	if len(heads) == 0 {
		return nil
	}

	var savedEntriesCIDs []cid.Cid

	for _, h := range heads {
		if h == nil {
			logger().Debug("warning: Given input entry was 'null'.")
			continue
		}

		if h.GetNext() == nil {
			h.SetNext([]cid.Cid{})
		}

		if h.GetRefs() == nil {
			h.SetRefs([]cid.Cid{})
		}

		identityProvider := b.Identity().Provider
		if identityProvider == nil {
			return errors.New("identity-provider is required, cannot verify entry")
		}

		canAppend := b.AccessController().CanAppend(h, identityProvider, &CanAppendContext{log: b.OpLog()})
		if canAppend != nil {
			logger().Debug("warning: Given input entry is not allowed in this log and was discarded (no write access)", zap.Error(canAppend))
			continue
		}

		hash, err := io.WriteCBOR(ctx, b.IPFS(), h.ToCborEntry(), nil)
		if err != nil {
			return errors.Wrap(err, "unable to write entry on dag")
		}

		if hash.String() != h.GetHash().String() {
			return errors.New("WARNING! Head hash didn't match the contents")
		}

		savedEntriesCIDs = append(savedEntriesCIDs, hash)
	}

	b.Replicator().Load(ctx, savedEntriesCIDs)

	return nil
}

func (b *BaseStore) LoadMoreFrom(ctx context.Context, amount uint, cids []cid.Cid) {
	b.Replicator().Load(ctx, cids)
	// TODO: can this return an error?
}

type storeSnapshot struct {
	ID    string         `json:"id,omitempty"`
	Heads []*entry.Entry `json:"heads,omitempty"`
	Size  int            `json:"size,omitempty"`
	Type  string         `json:"type,omitempty"`
}

func (b *BaseStore) LoadFromSnapshot(ctx context.Context) error {
	b.muJoining.Lock()
	defer b.muJoining.Unlock()

	b.Emit(ctx, stores.NewEventLoad(b.Address(), nil))

	queueJSON, err := b.Cache().Get(datastore.NewKey("queue"))
	if err != nil && err != datastore.ErrNotFound {
		return errors.Wrap(err, "unable to get value from cache")
	}

	if err != datastore.ErrNotFound {
		var queue []cid.Cid

		var entries []ipfslog.Entry

		if err := json.Unmarshal(queueJSON, &queue); err != nil {
			return errors.Wrap(err, "unable to deserialize queued CIDs")
		}

		for _, h := range queue {
			entries = append(entries, &entry.Entry{Hash: h})
		}

		if err := b.Sync(ctx, entries); err != nil {
			return errors.Wrap(err, "unable to sync queued CIDs")
		}
	}

	snapshot, err := b.Cache().Get(datastore.NewKey("snapshot"))
	if err == datastore.ErrNotFound {
		return errors.Wrap(err, "not found")
	}

	if err != nil {
		return errors.Wrap(err, "unable to get value from cache")
	}

	logger().Debug("loading snapshot from path", zap.String("snapshot", string(snapshot)))

	resNode, err := b.IPFS().Unixfs().Get(ctx, path.New(string(snapshot)))
	if err != nil {
		return errors.Wrap(err, "unable to get snapshot from ipfs")
	}

	res, ok := resNode.(files.File)
	if !ok {
		return errors.New("unable to cast fetched data as a file")
	}

	headerLengthRaw := make([]byte, 2)
	if _, err := res.Read(headerLengthRaw); err != nil {
		return errors.Wrap(err, "unable to read from stream")
	}

	headerLength := binary.BigEndian.Uint16(headerLengthRaw)
	header := &storeSnapshot{}
	headerRaw := make([]byte, headerLength)
	if _, err := res.Read(headerRaw); err != nil {
		return errors.Wrap(err, "unable to read from stream")
	}

	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return errors.Wrap(err, "unable to decode header from ipfs data")
	}

	var entries []ipfslog.Entry
	maxClock := 0

	for i := 0; i < header.Size; i++ {
		entryLengthRaw := make([]byte, 2)
		if _, err := res.Read(entryLengthRaw); err != nil {
			return errors.Wrap(err, "unable to read from stream")
		}

		entryLength := binary.BigEndian.Uint16(entryLengthRaw)
		e := &entry.Entry{}
		entryRaw := make([]byte, entryLength)

		if _, err := res.Read(entryRaw); err != nil {
			return errors.Wrap(err, "unable to read from stream")
		}

		logger().Debug(fmt.Sprintf("Entry raw: %s", string(entryRaw)))

		if err = json.Unmarshal(entryRaw, e); err != nil {
			return errors.Wrap(err, "unable to unmarshal entry from ipfs data")
		}

		entries = append(entries, e)
		if maxClock < e.Clock.GetTime() {
			maxClock = e.Clock.GetTime()
		}
	}

	b.recalculateReplicationMax(maxClock)

	var headsCids []cid.Cid
	for _, h := range header.Heads {
		headsCids = append(headsCids, h.GetHash())
	}

	log, err := ipfslog.NewFromJSON(ctx, b.IPFS(), b.Identity(), &ipfslog.JSONLog{
		Heads: headsCids,
		ID:    header.ID,
	}, &ipfslog.LogOptions{
		Entries:          entry.NewOrderedMapFromEntries(entries),
		ID:               header.ID,
		AccessController: b.AccessController(),
		SortFn:           b.SortFn(),
	}, &entry.FetchOptions{
		Length:  intPtr(-1),
		Timeout: time.Second,
	})

	if err != nil {
		return errors.Wrap(err, "unable to load log")
	}

	if _, err = b.OpLog().Join(log, -1); err != nil {
		return errors.Wrap(err, "unable to join log")
	}

	if err := b.updateIndex(); err != nil {
		return errors.Wrap(err, "unable to update index")
	}

	return nil
}

func intPtr(i int) *int {
	return &i
}

func (b *BaseStore) AddOperation(ctx context.Context, op operation.Operation, onProgressCallback chan<- ipfslog.Entry) (ipfslog.Entry, error) {
	data, err := op.Marshal()
	if err != nil {
		return nil, errors.Wrap(err, "unable to marshal operation")
	}

	oplog := b.OpLog()

	e, err := oplog.Append(ctx, data, &ipfslog.AppendOptions{PointerCount: b.referenceCount})
	if err != nil {
		return nil, errors.Wrap(err, "unable to append data on log")
	}
	b.recalculateReplicationStatus(b.ReplicationStatus().GetProgress()+1, e.GetClock().GetTime())

	marshaledEntry, err := json.Marshal([]ipfslog.Entry{e})
	if err != nil {
		return nil, errors.Wrap(err, "unable to marshal entry")
	}

	err = b.Cache().Put(datastore.NewKey("_localHeads"), marshaledEntry)
	if err != nil {
		return nil, errors.Wrap(err, "unable to add data to cache")
	}

	if err := b.updateIndex(); err != nil {
		return nil, errors.Wrap(err, "unable to update index")
	}

	b.Emit(ctx, stores.NewEventWrite(b.Address(), e, oplog.Heads().Slice()))

	if onProgressCallback != nil {
		onProgressCallback <- e
	}

	return e, nil
}

func (b *BaseStore) recalculateReplicationProgress(max int) {
	if opLogLen := b.OpLog().GetEntries().Len(); opLogLen > max {
		max = opLogLen

	} else if replMax := b.ReplicationStatus().GetMax(); replMax > max {
		max = replMax
	}

	b.ReplicationStatus().SetProgress(max)

	b.recalculateReplicationMax(b.ReplicationStatus().GetProgress())
}

func (b *BaseStore) recalculateReplicationMax(max int) {
	if opLogLen := b.OpLog().GetEntries().Len(); opLogLen > max {
		max = opLogLen

	} else if replMax := b.ReplicationStatus().GetMax(); replMax > max {
		max = replMax
	}

	b.ReplicationStatus().SetMax(max)
}

func (b *BaseStore) recalculateReplicationStatus(maxProgress, maxTotal int) {
	b.recalculateReplicationProgress(maxProgress)
	b.recalculateReplicationMax(maxTotal)
}

func (b *BaseStore) updateIndex() error {
	b.recalculateReplicationMax(0)
	if err := b.Index().UpdateIndex(b.OpLog(), []ipfslog.Entry{}); err != nil {
		return errors.Wrap(err, "unable to update index")
	}
	b.recalculateReplicationProgress(0)

	return nil
}

func (b *BaseStore) replicationLoadComplete(ctx context.Context, logs []ipfslog.Log) {
	b.muJoining.Lock()
	defer b.muJoining.Unlock()

	oplog := b.OpLog()

	logger().Debug("replication load complete")
	for _, log := range logs {
		_, err := oplog.Join(log, -1)
		if err != nil {
			logger().Error("unable to join logs", zap.Error(err))
			return
		}
	}
	b.ReplicationStatus().DecreaseQueued(len(logs))
	b.ReplicationStatus().SetBuffered(b.Replicator().GetBufferLen())
	err := b.updateIndex()
	if err != nil {
		logger().Error("unable to update index", zap.Error(err))
		return
	}

	// only store heads that has been verified and merges
	heads := oplog.Heads()

	headsBytes, err := json.Marshal(heads.Slice())
	if err != nil {
		logger().Error("unable to serialize heads cache", zap.Error(err))
		return
	}

	err = b.Cache().Put(datastore.NewKey("_remoteHeads"), headsBytes)
	if err != nil {
		logger().Error("unable to update heads cache", zap.Error(err))
		return
	}

	logger().Debug(fmt.Sprintf("Saved heads %d", heads.Len()))

	// logger.debug(`<replicated>`)
	b.Emit(ctx, stores.NewEventReplicated(b.Address(), len(logs)))
}

func (b *BaseStore) SortFn() ipfslog.SortFn {
	return b.sortFn
}

type CanAppendContext struct {
	log ipfslog.Log
}

func (c *CanAppendContext) GetLogEntries() []logac.LogEntry {
	logEntries := c.log.GetEntries().Slice()

	var entries = make([]logac.LogEntry, len(logEntries))
	for i := range logEntries {
		entries[i] = logEntries[i]
	}

	return entries
}

var _ iface.Store = &BaseStore{}
