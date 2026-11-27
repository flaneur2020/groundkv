// Package engine implements GroundKV's imperative storage shell: a BoltDB-inspired
// embedded key-value store built on a copy-on-write (COW) B+Tree with dual meta
// pages, MVCC snapshot isolation, reader-aware freelist reclamation, exclusive
// file locking (flock), CRC32 page framing, and 4-phase crash-safe commits.
//
// Architecture (Functional Core, Imperative Shell):
//   - engine/core:     pure deterministic types, constants, errors, and four
//     domain interfaces (Codec, BTreeNode, Freelist, Protocol) unified in Core.
//   - engine/gocore:   default pure-Go stdlib Core implementation (zero external deps).
//   - engine/verified: Z3-verified Dafny-compiled Core implementation.
//   - engine (this package): imperative I/O, locking, caching, CRC32 framing,
//     lazy page walkers, and structured NDJSON protocol tracing, calling strictly
//     downward into Core.
package engine
