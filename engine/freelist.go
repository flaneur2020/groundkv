package engine

import (
	"encoding/binary"
	"fmt"
	"sort"
)

const (
	// freelistHeaderExtra: next(uint64) chain pointer immediately after pageHeaderSize.
	freelistHeaderExtra = 8
	freelistEntrySize   = 8
)

// freelist tracks freed pages in two buckets (BoltDB / LMDB MVCC discipline):
//   - pending[txid]: pages freed by committed transaction txid that cannot be
//     reused yet because an older meta page or active MVCC reader may still
//     reference them.
//   - free: pages released once freeTxID <= releaseBound, safe to recycle.
type freelist struct {
	free          map[PageID]struct{}
	pending       map[uint64][]PageID
	reclaimedRuns uint64
}

func newFreelist() *freelist {
	return &freelist{
		free:    make(map[PageID]struct{}),
		pending: make(map[uint64][]PageID),
	}
}

// addPending registers pages freed by commit txid.
func (fl *freelist) addPending(txid uint64, ids []PageID) {
	if len(ids) == 0 {
		return
	}
	seen := make(map[PageID]struct{}, len(ids))
	for _, existing := range fl.pending[txid] {
		seen[existing] = struct{}{}
	}
	for _, id := range ids {
		if id < 2 {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		fl.pending[txid] = append(fl.pending[txid], id)
	}
	sort.Slice(fl.pending[txid], func(i, j int) bool { return fl.pending[txid][i] < fl.pending[txid][j] })
}

// release moves every pending bucket with freeTxID <= maxSafeTxID into the
// reusable free pool.
func (fl *freelist) release(maxSafeTxID uint64) {
	for txid, ids := range fl.pending {
		if txid <= maxSafeTxID {
			for _, id := range ids {
				if id >= 2 {
					fl.free[id] = struct{}{}
				}
			}
			delete(fl.pending, txid)
		}
	}
}

// pop removes and returns the lowest available free PageID (deterministic order).
func (fl *freelist) pop() (PageID, bool) {
	if len(fl.free) == 0 {
		return 0, false
	}
	var lowest PageID
	first := true
	for id := range fl.free {
		if first || id < lowest {
			lowest = id
			first = false
		}
	}
	delete(fl.free, lowest)
	return lowest, true
}

// returnToFree puts recycled pages back into free (used on write tx Rollback).
func (fl *freelist) returnToFree(ids []PageID) {
	for _, id := range ids {
		if id >= 2 {
			fl.free[id] = struct{}{}
		}
	}
}

func (fl *freelist) freeCount() int { return len(fl.free) }

func (fl *freelist) freeIDs() []PageID {
	out := make([]PageID, 0, len(fl.free))
	for id := range fl.free {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (fl *freelist) pendingTxIDs() []uint64 {
	out := make([]uint64, 0, len(fl.pending))
	for txid, ids := range fl.pending {
		if len(ids) > 0 {
			out = append(out, txid)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// snapshotAllIDs returns the deduplicated sorted union of free, pending, and
// extraIDs for persisting on disk.
func (fl *freelist) snapshotAllIDs(extraIDs []PageID) []PageID {
	uniq := make(map[PageID]struct{}, len(fl.free)+len(extraIDs))
	for id := range fl.free {
		if id >= 2 {
			uniq[id] = struct{}{}
		}
	}
	for _, ids := range fl.pending {
		for _, id := range ids {
			if id >= 2 {
				uniq[id] = struct{}{}
			}
		}
	}
	for _, id := range extraIDs {
		if id >= 2 {
			uniq[id] = struct{}{}
		}
	}
	out := make([]PageID, 0, len(uniq))
	for id := range uniq {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func maxFreelistIDsPerPage(pageSize int) int {
	return (pageSize - pageHeaderSize - freelistHeaderExtra) / freelistEntrySize
}

func encodeFreelistPage(pageSize int, pgid PageID, txid uint64, next PageID, ids []PageID) ([]byte, error) {
	if len(ids) > maxFreelistIDsPerPage(pageSize) {
		return nil, fmt.Errorf("%w: too many freelist IDs (%d) for page size %d", ErrValueTooLarge, len(ids), pageSize)
	}
	buf := make([]byte, pageSize)
	off := pageHeaderSize
	binary.LittleEndian.PutUint64(buf[off:off+8], uint64(next))
	off += freelistHeaderExtra
	for _, id := range ids {
		binary.LittleEndian.PutUint64(buf[off:off+8], uint64(id))
		off += freelistEntrySize
	}
	writePageHeader(buf, pageHeader{
		pgid:  pgid,
		flags: PageFlagFreelist,
		count: uint16(len(ids)),
		txid:  txid,
	})
	return buf, nil
}

func decodeFreelistPage(buf []byte, h pageHeader) (next PageID, ids []PageID, err error) {
	if h.flags != PageFlagFreelist {
		return 0, nil, fmt.Errorf("%w: expected freelist page, got flags %d", ErrCorruptedPage, h.flags)
	}
	if len(buf) < pageHeaderSize+freelistHeaderExtra+int(h.count)*freelistEntrySize {
		return 0, nil, ErrCorruptedPage
	}
	off := pageHeaderSize
	next = PageID(binary.LittleEndian.Uint64(buf[off : off+8]))
	off += freelistHeaderExtra
	ids = make([]PageID, int(h.count))
	for i := 0; i < int(h.count); i++ {
		ids[i] = PageID(binary.LittleEndian.Uint64(buf[off : off+8]))
		off += freelistEntrySize
	}
	return next, ids, nil
}

// readFreelistChain reads all freed PageIDs and the chain's own page IDs
// starting from headPGID.
func (db *DB) readFreelistChain(headPGID PageID) (freedIDs []PageID, chainPages []PageID, err error) {
	if headPGID == 0 {
		return nil, nil, nil
	}
	visited := make(map[PageID]bool)
	curr := headPGID
	for curr != 0 {
		if visited[curr] {
			return nil, nil, fmt.Errorf("%w: cycle in freelist chain at pgid %d", ErrCorruptedPage, curr)
		}
		visited[curr] = true
		buf, h, err := db.readCommittedPage(curr)
		if err != nil {
			return nil, nil, err
		}
		next, ids, err := decodeFreelistPage(buf, h)
		if err != nil {
			return nil, nil, err
		}
		chainPages = append(chainPages, curr)
		freedIDs = append(freedIDs, ids...)
		curr = next
	}
	return freedIDs, chainPages, nil
}
