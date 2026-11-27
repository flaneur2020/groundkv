package gocore

import (
	"encoding/binary"
	"fmt"

	"github.com/flaneur2020/groundkv/engine/core"
)

type goCodec struct{}

var _ core.Codec = goCodec{}

func (goCodec) EncodePageHeader(h core.PageHeader) []byte {
	buf := make([]byte, core.PageHeaderSize)
	binary.LittleEndian.PutUint64(buf[0:8], uint64(h.PGID))
	binary.LittleEndian.PutUint16(buf[8:10], h.Flags)
	binary.LittleEndian.PutUint16(buf[10:12], h.Count)
	binary.LittleEndian.PutUint32(buf[12:16], h.Checksum)
	binary.LittleEndian.PutUint64(buf[16:24], h.TxID)
	return buf
}

func (goCodec) DecodePageHeader(buf []byte) (core.PageHeader, error) {
	if len(buf) < core.PageHeaderSize {
		return core.PageHeader{}, core.ErrCorruptedPage
	}
	return core.PageHeader{
		PGID:     core.PageID(binary.LittleEndian.Uint64(buf[0:8])),
		Flags:    binary.LittleEndian.Uint16(buf[8:10]),
		Count:    binary.LittleEndian.Uint16(buf[10:12]),
		Checksum: binary.LittleEndian.Uint32(buf[12:16]),
		TxID:     binary.LittleEndian.Uint64(buf[16:24]),
	}, nil
}

func metaSlot(txid uint64) core.PageID {
	return core.PageID(txid % 2)
}

func (goCodec) MetaSlot(txid uint64) core.PageID {
	return metaSlot(txid)
}

func (c goCodec) EncodeMetaPage(pageSize int, slot core.PageID, m core.Meta) ([]byte, error) {
	if pageSize < core.PageHeaderSize+core.MetaPayloadSize {
		return nil, core.ErrCorruptedMeta
	}
	buf := make([]byte, pageSize)
	off := core.PageHeaderSize
	binary.LittleEndian.PutUint32(buf[off:off+4], m.Magic)
	binary.LittleEndian.PutUint32(buf[off+4:off+8], m.Version)
	binary.LittleEndian.PutUint32(buf[off+8:off+12], m.PageSize)
	binary.LittleEndian.PutUint32(buf[off+12:off+16], 0)
	binary.LittleEndian.PutUint64(buf[off+16:off+24], uint64(m.Root))
	binary.LittleEndian.PutUint64(buf[off+24:off+32], uint64(m.HighWater))
	binary.LittleEndian.PutUint64(buf[off+32:off+40], m.TxID)
	binary.LittleEndian.PutUint64(buf[off+40:off+48], uint64(m.Freelist))
	copy(buf[0:core.PageHeaderSize], c.EncodePageHeader(core.PageHeader{
		PGID:  slot,
		Flags: core.PageFlagMeta,
		Count: 1,
		TxID:  m.TxID,
	}))
	return buf, nil
}

func (goCodec) DecodeMetaPayload(buf []byte, slot core.PageID, h core.PageHeader) (core.Meta, error) {
	if h.Flags != core.PageFlagMeta || len(buf) < core.PageHeaderSize+core.MetaPayloadSize {
		return core.Meta{}, fmt.Errorf("%w: page %d is not a valid meta page", core.ErrCorruptedMeta, slot)
	}
	off := core.PageHeaderSize
	m := core.Meta{
		Magic:     binary.LittleEndian.Uint32(buf[off : off+4]),
		Version:   binary.LittleEndian.Uint32(buf[off+4 : off+8]),
		PageSize:  binary.LittleEndian.Uint32(buf[off+8 : off+12]),
		Root:      core.PageID(binary.LittleEndian.Uint64(buf[off+16 : off+24])),
		HighWater: core.PageID(binary.LittleEndian.Uint64(buf[off+24 : off+32])),
		TxID:      binary.LittleEndian.Uint64(buf[off+32 : off+40]),
		Freelist:  core.PageID(binary.LittleEndian.Uint64(buf[off+40 : off+48])),
	}
	if m.Magic != core.MetaMagic || m.Version != core.MetaVersion {
		return core.Meta{}, fmt.Errorf("%w: magic/version mismatch on meta %d", core.ErrCorruptedMeta, slot)
	}
	if h.TxID != m.TxID {
		return core.Meta{}, fmt.Errorf("%w: header txid %d != payload txid %d on meta %d", core.ErrCorruptedMeta, h.TxID, m.TxID, slot)
	}
	if m.TxID > 0 && metaSlot(m.TxID) != slot {
		return core.Meta{}, fmt.Errorf("%w: meta slot %d parity mismatch for txid %d", core.ErrCorruptedMeta, slot, m.TxID)
	}
	if m.HighWater < 2 {
		return core.Meta{}, fmt.Errorf("%w: invalid highWater %d on meta %d", core.ErrCorruptedMeta, m.HighWater, slot)
	}
	if m.Root != 0 && (m.Root < 2 || m.Root >= m.HighWater) {
		return core.Meta{}, fmt.Errorf("%w: invalid root %d (highWater %d) on meta %d", core.ErrCorruptedMeta, m.Root, m.HighWater, slot)
	}
	if m.Freelist != 0 && (m.Freelist < 2 || m.Freelist >= m.HighWater) {
		return core.Meta{}, fmt.Errorf("%w: invalid freelist %d (highWater %d) on meta %d", core.ErrCorruptedMeta, m.Freelist, m.HighWater, slot)
	}
	return m, nil
}

func leafEncodedSize(entries []core.LeafEntry) int {
	size := core.PageHeaderSize
	for _, e := range entries {
		size += core.LeafEntryHeaderSize + len(e.Key) + len(e.Value)
	}
	return size
}

func branchEncodedSize(entries []core.BranchEntry) int {
	size := core.PageHeaderSize
	for _, e := range entries {
		size += core.BranchEntryHeaderSize + len(e.Key)
	}
	return size
}

func (goCodec) LeafEncodedSize(entries []core.LeafEntry) int {
	return leafEncodedSize(entries)
}

func (c goCodec) EncodeLeafPage(pageSize int, pgid core.PageID, txid uint64, entries []core.LeafEntry) ([]byte, error) {
	if leafEncodedSize(entries) > pageSize {
		return nil, fmt.Errorf("%w: leaf entries exceed page size %d", core.ErrValueTooLarge, pageSize)
	}
	buf := make([]byte, pageSize)
	off := core.PageHeaderSize
	for _, e := range entries {
		binary.LittleEndian.PutUint32(buf[off:off+4], uint32(len(e.Key)))
		binary.LittleEndian.PutUint32(buf[off+4:off+8], uint32(len(e.Value)))
		off += core.LeafEntryHeaderSize
		copy(buf[off:off+len(e.Key)], e.Key)
		off += len(e.Key)
		copy(buf[off:off+len(e.Value)], e.Value)
		off += len(e.Value)
	}
	copy(buf[0:core.PageHeaderSize], c.EncodePageHeader(core.PageHeader{
		PGID:  pgid,
		Flags: core.PageFlagLeaf,
		Count: uint16(len(entries)),
		TxID:  txid,
	}))
	return buf, nil
}

func (goCodec) DecodeLeafPage(buf []byte, h core.PageHeader) ([]core.LeafEntry, error) {
	if h.Flags != core.PageFlagLeaf {
		return nil, fmt.Errorf("%w: expected leaf page, got flags %d", core.ErrCorruptedPage, h.Flags)
	}
	entries := make([]core.LeafEntry, 0, h.Count)
	off := core.PageHeaderSize
	for i := 0; i < int(h.Count); i++ {
		if off+core.LeafEntryHeaderSize > len(buf) {
			return nil, core.ErrCorruptedPage
		}
		klen := int(binary.LittleEndian.Uint32(buf[off : off+4]))
		vlen := int(binary.LittleEndian.Uint32(buf[off+4 : off+8]))
		off += core.LeafEntryHeaderSize
		if off+klen+vlen > len(buf) {
			return nil, core.ErrCorruptedPage
		}
		k := make([]byte, klen)
		copy(k, buf[off:off+klen])
		off += klen
		v := make([]byte, vlen)
		copy(v, buf[off:off+vlen])
		off += vlen
		entries = append(entries, core.LeafEntry{Key: k, Value: v})
	}
	return entries, nil
}

func (goCodec) BranchEncodedSize(entries []core.BranchEntry) int {
	return branchEncodedSize(entries)
}

func (c goCodec) EncodeBranchPage(pageSize int, pgid core.PageID, txid uint64, entries []core.BranchEntry) ([]byte, error) {
	if branchEncodedSize(entries) > pageSize {
		return nil, fmt.Errorf("%w: branch entries exceed page size %d", core.ErrKeyTooLarge, pageSize)
	}
	buf := make([]byte, pageSize)
	off := core.PageHeaderSize
	for _, e := range entries {
		binary.LittleEndian.PutUint32(buf[off:off+4], uint32(len(e.Key)))
		binary.LittleEndian.PutUint64(buf[off+4:off+12], uint64(e.Child))
		off += core.BranchEntryHeaderSize
		copy(buf[off:off+len(e.Key)], e.Key)
		off += len(e.Key)
	}
	copy(buf[0:core.PageHeaderSize], c.EncodePageHeader(core.PageHeader{
		PGID:  pgid,
		Flags: core.PageFlagBranch,
		Count: uint16(len(entries)),
		TxID:  txid,
	}))
	return buf, nil
}

func (goCodec) DecodeBranchPage(buf []byte, h core.PageHeader) ([]core.BranchEntry, error) {
	if h.Flags != core.PageFlagBranch {
		return nil, fmt.Errorf("%w: expected branch page, got flags %d", core.ErrCorruptedPage, h.Flags)
	}
	entries := make([]core.BranchEntry, 0, h.Count)
	off := core.PageHeaderSize
	for i := 0; i < int(h.Count); i++ {
		if off+core.BranchEntryHeaderSize > len(buf) {
			return nil, core.ErrCorruptedPage
		}
		klen := int(binary.LittleEndian.Uint32(buf[off : off+4]))
		child := core.PageID(binary.LittleEndian.Uint64(buf[off+4 : off+12]))
		off += core.BranchEntryHeaderSize
		if off+klen > len(buf) {
			return nil, core.ErrCorruptedPage
		}
		k := make([]byte, klen)
		copy(k, buf[off:off+klen])
		off += klen
		entries = append(entries, core.BranchEntry{Key: k, Child: child})
	}
	return entries, nil
}

func maxFreelistIDsPerPage(pageSize int) int {
	return (pageSize - core.PageHeaderSize - core.FreelistHeaderExtra) / core.FreelistEntrySize
}

func (goCodec) MaxFreelistIDsPerPage(pageSize int) int {
	return maxFreelistIDsPerPage(pageSize)
}

func (c goCodec) EncodeFreelistPage(pageSize int, pgid core.PageID, txid uint64, next core.PageID, ids []core.PageID) ([]byte, error) {
	if len(ids) > maxFreelistIDsPerPage(pageSize) {
		return nil, fmt.Errorf("%w: too many freelist IDs (%d) for page size %d", core.ErrValueTooLarge, len(ids), pageSize)
	}
	buf := make([]byte, pageSize)
	off := core.PageHeaderSize
	binary.LittleEndian.PutUint64(buf[off:off+8], uint64(next))
	off += core.FreelistHeaderExtra
	for _, id := range ids {
		binary.LittleEndian.PutUint64(buf[off:off+8], uint64(id))
		off += core.FreelistEntrySize
	}
	copy(buf[0:core.PageHeaderSize], c.EncodePageHeader(core.PageHeader{
		PGID:  pgid,
		Flags: core.PageFlagFreelist,
		Count: uint16(len(ids)),
		TxID:  txid,
	}))
	return buf, nil
}

func (goCodec) DecodeFreelistPage(buf []byte, h core.PageHeader) (core.PageID, []core.PageID, error) {
	if h.Flags != core.PageFlagFreelist {
		return 0, nil, fmt.Errorf("%w: expected freelist page, got flags %d", core.ErrCorruptedPage, h.Flags)
	}
	if len(buf) < core.PageHeaderSize+core.FreelistHeaderExtra+int(h.Count)*core.FreelistEntrySize {
		return 0, nil, core.ErrCorruptedPage
	}
	off := core.PageHeaderSize
	next := core.PageID(binary.LittleEndian.Uint64(buf[off : off+8]))
	off += core.FreelistHeaderExtra
	ids := make([]core.PageID, int(h.Count))
	for i := 0; i < int(h.Count); i++ {
		ids[i] = core.PageID(binary.LittleEndian.Uint64(buf[off : off+8]))
		off += core.FreelistEntrySize
	}
	return next, ids, nil
}
