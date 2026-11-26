package verified

import (
	"fmt"

	"github.com/flaneur2020/groundkv/verified/dafnygen/GroundKVCodec"
	"github.com/flaneur2020/groundkv/verified/dafnygen/GroundKVTypes"
	_dafny "github.com/flaneur2020/groundkv/verified/dafnygen/dafny"
)

const (
	metaMagic   uint32 = 0x47524B56 // "GRKV"
	metaVersion uint32 = 1
)

func metaSlot(txid uint64) PageID {
	return PageID(GroundKVCodec.Companion_Default___.MetaSlot(txid))
}

func encodeMetaPage(pageSize int, slot PageID, m Meta) []byte {
	res := GroundKVCodec.Companion_Default___.EncodeMetaPage(uint32(pageSize), uint64(slot), toDafnyMeta(m))
	if res.Is_Err() {
		panic(fmt.Sprintf("groundkv/verified: encodeMetaPage invalid pageSize %d", pageSize))
	}
	buf := _dafny.ToByteArray(res.Dtor_value().(_dafny.Sequence))
	stampPageChecksum(buf)
	return buf
}

func decodeMetaPage(buf []byte, slot PageID) (Meta, error) {
	h, err := readAndVerifyPageHeader(buf, slot)
	if err != nil {
		return Meta{}, err
	}
	res := GroundKVCodec.Companion_Default___.DecodeMetaPayload(
		_dafny.SeqOfBytes(buf),
		uint64(slot),
		toDafnyHeader(h),
	)
	if res.Is_Err() {
		return Meta{}, fmt.Errorf("%w: invalid meta page %d",
			fromDafnyErrCode(res.Dtor_error().(GroundKVTypes.ErrCode)), slot)
	}
	return fromDafnyMeta(res.Dtor_value().(GroundKVTypes.Meta)), nil
}
