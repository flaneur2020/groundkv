package engine

import (
	"errors"
	"fmt"
)

// Tx represents either a read-only MVCC snapshot transaction or a single-writer
// copy-on-write transaction.
type Tx struct {
	db       *DB
	writable bool
	closed   bool
	phase    CommitPhase

	// meta is a private copy pinned when the transaction begins. For a write
	// transaction, meta.Root, meta.Freelist, and meta.HighWater advance in memory
	// as COW pages are allocated; meta.TxID is incremented during Commit.
	meta Meta

	// nextMeta holds the pending committed Meta between WriteMetaPage and SyncMetaPage.
	nextMeta Meta

	// dirty holds freshly allocated COW pages for a write transaction. None of
	// these pages overwrite pages reachable from any live meta page or reader.
	dirty map[PageID][]byte

	// freed tracks page IDs made unreachable by this write transaction.
	freed map[PageID]struct{}

	// recycled tracks page IDs popped from db.freelist during this write tx
	// so Rollback can return them cleanly without leaking pages.
	recycled []PageID

	// localFree holds uncommitted page IDs allocated and then superseded within
	// this same write transaction. They are safe to reuse immediately within the
	// same transaction because no committed meta page or MVCC reader can see them.
	localFree []PageID

	// newFreelistPages tracks the page IDs of the newly written freelist chain.
	newFreelistPages []PageID
}

// Writable reports whether this transaction permits mutations.
func (tx *Tx) Writable() bool { return tx.writable }

// TxID returns the transaction ID of the snapshot (or base txid for an uncommitted write tx).
func (tx *Tx) TxID() uint64 { return tx.meta.TxID }

// RootPageID returns the B+Tree root page ID visible to this transaction.
func (tx *Tx) RootPageID() PageID { return tx.meta.Root }

// CommitPhase returns the current durability commit phase of a write transaction:
// "STAGED", "DATA_WRITTEN", "DATA_SYNCED", "META_WRITTEN", or "IDLE" when closed.
func (tx *Tx) CommitPhase() string {
	if tx == nil || tx.closed || !tx.writable {
		return CommitPhaseIdle.String()
	}
	return tx.phase.String()
}

func (tx *Tx) checkReadable() error {
	if tx.closed {
		return ErrTxClosed
	}
	if tx.db.isClosed() {
		return ErrDBClosed
	}
	return nil
}

func (tx *Tx) checkMutable() error {
	if tx.closed {
		return ErrTxClosed
	}
	if !tx.writable {
		return ErrTxReadOnly
	}
	if tx.phase != CommitPhaseActive {
		return errors.New("groundkv: cannot mutate transaction after commit has started")
	}
	return nil
}

func (tx *Tx) checkCommitPhase(next CommitPhase, op string) error {
	if tx.closed {
		return ErrTxClosed
	}
	if !tx.writable {
		return ErrTxReadOnly
	}
	if !tx.db.core.ValidNextPhase(tx.phase, next) {
		return fmt.Errorf("groundkv: %s called out of order", op)
	}
	return nil
}

func (tx *Tx) resetScratch() {
	tx.dirty = nil
	tx.freed = nil
	tx.recycled = nil
	tx.localFree = nil
}

// Get retrieves the value for key as seen by this transaction.
// Returns ErrNotFound if the key does not exist.
func (tx *Tx) Get(key []byte) ([]byte, error) {
	if err := tx.checkReadable(); err != nil {
		return nil, err
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
	if err := tx.checkReadable(); err != nil {
		return err
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
	if err := tx.checkMutable(); err != nil {
		return err
	}
	newRoot, err := btreePut(tx, tx.meta.Root, key, value)
	if err != nil {
		return err
	}
	tx.meta.Root = newRoot
	return nil
}

// Delete removes key from the B+Tree using copy-on-write page allocation.
// Deleting a non-existent key is a no-op.
func (tx *Tx) Delete(key []byte) error {
	if err := tx.checkMutable(); err != nil {
		return err
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
	if err := tx.checkReadable(); err != nil {
		return err
	}
	return btreeValidate(tx, tx.meta.Root, tx.meta.HighWater)
}

// --- pageAccess implementation ---

func (tx *Tx) core() Core         { return tx.db.core }
func (tx *Tx) pageSize() int      { return tx.db.opts.PageSize }
func (tx *Tx) maxLeafKeys() int   { return tx.db.opts.MaxLeafKeys }
func (tx *Tx) maxBranchKeys() int { return tx.db.opts.MaxBranchKeys }
func (tx *Tx) txid() uint64       { return tx.meta.TxID + 1 }

func (tx *Tx) readPage(pgid PageID) ([]byte, PageHeader, error) {
	if tx.dirty != nil {
		if buf, ok := tx.dirty[pgid]; ok {
			h, err := verifyPageHeader(tx.db.core, buf, pgid)
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
		pgid, ok := tx.db.freelist.Pop()
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
	// If pgid was allocated earlier in this same uncommitted write tx, drop its
	// obsolete dirty buffer and recycle it immediately within this tx so
	// multi-op transactions don't inflate pending/freelist churn.
	if _, isLocal := tx.dirty[pgid]; isLocal {
		delete(tx.dirty, pgid)
		tx.localFree = append(tx.localFree, pgid)
		return
	}
	if tx.freed != nil {
		tx.freed[pgid] = struct{}{}
	}
}
