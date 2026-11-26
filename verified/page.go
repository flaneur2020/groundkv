package verified

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"

	"github.com/flaneur2020/groundkv/engine"
	"github.com/flaneur2020/groundkv/verified/dafnygen/GroundKVCodec"
	"github.com/flaneur2020/groundkv/verified/dafnygen/GroundKVTypes"
	_dafny "github.com/flaneur2020/groundkv/verified/dafnygen/dafny"
)

const (
	pageHeaderSize        = 24
	leafEntryHeaderSize   = 8
	branchEntryHeaderSize = 12
)

type pageHeader struct {
	pgid     PageID
	flags    uint16
	count    uint16
	checksum uint32
	txid     uint64
}

type leafEntry struct {
	key   []byte
	value []byte
}

type branchEntry struct {
	key   []byte
	child PageID
}

func readAndVerifyPageHeader(buf []byte, expectedPGID PageID) (pageHeader, error) {
	res := GroundKVCodec.Companion_Default___.DecodePageHeader(_dafny.SeqOfBytes(buf))
	if res.Is_Err() {
		return pageHeader{}, fromDafnyErrCode(res.Dtor_error().(GroundKVTypes.ErrCode))
	}
	h := fromDafnyHeader(res.Dtor_value().(GroundKVTypes.PageHeader))
	if h.pgid != expectedPGID {
		return pageHeader{}, fmt.Errorf("%w: expected pgid %d, got %d", engine.ErrCorruptedPage, expectedPGID, h.pgid)
	}
	tmp := make([]byte, len(buf))
	copy(tmp, buf)
	binary.LittleEndian.PutUint32(tmp[12:16], 0)
	if crc32.ChecksumIEEE(tmp) != h.checksum {
		return pageHeader{}, fmt.Errorf("%w: checksum mismatch on pgid %d", engine.ErrCorruptedPage, expectedPGID)
	}
	return h, nil
}

func leafEncodedSize(entries []leafEntry) int {
	return GroundKVCodec.Companion_Default___.LeafEncodedSize(toDafnyLeafEntries(entries)).Int()
}

func branchEncodedSize(entries []branchEntry) int {
	return GroundKVCodec.Companion_Default___.BranchEncodedSize(toDafnyBranchEntries(entries)).Int()
}

func encodeLeafPage(pageSize int, pgid PageID, txid uint64, entries []leafEntry) ([]byte, error) {
	res := GroundKVCodec.Companion_Default___.EncodeLeafPage(
		uint32(pageSize),
		uint64(pgid),
		txid,
		toDafnyLeafEntries(entries),
	)
	if res.Is_Err() {
		return nil, fmt.Errorf("%w: leaf entries exceed page size %d",
			fromDafnyErrCode(res.Dtor_error().(GroundKVTypes.ErrCode)), pageSize)
	}
	buf := _dafny.ToByteArray(res.Dtor_value().(_dafny.Sequence))
	stampPageChecksum(buf)
	return buf, nil
}

func decodeLeafPage(buf []byte, h pageHeader) ([]leafEntry, error) {
	res := GroundKVCodec.Companion_Default___.DecodeLeafPage(
		_dafny.SeqOfBytes(buf),
		toDafnyHeader(h),
	)
	if res.Is_Err() {
		return nil, fmt.Errorf("%w: leaf decode failed (flags %d)",
			fromDafnyErrCode(res.Dtor_error().(GroundKVTypes.ErrCode)), h.flags)
	}
	return fromDafnyLeafEntries(res.Dtor_value().(_dafny.Sequence)), nil
}

func encodeBranchPage(pageSize int, pgid PageID, txid uint64, entries []branchEntry) ([]byte, error) {
	res := GroundKVCodec.Companion_Default___.EncodeBranchPage(
		uint32(pageSize),
		uint64(pgid),
		txid,
		toDafnyBranchEntries(entries),
	)
	if res.Is_Err() {
		return nil, fmt.Errorf("%w: branch entries exceed page size %d",
			fromDafnyErrCode(res.Dtor_error().(GroundKVTypes.ErrCode)), pageSize)
	}
	buf := _dafny.ToByteArray(res.Dtor_value().(_dafny.Sequence))
	stampPageChecksum(buf)
	return buf, nil
}

func decodeBranchPage(buf []byte, h pageHeader) ([]branchEntry, error) {
	res := GroundKVCodec.Companion_Default___.DecodeBranchPage(
		_dafny.SeqOfBytes(buf),
		toDafnyHeader(h),
	)
	if res.Is_Err() {
		return nil, fmt.Errorf("%w: branch decode failed (flags %d)",
			fromDafnyErrCode(res.Dtor_error().(GroundKVTypes.ErrCode)), h.flags)
	}
	return fromDafnyBranchEntries(res.Dtor_value().(_dafny.Sequence)), nil
}
