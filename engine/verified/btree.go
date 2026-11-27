package verified

import (
	"github.com/flaneur2020/groundkv/engine/core"
	"github.com/flaneur2020/groundkv/engine/verified/internal/dafnygen/GroundKVNode"
	_dafny "github.com/flaneur2020/groundkv/engine/verified/internal/dafnygen/dafny"
)

type dafnyBTreeNode struct{}

var _ core.BTreeNode = dafnyBTreeNode{}

func (dafnyBTreeNode) LeafGet(entries []core.LeafEntry, key []byte) ([]byte, bool) {
	opt := GroundKVNode.Companion_Default___.LeafGet(toDafnyLeafEntries(entries), _dafny.SeqOfBytes(key))
	if opt.Is_Some() {
		return _dafny.ToByteArray(opt.Dtor_value().(_dafny.Sequence)), true
	}
	return nil, false
}

func (dafnyBTreeNode) LeafPut(entries []core.LeafEntry, key, value []byte) []core.LeafEntry {
	return fromDafnyLeafEntries(GroundKVNode.Companion_Default___.LeafPut(
		toDafnyLeafEntries(entries), _dafny.SeqOfBytes(key), _dafny.SeqOfBytes(value),
	))
}

func (dafnyBTreeNode) LeafDelete(entries []core.LeafEntry, key []byte) ([]core.LeafEntry, bool) {
	updatedSeq, deleted := GroundKVNode.Companion_Default___.LeafDelete(
		toDafnyLeafEntries(entries), _dafny.SeqOfBytes(key),
	)
	if !deleted {
		return entries, false
	}
	return fromDafnyLeafEntries(updatedSeq), true
}

func (dafnyBTreeNode) BranchChildIndex(entries []core.BranchEntry, key []byte) int {
	return GroundKVNode.Companion_Default___.BranchChildIndex(
		toDafnyBranchEntries(entries), _dafny.SeqOfBytes(key),
	).Int()
}

func (dafnyBTreeNode) SplitLeafEntries(entries []core.LeafEntry, pageSize, maxKeys int) [][]core.LeafEntry {
	seq := GroundKVNode.Companion_Default___.SplitLeafEntries(
		toDafnyLeafEntries(entries), uint32(pageSize), _dafny.IntOfInt64(int64(maxKeys)),
	)
	n := int(seq.Cardinality())
	chunks := make([][]core.LeafEntry, n)
	for i := 0; i < n; i++ {
		chunks[i] = fromDafnyLeafEntries(seq.Select(uint32(i)).(_dafny.Sequence))
	}
	return chunks
}

func (dafnyBTreeNode) SplitBranchEntries(entries []core.BranchEntry, pageSize, maxKeys int) [][]core.BranchEntry {
	seq := GroundKVNode.Companion_Default___.SplitBranchEntries(
		toDafnyBranchEntries(entries), uint32(pageSize), _dafny.IntOfInt64(int64(maxKeys)),
	)
	n := int(seq.Cardinality())
	chunks := make([][]core.BranchEntry, n)
	for i := 0; i < n; i++ {
		chunks[i] = fromDafnyBranchEntries(seq.Select(uint32(i)).(_dafny.Sequence))
	}
	return chunks
}
