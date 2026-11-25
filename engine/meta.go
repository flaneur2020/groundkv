package engine

import (
	"encoding/binary"
	"fmt"
)

const (
	metaMagic   uint32 = 0x47524B56 // "GRKV"
	metaVersion uint32 = 1
	// metaPayloadSize: magic(4) + version(4) + pageSize(4) + reserved(4) + root(8) + highWater(8) + txid(8) + freelist(8) = 48
	metaPayloadSize = 48
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
	Freelist  PageID
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
	binary.LittleEndian.PutUint64(buf[off+40:off+48], uint64(m.Freelist))

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
		Freelist:  PageID(binary.LittleEndian.Uint64(buf[off+40 : off+48])),
	}
	if m.Magic != metaMagic || m.Version != metaVersion {
		return Meta{}, fmt.Errorf("%w: magic/version mismatch on meta %d", ErrCorruptedMeta, slot)
	}
	if h.txid != m.TxID {
		return Meta{}, fmt.Errorf("%w: header txid %d != payload txid %d on meta %d", ErrCorruptedMeta, h.txid, m.TxID, slot)
	}
	if m.TxID > 0 && metaSlot(m.TxID) != slot {
		return Meta{}, fmt.Errorf("%w: meta slot %d parity mismatch for txid %d", ErrCorruptedMeta, slot, m.TxID)
	}
	if m.HighWater < 2 {
		return Meta{}, fmt.Errorf("%w: invalid highWater %d on meta %d", ErrCorruptedMeta, m.HighWater, slot)
	}
	if m.Root != 0 && (m.Root < 2 || m.Root >= m.HighWater) {
		return Meta{}, fmt.Errorf("%w: invalid root %d (highWater %d) on meta %d", ErrCorruptedMeta, m.Root, m.HighWater, slot)
	}
	if m.Freelist != 0 && (m.Freelist < 2 || m.Freelist >= m.HighWater) {
		return Meta{}, fmt.Errorf("%w: invalid freelist %d (highWater %d) on meta %d", ErrCorruptedMeta, m.Freelist, m.HighWater, slot)
	}
	return m, nil
}
