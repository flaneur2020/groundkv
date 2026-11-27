package engine

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"

	"github.com/flaneur2020/groundkv/engine/core"
)

// StampPageChecksum computes the CRC32-IEEE checksum over buf (with header
// bytes [12:16] zeroed) and writes it into buf[12:16].
func StampPageChecksum(buf []byte) {
	binary.LittleEndian.PutUint32(buf[12:16], 0)
	sum := crc32.ChecksumIEEE(buf)
	binary.LittleEndian.PutUint32(buf[12:16], sum)
}

// pageChecksum computes CRC32-IEEE over buf with bytes [12:16] treated as zero
// without allocating or mutating buf.
func pageChecksum(buf []byte) uint32 {
	var zero [4]byte
	crc := crc32.Update(0, crc32.IEEETable, buf[:12])
	crc = crc32.Update(crc, crc32.IEEETable, zero[:])
	return crc32.Update(crc, crc32.IEEETable, buf[16:])
}

func verifyPageHeader(c core.Codec, buf []byte, expectedPGID PageID) (PageHeader, error) {
	h, err := c.DecodePageHeader(buf)
	if err != nil {
		return PageHeader{}, err
	}
	if h.PGID != expectedPGID {
		return PageHeader{}, fmt.Errorf("%w: expected pgid %d, got %d", ErrCorruptedPage, expectedPGID, h.PGID)
	}
	if pageChecksum(buf) != h.Checksum {
		return PageHeader{}, fmt.Errorf("%w: checksum mismatch on pgid %d", ErrCorruptedPage, expectedPGID)
	}
	return h, nil
}

func encodeMetaPage(c core.Codec, pageSize int, slot PageID, m Meta) []byte {
	buf, err := c.EncodeMetaPage(pageSize, slot, m)
	if err != nil {
		panic(fmt.Sprintf("groundkv: encodeMetaPage failed for pageSize %d: %v", pageSize, err))
	}
	StampPageChecksum(buf)
	return buf
}

func decodeMetaPage(c core.Codec, buf []byte, slot PageID) (Meta, error) {
	h, err := verifyPageHeader(c, buf, slot)
	if err != nil {
		return Meta{}, err
	}
	return c.DecodeMetaPayload(buf, slot, h)
}

func encodeLeafPage(c core.Codec, pageSize int, pgid PageID, txid uint64, entries []LeafEntry) ([]byte, error) {
	buf, err := c.EncodeLeafPage(pageSize, pgid, txid, entries)
	if err != nil {
		return nil, err
	}
	StampPageChecksum(buf)
	return buf, nil
}

func encodeBranchPage(c core.Codec, pageSize int, pgid PageID, txid uint64, entries []BranchEntry) ([]byte, error) {
	buf, err := c.EncodeBranchPage(pageSize, pgid, txid, entries)
	if err != nil {
		return nil, err
	}
	StampPageChecksum(buf)
	return buf, nil
}

func encodeFreelistPage(c core.Codec, pageSize int, pgid PageID, txid uint64, next PageID, ids []PageID) ([]byte, error) {
	buf, err := c.EncodeFreelistPage(pageSize, pgid, txid, next, ids)
	if err != nil {
		return nil, err
	}
	StampPageChecksum(buf)
	return buf, nil
}
