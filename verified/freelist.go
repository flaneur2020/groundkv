package verified

import (
	"fmt"

	"github.com/flaneur2020/groundkv/engine"
	"github.com/flaneur2020/groundkv/verified/dafnygen/GroundKVCodec"
	"github.com/flaneur2020/groundkv/verified/dafnygen/GroundKVFreelist"
	"github.com/flaneur2020/groundkv/verified/dafnygen/GroundKVTypes"
	_dafny "github.com/flaneur2020/groundkv/verified/dafnygen/dafny"
)

// freelist tracks freed pages in two buckets backed directly by the Z3-verified
// Dafny GroundKVFreelist.FreelistState.
type freelist struct {
	state         GroundKVFreelist.FreelistState
	reclaimedRuns uint64
}

func newFreelist() *freelist {
	return &freelist{
		state: GroundKVFreelist.Companion_Default___.EmptyFreelist(),
	}
}

func (fl *freelist) freeCount() int {
	return fl.state.Dtor_free().Cardinality().Int()
}

func (fl *freelist) addPending(txid uint64, ids []PageID) {
	if len(ids) == 0 {
		return
	}
	fl.state = GroundKVFreelist.Companion_Default___.AddPending(fl.state, txid, toDafnyPageIDSet(ids))
}

func (fl *freelist) release(maxSafeTxID uint64) {
	fl.state = GroundKVFreelist.Companion_Default___.ReleaseEligible(fl.state, maxSafeTxID)
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

func (fl *freelist) pop() (PageID, bool) {
	freeSet := fl.state.Dtor_free()
	if freeSet.Cardinality().Int() == 0 {
		return 0, false
	}
	lowest := GroundKVFreelist.Companion_Default___.MinPageIDInSeq(setToDafnyPageIDSeq(freeSet))
	fl.state = GroundKVFreelist.Companion_FreelistState_.Create_FreelistState_(
		freeSet.Difference(_dafny.SetOf(lowest)),
		fl.state.Dtor_pending(),
	)
	return PageID(lowest), true
}

func (fl *freelist) returnToFree(ids []PageID) {
	if len(ids) == 0 {
		return
	}
	fl.state = GroundKVFreelist.Companion_Default___.ReturnToFree(fl.state, toDafnyPageIDSet(ids))
}

func (fl *freelist) freeIDs() []PageID {
	seq := setToDafnyPageIDSeq(fl.state.Dtor_free())
	return fromDafnyPageIDs(GroundKVFreelist.Companion_Default___.SortDedupPageIDs(seq))
}

func (fl *freelist) pendingTxIDs() []uint64 {
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

func (fl *freelist) snapshotAllIDs(extraIDs []PageID) []PageID {
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
		return []PageID{}
	}
	return fromDafnyPageIDs(GroundKVFreelist.Companion_Default___.SortDedupPageIDs(_dafny.SeqOf(items...)))
}

func maxFreelistIDsPerPage(pageSize int) int {
	return GroundKVCodec.Companion_Default___.MaxFreelistIDsPerPage(uint32(pageSize)).Int()
}

func encodeFreelistPage(pageSize int, pgid PageID, txid uint64, next PageID, ids []PageID) ([]byte, error) {
	res := GroundKVCodec.Companion_Default___.EncodeFreelistPage(
		uint32(pageSize),
		uint64(pgid),
		txid,
		uint64(next),
		toDafnyPageIDs(ids),
	)
	if res.Is_Err() {
		return nil, fmt.Errorf("%w: too many freelist IDs (%d) for page size %d",
			fromDafnyErrCode(res.Dtor_error().(GroundKVTypes.ErrCode)), len(ids), pageSize)
	}
	buf := _dafny.ToByteArray(res.Dtor_value().(_dafny.Sequence))
	stampPageChecksum(buf)
	return buf, nil
}

func decodeFreelistPage(buf []byte, h pageHeader) (next PageID, ids []PageID, err error) {
	res := GroundKVCodec.Companion_Default___.DecodeFreelistPage(
		_dafny.SeqOfBytes(buf),
		toDafnyHeader(h),
	)
	if res.Is_Err() {
		return 0, nil, fmt.Errorf("%w: freelist decode failed (flags %d)",
			fromDafnyErrCode(res.Dtor_error().(GroundKVTypes.ErrCode)), h.flags)
	}
	dec := res.Dtor_value().(GroundKVCodec.DecodedFreelist)
	return PageID(dec.Dtor_next()), fromDafnyPageIDs(dec.Dtor_ids()), nil
}

func (db *DB) readFreelistChainLocked(headPGID PageID) (freedIDs []PageID, chainPages []PageID, err error) {
	if headPGID == 0 {
		return nil, nil, nil
	}
	visited := make(map[PageID]bool)
	curr := headPGID
	for curr != 0 {
		if visited[curr] {
			return nil, nil, fmt.Errorf("%w: cycle in freelist chain at pgid %d", engine.ErrCorruptedPage, curr)
		}
		visited[curr] = true
		buf, h, err := db.readCommittedPageLocked(curr)
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
