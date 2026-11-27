package engine

// Core unifies GroundKV's pure deterministic domain kernels:
//   - PageCodec:    fixed page headers and B+Tree leaf/branch binary codecs
//   - MetaCore:     dual-meta superblock codecs, slot parity, and recovery selection
//   - BTreeCore:    sorted node lookup/mutation and byte-aware multi-way splitting
//   - FreelistCore: MVCC freelist page codecs, release bound, chain sizing, and pool
//   - CommitCore:   4-phase commit state-machine transition guards
//
// Each domain sub-interface is defined alongside its default pure-Go
// implementation in its own file (page.go, meta.go, btree.go, freelist.go,
// tx.go), while callers configure a single unified Core via Options.Core.
//
// By default engine.Open / engine.OpenStorage uses GoCore() (pure Go stdlib,
// zero external runtime dependencies). Callers may pass verified.NewCore() via
// Options.Core (or call verified.Open / verified.OpenStorage) to execute the
// Z3-verified Dafny-compiled Go kernel inside the exact same I/O & locking shell.
type Core interface {
	PageCodec
	MetaCore
	BTreeCore
	FreelistCore
	CommitCore
}

// CompositeCore builds a unified Core from independent domain implementations.
// Because every Core satisfies all five domain sub-interfaces, callers can
// compose mixed kernels (e.g., verified BTreeCore + FreelistCore with pure-Go
// PageCodec) or replace a single domain in tests.
type CompositeCore struct {
	PageCodec
	MetaCore
	BTreeCore
	FreelistCore
	CommitCore
}

type goCore struct {
	goPageCodec
	goMetaCore
	goBTreeCore
	goFreelistCore
	goCommitCore
}

var (
	_ Core = goCore{}
	_ Core = CompositeCore{}

	defaultGoCore Core = goCore{}
)

// GoCore returns GroundKV's default pure-Go Core implementation.
func GoCore() Core { return defaultGoCore }
