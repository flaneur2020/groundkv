// GroundKV — Unified Dafny Verification & Go Code Generation Entry Point
// Includes all Phase 1 (pure codecs, B+Tree nodes, MVCC freelist) and Phase 2
// (recursive COW B+Tree + 4-phase dual-meta crash-recovery engine) modules.

include "Types.dfy"
include "PageCodec.dfy"
include "BTreeNode.dfy"
include "Freelist.dfy"
include "BTree.dfy"
include "StorageEngine.dfy"
