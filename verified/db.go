package verified

import (
	"fmt"
	"sort"
	"sync"

	"github.com/flaneur2020/groundkv/engine"
	"github.com/flaneur2020/groundkv/verified/dafnygen/GroundKVFreelist"
)

// DB is GroundKV's Dafny-verified embedded KV store implementation.
// It exposes the same API and binary on-disk format as engine.DB while
// executing Z3-verified Dafny-compiled Go code for codecs, B+Tree node
// operations, splitting, MVCC freelist reclamation, dual-meta recovery
// selection, and 4-phase commit guards.
type DB struct {
	mu            sync.RWMutex
	storage       Storage
	opts          Options
	meta          Meta
	writeTx       *Tx
	readerCounts  map[uint64]int
	freelist      *freelist
	freelistPages []PageID
	dirtyAltMeta  bool
	closed        bool
}

// HasDirtyAltMeta reports whether an aborted commit left an un-neutralized meta
// page in the alternate meta slot.
func (db *DB) HasDirtyAltMeta() bool {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.dirtyAltMeta
}

// Open opens or initializes a Dafny-verified GroundKV database backed by a file at path.
func Open(path string, opts *Options) (*DB, error) {
	st, err := engine.OpenOSFile(path)
	if err != nil {
		return nil, err
	}
	db, err := OpenStorage(st, opts)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	return db, nil
}

// OpenStorage opens or initializes a Dafny-verified GroundKV database backed by st.
func OpenStorage(st Storage, opts *Options) (*DB, error) {
	cfg, err := normalizeOptions(opts)
	if err != nil {
		return nil, err
	}

	size, err := st.Size()
	if err != nil {
		return nil, err
	}

	db := &DB{
		storage:      st,
		opts:         cfg,
		readerCounts: make(map[uint64]int),
		freelist:     newFreelist(),
	}

	if size == 0 {
		if err := db.initEmpty(); err != nil {
			return nil, err
		}
		db.emitTraceLocked("Open")
		return db, nil
	}

	if err := db.recover(); err != nil {
		return nil, err
	}
	db.emitTraceLocked("Open")
	return db, nil
}

func (db *DB) initEmpty() error {
	m := Meta{
		Magic:     metaMagic,
		Version:   metaVersion,
		PageSize:  uint32(db.opts.PageSize),
		Root:      0,
		HighWater: 2,
		TxID:      0,
		Freelist:  0,
	}
	for slot := PageID(0); slot < 2; slot++ {
		buf := encodeMetaPage(db.opts.PageSize, slot, m)
		if _, err := db.storage.WriteAt(buf, int64(slot)*int64(db.opts.PageSize)); err != nil {
			return err
		}
	}
	if !db.opts.NoSync {
		if err := db.storage.Sync(); err != nil {
			return err
		}
	}
	db.meta = m
	return nil
}

func (db *DB) recover() error {
	candidateSizes := []int{db.opts.PageSize}
	for ps := engine.MinPageSize; ps <= 65536; ps <<= 1 {
		if ps != db.opts.PageSize {
			candidateSizes = append(candidateSizes, ps)
		}
	}

	var lastErr0, lastErr1 error
	recovered := false
	for _, ps := range candidateSizes {
		meta0, err0 := db.readMetaSlotWithSize(0, ps)
		meta1, err1 := db.readMetaSlotWithSize(1, ps)

		if err0 == nil && int(meta0.PageSize) != ps {
			err0 = fmt.Errorf("%w: meta0 pageSize %d != %d", engine.ErrCorruptedMeta, meta0.PageSize, ps)
		}
		if err1 == nil && int(meta1.PageSize) != ps {
			err1 = fmt.Errorf("%w: meta1 pageSize %d != %d", engine.ErrCorruptedMeta, meta1.PageSize, ps)
		}

		if chosen, selErr := selectRecoveredMeta(meta0, err0 == nil, meta1, err1 == nil); selErr == nil {
			db.meta = chosen
			db.opts.PageSize = ps
			recovered = true
		} else if ps == db.opts.PageSize {
			lastErr0, lastErr1 = err0, err1
		}
		if recovered {
			break
		}
	}

	if !recovered {
		return fmt.Errorf("%w: meta0=(%v), meta1=(%v)", engine.ErrCorruptedMeta, lastErr0, lastErr1)
	}

	if db.meta.Root != 0 {
		if _, h, err := db.readCommittedPageLocked(db.meta.Root); err != nil {
			return fmt.Errorf("groundkv/verified: recover root page %d: %w", db.meta.Root, err)
		} else if h.flags != engine.PageFlagLeaf && h.flags != engine.PageFlagBranch {
			return fmt.Errorf("%w: root page %d has invalid flags %d", engine.ErrCorruptedPage, db.meta.Root, h.flags)
		}
	}

	if !db.opts.NoFreelist && db.meta.Freelist != 0 {
		freedIDs, chainPages, err := db.readFreelistChainLocked(db.meta.Freelist)
		if err != nil {
			return fmt.Errorf("groundkv/verified: recover freelist: %w", err)
		}
		db.freelistPages = chainPages
		db.freelist.addPending(db.meta.TxID, freedIDs)
	}
	return nil
}

func (db *DB) isClosed() bool {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.closed
}

func (db *DB) readMetaSlot(slot PageID) (Meta, error) {
	return db.readMetaSlotWithSize(slot, db.opts.PageSize)
}

func (db *DB) readMetaSlotWithSize(slot PageID, pageSize int) (Meta, error) {
	buf := make([]byte, pageSize)
	n, err := db.storage.ReadAt(buf, int64(slot)*int64(pageSize))
	if err != nil && n < pageSize {
		return Meta{}, err
	}
	return decodeMetaPage(buf, slot)
}

func (db *DB) readCommittedPage(pgid PageID) ([]byte, pageHeader, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.readCommittedPageLocked(pgid)
}

func (db *DB) readCommittedPageLocked(pgid PageID) ([]byte, pageHeader, error) {
	if db.closed {
		return nil, pageHeader{}, engine.ErrDBClosed
	}
	buf := make([]byte, db.opts.PageSize)
	off := int64(pgid) * int64(db.opts.PageSize)
	n, err := db.storage.ReadAt(buf, off)
	if err != nil && n < db.opts.PageSize {
		return nil, pageHeader{}, err
	}
	h, err := readAndVerifyPageHeader(buf, pgid)
	return buf, h, err
}

func (db *DB) minActiveReaderTxIDLocked() (uint64, bool) {
	if len(db.readerCounts) == 0 {
		return 0, false
	}
	var minTx uint64
	first := true
	for txid, c := range db.readerCounts {
		if c <= 0 {
			continue
		}
		if first || txid < minTx {
			minTx = txid
			first = false
		}
	}
	if first {
		return 0, false
	}
	return minTx, true
}

func (db *DB) releaseBoundLocked() uint64 {
	minR, hasReader := db.minActiveReaderTxIDLocked()
	return GroundKVFreelist.Companion_Default___.ReleaseBound(db.meta.TxID, hasReader, minR)
}

func (db *DB) emitTraceLocked(event string) {
	if db.opts.Tracer == nil {
		return
	}
	phase := "IDLE"
	if db.writeTx != nil {
		phase = db.writeTx.CommitPhase()
	}
	activeReaders := 0
	for _, c := range db.readerCounts {
		if c > 0 {
			activeReaders += c
		}
	}
	minReader := int64(-1)
	if r, ok := db.minActiveReaderTxIDLocked(); ok {
		minReader = int64(r)
	}
	pending := db.freelist.pendingTxIDs()
	if pending == nil {
		pending = []uint64{}
	}
	db.opts.Tracer.Trace(TraceEvent{
		Event:         event,
		TxID:          db.meta.TxID,
		Phase:         phase,
		Root:          db.meta.Root,
		Freelist:      db.meta.Freelist,
		HighWater:     db.meta.HighWater,
		DirtyAltMeta:  db.dirtyAltMeta,
		ActiveReaders: activeReaders,
		MinReaderTxID: minReader,
		FreeCount:     db.freelist.freeCount(),
		PendingTxIDs:  pending,
		ReleaseBound:  db.releaseBoundLocked(),
	})
}

// MinActiveReaderTxID returns the lowest TxID among currently open MVCC snapshots.
func (db *DB) MinActiveReaderTxID() (uint64, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.minActiveReaderTxIDLocked()
}

// ReleaseBound returns the current MVCC + dual-meta freelist reclamation bound.
func (db *DB) ReleaseBound() uint64 {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.releaseBoundLocked()
}

// FreelistState returns a consistent snapshot of the freelist state.
func (db *DB) FreelistState() FreelistSnapshot {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return FreelistSnapshot{
		FreeIDs:       db.freelist.freeIDs(),
		PendingTxIDs:  db.freelist.pendingTxIDs(),
		ReclaimedHits: db.freelist.reclaimedRuns,
	}
}

// ReachablePages returns all page IDs reachable from root and freelistHead.
func (db *DB) ReachablePages(root PageID, freelistHead PageID) ([]PageID, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, engine.ErrDBClosed
	}

	visited := make(map[PageID]bool)
	if root != 0 {
		if err := db.collectTreePagesLocked(root, visited); err != nil {
			return nil, err
		}
	}
	if freelistHead != 0 {
		_, chainPages, err := db.readFreelistChainLocked(freelistHead)
		if err != nil {
			return nil, err
		}
		for _, id := range chainPages {
			visited[id] = true
		}
	}

	out := make([]PageID, 0, len(visited))
	for id := range visited {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func (db *DB) collectTreePagesLocked(pgid PageID, visited map[PageID]bool) error {
	if visited[pgid] {
		return nil
	}
	visited[pgid] = true
	buf, h, err := db.readCommittedPageLocked(pgid)
	if err != nil {
		return err
	}
	if h.flags == engine.PageFlagBranch {
		entries, err := decodeBranchPage(buf, h)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := db.collectTreePagesLocked(e.child, visited); err != nil {
				return err
			}
		}
	}
	return nil
}

// BeginRead starts a read-only MVCC snapshot transaction pinned to the latest committed TxID.
func (db *DB) BeginRead() (*Tx, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil, engine.ErrDBClosed
	}
	db.readerCounts[db.meta.TxID]++
	db.emitTraceLocked("BeginRead")
	return &Tx{
		db:       db,
		writable: false,
		meta:     db.meta,
	}, nil
}

// BeginWrite starts a single-writer copy-on-write transaction.
func (db *DB) BeginWrite() (*Tx, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil, engine.ErrDBClosed
	}
	if db.writeTx != nil {
		return nil, engine.ErrWriteTxActive
	}
	if !db.opts.NoFreelist {
		db.freelist.release(db.releaseBoundLocked())
	}
	tx := &Tx{
		db:       db,
		writable: true,
		meta:     db.meta,
		dirty:    make(map[PageID][]byte),
		freed:    make(map[PageID]struct{}),
	}
	db.writeTx = tx
	db.emitTraceLocked("BeginWrite")
	return tx, nil
}

// View executes fn inside a read-only MVCC snapshot transaction.
func (db *DB) View(fn func(tx *Tx) error) error {
	tx, err := db.BeginRead()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return fn(tx)
}

// Update executes fn inside a read-write transaction.
func (db *DB) Update(fn func(tx *Tx) error) error {
	tx, err := db.BeginWrite()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return err
	}
	return nil
}

// Get returns the latest committed value for key, or engine.ErrNotFound.
func (db *DB) Get(key []byte) ([]byte, error) {
	var val []byte
	err := db.View(func(tx *Tx) error {
		var getErr error
		val, getErr = tx.Get(key)
		return getErr
	})
	return val, err
}

// Put atomically writes key -> value in a single-key write transaction.
func (db *DB) Put(key, value []byte) error {
	return db.Update(func(tx *Tx) error {
		return tx.Put(key, value)
	})
}

// Delete atomically deletes key in a single-key write transaction.
func (db *DB) Delete(key []byte) error {
	return db.Update(func(tx *Tx) error {
		return tx.Delete(key)
	})
}

// Meta returns a copy of the currently active committed Meta.
func (db *DB) Meta() Meta {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.meta
}

// InspectMeta reads both on-disk meta pages without failing if one slot is torn.
func (db *DB) InspectMeta() (slot0 MetaSlot, slot1 MetaSlot, err error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return MetaSlot{}, MetaSlot{}, engine.ErrDBClosed
	}
	if m0, err0 := db.readMetaSlot(0); err0 == nil {
		slot0 = MetaSlot{Meta: m0, Valid: true}
	}
	if m1, err1 := db.readMetaSlot(1); err1 == nil {
		slot1 = MetaSlot{Meta: m1, Valid: true}
	}
	return slot0, slot1, nil
}

// DualMeta reads both on-disk meta pages.
func (db *DB) DualMeta() (meta0 Meta, meta1 Meta, err error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return Meta{}, Meta{}, engine.ErrDBClosed
	}
	meta0, err = db.readMetaSlot(0)
	if err != nil {
		return Meta{}, Meta{}, err
	}
	meta1, err = db.readMetaSlot(1)
	if err != nil {
		return Meta{}, Meta{}, err
	}
	return meta0, meta1, nil
}

// ReadTreeAtRoot returns all key-value pairs reachable from root.
func (db *DB) ReadTreeAtRoot(root PageID) (map[string]string, error) {
	db.mu.RLock()
	if db.closed {
		db.mu.RUnlock()
		return nil, engine.ErrDBClosed
	}
	tx := &Tx{
		db:       db,
		writable: false,
		meta:     Meta{Root: root, HighWater: db.meta.HighWater, TxID: db.meta.TxID},
	}
	db.mu.RUnlock()
	return tx.All()
}

// RawPage reads the raw bytes of a committed page from storage.
func (db *DB) RawPage(pgid PageID) ([]byte, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, engine.ErrDBClosed
	}
	buf, _, err := db.readCommittedPageLocked(pgid)
	return buf, err
}

// Validate checks the structural integrity of the active committed B+Tree.
func (db *DB) Validate() error {
	return db.View(func(tx *Tx) error {
		return tx.Validate()
	})
}

// Close closes the database and its underlying storage.
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil
	}
	db.closed = true
	if db.writeTx != nil {
		db.writeTx.closed = true
		db.writeTx.dirty = nil
		db.writeTx = nil
	}
	clear(db.readerCounts)
	return db.storage.Close()
}
