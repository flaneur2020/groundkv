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

// MetaCore abstracts dual-meta superblock codecs, slot parity, and recovery selection.
type MetaCore interface {
	MetaSlot(txid uint64) PageID
	EncodeMetaPage(pageSize int, slot PageID, m Meta) ([]byte, error)
	DecodeMetaPayload(buf []byte, slot PageID, h PageHeader) (Meta, error)
	SelectRecoveredMeta(meta0 Meta, ok0 bool, meta1 Meta, ok1 bool) (Meta, error)
}

func encodeMetaPageWithCore(c MetaCore, pageSize int, slot PageID, m Meta) []byte {
	buf, err := c.EncodeMetaPage(pageSize, slot, m)
	if err != nil {
		panic(fmt.Sprintf("groundkv: encodeMetaPage failed for pageSize %d: %v", pageSize, err))
	}
	StampPageChecksum(buf)
	return buf
}

func decodeMetaPageWithCore(c Core, buf []byte, slot PageID) (Meta, error) {
	h, err := readAndVerifyPageHeaderWithCore(c, buf, slot)
	if err != nil {
		return Meta{}, err
	}
	return c.DecodeMetaPayload(buf, slot, h)
}

// --- Pure-Go MetaCore implementation ---

type goMetaCore struct{}

func metaSlot(txid uint64) PageID {
	return PageID(txid % 2)
}

func (goMetaCore) MetaSlot(txid uint64) PageID {
	return metaSlot(txid)
}

func (goMetaCore) EncodeMetaPage(pageSize int, slot PageID, m Meta) ([]byte, error) {
	if pageSize < pageHeaderSize+metaPayloadSize {
		return nil, ErrCorruptedMeta
	}
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
	copy(buf[0:pageHeaderSize], (goPageCodec{}).EncodePageHeader(PageHeader{
		PGID:  slot,
		Flags: PageFlagMeta,
		Count: 1,
		TxID:  m.TxID,
	}))
	return buf, nil
}

func (goMetaCore) DecodeMetaPayload(buf []byte, slot PageID, h PageHeader) (Meta, error) {
	if h.Flags != PageFlagMeta || len(buf) < pageHeaderSize+metaPayloadSize {
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
	if h.TxID != m.TxID {
		return Meta{}, fmt.Errorf("%w: header txid %d != payload txid %d on meta %d", ErrCorruptedMeta, h.TxID, m.TxID, slot)
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

func (goMetaCore) SelectRecoveredMeta(meta0 Meta, ok0 bool, meta1 Meta, ok1 bool) (Meta, error) {
	switch {
	case ok0 && ok1:
		if meta0.TxID >= meta1.TxID {
			return meta0, nil
		}
		return meta1, nil
	case ok0:
		return meta0, nil
	case ok1:
		return meta1, nil
	default:
		return Meta{}, ErrCorruptedMeta
	}
}

// Package-internal default helpers used by tests and fuzzers.

func encodeMetaPage(pageSize int, slot PageID, m Meta) []byte {
	return encodeMetaPageWithCore(defaultGoCore, pageSize, slot, m)
}

func decodeMetaPage(buf []byte, slot PageID) (Meta, error) {
	return decodeMetaPageWithCore(defaultGoCore, buf, slot)
}
