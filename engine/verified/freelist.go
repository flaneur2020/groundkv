package verified

import (
	"github.com/flaneur2020/groundkv/engine/core"
	"github.com/flaneur2020/groundkv/engine/verified/internal/dafnygen/GroundKVFreelist"
	_dafny "github.com/flaneur2020/groundkv/engine/verified/internal/dafnygen/dafny"
)

type dafnyFreelistCore struct{}

var (
	_ core.Freelist     = dafnyFreelistCore{}
	_ core.FreelistPool = (*dafnyFreelist)(nil)
)

func toDafnyPageIDSet(ids []core.PageID) _dafny.Set {
	if len(ids) == 0 {
		return _dafny.EmptySet
	}
	sb := _dafny.NewBuilder()
	for _, id := range ids {
		sb.Add(uint64(id))
	}
	return sb.ToSet()
}

func appendDafnySet(dst []interface{}, s _dafny.Set) []interface{} {
	for iter := _dafny.Iterate(s.Elements()); ; {
		v, ok := iter()
		if !ok {
			return dst
		}
		dst = append(dst, v)
	}
}

func setToDafnyPageIDSeq(s _dafny.Set) _dafny.Sequence {
	n := s.Cardinality().Int()
	if n == 0 {
		return _dafny.EmptySeq
	}
	return _dafny.SeqOf(appendDafnySet(make([]interface{}, 0, n), s)...)
}

func (dafnyFreelistCore) ReleaseBound(currentTxID uint64, hasReader bool, minActiveReaderTxID uint64) uint64 {
	return GroundKVFreelist.Companion_Default___.ReleaseBound(currentTxID, hasReader, minActiveReaderTxID)
}

func (dafnyFreelistCore) ComputeFreelistPageCount(initialAllLen, freeCount, localFreeLen, perPage int) int {
	return GroundKVFreelist.Companion_Default___.ComputeFreelistPageCount(
		_dafny.IntOfInt64(int64(initialAllLen)),
		_dafny.IntOfInt64(int64(freeCount)),
		_dafny.IntOfInt64(int64(localFreeLen)),
		_dafny.IntOfInt64(int64(perPage)),
	).Int()
}

func (dafnyFreelistCore) NewFreelist() core.FreelistPool {
	return &dafnyFreelist{state: GroundKVFreelist.Companion_Default___.EmptyFreelist()}
}

// --- dafnyFreelist: core.FreelistPool backed by GroundKVFreelist.FreelistState ---

type dafnyFreelist struct {
	state GroundKVFreelist.FreelistState
}

func (fl *dafnyFreelist) FreeCount() int {
	return fl.state.Dtor_free().Cardinality().Int()
}

func (fl *dafnyFreelist) AddPending(txid uint64, ids []core.PageID) {
	if len(ids) == 0 {
		return
	}
	fl.state = GroundKVFreelist.Companion_Default___.AddPending(fl.state, txid, toDafnyPageIDSet(ids))
}

func (fl *dafnyFreelist) Release(maxSafeTxID uint64) {
	fl.state = GroundKVFreelist.Companion_Default___.ReleaseEligible(fl.state, maxSafeTxID)
}

func (fl *dafnyFreelist) Pop() (core.PageID, bool) {
	freeSet := fl.state.Dtor_free()
	if freeSet.Cardinality().Int() == 0 {
		return 0, false
	}
	lowest := GroundKVFreelist.Companion_Default___.MinPageIDInSeq(setToDafnyPageIDSeq(freeSet))
	fl.state = GroundKVFreelist.Companion_FreelistState_.Create_FreelistState_(
		freeSet.Difference(_dafny.SetOf(lowest)),
		fl.state.Dtor_pending(),
	)
	return core.PageID(lowest), true
}

func (fl *dafnyFreelist) ReturnToFree(ids []core.PageID) {
	if len(ids) == 0 {
		return
	}
	fl.state = GroundKVFreelist.Companion_Default___.ReturnToFree(fl.state, toDafnyPageIDSet(ids))
}

func (fl *dafnyFreelist) FreeIDs() []core.PageID {
	seq := setToDafnyPageIDSeq(fl.state.Dtor_free())
	return fromDafnyPageIDs(GroundKVFreelist.Companion_Default___.SortDedupPageIDs(seq))
}

func (fl *dafnyFreelist) PendingTxIDs() []uint64 {
	pending := fl.state.Dtor_pending()
	var items []interface{}
	for iter := _dafny.Iterate(pending.Keys().Elements()); ; {
		k, ok := iter()
		if !ok {
			break
		}
		v, found := pending.Find(k)
		if found && v.(_dafny.Set).Cardinality().Int() > 0 {
			items = append(items, k.(uint64))
		}
	}
	if len(items) == 0 {
		return []uint64{}
	}
	sorted := GroundKVFreelist.Companion_Default___.SortDedupPageIDs(_dafny.SeqOf(items...))
	n := int(sorted.Cardinality())
	out := make([]uint64, n)
	for i := 0; i < n; i++ {
		out[i] = sorted.Select(uint32(i)).(uint64)
	}
	return out
}

func (fl *dafnyFreelist) SnapshotAllIDs(extraIDs []core.PageID) []core.PageID {
	items := appendDafnySet(nil, fl.state.Dtor_free())
	pending := fl.state.Dtor_pending()
	for iter := _dafny.Iterate(pending.Values().Elements()); ; {
		v, ok := iter()
		if !ok {
			break
		}
		items = appendDafnySet(items, v.(_dafny.Set))
	}
	for _, id := range extraIDs {
		if id >= 2 {
			items = append(items, uint64(id))
		}
	}
	if len(items) == 0 {
		return []core.PageID{}
	}
	return fromDafnyPageIDs(GroundKVFreelist.Companion_Default___.SortDedupPageIDs(_dafny.SeqOf(items...)))
}
