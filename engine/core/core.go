package core

// Codec abstracts pure little-endian binary page wire formats and sizing
// (corresponds to formal/dafny/PageCodec.dfy).
type Codec interface {
	EncodePageHeader(h PageHeader) []byte
	DecodePageHeader(buf []byte) (PageHeader, error)

	MetaSlot(txid uint64) PageID
	EncodeMetaPage(pageSize int, slot PageID, m Meta) ([]byte, error)
	DecodeMetaPayload(buf []byte, slot PageID, h PageHeader) (Meta, error)

	LeafEncodedSize(entries []LeafEntry) int
	EncodeLeafPage(pageSize int, pgid PageID, txid uint64, entries []LeafEntry) ([]byte, error)
	DecodeLeafPage(buf []byte, h PageHeader) ([]LeafEntry, error)

	BranchEncodedSize(entries []BranchEntry) int
	EncodeBranchPage(pageSize int, pgid PageID, txid uint64, entries []BranchEntry) ([]byte, error)
	DecodeBranchPage(buf []byte, h PageHeader) ([]BranchEntry, error)

	MaxFreelistIDsPerPage(pageSize int) int
	EncodeFreelistPage(pageSize int, pgid PageID, txid uint64, next PageID, ids []PageID) ([]byte, error)
	DecodeFreelistPage(buf []byte, h PageHeader) (next PageID, ids []PageID, err error)
}

// BTreeNode abstracts pure sorted B+Tree node lookup, mutation, and byte-aware
// multi-way splitting (corresponds to formal/dafny/BTreeNode.dfy).
type BTreeNode interface {
	LeafGet(entries []LeafEntry, key []byte) ([]byte, bool)
	LeafPut(entries []LeafEntry, key, value []byte) []LeafEntry
	LeafDelete(entries []LeafEntry, key []byte) ([]LeafEntry, bool)
	BranchChildIndex(entries []BranchEntry, key []byte) int
	SplitLeafEntries(entries []LeafEntry, pageSize, maxKeys int) [][]LeafEntry
	SplitBranchEntries(entries []BranchEntry, pageSize, maxKeys int) [][]BranchEntry
}

// FreelistPool abstracts the pure in-memory MVCC pending/free page pool
// (corresponds to GroundKVFreelist.FreelistState in formal/dafny/Freelist.dfy).
type FreelistPool interface {
	FreeCount() int
	AddPending(txid uint64, ids []PageID)
	Release(maxSafeTxID uint64)
	Pop() (PageID, bool)
	ReturnToFree(ids []PageID)
	FreeIDs() []PageID
	PendingTxIDs() []uint64
	SnapshotAllIDs(extraIDs []PageID) []PageID
}

// Freelist abstracts MVCC freelist release-bound math, chain sizing, and pool
// construction (corresponds to formal/dafny/Freelist.dfy).
type Freelist interface {
	ReleaseBound(currentTxID uint64, hasReader bool, minActiveReaderTxID uint64) uint64
	ComputeFreelistPageCount(initialAllLen, freeCount, localFreeLen, perPage int) int
	NewFreelist() FreelistPool
}

// Protocol abstracts dual-meta crash-recovery selection and 4-phase commit
// state-machine guards (corresponds to formal/dafny/StorageEngine.dfy).
type Protocol interface {
	SelectRecoveredMeta(meta0 Meta, ok0 bool, meta1 Meta, ok1 bool) (Meta, error)
	ValidNextPhase(current, next CommitPhase) bool
}

// Core unifies GroundKV's four pure deterministic domain kernels:
//   - Codec:     page headers, meta, leaf, branch, and freelist binary codecs
//   - BTreeNode: sorted node lookup/mutation and byte-aware multi-way splitting
//   - Freelist:  MVCC release-bound math, chain sizing, and pending/free pool
//   - Protocol:  dual-meta recovery selection and 4-phase commit phase guards
type Core interface {
	Codec
	BTreeNode
	Freelist
	Protocol
}

// CompositeCore builds a unified Core from independent domain implementations.
// Because every Core satisfies all four domain interfaces, callers can mix and
// match verified and pure-Go kernels per domain.
type CompositeCore struct {
	Codec
	BTreeNode
	Freelist
	Protocol
}

var _ Core = CompositeCore{}
