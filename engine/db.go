package engine

import (
	"fmt"
	"sort"
	"sync"
)

// DB is a BoltDB-inspired embedded KV store backed by a copy-on-write B+Tree
// with MVCC reader-aware freelist page reclamation.
// It supports multiple concurrent MVCC readers and a single serialized writer.
type DB struct {
	mu            sync.RWMutex
	storage       Storage
	opts          Options
	core          Core
	meta          Meta
	writeTx       *Tx
	readerCounts  map[uint64]int
	freelist      FreelistPool
	reclaimedRuns uint64
	freelistPages []PageID
	dirtyAltMeta  bool
	closed        bool
}

// HasDirtyAltMeta reports whether an aborted commit left an un-neutralized meta
// page in the alternate meta slot because both SyncMetaPage and the rollback
// invalidation write failed with I/O errors.
func (db *DB) HasDirtyAltMeta() bool {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.dirtyAltMeta
}

// Open opens or initializes a GroundKV database backed by a file at path.
func Open(path string, opts *Options) (*DB, error) {
	st, err := OpenOSFile(path)
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

// OpenStorage opens or initializes a GroundKV database backed by a custom Storage.
func OpenStorage(st Storage, opts *Options) (*DB, error) {
	cfg, err := opts.withDefaults()
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
		core:         cfg.Core,
		readerCounts: make(map[uint64]int),
		freelist:     cfg.Core.NewFreelist(),
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
		buf := encodeMetaPageWithCore(db.core, db.opts.PageSize, slot, m)
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
	// Auto-detect on-disk PageSize so reopening with nil Options works for any
	// configured PageSize (even if meta page 0 was torn).
	candidateSizes := []int{db.opts.PageSize}
	for ps := MinPageSize; ps <= 65536; ps <<= 1 {
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
			err0 = fmt.Errorf("%w: meta0 pageSize %d != %d", ErrCorruptedMeta, meta0.PageSize, ps)
		}
		if err1 == nil && int(meta1.PageSize) != ps {
			err1 = fmt.Errorf("%w: meta1 pageSize %d != %d", ErrCorruptedMeta, meta1.PageSize, ps)
		}

		if chosen, selErr := db.core.SelectRecoveredMeta(meta0, err0 == nil, meta1, err1 == nil); selErr == nil {
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
		return fmt.Errorf("%w: meta0=(%v), meta1=(%v)", ErrCorruptedMeta, lastErr0, lastErr1)
	}

	// Verify that the active meta's root page header & checksum are intact so
	// durable bit-rot or a violated sync barrier fails loudly on Open rather
	// than silently rolling back an acknowledged commit.
	if db.meta.Root != 0 {
		if _, h, err := db.readCommittedPageLocked(db.meta.Root); err != nil {
			return fmt.Errorf("groundkv: recover root page %d: %w", db.meta.Root, err)
		} else if h.Flags != PageFlagLeaf && h.Flags != PageFlagBranch {
			return fmt.Errorf("%w: root page %d has invalid flags %d", ErrCorruptedPage, db.meta.Root, h.Flags)
		}
	}

	// Reload on-disk freelist into pending[activeMeta.TxID] so pages that may
	// still be referenced by the older meta slot (txid-1) are protected until
	// the next commit rotates the older meta slot.
	if !db.opts.NoFreelist && db.meta.Freelist != 0 {
		freedIDs, chainPages, err := db.readFreelistChainLocked(db.meta.Freelist)
		if err != nil {
			return fmt.Errorf("groundkv: recover freelist: %w", err)
		}
		db.freelistPages = chainPages
		db.freelist.AddPending(db.meta.TxID, freedIDs)
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
	return decodeMetaPageWithCore(db.core, buf, slot)
}

func (db *DB) readCommittedPage(pgid PageID) ([]byte, PageHeader, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.readCommittedPageLocked(pgid)
}

func (db *DB) readCommittedPageLocked(pgid PageID) ([]byte, PageHeader, error) {
	if db.closed {
		return nil, PageHeader{}, ErrDBClosed
	}
	buf := make([]byte, db.opts.PageSize)
	off := int64(pgid) * int64(db.opts.PageSize)
	n, err := db.storage.ReadAt(buf, off)
	if err != nil && n < db.opts.PageSize {
		return nil, PageHeader{}, err
	}
	h, err := readAndVerifyPageHeaderWithCore(db.core, buf, pgid)
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

// releaseBoundLocked returns the highest freeTxID whose freed pages are safe
// to recycle without overwriting any page reachable from either on-disk meta
// slot (currentTxID and currentTxID-1) or any open MVCC read snapshot.
func (db *DB) releaseBoundLocked() uint64 {
	minR, hasReader := db.minActiveReaderTxIDLocked()
	return db.core.ReleaseBound(db.meta.TxID, hasReader, minR)
}

// MinActiveReaderTxID returns the lowest TxID among currently open read-only
// MVCC snapshots, or (0, false) if no read snapshots are open.
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

// FreelistSnapshot summarizes the live freelist state for diagnostics and MBT.
type FreelistSnapshot struct {
	FreeIDs       []PageID
	PendingTxIDs  []uint64
	ReclaimedHits uint64
}

// FreelistState returns a consistent snapshot of the freelist's free pool,
// pending transaction IDs, and total recycled page allocations.
func (db *DB) FreelistState() FreelistSnapshot {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return FreelistSnapshot{
		FreeIDs:       db.freelist.FreeIDs(),
		PendingTxIDs:  db.freelist.PendingTxIDs(),
		ReclaimedHits: db.reclaimedRuns,
	}
}

// ReachablePages returns all page IDs reachable from the given B+Tree root and
// optional freelist chain head. Used by tests and FizzBee MBT to assert that no
// reachable page is ever present in the reusable free pool.
func (db *DB) ReachablePages(root PageID, freelistHead PageID) ([]PageID, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, ErrDBClosed
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
	if h.Flags == PageFlagBranch {
		entries, err := db.core.DecodeBranchPage(buf, h)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := db.collectTreePagesLocked(e.Child, visited); err != nil {
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
		return nil, ErrDBClosed
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
// Returns ErrWriteTxActive if another write transaction is currently open.
func (db *DB) BeginWrite() (*Tx, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil, ErrDBClosed
	}
	if db.writeTx != nil {
		return nil, ErrWriteTxActive
	}
	if !db.opts.NoFreelist {
		db.freelist.Release(db.releaseBoundLocked())
	}
	tx := &Tx{
		db:       db,
		writable: true,
		phase:    CommitPhaseActive,
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

// Update executes fn inside a read-write transaction, committing if fn returns nil
// and rolling back if fn or Commit returns an error (preventing writer-lock leaks).
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

// Get returns the latest committed value for key, or ErrNotFound.
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

// MetaSlot describes one of the two on-disk meta pages, including whether its
// CRC32 checksum and header are currently valid (false after a torn meta write).
type MetaSlot struct {
	Meta  Meta
	Valid bool
}

// InspectMeta reads both on-disk meta pages (Page 0 and Page 1) without failing
// if one slot is torn/corrupted. Used by crash-recovery tests and FizzBee MBT.
func (db *DB) InspectMeta() (slot0 MetaSlot, slot1 MetaSlot, err error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return MetaSlot{}, MetaSlot{}, ErrDBClosed
	}
	if m0, err0 := db.readMetaSlot(0); err0 == nil {
		slot0 = MetaSlot{Meta: m0, Valid: true}
	}
	if m1, err1 := db.readMetaSlot(1); err1 == nil {
		slot1 = MetaSlot{Meta: m1, Valid: true}
	}
	return slot0, slot1, nil
}

// DualMeta reads both on-disk meta pages (Page 0 and Page 1) for verification
// and FizzBee MBT state inspection.
func (db *DB) DualMeta() (meta0 Meta, meta1 Meta, err error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return Meta{}, Meta{}, ErrDBClosed
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

// ReadTreeAtRoot returns all key-value pairs reachable from a specific committed
// root page ID. Used by diagnostics and FizzBee MBT state verification.
func (db *DB) ReadTreeAtRoot(root PageID) (map[string]string, error) {
	db.mu.RLock()
	if db.closed {
		db.mu.RUnlock()
		return nil, ErrDBClosed
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
// Used by COW immutability tests and FizzBee MBT adapters.
func (db *DB) RawPage(pgid PageID) ([]byte, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, ErrDBClosed
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

// Close closes the database and its underlying storage. Any concurrent in-flight
// committed page reads finish before Close acquires db.mu, and subsequent reads
// on still-open read-only Tx handles cleanly return ErrDBClosed without races.
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
