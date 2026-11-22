package engine

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

// PageID uniquely identifies a fixed-size page in the database file.
// Pages 0 and 1 are reserved for dual meta pages. B+Tree pages start at PageID 2.
type PageID uint64

const (
	PageFlagMeta   uint16 = 0x01
	PageFlagBranch uint16 = 0x02
	PageFlagLeaf   uint16 = 0x04

	// pageHeaderSize is the fixed on-disk page header size in bytes:
	//   [0:8]   pgid     (uint64)
	//   [8:10]  flags    (uint16)
	//   [10:12] count    (uint16)
	//   [12:16] checksum (uint32, CRC32-IEEE over entire page with [12:16] zeroed)
	//   [16:24] txid     (uint64, transaction that allocated this page)
	pageHeaderSize = 24

	// leafEntryHeaderSize: ksize(uint32) + vsize(uint32)
	leafEntryHeaderSize = 8
	// branchEntryHeaderSize: ksize(uint32) + child(uint64)
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

func writePageHeader(buf []byte, h pageHeader) {
	binary.LittleEndian.PutUint64(buf[0:8], uint64(h.pgid))
	binary.LittleEndian.PutUint16(buf[8:10], h.flags)
	binary.LittleEndian.PutUint16(buf[10:12], h.count)
	binary.LittleEndian.PutUint32(buf[12:16], 0)
	binary.LittleEndian.PutUint64(buf[16:24], h.txid)
	sum := crc32.ChecksumIEEE(buf)
	binary.LittleEndian.PutUint32(buf[12:16], sum)
}

func readAndVerifyPageHeader(buf []byte, expectedPGID PageID) (pageHeader, error) {
	if len(buf) < pageHeaderSize {
		return pageHeader{}, ErrCorruptedPage
	}
	h := pageHeader{
		pgid:     PageID(binary.LittleEndian.Uint64(buf[0:8])),
		flags:    binary.LittleEndian.Uint16(buf[8:10]),
		count:    binary.LittleEndian.Uint16(buf[10:12]),
		checksum: binary.LittleEndian.Uint32(buf[12:16]),
		txid:     binary.LittleEndian.Uint64(buf[16:24]),
	}
	if h.pgid != expectedPGID {
		return pageHeader{}, fmt.Errorf("%w: expected pgid %d, got %d", ErrCorruptedPage, expectedPGID, h.pgid)
	}
	tmp := make([]byte, len(buf))
	copy(tmp, buf)
	binary.LittleEndian.PutUint32(tmp[12:16], 0)
	if crc32.ChecksumIEEE(tmp) != h.checksum {
		return pageHeader{}, fmt.Errorf("%w: checksum mismatch on pgid %d", ErrCorruptedPage, expectedPGID)
	}
	return h, nil
}

func leafEncodedSize(entries []leafEntry) int {
	size := pageHeaderSize
	for _, e := range entries {
		size += leafEntryHeaderSize + len(e.key) + len(e.value)
	}
	return size
}

func branchEncodedSize(entries []branchEntry) int {
	size := pageHeaderSize
	for _, e := range entries {
		size += branchEntryHeaderSize + len(e.key)
	}
	return size
}

func encodeLeafPage(pageSize int, pgid PageID, txid uint64, entries []leafEntry) ([]byte, error) {
	if leafEncodedSize(entries) > pageSize {
		return nil, fmt.Errorf("%w: leaf entries exceed page size %d", ErrValueTooLarge, pageSize)
	}
	buf := make([]byte, pageSize)
	off := pageHeaderSize
	for _, e := range entries {
		binary.LittleEndian.PutUint32(buf[off:off+4], uint32(len(e.key)))
		binary.LittleEndian.PutUint32(buf[off+4:off+8], uint32(len(e.value)))
		off += leafEntryHeaderSize
		copy(buf[off:off+len(e.key)], e.key)
		off += len(e.key)
		copy(buf[off:off+len(e.value)], e.value)
		off += len(e.value)
	}
	writePageHeader(buf, pageHeader{
		pgid:  pgid,
		flags: PageFlagLeaf,
		count: uint16(len(entries)),
		txid:  txid,
	})
	return buf, nil
}

func decodeLeafPage(buf []byte, h pageHeader) ([]leafEntry, error) {
	if h.flags != PageFlagLeaf {
		return nil, fmt.Errorf("%w: expected leaf page, got flags %d", ErrCorruptedPage, h.flags)
	}
	entries := make([]leafEntry, 0, h.count)
	off := pageHeaderSize
	for i := 0; i < int(h.count); i++ {
		if off+leafEntryHeaderSize > len(buf) {
			return nil, ErrCorruptedPage
		}
		klen := int(binary.LittleEndian.Uint32(buf[off : off+4]))
		vlen := int(binary.LittleEndian.Uint32(buf[off+4 : off+8]))
		off += leafEntryHeaderSize
		if off+klen+vlen > len(buf) {
			return nil, ErrCorruptedPage
		}
		k := make([]byte, klen)
		copy(k, buf[off:off+klen])
		off += klen
		v := make([]byte, vlen)
		copy(v, buf[off:off+vlen])
		off += vlen
		entries = append(entries, leafEntry{key: k, value: v})
	}
	return entries, nil
}

func encodeBranchPage(pageSize int, pgid PageID, txid uint64, entries []branchEntry) ([]byte, error) {
	if branchEncodedSize(entries) > pageSize {
		return nil, fmt.Errorf("%w: branch entries exceed page size %d", ErrKeyTooLarge, pageSize)
	}
	buf := make([]byte, pageSize)
	off := pageHeaderSize
	for _, e := range entries {
		binary.LittleEndian.PutUint32(buf[off:off+4], uint32(len(e.key)))
		binary.LittleEndian.PutUint64(buf[off+4:off+12], uint64(e.child))
		off += branchEntryHeaderSize
		copy(buf[off:off+len(e.key)], e.key)
		off += len(e.key)
	}
	writePageHeader(buf, pageHeader{
		pgid:  pgid,
		flags: PageFlagBranch,
		count: uint16(len(entries)),
		txid:  txid,
	})
	return buf, nil
}

func decodeBranchPage(buf []byte, h pageHeader) ([]branchEntry, error) {
	if h.flags != PageFlagBranch {
		return nil, fmt.Errorf("%w: expected branch page, got flags %d", ErrCorruptedPage, h.flags)
	}
	entries := make([]branchEntry, 0, h.count)
	off := pageHeaderSize
	for i := 0; i < int(h.count); i++ {
		if off+branchEntryHeaderSize > len(buf) {
			return nil, ErrCorruptedPage
		}
		klen := int(binary.LittleEndian.Uint32(buf[off : off+4]))
		child := PageID(binary.LittleEndian.Uint64(buf[off+4 : off+12]))
		off += branchEntryHeaderSize
		if off+klen > len(buf) {
			return nil, ErrCorruptedPage
		}
		k := make([]byte, klen)
		copy(k, buf[off:off+klen])
		off += klen
		entries = append(entries, branchEntry{key: k, child: child})
	}
	return entries, nil
}
