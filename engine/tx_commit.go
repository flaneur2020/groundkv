package engine

import (
	"errors"
	"fmt"
	"slices"
)

// Commit atomically persists all COW pages allocated by a write transaction
// by executing the 4 durability phases in strict order:
//  1. WriteDataPages — write dirty B+Tree pages + COW freelist pages.
//  2. SyncDataPages  — fsync data pages BEFORE touching either meta page.
//  3. WriteMetaPage  — write the alternate meta slot (newTxID % 2).
//  4. SyncMetaPage   — fsync the meta page, update freelist, and publish in memory.
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

// WriteDataPages executes Commit Phase 1: encodes the COW freelist chain (when
// enabled) and writes all dirty COW pages in ascending PageID order without syncing yet.
func (tx *Tx) WriteDataPages() error {
	if err := tx.checkCommitPhase(CommitPhaseDataWritten, "WriteDataPages"); err != nil {
		return err
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
		slices.Sort(pgids)

		for _, id := range pgids {
			buf := tx.dirty[id]
			off := int64(id) * int64(tx.db.opts.PageSize)
			if _, err := tx.db.storage.WriteAt(buf, off); err != nil {
				return fmt.Errorf("groundkv: write page %d: %w", id, err)
			}
		}
	}
	tx.phase = CommitPhaseDataWritten
	if tx.db.opts.Tracer != nil {
		tx.db.mu.RLock()
		tx.db.emitTraceLocked("WriteDataPages")
		tx.db.mu.RUnlock()
	}
	return nil
}

func (tx *Tx) prepareFreelistPages() error {
	tx.db.mu.Lock()
	// Free the previous committed freelist chain pages (COW on the freelist itself).
	for _, oldID := range tx.db.freelistPages {
		if oldID >= 2 {
			tx.freed[oldID] = struct{}{}
		}
	}
	freeCount := tx.db.freelist.FreeCount()
	initialAll := tx.db.freelist.SnapshotAllIDs(tx.freedSlice())
	tx.db.mu.Unlock()

	if len(initialAll)+len(tx.localFree) == 0 {
		tx.meta.Freelist = 0
		tx.newFreelistPages = nil
		return nil
	}

	perPage := tx.db.core.MaxFreelistIDsPerPage(tx.pageSize())
	if tx.db.opts.MaxFreelistIDs > 0 && tx.db.opts.MaxFreelistIDs < perPage {
		perPage = tx.db.opts.MaxFreelistIDs
	}

	numPages := tx.db.core.ComputeFreelistPageCount(len(initialAll), freeCount, len(tx.localFree), perPage)

	chainIDs := make([]PageID, numPages)
	for i := 0; i < numPages; i++ {
		chainIDs[i] = tx.allocPageID()
	}

	// Any leftover tx.localFree pages that were not reused for chainIDs must be
	// recorded in tx.freed so they are not leaked on commit.
	for _, id := range tx.localFree {
		tx.freed[id] = struct{}{}
	}
	tx.localFree = nil

	// Recompute after allocating chainIDs (popping from localFree/free shrinks
	// the list so it always fits in numPages and never includes any live chainID).
	tx.db.mu.Lock()
	finalAll := tx.db.freelist.SnapshotAllIDs(tx.freedSlice())
	tx.db.mu.Unlock()

	for i, pgid := range chainIDs {
		start := i * perPage
		var chunk []PageID
		if start < len(finalAll) {
			end := min(start+perPage, len(finalAll))
			chunk = finalAll[start:end]
		}
		var next PageID
		if i+1 < len(chainIDs) {
			next = chainIDs[i+1]
		}
		buf, err := encodeFreelistPage(tx.db.core, tx.pageSize(), pgid, tx.txid(), next, chunk)
		if err != nil {
			return err
		}
		tx.dirty[pgid] = buf
	}

	tx.meta.Freelist = chainIDs[0]
	tx.newFreelistPages = chainIDs
	return nil
}

// SyncDataPages executes Commit Phase 2: fsyncs appended/recycled B+Tree and
// freelist data pages so they are guaranteed durable before the alternate meta page is written.
func (tx *Tx) SyncDataPages() error {
	if err := tx.checkCommitPhase(CommitPhaseDataSynced, "SyncDataPages"); err != nil {
		return err
	}

	tx.db.mu.Lock()
	needClearAlt := tx.db.dirtyAltMeta
	tx.db.mu.Unlock()
	if needClearAlt {
		// An earlier aborted tx left an uncommitted meta page in the alternate
		// meta slot because both SyncMetaPage and rollback's invalidation write
		// failed. Neutralize that slot BEFORE calling file-wide storage.Sync()
		// so fsync cannot resurrect the rolled-back transaction.
		slot := tx.db.core.MetaSlot(tx.meta.TxID + 1)
		zeros := make([]byte, tx.db.opts.PageSize)
		if _, err := tx.db.storage.WriteAt(zeros, int64(slot)*int64(tx.db.opts.PageSize)); err != nil {
			return fmt.Errorf("groundkv: clear dirty alt meta %d: %w", slot, err)
		}
		tx.db.mu.Lock()
		tx.db.dirtyAltMeta = false
		tx.db.mu.Unlock()
	}

	if len(tx.dirty) > 0 && !tx.db.opts.NoSync {
		if err := tx.db.storage.Sync(); err != nil {
			return fmt.Errorf("groundkv: sync data pages: %w", err)
		}
	}
	tx.phase = CommitPhaseDataSynced
	if tx.db.opts.Tracer != nil {
		tx.db.mu.RLock()
		tx.db.emitTraceLocked("SyncDataPages")
		tx.db.mu.RUnlock()
	}
	return nil
}

// WriteMetaPage executes Commit Phase 3: writes the alternate meta page slot
// (nextTxID % 2) to storage without syncing yet.
func (tx *Tx) WriteMetaPage() error {
	if err := tx.checkCommitPhase(CommitPhaseMetaWritten, "WriteMetaPage"); err != nil {
		return err
	}

	nextMeta := tx.meta
	nextMeta.TxID++
	slot := tx.db.core.MetaSlot(nextMeta.TxID)
	metaBuf := encodeMetaPage(tx.db.core, tx.db.opts.PageSize, slot, nextMeta)
	if _, err := tx.db.storage.WriteAt(metaBuf, int64(slot)*int64(tx.db.opts.PageSize)); err != nil {
		return fmt.Errorf("groundkv: write meta page %d: %w", slot, err)
	}
	tx.nextMeta = nextMeta
	tx.phase = CommitPhaseMetaWritten
	if tx.db.opts.Tracer != nil {
		tx.db.mu.RLock()
		tx.db.emitTraceLocked("WriteMetaPage")
		tx.db.mu.RUnlock()
	}
	return nil
}

// SyncMetaPage executes Commit Phase 4: fsyncs the alternate meta page, updates
// the freelist pending/reclaimed state, releases eligible pages under the current
// MVCC & dual-meta barrier, and publishes the newly committed Meta.
func (tx *Tx) SyncMetaPage() error {
	if err := tx.checkCommitPhase(CommitPhaseIdle, "SyncMetaPage"); err != nil {
		return err
	}
	if tx.phase != CommitPhaseMetaWritten {
		return errors.New("groundkv: SyncMetaPage called out of order")
	}

	slot := tx.db.core.MetaSlot(tx.nextMeta.TxID)
	if !tx.db.opts.NoSync {
		if err := tx.db.storage.Sync(); err != nil {
			return fmt.Errorf("groundkv: sync meta page %d: %w", slot, err)
		}
	}

	freedList := tx.freedSlice()

	tx.db.mu.Lock()
	tx.db.meta = tx.nextMeta
	if !tx.db.opts.NoFreelist {
		tx.db.freelist.AddPending(tx.nextMeta.TxID, freedList)
		tx.db.reclaimedRuns += uint64(len(tx.recycled))
		tx.db.freelistPages = tx.newFreelistPages
		tx.db.freelist.Release(tx.db.releaseBoundLocked())
	}
	tx.db.writeTx = nil
	tx.db.emitTraceLocked("SyncMetaPage")
	tx.db.mu.Unlock()

	tx.closed = true
	tx.resetScratch()
	return nil
}

// Rollback closes the transaction without committing any changes.
// For write transactions, all dirty COW pages are discarded and any pages
// popped from the freelist are returned cleanly without touching disk.
// For read transactions, closing may advance minActiveReaderTxID and release
// newly eligible pending pages into the reusable free pool.
func (tx *Tx) Rollback() error {
	if tx.closed {
		return nil
	}
	tx.closed = true

	var altMetaDirty bool
	if tx.writable && tx.phase >= CommitPhaseMetaWritten {
		// WriteMetaPage already wrote nextMeta into the alternate meta slot in
		// the OS page cache before SyncMetaPage failed. Overwrite that slot with
		// an invalid (zeroed) page so a subsequent transaction's file-wide
		// SyncDataPages() fsync cannot resurrect this rolled-back transaction.
		// The active committed slot (db.meta.TxID % 2) remains durable and untouched.
		slot := tx.db.core.MetaSlot(tx.nextMeta.TxID)
		zeros := make([]byte, tx.db.opts.PageSize)
		if _, err := tx.db.storage.WriteAt(zeros, int64(slot)*int64(tx.db.opts.PageSize)); err != nil {
			altMetaDirty = true
		}
	}

	tx.db.mu.Lock()
	if tx.db.closed {
		tx.db.mu.Unlock()
		tx.resetScratch()
		return nil
	}
	if tx.writable {
		if tx.phase >= CommitPhaseMetaWritten {
			tx.db.dirtyAltMeta = altMetaDirty
		}
		if !tx.db.opts.NoFreelist {
			if tx.phase >= CommitPhaseMetaWritten {
				// Advance HighWater past any pages written for nextMeta and hold
				// all of them (both recycled and newly allocated) in
				// pending[nextMeta.TxID] until a subsequent commit rotates past
				// that meta slot.
				held := append([]PageID(nil), tx.recycled...)
				if tx.nextMeta.HighWater > tx.db.meta.HighWater {
					for id := tx.db.meta.HighWater; id < tx.nextMeta.HighWater; id++ {
						held = append(held, id)
					}
					tx.db.meta.HighWater = tx.nextMeta.HighWater
				}
				tx.db.freelist.AddPending(tx.nextMeta.TxID, held)
			} else if len(tx.recycled) > 0 {
				tx.db.freelist.ReturnToFree(tx.recycled)
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
			tx.db.freelist.Release(tx.db.releaseBoundLocked())
		}
		tx.db.emitTraceLocked("CloseRead")
	}
	tx.db.mu.Unlock()

	tx.resetScratch()
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
	slices.Sort(out)
	return out
}
