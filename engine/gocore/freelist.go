package gocore

import (
	"slices"

	"github.com/flaneur2020/groundkv/engine/core"
)

type goFreelist struct{}

var (
	_ core.Freelist     = goFreelist{}
	_ core.FreelistPool = (*freelist)(nil)
)

func (goFreelist) ReleaseBound(currentTxID uint64, hasReader bool, minActiveReaderTxID uint64) uint64 {
	var bound uint64
	if currentTxID > 0 {
		bound = currentTxID - 1
	}
	if hasReader && minActiveReaderTxID < bound {
		bound = minActiveReaderTxID
	}
	return bound
}

func (goFreelist) ComputeFreelistPageCount(initialAllLen, freeCount, localFreeLen, perPage int) int {
	total := initialAllLen + localFreeLen
	nonReusable := max(0, initialAllLen-freeCount)
	numPages := (total + perPage) / (perPage + 1)
	if nonReusable > 0 {
		numPages = max(numPages, (nonReusable+perPage-1)/perPage)
	}
	return max(1, numPages)
}

func (goFreelist) NewFreelist() core.FreelistPool {
	return &freelist{
		free:    make(map[core.PageID]struct{}),
		pending: make(map[uint64][]core.PageID),
	}
}

// freelist is the pure-Go core.FreelistPool tracking freed pages in two buckets:
//   - pending[txid]: pages freed by committed transaction txid still protected
//     by the dual-meta slot or an active MVCC reader.
//   - free: pages released once freeTxID <= releaseBound, safe to recycle.
type freelist struct {
	free    map[core.PageID]struct{}
	pending map[uint64][]core.PageID
}

func (fl *freelist) FreeCount() int {
	return len(fl.free)
}

func (fl *freelist) AddPending(txid uint64, ids []core.PageID) {
	if len(ids) == 0 {
		return
	}
	seen := make(map[core.PageID]struct{}, len(fl.pending[txid])+len(ids))
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
	slices.Sort(fl.pending[txid])
}

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

func (fl *freelist) Pop() (core.PageID, bool) {
	if len(fl.free) == 0 {
		return 0, false
	}
	var lowest core.PageID
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

func (fl *freelist) ReturnToFree(ids []core.PageID) {
	for _, id := range ids {
		if id >= 2 {
			fl.free[id] = struct{}{}
		}
	}
}

func (fl *freelist) FreeIDs() []core.PageID {
	out := make([]core.PageID, 0, len(fl.free))
	for id := range fl.free {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

func (fl *freelist) PendingTxIDs() []uint64 {
	out := make([]uint64, 0, len(fl.pending))
	for txid, ids := range fl.pending {
		if len(ids) > 0 {
			out = append(out, txid)
		}
	}
	slices.Sort(out)
	return out
}

func (fl *freelist) SnapshotAllIDs(extraIDs []core.PageID) []core.PageID {
	uniq := make(map[core.PageID]struct{}, len(fl.free)+len(extraIDs))
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
	out := make([]core.PageID, 0, len(uniq))
	for id := range uniq {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}
