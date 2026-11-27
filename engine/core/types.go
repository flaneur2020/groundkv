// Package core defines GroundKV's pure deterministic kernel contract:
// on-disk data types, wire-format constants, domain sentinel errors, and
// four composable domain interfaces aligned 1:1 with formal/dafny/*.dfy.
//
// This package contains zero I/O, zero locks, and zero implementation logic,
// enforcing a strictly unidirectional call boundary from the imperative shell
// (`engine`) down into swappable kernel implementations (`engine/gocore` and
// `engine/verified`).
package core

import "errors"

// PageID uniquely identifies a fixed-size page in the database file.
// Pages 0 and 1 are reserved for dual meta pages. Data and freelist pages start at 2.
type PageID uint64

const (
	PageFlagMeta     uint16 = 0x01
	PageFlagBranch   uint16 = 0x02
	PageFlagLeaf     uint16 = 0x04
	PageFlagFreelist uint16 = 0x08

	// PageHeaderSize is the fixed on-disk page header size in bytes:
	//   [0:8]   pgid     (uint64)
	//   [8:10]  flags    (uint16)
	//   [10:12] count    (uint16)
	//   [12:16] checksum (uint32, CRC32-IEEE over entire page with [12:16] zeroed)
	//   [16:24] txid     (uint64, transaction that allocated this page)
	PageHeaderSize = 24

	// LeafEntryHeaderSize: ksize(uint32) + vsize(uint32)
	LeafEntryHeaderSize = 8
	// BranchEntryHeaderSize: ksize(uint32) + child(uint64)
	BranchEntryHeaderSize = 12

	MetaMagic   uint32 = 0x47524B56 // "GRKV"
	MetaVersion uint32 = 1
	// MetaPayloadSize: magic(4) + version(4) + pageSize(4) + reserved(4) + root(8) + highWater(8) + txid(8) + freelist(8) = 48
	MetaPayloadSize = 48

	// FreelistHeaderExtra: next(uint64) chain pointer immediately after PageHeaderSize.
	FreelistHeaderExtra = 8
	FreelistEntrySize   = 8
)

var (
	ErrNotFound      = errors.New("groundkv: key not found")
	ErrEmptyKey      = errors.New("groundkv: key must not be empty")
	ErrKeyTooLarge   = errors.New("groundkv: key exceeds page capacity")
	ErrValueTooLarge = errors.New("groundkv: value exceeds page capacity")
	ErrCorruptedMeta = errors.New("groundkv: both meta pages are corrupted")
	ErrCorruptedPage = errors.New("groundkv: page checksum or format mismatch")
	ErrWriteTxActive = errors.New("groundkv: write transaction already active")
)

// PageHeader holds the decoded 24-byte fixed on-disk page header.
type PageHeader struct {
	PGID     PageID
	Flags    uint16
	Count    uint16
	Checksum uint32
	TxID     uint64
}

// Meta represents the superblock stored in Page 0 and Page 1.
// Commits alternate between Page 0 (even TxID) and Page 1 (odd TxID).
type Meta struct {
	Magic     uint32
	Version   uint32
	PageSize  uint32
	Root      PageID
	HighWater PageID
	TxID      uint64
	Freelist  PageID
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

// CommitPhase identifies the stage of a 4-phase write transaction.
type CommitPhase uint8

const (
	CommitPhaseIdle CommitPhase = iota
	CommitPhaseActive
	CommitPhaseDataWritten
	CommitPhaseDataSynced
	CommitPhaseMetaWritten
)

// String returns the canonical protocol phase name used by diagnostics and TLA+ traces.
func (p CommitPhase) String() string {
	switch p {
	case CommitPhaseActive:
		return "STAGED"
	case CommitPhaseDataWritten:
		return "DATA_WRITTEN"
	case CommitPhaseDataSynced:
		return "DATA_SYNCED"
	case CommitPhaseMetaWritten:
		return "META_WRITTEN"
	default:
		return "IDLE"
	}
}
