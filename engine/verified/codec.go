package verified

import (
	"fmt"

	"github.com/flaneur2020/groundkv/engine/core"
	"github.com/flaneur2020/groundkv/engine/verified/internal/dafnygen/GroundKVCodec"
	"github.com/flaneur2020/groundkv/engine/verified/internal/dafnygen/GroundKVTypes"
	_dafny "github.com/flaneur2020/groundkv/engine/verified/internal/dafnygen/dafny"
)

type dafnyCodec struct{}

var _ core.Codec = dafnyCodec{}

func toDafnyHeader(h core.PageHeader) GroundKVTypes.PageHeader {
	return GroundKVTypes.Companion_PageHeader_.Create_PageHeader_(
		uint64(h.PGID), h.Flags, h.Count, h.Checksum, h.TxID,
	)
}

func fromDafnyHeader(h GroundKVTypes.PageHeader) core.PageHeader {
	return core.PageHeader{
		PGID:     core.PageID(h.Dtor_pgid()),
		Flags:    h.Dtor_flags(),
		Count:    h.Dtor_count(),
		Checksum: h.Dtor_checksum(),
		TxID:     h.Dtor_txid(),
	}
}

func toDafnyMeta(m core.Meta) GroundKVTypes.Meta {
	return GroundKVTypes.Companion_Meta_.Create_Meta_(
		m.Magic, m.Version, m.PageSize, uint64(m.Root), uint64(m.HighWater), m.TxID, uint64(m.Freelist),
	)
}

func fromDafnyMeta(m GroundKVTypes.Meta) core.Meta {
	return core.Meta{
		Magic:     m.Dtor_magic(),
		Version:   m.Dtor_version(),
		PageSize:  m.Dtor_pageSize(),
		Root:      core.PageID(m.Dtor_root()),
		HighWater: core.PageID(m.Dtor_highWater()),
		TxID:      m.Dtor_txid(),
		Freelist:  core.PageID(m.Dtor_freelist()),
	}
}

func toDafnyLeafEntries(entries []core.LeafEntry) _dafny.Sequence {
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

func fromDafnyLeafEntries(seq _dafny.Sequence) []core.LeafEntry {
	n := int(seq.Cardinality())
	out := make([]core.LeafEntry, n)
	for i := 0; i < n; i++ {
		e := seq.Select(uint32(i)).(GroundKVTypes.LeafEntry)
		out[i] = core.LeafEntry{
			Key:   _dafny.ToByteArray(e.Dtor_key()),
			Value: _dafny.ToByteArray(e.Dtor_value()),
		}
	}
	return out
}

func toDafnyBranchEntries(entries []core.BranchEntry) _dafny.Sequence {
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

func fromDafnyBranchEntries(seq _dafny.Sequence) []core.BranchEntry {
	n := int(seq.Cardinality())
	out := make([]core.BranchEntry, n)
	for i := 0; i < n; i++ {
		e := seq.Select(uint32(i)).(GroundKVTypes.BranchEntry)
		out[i] = core.BranchEntry{
			Key:   _dafny.ToByteArray(e.Dtor_key()),
			Child: core.PageID(e.Dtor_child()),
		}
	}
	return out
}

func toDafnyPageIDs(ids []core.PageID) _dafny.Sequence {
	if len(ids) == 0 {
		return _dafny.EmptySeq
	}
	items := make([]interface{}, len(ids))
	for i, id := range ids {
		items[i] = uint64(id)
	}
	return _dafny.SeqOf(items...)
}

func fromDafnyPageIDs(seq _dafny.Sequence) []core.PageID {
	n := int(seq.Cardinality())
	out := make([]core.PageID, n)
	for i := 0; i < n; i++ {
		out[i] = core.PageID(seq.Select(uint32(i)).(uint64))
	}
	return out
}

func (dafnyCodec) EncodePageHeader(h core.PageHeader) []byte {
	return _dafny.ToByteArray(GroundKVCodec.Companion_Default___.EncodePageHeader(toDafnyHeader(h)))
}

func (dafnyCodec) DecodePageHeader(buf []byte) (core.PageHeader, error) {
	res := GroundKVCodec.Companion_Default___.DecodePageHeader(_dafny.SeqOfBytes(buf))
	if res.Is_Err() {
		return core.PageHeader{}, fromDafnyErrCode(res.Dtor_error().(GroundKVTypes.ErrCode))
	}
	return fromDafnyHeader(res.Dtor_value().(GroundKVTypes.PageHeader)), nil
}

func (dafnyCodec) MetaSlot(txid uint64) core.PageID {
	return core.PageID(GroundKVCodec.Companion_Default___.MetaSlot(txid))
}

func (dafnyCodec) EncodeMetaPage(pageSize int, slot core.PageID, m core.Meta) ([]byte, error) {
	res := GroundKVCodec.Companion_Default___.EncodeMetaPage(uint32(pageSize), uint64(slot), toDafnyMeta(m))
	if res.Is_Err() {
		return nil, fromDafnyErrCode(res.Dtor_error().(GroundKVTypes.ErrCode))
	}
	return _dafny.ToByteArray(res.Dtor_value().(_dafny.Sequence)), nil
}

func (dafnyCodec) DecodeMetaPayload(buf []byte, slot core.PageID, h core.PageHeader) (core.Meta, error) {
	res := GroundKVCodec.Companion_Default___.DecodeMetaPayload(_dafny.SeqOfBytes(buf), uint64(slot), toDafnyHeader(h))
	if res.Is_Err() {
		return core.Meta{}, fmt.Errorf("%w: invalid meta page %d",
			fromDafnyErrCode(res.Dtor_error().(GroundKVTypes.ErrCode)), slot)
	}
	return fromDafnyMeta(res.Dtor_value().(GroundKVTypes.Meta)), nil
}

func (dafnyCodec) LeafEncodedSize(entries []core.LeafEntry) int {
	return GroundKVCodec.Companion_Default___.LeafEncodedSize(toDafnyLeafEntries(entries)).Int()
}

func (dafnyCodec) EncodeLeafPage(pageSize int, pgid core.PageID, txid uint64, entries []core.LeafEntry) ([]byte, error) {
	res := GroundKVCodec.Companion_Default___.EncodeLeafPage(uint32(pageSize), uint64(pgid), txid, toDafnyLeafEntries(entries))
	if res.Is_Err() {
		return nil, fmt.Errorf("%w: leaf entries exceed page size %d",
			fromDafnyErrCode(res.Dtor_error().(GroundKVTypes.ErrCode)), pageSize)
	}
	return _dafny.ToByteArray(res.Dtor_value().(_dafny.Sequence)), nil
}

func (dafnyCodec) DecodeLeafPage(buf []byte, h core.PageHeader) ([]core.LeafEntry, error) {
	res := GroundKVCodec.Companion_Default___.DecodeLeafPage(_dafny.SeqOfBytes(buf), toDafnyHeader(h))
	if res.Is_Err() {
		return nil, fmt.Errorf("%w: leaf decode failed (flags %d)",
			fromDafnyErrCode(res.Dtor_error().(GroundKVTypes.ErrCode)), h.Flags)
	}
	return fromDafnyLeafEntries(res.Dtor_value().(_dafny.Sequence)), nil
}

func (dafnyCodec) BranchEncodedSize(entries []core.BranchEntry) int {
	return GroundKVCodec.Companion_Default___.BranchEncodedSize(toDafnyBranchEntries(entries)).Int()
}

func (dafnyCodec) EncodeBranchPage(pageSize int, pgid core.PageID, txid uint64, entries []core.BranchEntry) ([]byte, error) {
	res := GroundKVCodec.Companion_Default___.EncodeBranchPage(uint32(pageSize), uint64(pgid), txid, toDafnyBranchEntries(entries))
	if res.Is_Err() {
		return nil, fmt.Errorf("%w: branch entries exceed page size %d",
			fromDafnyErrCode(res.Dtor_error().(GroundKVTypes.ErrCode)), pageSize)
	}
	return _dafny.ToByteArray(res.Dtor_value().(_dafny.Sequence)), nil
}

func (dafnyCodec) DecodeBranchPage(buf []byte, h core.PageHeader) ([]core.BranchEntry, error) {
	res := GroundKVCodec.Companion_Default___.DecodeBranchPage(_dafny.SeqOfBytes(buf), toDafnyHeader(h))
	if res.Is_Err() {
		return nil, fmt.Errorf("%w: branch decode failed (flags %d)",
			fromDafnyErrCode(res.Dtor_error().(GroundKVTypes.ErrCode)), h.Flags)
	}
	return fromDafnyBranchEntries(res.Dtor_value().(_dafny.Sequence)), nil
}

func (dafnyCodec) MaxFreelistIDsPerPage(pageSize int) int {
	return GroundKVCodec.Companion_Default___.MaxFreelistIDsPerPage(uint32(pageSize)).Int()
}

func (dafnyCodec) EncodeFreelistPage(pageSize int, pgid core.PageID, txid uint64, next core.PageID, ids []core.PageID) ([]byte, error) {
	res := GroundKVCodec.Companion_Default___.EncodeFreelistPage(uint32(pageSize), uint64(pgid), txid, uint64(next), toDafnyPageIDs(ids))
	if res.Is_Err() {
		return nil, fmt.Errorf("%w: too many freelist IDs (%d) for page size %d",
			fromDafnyErrCode(res.Dtor_error().(GroundKVTypes.ErrCode)), len(ids), pageSize)
	}
	return _dafny.ToByteArray(res.Dtor_value().(_dafny.Sequence)), nil
}

func (dafnyCodec) DecodeFreelistPage(buf []byte, h core.PageHeader) (core.PageID, []core.PageID, error) {
	res := GroundKVCodec.Companion_Default___.DecodeFreelistPage(_dafny.SeqOfBytes(buf), toDafnyHeader(h))
	if res.Is_Err() {
		return 0, nil, fmt.Errorf("%w: freelist decode failed (flags %d)",
			fromDafnyErrCode(res.Dtor_error().(GroundKVTypes.ErrCode)), h.Flags)
	}
	dec := res.Dtor_value().(GroundKVCodec.DecodedFreelist)
	return core.PageID(dec.Dtor_next()), fromDafnyPageIDs(dec.Dtor_ids()), nil
}
