package engine

import (
	"errors"
	"fmt"
	"sort"
)

type txCommitPhase uint8

const (
	txPhaseActive txCommitPhase = iota
	txPhaseDataWritten
	txPhaseDataSynced
	txPhaseMetaWritten
)

// Tx represents either a read-only MVCC snapshot transaction or a single-writer
// copy-on-write transaction.
type Tx struct {
	db       *DB
	writable bool
	closed   bool
	phase    txCommitPhase

	// meta is a private copy pinned when the transaction begins. For a write
	// transaction, meta.Root and meta.HighWater advance in memory as COW pages
	// are allocated; meta.TxID is incremented during Commit.
	meta Meta

	// nextMeta holds the pending committed Meta between WriteMetaPage and SyncMetaPage.
	nextMeta Meta

	// dirty holds freshly allocated COW pages for a write transaction. None of
	// these pages overwrite existing committed pages.
	dirty map[PageID][]byte
}

// Writable reports whether this transaction permits mutations.
func (tx *Tx) Writable() bool { return tx.writable }

// TxID returns the transaction ID of the snapshot (or base txid for an uncommitted write tx).
func (tx *Tx) TxID() uint64 { return tx.meta.TxID }

// RootPageID returns the B+Tree root page ID visible to this transaction.
func (tx *Tx) RootPageID() PageID { return tx.meta.Root }

// Get retrieves the value for key as seen by this transaction.
// Returns ErrNotFound if the key does not exist.
func (tx *Tx) Get(key []byte) ([]byte, error) {
	if tx.closed {
		return nil, ErrTxClosed
	}
	if len(key) == 0 {
		return nil, ErrEmptyKey
	}
	v, ok, err := btreeGet(tx, tx.meta.Root, key)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotFound
	}
	return v, nil
}

// Scan iterates over keys in [start, end) in lexicographical order.
// Passing nil for start starts at the smallest key; passing nil for end iterates to the end.
// Iteration stops early if fn returns false.
func (tx *Tx) Scan(start, end []byte, fn func(k, v []byte) bool) error {
	if tx.closed {
		return ErrTxClosed
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

// Put inserts or updates key with value using append-only copy-on-write page allocation.
func (tx *Tx) Put(key, value []byte) error {
	if tx.closed {
		return ErrTxClosed
	}
	if !tx.writable {
		return ErrTxReadOnly
	}
	if tx.phase != txPhaseActive {
		return errors.New("groundkv: cannot mutate transaction after commit has started")
	}
	newRoot, err := btreePut(tx, tx.meta.Root, key, value)
	if err != nil {
		return err
	}
	tx.meta.Root = newRoot
	return nil
}

// Delete removes key from the B+Tree using append-only copy-on-write page allocation.
// Deleting a non-existent key is a no-op.
func (tx *Tx) Delete(key []byte) error {
	if tx.closed {
		return ErrTxClosed
	}
	if !tx.writable {
		return ErrTxReadOnly
	}
	if tx.phase != txPhaseActive {
		return errors.New("groundkv: cannot mutate transaction after commit has started")
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
		return ErrTxClosed
	}
	return btreeValidate(tx, tx.meta.Root, tx.meta.HighWater)
}

// Commit atomically persists all COW pages allocated by a write transaction
// by executing the 4 durability phases in strict order:
//  1. WriteDataPages — append dirty B+Tree pages at newly allocated PageIDs.
//  2. SyncDataPages  — fsync data pages BEFORE touching either meta page.
//  3. WriteMetaPage  — write the alternate meta slot (newTxID % 2).
//  4. SyncMetaPage   — fsync the meta page and publish in memory.
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

// WriteDataPages executes Commit Phase 1: writes all dirty COW pages in
// ascending PageID order without syncing yet. Exposed so durability tests and
// FizzBee MBT harnesses can inject crashes between commit phases directly on
// the production commit path.
func (tx *Tx) WriteDataPages() error {
	if tx.closed {
		return ErrTxClosed
	}
	if !tx.writable {
		return ErrTxReadOnly
	}
	if tx.phase != txPhaseActive {
		return errors.New("groundkv: WriteDataPages called out of order")
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
				return fmt.Errorf("groundkv: write page %d: %w", id, err)
			}
		}
	}
	tx.phase = txPhaseDataWritten
	return nil
}

// SyncDataPages executes Commit Phase 2: fsyncs appended B+Tree data pages so
// they are guaranteed durable before the alternate meta page is written.
func (tx *Tx) SyncDataPages() error {
	if tx.closed {
		return ErrTxClosed
	}
	if !tx.writable {
		return ErrTxReadOnly
	}
	if tx.phase != txPhaseDataWritten {
		return errors.New("groundkv: SyncDataPages called out of order")
	}

	if len(tx.dirty) > 0 && !tx.db.opts.NoSync {
		if err := tx.db.storage.Sync(); err != nil {
			return fmt.Errorf("groundkv: sync data pages: %w", err)
		}
	}
	tx.phase = txPhaseDataSynced
	return nil
}

// WriteMetaPage executes Commit Phase 3: writes the alternate meta page slot
// (nextTxID % 2) to storage without syncing yet.
func (tx *Tx) WriteMetaPage() error {
	if tx.closed {
		return ErrTxClosed
	}
	if !tx.writable {
		return ErrTxReadOnly
	}
	if tx.phase != txPhaseDataSynced {
		return errors.New("groundkv: WriteMetaPage called out of order")
	}

	nextMeta := tx.meta
	nextMeta.TxID++
	slot := metaSlot(nextMeta.TxID)
	metaBuf := encodeMetaPage(tx.db.opts.PageSize, slot, nextMeta)
	if _, err := tx.db.storage.WriteAt(metaBuf, int64(slot)*int64(tx.db.opts.PageSize)); err != nil {
		return fmt.Errorf("groundkv: write meta page %d: %w", slot, err)
	}
	tx.nextMeta = nextMeta
	tx.phase = txPhaseMetaWritten
	return nil
}

// SyncMetaPage executes Commit Phase 4: fsyncs the alternate meta page and
// publishes the newly committed Meta to subsequent readers.
func (tx *Tx) SyncMetaPage() error {
	if tx.closed {
		return ErrTxClosed
	}
	if !tx.writable {
		return ErrTxReadOnly
	}
	if tx.phase != txPhaseMetaWritten {
		return errors.New("groundkv: SyncMetaPage called out of order")
	}

	slot := metaSlot(tx.nextMeta.TxID)
	if !tx.db.opts.NoSync {
		if err := tx.db.storage.Sync(); err != nil {
			return fmt.Errorf("groundkv: sync meta page %d: %w", slot, err)
		}
	}

	tx.db.mu.Lock()
	tx.db.meta = tx.nextMeta
	tx.db.writeTx = nil
	tx.db.mu.Unlock()

	tx.closed = true
	tx.dirty = nil
	return nil
}

// Rollback closes the transaction without committing any changes.
// For write transactions, all dirty COW pages are discarded without touching disk.
func (tx *Tx) Rollback() error {
	if tx.closed {
		return nil
	}
	tx.closed = true
	tx.dirty = nil

	tx.db.mu.Lock()
	if tx.writable {
		if tx.db.writeTx == tx {
			tx.db.writeTx = nil
		}
	} else {
		if tx.db.activeReaders > 0 {
			tx.db.activeReaders--
		}
	}
	tx.db.mu.Unlock()
	return nil
}

// --- pageAccess implementation ---

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

func (tx *Tx) allocPage(encode func(pgid PageID) ([]byte, error)) (PageID, error) {
	pgid := tx.meta.HighWater
	buf, err := encode(pgid)
	if err != nil {
		return 0, err
	}
	tx.meta.HighWater++
	tx.dirty[pgid] = buf
	return pgid, nil
}
