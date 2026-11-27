package verified

import (
	"fmt"

	"github.com/flaneur2020/groundkv/engine"
	"github.com/flaneur2020/groundkv/engine/verified/internal/dafnygen/GroundKVCodec"
	"github.com/flaneur2020/groundkv/engine/verified/internal/dafnygen/GroundKVFreelist"
	"github.com/flaneur2020/groundkv/engine/verified/internal/dafnygen/GroundKVTypes"
	_dafny "github.com/flaneur2020/groundkv/engine/verified/internal/dafnygen/dafny"
)

type dafnyFreelistCore struct{}

var (
	_ engine.FreelistCore = dafnyFreelistCore{}
	_ engine.FreelistPool = (*dafnyFreelist)(nil)
)

func toDafnyPageIDs(ids []engine.PageID) _dafny.Sequence {
	if len(ids) == 0 {
		return _dafny.EmptySeq
	}
	items := make([]interface{}, len(ids))
	for i, id := range ids {
		items[i] = uint64(id)
	}
	return _dafny.SeqOf(items...)
}

func fromDafnyPageIDs(seq _dafny.Sequence) []engine.PageID {
	n := int(seq.Cardinality())
	out := make([]engine.PageID, n)
	for i := 0; i < n; i++ {
		out[i] = engine.PageID(seq.Select(uint32(i)).(uint64))
	}
	return out
}

func toDafnyPageIDSet(ids []engine.PageID) _dafny.Set {
	if len(ids) == 0 {
		return _dafny.EmptySet
	}
	sb := _dafny.NewBuilder()
	for _, id := range ids {
		sb.Add(uint64(id))
	}
	return sb.ToSet()
}

func setToDafnyPageIDSeq(s _dafny.Set) _dafny.Sequence {
	n := s.Cardinality().Int()
	if n == 0 {
		return _dafny.EmptySeq
	}
	items := make([]interface{}, 0, n)
	for iter := _dafny.Iterate(s.Elements()); ; {
		v, ok := iter()
		if !ok {
			break
		}
		items = append(items, v)
	}
	return _dafny.SeqOf(items...)
}

func (dafnyFreelistCore) MaxFreelistIDsPerPage(pageSize int) int {
	return GroundKVCodec.Companion_Default___.MaxFreelistIDsPerPage(uint32(pageSize)).Int()
}

func (dafnyFreelistCore) EncodeFreelistPage(pageSize int, pgid engine.PageID, txid uint64, next engine.PageID, ids []engine.PageID) ([]byte, error) {
	res := GroundKVCodec.Companion_Default___.EncodeFreelistPage(uint32(pageSize), uint64(pgid), txid, uint64(next), toDafnyPageIDs(ids))
	if res.Is_Err() {
		return nil, fmt.Errorf("%w: too many freelist IDs (%d) for page size %d",
			fromDafnyErrCode(res.Dtor_error().(GroundKVTypes.ErrCode)), len(ids), pageSize)
	}
	return _dafny.ToByteArray(res.Dtor_value().(_dafny.Sequence)), nil
}

func (dafnyFreelistCore) DecodeFreelistPage(buf []byte, h engine.PageHeader) (engine.PageID, []engine.PageID, error) {
	res := GroundKVCodec.Companion_Default___.DecodeFreelistPage(_dafny.SeqOfBytes(buf), toDafnyHeader(h))
	if res.Is_Err() {
		return 0, nil, fmt.Errorf("%w: freelist decode failed (flags %d)",
			fromDafnyErrCode(res.Dtor_error().(GroundKVTypes.ErrCode)), h.Flags)
	}
	dec := res.Dtor_value().(GroundKVCodec.DecodedFreelist)
	return engine.PageID(dec.Dtor_next()), fromDafnyPageIDs(dec.Dtor_ids()), nil
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

func (dafnyFreelistCore) NewFreelist() engine.FreelistPool {
	return &dafnyFreelist{state: GroundKVFreelist.Companion_Default___.EmptyFreelist()}
}

// --- dafnyFreelist: engine.FreelistPool backed by GroundKVFreelist.FreelistState ---

type dafnyFreelist struct {
	state GroundKVFreelist.FreelistState
}

func (fl *dafnyFreelist) FreeCount() int {
	return fl.state.Dtor_free().Cardinality().Int()
}

func (fl *dafnyFreelist) AddPending(txid uint64, ids []engine.PageID) {
	if len(ids) == 0 {
		return
	}
	fl.state = GroundKVFreelist.Companion_Default___.AddPending(fl.state, txid, toDafnyPageIDSet(ids))
}

func (fl *dafnyFreelist) Release(maxSafeTxID uint64) {
	fl.state = GroundKVFreelist.Companion_Default___.ReleaseEligible(fl.state, maxSafeTxID)
}

func (fl *dafnyFreelist) Pop() (engine.PageID, bool) {
	freeSet := fl.state.Dtor_free()
	if freeSet.Cardinality().Int() == 0 {
		return 0, false
	}
	lowest := GroundKVFreelist.Companion_Default___.MinPageIDInSeq(setToDafnyPageIDSeq(freeSet))
	fl.state = GroundKVFreelist.Companion_FreelistState_.Create_FreelistState_(
		freeSet.Difference(_dafny.SetOf(lowest)),
		fl.state.Dtor_pending(),
	)
	return engine.PageID(lowest), true
}

func (fl *dafnyFreelist) ReturnToFree(ids []engine.PageID) {
	if len(ids) == 0 {
		return
	}
	fl.state = GroundKVFreelist.Companion_Default___.ReturnToFree(fl.state, toDafnyPageIDSet(ids))
}

func (fl *dafnyFreelist) FreeIDs() []engine.PageID {
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

func (fl *dafnyFreelist) SnapshotAllIDs(extraIDs []engine.PageID) []engine.PageID {
	var items []interface{}
	for iter := _dafny.Iterate(fl.state.Dtor_free().Elements()); ; {
		v, ok := iter()
		if !ok {
			break
		}
		items = append(items, v.(uint64))
	}
	pending := fl.state.Dtor_pending()
	for iter := _dafny.Iterate(pending.Values().Elements()); ; {
		v, ok := iter()
		if !ok {
			break
		}
		for elem := _dafny.Iterate(v.(_dafny.Set).Elements()); ; {
			id, ok2 := elem()
			if !ok2 {
				break
			}
			items = append(items, id.(uint64))
		}
	}
	for _, id := range extraIDs {
		if id >= 2 {
			items = append(items, uint64(id))
		}
	}
	if len(items) == 0 {
		return []engine.PageID{}
	}
	return fromDafnyPageIDs(GroundKVFreelist.Companion_Default___.SortDedupPageIDs(_dafny.SeqOf(items...)))
}
