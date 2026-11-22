package engine

import (
	"encoding/binary"
	"fmt"
)

const (
	metaMagic   uint32 = 0x47524B56 // "GRKV"
	metaVersion uint32 = 1
	// metaPayloadSize: magic(4) + version(4) + pageSize(4) + reserved(4) + root(8) + highWater(8) + txid(8) = 40
	metaPayloadSize = 40
)

// Meta represents the superblock stored in Page 0 and Page 1.
// Commits alternate between Page 0 (even TxID) and Page 1 (odd TxID), matching
// BoltDB's dual-meta atomic commit protocol.
type Meta struct {
	Magic     uint32
	Version   uint32
	PageSize  uint32
	Root      PageID
	HighWater PageID
	TxID      uint64
}

func metaSlot(txid uint64) PageID {
	return PageID(txid % 2)
}

func encodeMetaPage(pageSize int, slot PageID, m Meta) []byte {
	buf := make([]byte, pageSize)
	off := pageHeaderSize
	binary.LittleEndian.PutUint32(buf[off:off+4], m.Magic)
	binary.LittleEndian.PutUint32(buf[off+4:off+8], m.Version)
	binary.LittleEndian.PutUint32(buf[off+8:off+12], m.PageSize)
	binary.LittleEndian.PutUint32(buf[off+12:off+16], 0)
	binary.LittleEndian.PutUint64(buf[off+16:off+24], uint64(m.Root))
	binary.LittleEndian.PutUint64(buf[off+24:off+32], uint64(m.HighWater))
	binary.LittleEndian.PutUint64(buf[off+32:off+40], m.TxID)

	writePageHeader(buf, pageHeader{
		pgid:  slot,
		flags: PageFlagMeta,
		count: 1,
		txid:  m.TxID,
	})
	return buf
}

func decodeMetaPage(buf []byte, slot PageID) (Meta, error) {
	h, err := readAndVerifyPageHeader(buf, slot)
	if err != nil {
		return Meta{}, err
	}
	if h.flags != PageFlagMeta || len(buf) < pageHeaderSize+metaPayloadSize {
		return Meta{}, fmt.Errorf("%w: page %d is not a valid meta page", ErrCorruptedMeta, slot)
	}
	off := pageHeaderSize
	m := Meta{
		Magic:     binary.LittleEndian.Uint32(buf[off : off+4]),
		Version:   binary.LittleEndian.Uint32(buf[off+4 : off+8]),
		PageSize:  binary.LittleEndian.Uint32(buf[off+8 : off+12]),
		Root:      PageID(binary.LittleEndian.Uint64(buf[off+16 : off+24])),
		HighWater: PageID(binary.LittleEndian.Uint64(buf[off+24 : off+32])),
		TxID:      binary.LittleEndian.Uint64(buf[off+32 : off+40]),
	}
	if m.Magic != metaMagic || m.Version != metaVersion {
		return Meta{}, fmt.Errorf("%w: magic/version mismatch on meta %d", ErrCorruptedMeta, slot)
	}
	if m.HighWater < 2 {
		return Meta{}, fmt.Errorf("%w: invalid highWater %d on meta %d", ErrCorruptedMeta, m.HighWater, slot)
	}
	if m.Root != 0 && (m.Root < 2 || m.Root >= m.HighWater) {
		return Meta{}, fmt.Errorf("%w: invalid root %d (highWater %d) on meta %d", ErrCorruptedMeta, m.Root, m.HighWater, slot)
	}
	return m, nil
}
