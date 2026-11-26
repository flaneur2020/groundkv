package verified

import (
	"errors"
	"fmt"
	"sort"

	"github.com/flaneur2020/groundkv/engine"
	"github.com/flaneur2020/groundkv/verified/dafnygen/GroundKVEngine"
	"github.com/flaneur2020/groundkv/verified/dafnygen/GroundKVFreelist"
	_dafny "github.com/flaneur2020/groundkv/verified/dafnygen/dafny"
)

type txCommitPhase uint8

const (
	txPhaseActive txCommitPhase = iota
	txPhaseDataWritten
	txPhaseDataSynced
	txPhaseMetaWritten
)

// Tx represents either a read-only MVCC snapshot transaction or a single-writer
// copy-on-write transaction backed by GroundKV's Dafny-verified core.
type Tx struct {
	db       *DB
	writable bool
	closed   bool
	phase    txCommitPhase

	meta             Meta
	nextMeta         Meta
	dirty            map[PageID][]byte
	freed            map[PageID]struct{}
	recycled         []PageID
	localFree        []PageID
	newFreelistPages []PageID
}

// Writable reports whether this transaction permits mutations.
func (tx *Tx) Writable() bool { return tx.writable }

// TxID returns the transaction ID of the snapshot (or base txid for an uncommitted write tx).
func (tx *Tx) TxID() uint64 { return tx.meta.TxID }

// RootPageID returns the B+Tree root page ID visible to this transaction.
func (tx *Tx) RootPageID() PageID { return tx.meta.Root }

// CommitPhase returns the current durability commit phase of a write transaction.
func (tx *Tx) CommitPhase() string {
	if tx == nil || tx.closed || !tx.writable {
		return "IDLE"
	}
	switch tx.phase {
	case txPhaseActive:
		return "STAGED"
	case txPhaseDataWritten:
		return "DATA_WRITTEN"
	case txPhaseDataSynced:
		return "DATA_SYNCED"
	case txPhaseMetaWritten:
		return "META_WRITTEN"
	default:
		return "IDLE"
	}
}

// Get retrieves the value for key as seen by this transaction.
func (tx *Tx) Get(key []byte) ([]byte, error) {
	if tx.closed {
		return nil, engine.ErrTxClosed
	}
	if tx.db.isClosed() {
		return nil, engine.ErrDBClosed
	}
	if len(key) == 0 {
		return nil, engine.ErrEmptyKey
	}
	v, ok, err := btreeGet(tx, tx.meta.Root, key)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, engine.ErrNotFound
	}
	return v, nil
}

// Scan iterates over keys in [start, end) in lexicographical order.
func (tx *Tx) Scan(start, end []byte, fn func(k, v []byte) bool) error {
	if tx.closed {
		return engine.ErrTxClosed
	}
	if tx.db.isClosed() {
		return engine.ErrDBClosed
	}
	return btreeScan(tx, tx.meta.Root, start, end, fn)
}

// All returns a fresh map[string]string snapshot of all entries visible to this transaction.
func (tx *Tx) All() (map[string]string, error) {
	out := make(map[string]string)
	err := tx.Scan(nil, nil, func(k, v []byte) bool {
		out[string(k)] = string(v)
		return true
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Put inserts or updates key with value using copy-on-write page allocation.
func (tx *Tx) Put(key, value []byte) error {
	if tx.closed {
		return engine.ErrTxClosed
	}
	if !tx.writable {
		return engine.ErrTxReadOnly
	}
	if tx.phase != txPhaseActive {
		return errors.New("groundkv/verified: cannot mutate transaction after commit has started")
	}
	newRoot, err := btreePut(tx, tx.meta.Root, key, value)
	if err != nil {
		return err
	}
	tx.meta.Root = newRoot
	return nil
}

// Delete removes key from the B+Tree using copy-on-write page allocation.
func (tx *Tx) Delete(key []byte) error {
	if tx.closed {
		return engine.ErrTxClosed
	}
	if !tx.writable {
		return engine.ErrTxReadOnly
	}
	if tx.phase != txPhaseActive {
		return errors.New("groundkv/verified: cannot mutate transaction after commit has started")
	}
	newRoot, _, err := btreeDelete(tx, tx.meta.Root, key)
	if err != nil {
		return err
	}
	tx.meta.Root = newRoot
	return nil
}

// Validate checks B+Tree structural integrity for this transaction's view.
func (tx *Tx) Validate() error {
	if tx.closed {
		return engine.ErrTxClosed
	}
	if tx.db.isClosed() {
		return engine.ErrDBClosed
	}
	return btreeValidate(tx, tx.meta.Root, tx.meta.HighWater)
}

// Commit atomically persists all COW pages allocated by a write transaction
// using the 4-phase dual-meta commit protocol.
func (tx *Tx) Commit() error {
	if err := tx.WriteDataPages(); err != nil {
		return err
	}
	if err := tx.SyncDataPages(); err != nil {
		return err
	}
	if err := tx.WriteMetaPage(); err != nil {
		return err
	}
	return tx.SyncMetaPage()
}

// WriteDataPages executes Commit Phase 1.
func (tx *Tx) WriteDataPages() error {
	if tx.closed {
		return engine.ErrTxClosed
	}
	if !tx.writable {
		return engine.ErrTxReadOnly
	}
	if !GroundKVEngine.Companion_Default___.ValidNextPhase(
		toDafnyCommitPhase(tx.phase, tx.closed),
		GroundKVEngine.Companion_CommitPhase_.Create_PhaseDataWritten_(),
	) {
		return errors.New("groundkv/verified: WriteDataPages called out of order")
	}

	if !tx.db.opts.NoFreelist {
		if err := tx.prepareFreelistPages(); err != nil {
			return err
		}
	}

	if len(tx.dirty) > 0 {
		pgids := make([]PageID, 0, len(tx.dirty))
		for id := range tx.dirty {
			pgids = append(pgids, id)
		}
		sort.Slice(pgids, func(i, j int) bool { return pgids[i] < pgids[j] })

		for _, id := range pgids {
			buf := tx.dirty[id]
			off := int64(id) * int64(tx.db.opts.PageSize)
			if _, err := tx.db.storage.WriteAt(buf, off); err != nil {
				return fmt.Errorf("groundkv/verified: write page %d: %w", id, err)
			}
		}
	}
	tx.phase = txPhaseDataWritten
	if tx.db.opts.Tracer != nil {
		tx.db.mu.RLock()
		tx.db.emitTraceLocked("WriteDataPages")
		tx.db.mu.RUnlock()
	}
	return nil
}

func (tx *Tx) prepareFreelistPages() error {
	tx.db.mu.Lock()
	for _, oldID := range tx.db.freelistPages {
		if oldID >= 2 {
			tx.freed[oldID] = struct{}{}
		}
	}
	freeCount := tx.db.freelist.freeCount()
	initialAll := tx.db.freelist.snapshotAllIDs(tx.freedSlice())
	tx.db.mu.Unlock()

	if len(initialAll)+len(tx.localFree) == 0 {
		tx.meta.Freelist = 0
		tx.newFreelistPages = nil
		return nil
	}

	perPage := maxFreelistIDsPerPage(tx.pageSize())
	if tx.db.opts.MaxFreelistIDs > 0 && tx.db.opts.MaxFreelistIDs < perPage {
		perPage = tx.db.opts.MaxFreelistIDs
	}

	// Exact minimal chain page count via Z3-verified GroundKVFreelist.ComputeFreelistPageCount.
	numPages := GroundKVFreelist.Companion_Default___.ComputeFreelistPageCount(
		_dafny.IntOfInt64(int64(len(initialAll))),
		_dafny.IntOfInt64(int64(freeCount)),
		_dafny.IntOfInt64(int64(len(tx.localFree))),
		_dafny.IntOfInt64(int64(perPage)),
	).Int()

	chainIDs := make([]PageID, numPages)
	for i := 0; i < numPages; i++ {
		chainIDs[i] = tx.allocPageID()
	}

	for _, id := range tx.localFree {
		tx.freed[id] = struct{}{}
	}
	tx.localFree = nil

	tx.db.mu.Lock()
	finalAll := tx.db.freelist.snapshotAllIDs(tx.freedSlice())
	tx.db.mu.Unlock()

	for i, pgid := range chainIDs {
		start := i * perPage
		var chunk []PageID
		if start < len(finalAll) {
			end := start + perPage
			if end > len(finalAll) {
				end = len(finalAll)
			}
			chunk = finalAll[start:end]
		}
		var next PageID
		if i+1 < len(chainIDs) {
			next = chainIDs[i+1]
		}
		buf, err := encodeFreelistPage(tx.pageSize(), pgid, tx.txid(), next, chunk)
		if err != nil {
			return err
		}
		tx.dirty[pgid] = buf
	}

	tx.meta.Freelist = chainIDs[0]
	tx.newFreelistPages = chainIDs
	return nil
}

// SyncDataPages executes Commit Phase 2 (including the dirtyAltMeta fence).
func (tx *Tx) SyncDataPages() error {
	if tx.closed {
		return engine.ErrTxClosed
	}
	if !tx.writable {
		return engine.ErrTxReadOnly
	}
	if !GroundKVEngine.Companion_Default___.ValidNextPhase(
		toDafnyCommitPhase(tx.phase, tx.closed),
		GroundKVEngine.Companion_CommitPhase_.Create_PhaseDataSynced_(),
	) {
		return errors.New("groundkv/verified: SyncDataPages called out of order")
	}

	tx.db.mu.Lock()
	needClearAlt := tx.db.dirtyAltMeta
	tx.db.mu.Unlock()
	if needClearAlt {
		slot := metaSlot(tx.meta.TxID + 1)
		zeros := make([]byte, tx.db.opts.PageSize)
		if _, err := tx.db.storage.WriteAt(zeros, int64(slot)*int64(tx.db.opts.PageSize)); err != nil {
			return fmt.Errorf("groundkv/verified: clear dirty alt meta %d: %w", slot, err)
		}
		tx.db.mu.Lock()
		tx.db.dirtyAltMeta = false
		tx.db.mu.Unlock()
	}

	if len(tx.dirty) > 0 && !tx.db.opts.NoSync {
		if err := tx.db.storage.Sync(); err != nil {
			return fmt.Errorf("groundkv/verified: sync data pages: %w", err)
		}
	}
	tx.phase = txPhaseDataSynced
	if tx.db.opts.Tracer != nil {
		tx.db.mu.RLock()
		tx.db.emitTraceLocked("SyncDataPages")
		tx.db.mu.RUnlock()
	}
	return nil
}

// WriteMetaPage executes Commit Phase 3.
func (tx *Tx) WriteMetaPage() error {
	if tx.closed {
		return engine.ErrTxClosed
	}
	if !tx.writable {
		return engine.ErrTxReadOnly
	}
	if !GroundKVEngine.Companion_Default___.ValidNextPhase(
		toDafnyCommitPhase(tx.phase, tx.closed),
		GroundKVEngine.Companion_CommitPhase_.Create_PhaseMetaWritten_(),
	) {
		return errors.New("groundkv/verified: WriteMetaPage called out of order")
	}

	nextMeta := tx.meta
	nextMeta.TxID++
	slot := metaSlot(nextMeta.TxID)
	metaBuf := encodeMetaPage(tx.db.opts.PageSize, slot, nextMeta)
	if _, err := tx.db.storage.WriteAt(metaBuf, int64(slot)*int64(tx.db.opts.PageSize)); err != nil {
		return fmt.Errorf("groundkv/verified: write meta page %d: %w", slot, err)
	}
	tx.nextMeta = nextMeta
	tx.phase = txPhaseMetaWritten
	if tx.db.opts.Tracer != nil {
		tx.db.mu.RLock()
		tx.db.emitTraceLocked("WriteMetaPage")
		tx.db.mu.RUnlock()
	}
	return nil
}

// SyncMetaPage executes Commit Phase 4.
func (tx *Tx) SyncMetaPage() error {
	if tx.closed {
		return engine.ErrTxClosed
	}
	if !tx.writable {
		return engine.ErrTxReadOnly
	}
	if !GroundKVEngine.Companion_Default___.ValidNextPhase(
		toDafnyCommitPhase(tx.phase, tx.closed),
		GroundKVEngine.Companion_CommitPhase_.Create_PhaseIdle_(),
	) || tx.phase != txPhaseMetaWritten {
		return errors.New("groundkv/verified: SyncMetaPage called out of order")
	}

	slot := metaSlot(tx.nextMeta.TxID)
	if !tx.db.opts.NoSync {
		if err := tx.db.storage.Sync(); err != nil {
			return fmt.Errorf("groundkv/verified: sync meta page %d: %w", slot, err)
		}
	}

	freedList := tx.freedSlice()

	tx.db.mu.Lock()
	tx.db.meta = tx.nextMeta
	if !tx.db.opts.NoFreelist {
		tx.db.freelist.addPending(tx.nextMeta.TxID, freedList)
		tx.db.freelist.reclaimedRuns += uint64(len(tx.recycled))
		tx.db.freelistPages = tx.newFreelistPages
		tx.db.freelist.release(tx.db.releaseBoundLocked())
	}
	tx.db.writeTx = nil
	tx.db.emitTraceLocked("SyncMetaPage")
	tx.db.mu.Unlock()

	tx.closed = true
	tx.dirty = nil
	tx.freed = nil
	tx.recycled = nil
	tx.localFree = nil
	return nil
}

// Rollback closes the transaction without committing any changes.
func (tx *Tx) Rollback() error {
	if tx.closed {
		return nil
	}
	tx.closed = true

	var altMetaDirty bool
	if tx.writable && tx.phase >= txPhaseMetaWritten {
		slot := metaSlot(tx.nextMeta.TxID)
		zeros := make([]byte, tx.db.opts.PageSize)
		if _, err := tx.db.storage.WriteAt(zeros, int64(slot)*int64(tx.db.opts.PageSize)); err != nil {
			altMetaDirty = true
		}
	}

	tx.db.mu.Lock()
	if tx.db.closed {
		tx.db.mu.Unlock()
		tx.dirty = nil
		tx.freed = nil
		tx.recycled = nil
		tx.localFree = nil
		return nil
	}
	if tx.writable {
		if tx.phase >= txPhaseMetaWritten {
			tx.db.dirtyAltMeta = altMetaDirty
		}
		if !tx.db.opts.NoFreelist {
			if tx.phase >= txPhaseMetaWritten {
				held := append([]PageID(nil), tx.recycled...)
				if tx.nextMeta.HighWater > tx.db.meta.HighWater {
					for id := tx.db.meta.HighWater; id < tx.nextMeta.HighWater; id++ {
						held = append(held, id)
					}
					tx.db.meta.HighWater = tx.nextMeta.HighWater
				}
				tx.db.freelist.addPending(tx.nextMeta.TxID, held)
			} else if len(tx.recycled) > 0 {
				tx.db.freelist.returnToFree(tx.recycled)
			}
		}
		if tx.db.writeTx == tx {
			tx.db.writeTx = nil
		}
		tx.db.emitTraceLocked("RollbackWrite")
	} else {
		if c := tx.db.readerCounts[tx.meta.TxID]; c <= 1 {
			delete(tx.db.readerCounts, tx.meta.TxID)
		} else {
			tx.db.readerCounts[tx.meta.TxID] = c - 1
		}
		if !tx.db.opts.NoFreelist {
			tx.db.freelist.release(tx.db.releaseBoundLocked())
		}
		tx.db.emitTraceLocked("CloseRead")
	}
	tx.db.mu.Unlock()

	tx.dirty = nil
	tx.freed = nil
	tx.recycled = nil
	tx.localFree = nil
	return nil
}

func (tx *Tx) freedSlice() []PageID {
	if len(tx.freed) == 0 {
		return nil
	}
	out := make([]PageID, 0, len(tx.freed))
	for id := range tx.freed {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (tx *Tx) pageSize() int      { return tx.db.opts.PageSize }
func (tx *Tx) maxLeafKeys() int   { return tx.db.opts.MaxLeafKeys }
func (tx *Tx) maxBranchKeys() int { return tx.db.opts.MaxBranchKeys }
func (tx *Tx) txid() uint64       { return tx.meta.TxID + 1 }

func (tx *Tx) readPage(pgid PageID) ([]byte, pageHeader, error) {
	if tx.dirty != nil {
		if buf, ok := tx.dirty[pgid]; ok {
			h, err := readAndVerifyPageHeader(buf, pgid)
			return buf, h, err
		}
	}
	return tx.db.readCommittedPage(pgid)
}

func (tx *Tx) allocPageID() PageID {
	if len(tx.localFree) > 0 {
		last := len(tx.localFree) - 1
		pgid := tx.localFree[last]
		tx.localFree = tx.localFree[:last]
		return pgid
	}
	if !tx.db.opts.NoFreelist {
		tx.db.mu.Lock()
		pgid, ok := tx.db.freelist.pop()
		tx.db.mu.Unlock()
		if ok {
			tx.recycled = append(tx.recycled, pgid)
			return pgid
		}
	}
	pgid := tx.meta.HighWater
	tx.meta.HighWater++
	return pgid
}

func (tx *Tx) allocPage(encode func(pgid PageID) ([]byte, error)) (PageID, error) {
	pgid := tx.allocPageID()
	buf, err := encode(pgid)
	if err != nil {
		if !tx.db.opts.NoFreelist {
			tx.localFree = append(tx.localFree, pgid)
		}
		return 0, err
	}
	tx.dirty[pgid] = buf
	return pgid, nil
}

func (tx *Tx) freePage(pgid PageID) {
	if pgid < 2 || tx.db.opts.NoFreelist {
		return
	}
	if _, isLocal := tx.dirty[pgid]; isLocal {
		delete(tx.dirty, pgid)
		tx.localFree = append(tx.localFree, pgid)
		return
	}
	if tx.freed != nil {
		tx.freed[pgid] = struct{}{}
	}
}
