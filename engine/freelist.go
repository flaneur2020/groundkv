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

// FreelistPool abstracts the pure in-memory MVCC pending/free page pool.
type FreelistPool interface {
	FreeCount() int
	AddPending(txid uint64, ids []PageID)
	Release(maxSafeTxID uint64)
	Pop() (PageID, bool)
	ReturnToFree(ids []PageID)
	FreeIDs() []PageID
	PendingTxIDs() []uint64
	SnapshotAllIDs(extraIDs []PageID) []PageID
}

// FreelistCore abstracts MVCC freelist page codecs, release-bound calculation,
// chain sizing, and pool construction.
type FreelistCore interface {
	MaxFreelistIDsPerPage(pageSize int) int
	EncodeFreelistPage(pageSize int, pgid PageID, txid uint64, next PageID, ids []PageID) ([]byte, error)
	DecodeFreelistPage(buf []byte, h PageHeader) (next PageID, ids []PageID, err error)
	ReleaseBound(currentTxID uint64, hasReader bool, minActiveReaderTxID uint64) uint64
	ComputeFreelistPageCount(initialAllLen, freeCount, localFreeLen, perPage int) int
	NewFreelist() FreelistPool
}

func encodeFreelistPageWithCore(c FreelistCore, pageSize int, pgid PageID, txid uint64, next PageID, ids []PageID) ([]byte, error) {
	buf, err := c.EncodeFreelistPage(pageSize, pgid, txid, next, ids)
	if err != nil {
		return nil, err
	}
	StampPageChecksum(buf)
	return buf, nil
}

// --- Pure-Go FreelistCore implementation ---

type goFreelistCore struct{}

func maxFreelistIDsPerPage(pageSize int) int {
	return (pageSize - pageHeaderSize - freelistHeaderExtra) / freelistEntrySize
}

func (goFreelistCore) MaxFreelistIDsPerPage(pageSize int) int {
	return maxFreelistIDsPerPage(pageSize)
}

func (goFreelistCore) EncodeFreelistPage(pageSize int, pgid PageID, txid uint64, next PageID, ids []PageID) ([]byte, error) {
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
	copy(buf[0:pageHeaderSize], (goPageCodec{}).EncodePageHeader(PageHeader{
		PGID:  pgid,
		Flags: PageFlagFreelist,
		Count: uint16(len(ids)),
		TxID:  txid,
	}))
	return buf, nil
}

func (goFreelistCore) DecodeFreelistPage(buf []byte, h PageHeader) (PageID, []PageID, error) {
	if h.Flags != PageFlagFreelist {
		return 0, nil, fmt.Errorf("%w: expected freelist page, got flags %d", ErrCorruptedPage, h.Flags)
	}
	if len(buf) < pageHeaderSize+freelistHeaderExtra+int(h.Count)*freelistEntrySize {
		return 0, nil, ErrCorruptedPage
	}
	off := pageHeaderSize
	next := PageID(binary.LittleEndian.Uint64(buf[off : off+8]))
	off += freelistHeaderExtra
	ids := make([]PageID, int(h.Count))
	for i := 0; i < int(h.Count); i++ {
		ids[i] = PageID(binary.LittleEndian.Uint64(buf[off : off+8]))
		off += freelistEntrySize
	}
	return next, ids, nil
}

func (goFreelistCore) ReleaseBound(currentTxID uint64, hasReader bool, minActiveReaderTxID uint64) uint64 {
	var bound uint64
	if currentTxID > 0 {
		bound = currentTxID - 1
	}
	if hasReader && minActiveReaderTxID < bound {
		bound = minActiveReaderTxID
	}
	return bound
}

func (goFreelistCore) ComputeFreelistPageCount(initialAllLen, freeCount, localFreeLen, perPage int) int {
	total := initialAllLen + localFreeLen
	nonReusable := initialAllLen - freeCount
	if nonReusable < 0 {
		nonReusable = 0
	}
	numPages := (total + perPage) / (perPage + 1)
	if nonReusable > 0 {
		if minForNonReusable := (nonReusable + perPage - 1) / perPage; minForNonReusable > numPages {
			numPages = minForNonReusable
		}
	}
	if numPages < 1 {
		numPages = 1
	}
	return numPages
}

func (goFreelistCore) NewFreelist() FreelistPool {
	return newFreelist()
}

// --- Pure-Go FreelistPool implementation ---

// freelist is the default pure-Go FreelistPool implementation tracking freed
// pages in two buckets (BoltDB / LMDB MVCC discipline):
//   - pending[txid]: pages freed by committed transaction txid that cannot be
//     reused yet because an older meta page or active MVCC reader may still
//     reference them.
//   - free: pages released once freeTxID <= releaseBound, safe to recycle.
type freelist struct {
	free    map[PageID]struct{}
	pending map[uint64][]PageID
}

func newFreelist() *freelist {
	return &freelist{
		free:    make(map[PageID]struct{}),
		pending: make(map[uint64][]PageID),
	}
}

func (fl *freelist) FreeCount() int {
	return len(fl.free)
}

// AddPending registers pages freed by commit txid.
func (fl *freelist) AddPending(txid uint64, ids []PageID) {
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

// Release moves every pending bucket with freeTxID <= maxSafeTxID into the
// reusable free pool.
func (fl *freelist) Release(maxSafeTxID uint64) {
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

// Pop removes and returns the lowest available free PageID (deterministic order).
func (fl *freelist) Pop() (PageID, bool) {
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

// ReturnToFree puts recycled pages back into free (used on write tx Rollback).
func (fl *freelist) ReturnToFree(ids []PageID) {
	for _, id := range ids {
		if id >= 2 {
			fl.free[id] = struct{}{}
		}
	}
}

func (fl *freelist) FreeIDs() []PageID {
	out := make([]PageID, 0, len(fl.free))
	for id := range fl.free {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (fl *freelist) PendingTxIDs() []uint64 {
	out := make([]uint64, 0, len(fl.pending))
	for txid, ids := range fl.pending {
		if len(ids) > 0 {
			out = append(out, txid)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// SnapshotAllIDs returns the deduplicated sorted union of free, pending, and
// extraIDs for persisting on disk.
func (fl *freelist) SnapshotAllIDs(extraIDs []PageID) []PageID {
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

// Package-internal default helpers used by tests and fuzzers.

func encodeFreelistPage(pageSize int, pgid PageID, txid uint64, next PageID, ids []PageID) ([]byte, error) {
	return encodeFreelistPageWithCore(defaultGoCore, pageSize, pgid, txid, next, ids)
}

func decodeFreelistPage(buf []byte, h PageHeader) (next PageID, ids []PageID, err error) {
	return defaultGoCore.DecodeFreelistPage(buf, h)
}

// readFreelistChainLocked reads all freed PageIDs and the chain's own page IDs
// starting from headPGID. Caller must hold db.mu (read or write) or own db during recover.
func (db *DB) readFreelistChainLocked(headPGID PageID) (freedIDs []PageID, chainPages []PageID, err error) {
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
		buf, h, err := db.readCommittedPageLocked(curr)
		if err != nil {
			return nil, nil, err
		}
		next, ids, err := db.core.DecodeFreelistPage(buf, h)
		if err != nil {
			return nil, nil, err
		}
		chainPages = append(chainPages, curr)
		freedIDs = append(freedIDs, ids...)
		curr = next
	}
	return freedIDs, chainPages, nil
}
