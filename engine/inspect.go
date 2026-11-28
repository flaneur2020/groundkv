package engine

import "slices"

// HasDirtyAltMeta reports whether an aborted commit left an un-neutralized meta
// page in the alternate meta slot because both SyncMetaPage and the rollback
// invalidation write failed with I/O errors.
func (db *DB) HasDirtyAltMeta() bool {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.dirtyAltMeta
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
// optional freelist chain head. Used by tests and Quint MBT to assert that no
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
	slices.Sort(out)
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
// if one slot is torn/corrupted. Used by crash-recovery tests and Quint MBT.
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
// and Quint MBT state inspection.
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
// root page ID. Used by diagnostics and Quint MBT state verification.
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
// Used by COW immutability tests and Quint MBT drivers.
func (db *DB) RawPage(pgid PageID) ([]byte, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, ErrDBClosed
	}
	buf, _, err := db.readCommittedPageLocked(pgid)
	return buf, err
}
