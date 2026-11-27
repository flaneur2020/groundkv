package verified

import (
	"fmt"

	"github.com/flaneur2020/groundkv/engine"
	"github.com/flaneur2020/groundkv/verified/internal/dafnygen/GroundKVCodec"
	"github.com/flaneur2020/groundkv/verified/internal/dafnygen/GroundKVTypes"
	_dafny "github.com/flaneur2020/groundkv/verified/internal/dafnygen/dafny"
)

type dafnyPageCodec struct{}

var _ engine.PageCodec = dafnyPageCodec{}

func toDafnyHeader(h engine.PageHeader) GroundKVTypes.PageHeader {
	return GroundKVTypes.Companion_PageHeader_.Create_PageHeader_(
		uint64(h.PGID), h.Flags, h.Count, h.Checksum, h.TxID,
	)
}

func fromDafnyHeader(h GroundKVTypes.PageHeader) engine.PageHeader {
	return engine.PageHeader{
		PGID:     engine.PageID(h.Dtor_pgid()),
		Flags:    h.Dtor_flags(),
		Count:    h.Dtor_count(),
		Checksum: h.Dtor_checksum(),
		TxID:     h.Dtor_txid(),
	}
}

func toDafnyLeafEntries(entries []engine.LeafEntry) _dafny.Sequence {
	if len(entries) == 0 {
		return _dafny.EmptySeq
	}
	items := make([]interface{}, len(entries))
	for i, e := range entries {
		items[i] = GroundKVTypes.Companion_LeafEntry_.Create_LeafEntry_(
			_dafny.SeqOfBytes(e.Key),
			_dafny.SeqOfBytes(e.Value),
		)
	}
	return _dafny.SeqOf(items...)
}

func fromDafnyLeafEntries(seq _dafny.Sequence) []engine.LeafEntry {
	n := int(seq.Cardinality())
	out := make([]engine.LeafEntry, n)
	for i := 0; i < n; i++ {
		e := seq.Select(uint32(i)).(GroundKVTypes.LeafEntry)
		out[i] = engine.LeafEntry{
			Key:   _dafny.ToByteArray(e.Dtor_key()),
			Value: _dafny.ToByteArray(e.Dtor_value()),
		}
	}
	return out
}

func toDafnyBranchEntries(entries []engine.BranchEntry) _dafny.Sequence {
	if len(entries) == 0 {
		return _dafny.EmptySeq
	}
	items := make([]interface{}, len(entries))
	for i, e := range entries {
		items[i] = GroundKVTypes.Companion_BranchEntry_.Create_BranchEntry_(
			_dafny.SeqOfBytes(e.Key),
			uint64(e.Child),
		)
	}
	return _dafny.SeqOf(items...)
}

func fromDafnyBranchEntries(seq _dafny.Sequence) []engine.BranchEntry {
	n := int(seq.Cardinality())
	out := make([]engine.BranchEntry, n)
	for i := 0; i < n; i++ {
		e := seq.Select(uint32(i)).(GroundKVTypes.BranchEntry)
		out[i] = engine.BranchEntry{
			Key:   _dafny.ToByteArray(e.Dtor_key()),
			Child: engine.PageID(e.Dtor_child()),
		}
	}
	return out
}

func (dafnyPageCodec) EncodePageHeader(h engine.PageHeader) []byte {
	return _dafny.ToByteArray(GroundKVCodec.Companion_Default___.EncodePageHeader(toDafnyHeader(h)))
}

func (dafnyPageCodec) DecodePageHeader(buf []byte) (engine.PageHeader, error) {
	res := GroundKVCodec.Companion_Default___.DecodePageHeader(_dafny.SeqOfBytes(buf))
	if res.Is_Err() {
		return engine.PageHeader{}, fromDafnyErrCode(res.Dtor_error().(GroundKVTypes.ErrCode))
	}
	return fromDafnyHeader(res.Dtor_value().(GroundKVTypes.PageHeader)), nil
}

func (dafnyPageCodec) LeafEncodedSize(entries []engine.LeafEntry) int {
	return GroundKVCodec.Companion_Default___.LeafEncodedSize(toDafnyLeafEntries(entries)).Int()
}

func (dafnyPageCodec) EncodeLeafPage(pageSize int, pgid engine.PageID, txid uint64, entries []engine.LeafEntry) ([]byte, error) {
	res := GroundKVCodec.Companion_Default___.EncodeLeafPage(uint32(pageSize), uint64(pgid), txid, toDafnyLeafEntries(entries))
	if res.Is_Err() {
		return nil, fmt.Errorf("%w: leaf entries exceed page size %d",
			fromDafnyErrCode(res.Dtor_error().(GroundKVTypes.ErrCode)), pageSize)
	}
	return _dafny.ToByteArray(res.Dtor_value().(_dafny.Sequence)), nil
}

func (dafnyPageCodec) DecodeLeafPage(buf []byte, h engine.PageHeader) ([]engine.LeafEntry, error) {
	res := GroundKVCodec.Companion_Default___.DecodeLeafPage(_dafny.SeqOfBytes(buf), toDafnyHeader(h))
	if res.Is_Err() {
		return nil, fmt.Errorf("%w: leaf decode failed (flags %d)",
			fromDafnyErrCode(res.Dtor_error().(GroundKVTypes.ErrCode)), h.Flags)
	}
	return fromDafnyLeafEntries(res.Dtor_value().(_dafny.Sequence)), nil
}

func (dafnyPageCodec) BranchEncodedSize(entries []engine.BranchEntry) int {
	return GroundKVCodec.Companion_Default___.BranchEncodedSize(toDafnyBranchEntries(entries)).Int()
}

func (dafnyPageCodec) EncodeBranchPage(pageSize int, pgid engine.PageID, txid uint64, entries []engine.BranchEntry) ([]byte, error) {
	res := GroundKVCodec.Companion_Default___.EncodeBranchPage(uint32(pageSize), uint64(pgid), txid, toDafnyBranchEntries(entries))
	if res.Is_Err() {
		return nil, fmt.Errorf("%w: branch entries exceed page size %d",
			fromDafnyErrCode(res.Dtor_error().(GroundKVTypes.ErrCode)), pageSize)
	}
	return _dafny.ToByteArray(res.Dtor_value().(_dafny.Sequence)), nil
}

func (dafnyPageCodec) DecodeBranchPage(buf []byte, h engine.PageHeader) ([]engine.BranchEntry, error) {
	res := GroundKVCodec.Companion_Default___.DecodeBranchPage(_dafny.SeqOfBytes(buf), toDafnyHeader(h))
	if res.Is_Err() {
		return nil, fmt.Errorf("%w: branch decode failed (flags %d)",
			fromDafnyErrCode(res.Dtor_error().(GroundKVTypes.ErrCode)), h.Flags)
	}
	return fromDafnyBranchEntries(res.Dtor_value().(_dafny.Sequence)), nil
}
