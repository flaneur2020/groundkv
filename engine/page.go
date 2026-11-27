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
	PageFlagMeta     uint16 = 0x01
	PageFlagBranch   uint16 = 0x02
	PageFlagLeaf     uint16 = 0x04
	PageFlagFreelist uint16 = 0x08

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

// PageHeader holds the decoded 24-byte fixed on-disk page header.
type PageHeader struct {
	PGID     PageID
	Flags    uint16
	Count    uint16
	Checksum uint32
	TxID     uint64
}

// LeafEntry is a single key-value entry stored in a B+Tree leaf page.
type LeafEntry struct {
	Key   []byte
	Value []byte
}

// BranchEntry is a separator key and child PageID stored in a B+Tree branch page.
type BranchEntry struct {
	Key   []byte
	Child PageID
}

// PageCodec abstracts fixed page header and B+Tree leaf/branch binary codecs.
type PageCodec interface {
	EncodePageHeader(h PageHeader) []byte
	DecodePageHeader(buf []byte) (PageHeader, error)

	LeafEncodedSize(entries []LeafEntry) int
	EncodeLeafPage(pageSize int, pgid PageID, txid uint64, entries []LeafEntry) ([]byte, error)
	DecodeLeafPage(buf []byte, h PageHeader) ([]LeafEntry, error)

	BranchEncodedSize(entries []BranchEntry) int
	EncodeBranchPage(pageSize int, pgid PageID, txid uint64, entries []BranchEntry) ([]byte, error)
	DecodeBranchPage(buf []byte, h PageHeader) ([]BranchEntry, error)
}

// StampPageChecksum computes the CRC32-IEEE checksum over buf (with header
// bytes [12:16] zeroed) and writes it into buf[12:16].
func StampPageChecksum(buf []byte) {
	binary.LittleEndian.PutUint32(buf[12:16], 0)
	sum := crc32.ChecksumIEEE(buf)
	binary.LittleEndian.PutUint32(buf[12:16], sum)
}

func readAndVerifyPageHeaderWithCore(c PageCodec, buf []byte, expectedPGID PageID) (PageHeader, error) {
	h, err := c.DecodePageHeader(buf)
	if err != nil {
		return PageHeader{}, err
	}
	if h.PGID != expectedPGID {
		return PageHeader{}, fmt.Errorf("%w: expected pgid %d, got %d", ErrCorruptedPage, expectedPGID, h.PGID)
	}
	tmp := make([]byte, len(buf))
	copy(tmp, buf)
	binary.LittleEndian.PutUint32(tmp[12:16], 0)
	if crc32.ChecksumIEEE(tmp) != h.Checksum {
		return PageHeader{}, fmt.Errorf("%w: checksum mismatch on pgid %d", ErrCorruptedPage, expectedPGID)
	}
	return h, nil
}

func encodeLeafPageWithCore(c PageCodec, pageSize int, pgid PageID, txid uint64, entries []LeafEntry) ([]byte, error) {
	buf, err := c.EncodeLeafPage(pageSize, pgid, txid, entries)
	if err != nil {
		return nil, err
	}
	StampPageChecksum(buf)
	return buf, nil
}

func encodeBranchPageWithCore(c PageCodec, pageSize int, pgid PageID, txid uint64, entries []BranchEntry) ([]byte, error) {
	buf, err := c.EncodeBranchPage(pageSize, pgid, txid, entries)
	if err != nil {
		return nil, err
	}
	StampPageChecksum(buf)
	return buf, nil
}

// --- Pure-Go PageCodec implementation ---

type goPageCodec struct{}

func (goPageCodec) EncodePageHeader(h PageHeader) []byte {
	buf := make([]byte, pageHeaderSize)
	binary.LittleEndian.PutUint64(buf[0:8], uint64(h.PGID))
	binary.LittleEndian.PutUint16(buf[8:10], h.Flags)
	binary.LittleEndian.PutUint16(buf[10:12], h.Count)
	binary.LittleEndian.PutUint32(buf[12:16], h.Checksum)
	binary.LittleEndian.PutUint64(buf[16:24], h.TxID)
	return buf
}

func (goPageCodec) DecodePageHeader(buf []byte) (PageHeader, error) {
	if len(buf) < pageHeaderSize {
		return PageHeader{}, ErrCorruptedPage
	}
	return PageHeader{
		PGID:     PageID(binary.LittleEndian.Uint64(buf[0:8])),
		Flags:    binary.LittleEndian.Uint16(buf[8:10]),
		Count:    binary.LittleEndian.Uint16(buf[10:12]),
		Checksum: binary.LittleEndian.Uint32(buf[12:16]),
		TxID:     binary.LittleEndian.Uint64(buf[16:24]),
	}, nil
}

func leafEncodedSize(entries []LeafEntry) int {
	size := pageHeaderSize
	for _, e := range entries {
		size += leafEntryHeaderSize + len(e.Key) + len(e.Value)
	}
	return size
}

func branchEncodedSize(entries []BranchEntry) int {
	size := pageHeaderSize
	for _, e := range entries {
		size += branchEntryHeaderSize + len(e.Key)
	}
	return size
}

func (goPageCodec) LeafEncodedSize(entries []LeafEntry) int {
	return leafEncodedSize(entries)
}

func (c goPageCodec) EncodeLeafPage(pageSize int, pgid PageID, txid uint64, entries []LeafEntry) ([]byte, error) {
	if leafEncodedSize(entries) > pageSize {
		return nil, fmt.Errorf("%w: leaf entries exceed page size %d", ErrValueTooLarge, pageSize)
	}
	buf := make([]byte, pageSize)
	off := pageHeaderSize
	for _, e := range entries {
		binary.LittleEndian.PutUint32(buf[off:off+4], uint32(len(e.Key)))
		binary.LittleEndian.PutUint32(buf[off+4:off+8], uint32(len(e.Value)))
		off += leafEntryHeaderSize
		copy(buf[off:off+len(e.Key)], e.Key)
		off += len(e.Key)
		copy(buf[off:off+len(e.Value)], e.Value)
		off += len(e.Value)
	}
	copy(buf[0:pageHeaderSize], c.EncodePageHeader(PageHeader{
		PGID:  pgid,
		Flags: PageFlagLeaf,
		Count: uint16(len(entries)),
		TxID:  txid,
	}))
	return buf, nil
}

func (goPageCodec) DecodeLeafPage(buf []byte, h PageHeader) ([]LeafEntry, error) {
	if h.Flags != PageFlagLeaf {
		return nil, fmt.Errorf("%w: expected leaf page, got flags %d", ErrCorruptedPage, h.Flags)
	}
	entries := make([]LeafEntry, 0, h.Count)
	off := pageHeaderSize
	for i := 0; i < int(h.Count); i++ {
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
		entries = append(entries, LeafEntry{Key: k, Value: v})
	}
	return entries, nil
}

func (goPageCodec) BranchEncodedSize(entries []BranchEntry) int {
	return branchEncodedSize(entries)
}

func (c goPageCodec) EncodeBranchPage(pageSize int, pgid PageID, txid uint64, entries []BranchEntry) ([]byte, error) {
	if branchEncodedSize(entries) > pageSize {
		return nil, fmt.Errorf("%w: branch entries exceed page size %d", ErrKeyTooLarge, pageSize)
	}
	buf := make([]byte, pageSize)
	off := pageHeaderSize
	for _, e := range entries {
		binary.LittleEndian.PutUint32(buf[off:off+4], uint32(len(e.Key)))
		binary.LittleEndian.PutUint64(buf[off+4:off+12], uint64(e.Child))
		off += branchEntryHeaderSize
		copy(buf[off:off+len(e.Key)], e.Key)
		off += len(e.Key)
	}
	copy(buf[0:pageHeaderSize], c.EncodePageHeader(PageHeader{
		PGID:  pgid,
		Flags: PageFlagBranch,
		Count: uint16(len(entries)),
		TxID:  txid,
	}))
	return buf, nil
}

func (goPageCodec) DecodeBranchPage(buf []byte, h PageHeader) ([]BranchEntry, error) {
	if h.Flags != PageFlagBranch {
		return nil, fmt.Errorf("%w: expected branch page, got flags %d", ErrCorruptedPage, h.Flags)
	}
	entries := make([]BranchEntry, 0, h.Count)
	off := pageHeaderSize
	for i := 0; i < int(h.Count); i++ {
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
		entries = append(entries, BranchEntry{Key: k, Child: child})
	}
	return entries, nil
}

// Package-internal default helpers used by tests and fuzzers.

func readAndVerifyPageHeader(buf []byte, expectedPGID PageID) (PageHeader, error) {
	return readAndVerifyPageHeaderWithCore(defaultGoCore, buf, expectedPGID)
}

func encodeLeafPage(pageSize int, pgid PageID, txid uint64, entries []LeafEntry) ([]byte, error) {
	return encodeLeafPageWithCore(defaultGoCore, pageSize, pgid, txid, entries)
}

func decodeLeafPage(buf []byte, h PageHeader) ([]LeafEntry, error) {
	return defaultGoCore.DecodeLeafPage(buf, h)
}

func encodeBranchPage(pageSize int, pgid PageID, txid uint64, entries []BranchEntry) ([]byte, error) {
	return encodeBranchPageWithCore(defaultGoCore, pageSize, pgid, txid, entries)
}

func decodeBranchPage(buf []byte, h PageHeader) ([]BranchEntry, error) {
	return defaultGoCore.DecodeBranchPage(buf, h)
}
