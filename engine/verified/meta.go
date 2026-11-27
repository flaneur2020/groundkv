package verified

import (
	"fmt"

	"github.com/flaneur2020/groundkv/engine"
	"github.com/flaneur2020/groundkv/engine/verified/internal/dafnygen/GroundKVCodec"
	"github.com/flaneur2020/groundkv/engine/verified/internal/dafnygen/GroundKVEngine"
	"github.com/flaneur2020/groundkv/engine/verified/internal/dafnygen/GroundKVTypes"
	_dafny "github.com/flaneur2020/groundkv/engine/verified/internal/dafnygen/dafny"
)

type dafnyMetaCore struct{}

var _ engine.MetaCore = dafnyMetaCore{}

func toDafnyMeta(m engine.Meta) GroundKVTypes.Meta {
	return GroundKVTypes.Companion_Meta_.Create_Meta_(
		m.Magic, m.Version, m.PageSize, uint64(m.Root), uint64(m.HighWater), m.TxID, uint64(m.Freelist),
	)
}

func fromDafnyMeta(m GroundKVTypes.Meta) engine.Meta {
	return engine.Meta{
		Magic:     m.Dtor_magic(),
		Version:   m.Dtor_version(),
		PageSize:  m.Dtor_pageSize(),
		Root:      engine.PageID(m.Dtor_root()),
		HighWater: engine.PageID(m.Dtor_highWater()),
		TxID:      m.Dtor_txid(),
		Freelist:  engine.PageID(m.Dtor_freelist()),
	}
}

func (dafnyMetaCore) MetaSlot(txid uint64) engine.PageID {
	return engine.PageID(GroundKVCodec.Companion_Default___.MetaSlot(txid))
}

func (dafnyMetaCore) EncodeMetaPage(pageSize int, slot engine.PageID, m engine.Meta) ([]byte, error) {
	res := GroundKVCodec.Companion_Default___.EncodeMetaPage(uint32(pageSize), uint64(slot), toDafnyMeta(m))
	if res.Is_Err() {
		return nil, fromDafnyErrCode(res.Dtor_error().(GroundKVTypes.ErrCode))
	}
	return _dafny.ToByteArray(res.Dtor_value().(_dafny.Sequence)), nil
}

func (dafnyMetaCore) DecodeMetaPayload(buf []byte, slot engine.PageID, h engine.PageHeader) (engine.Meta, error) {
	res := GroundKVCodec.Companion_Default___.DecodeMetaPayload(_dafny.SeqOfBytes(buf), uint64(slot), toDafnyHeader(h))
	if res.Is_Err() {
		return engine.Meta{}, fmt.Errorf("%w: invalid meta page %d",
			fromDafnyErrCode(res.Dtor_error().(GroundKVTypes.ErrCode)), slot)
	}
	return fromDafnyMeta(res.Dtor_value().(GroundKVTypes.Meta)), nil
}

func (dafnyMetaCore) SelectRecoveredMeta(meta0 engine.Meta, ok0 bool, meta1 engine.Meta, ok1 bool) (engine.Meta, error) {
	opt0 := GroundKVTypes.Companion_Option_.Create_None_()
	if ok0 {
		opt0 = GroundKVTypes.Companion_Option_.Create_Some_(toDafnyMeta(meta0))
	}
	opt1 := GroundKVTypes.Companion_Option_.Create_None_()
	if ok1 {
		opt1 = GroundKVTypes.Companion_Option_.Create_Some_(toDafnyMeta(meta1))
	}
	res := GroundKVEngine.Companion_Default___.SelectRecoveredMeta(opt0, opt1)
	if res.Is_Err() {
		return engine.Meta{}, fromDafnyErrCode(res.Dtor_error().(GroundKVTypes.ErrCode))
	}
	return fromDafnyMeta(res.Dtor_value().(GroundKVTypes.Meta)), nil
}
