package verified

import (
	"github.com/flaneur2020/groundkv/engine"
	"github.com/flaneur2020/groundkv/verified/internal/dafnygen/GroundKVNode"
	_dafny "github.com/flaneur2020/groundkv/verified/internal/dafnygen/dafny"
)

type dafnyBTreeCore struct{}

var _ engine.BTreeCore = dafnyBTreeCore{}

func (dafnyBTreeCore) LeafGet(entries []engine.LeafEntry, key []byte) ([]byte, bool) {
	opt := GroundKVNode.Companion_Default___.LeafGet(toDafnyLeafEntries(entries), _dafny.SeqOfBytes(key))
	if opt.Is_Some() {
		return _dafny.ToByteArray(opt.Dtor_value().(_dafny.Sequence)), true
	}
	return nil, false
}

func (dafnyBTreeCore) LeafPut(entries []engine.LeafEntry, key, value []byte) []engine.LeafEntry {
	return fromDafnyLeafEntries(GroundKVNode.Companion_Default___.LeafPut(
		toDafnyLeafEntries(entries), _dafny.SeqOfBytes(key), _dafny.SeqOfBytes(value),
	))
}

func (dafnyBTreeCore) LeafDelete(entries []engine.LeafEntry, key []byte) ([]engine.LeafEntry, bool) {
	updatedSeq, deleted := GroundKVNode.Companion_Default___.LeafDelete(
		toDafnyLeafEntries(entries), _dafny.SeqOfBytes(key),
	)
	if !deleted {
		return entries, false
	}
	return fromDafnyLeafEntries(updatedSeq), true
}

func (dafnyBTreeCore) BranchChildIndex(entries []engine.BranchEntry, key []byte) int {
	return GroundKVNode.Companion_Default___.BranchChildIndex(
		toDafnyBranchEntries(entries), _dafny.SeqOfBytes(key),
	).Int()
}

func (dafnyBTreeCore) SplitLeafEntries(entries []engine.LeafEntry, pageSize, maxKeys int) [][]engine.LeafEntry {
	seq := GroundKVNode.Companion_Default___.SplitLeafEntries(
		toDafnyLeafEntries(entries), uint32(pageSize), _dafny.IntOfInt64(int64(maxKeys)),
	)
	n := int(seq.Cardinality())
	chunks := make([][]engine.LeafEntry, n)
	for i := 0; i < n; i++ {
		chunks[i] = fromDafnyLeafEntries(seq.Select(uint32(i)).(_dafny.Sequence))
	}
	return chunks
}

func (dafnyBTreeCore) SplitBranchEntries(entries []engine.BranchEntry, pageSize, maxKeys int) [][]engine.BranchEntry {
	seq := GroundKVNode.Companion_Default___.SplitBranchEntries(
		toDafnyBranchEntries(entries), uint32(pageSize), _dafny.IntOfInt64(int64(maxKeys)),
	)
	n := int(seq.Cardinality())
	chunks := make([][]engine.BranchEntry, n)
	for i := 0; i < n; i++ {
		chunks[i] = fromDafnyBranchEntries(seq.Select(uint32(i)).(_dafny.Sequence))
	}
	return chunks
}
