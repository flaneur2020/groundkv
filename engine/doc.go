// Package engine implements GroundKV, a BoltDB-inspired embedded key-value
// storage engine built on an append-only, copy-on-write (COW) B+Tree with dual
// meta pages, MVCC snapshot isolation, and deterministic crash recovery.
//
// Design highlights:
//   - Single-file, fixed-size page layout (default 4096 bytes).
//   - Dual meta pages at page 0 and page 1 with CRC32 checksums and monotonic
//     transaction IDs; commits atomically alternate meta slots after syncing
//     newly appended B+Tree pages.
//   - Append-only COW B+Tree: write transactions allocate strictly new page IDs
//     for every modified leaf and ancestor branch, never overwriting pages
//     reachable from any committed meta page or active MVCC read snapshot.
//   - First-class testability for FizzBee Model-Based Testing (MBT): pluggable
//     Storage abstraction (including crash-simulatable storage), configurable
//     node fanout caps to exercise multi-level splits on small MBT key domains,
//     and state inspection helpers for dual meta pages and committed snapshots.
package engine
