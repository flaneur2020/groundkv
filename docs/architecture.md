# GroundKV Architecture

GroundKV is a BoltDB-inspired embedded key-value store built around an **append-only, copy-on-write (COW) B+Tree**, designed from day one so that **FizzBee formal specifications and Model-Based Testing (MBT)** drive and validate every layer of the engine.

## 1. On-Disk Layout

Every GroundKV database is a single file partitioned into fixed-size pages (`PageSize`, default `4096` bytes, power of two >= `256`).

```text
+------------------+------------------+------------------+------------------+-----+
| Page 0: Meta 0   | Page 1: Meta 1   | Page 2: B+Tree   | Page 3: B+Tree   | ... |
| (even TxIDs)     | (odd TxIDs)      | (Leaf / Branch)  | (Leaf / Branch)  |     |
+------------------+------------------+------------------+------------------+-----+
```

### Page Header (24 bytes)

Every page (meta, branch, leaf) begins with a uniform header protected by a full-page **CRC32-IEEE** checksum:

| Offset | Size | Field      | Description |
|--------|------|------------|-------------|
| 0      | 8    | `pgid`     | Page ID (verified against read offset) |
| 8      | 2    | `flags`    | `0x01` Meta, `0x02` Branch, `0x04` Leaf |
| 10     | 2    | `count`    | Number of entries in the page |
| 12     | 4    | `checksum` | CRC32-IEEE over entire page with bytes `[12:16]` zeroed |
| 16     | 8    | `txid`     | Transaction ID that allocated/wrote this page |

### Dual Meta Pages (Atomic Commit & Crash Recovery)

- **Page 0** stores commits with even `TxID` (`txid % 2 == 0`).
- **Page 1** stores commits with odd `TxID` (`txid % 2 == 1`).
- Each `Meta` payload stores `Magic ("GRKV")`, `Version`, `PageSize`, `Root` (`PageID`), `HighWater` (next append-only `PageID`), and `TxID`.
- **Commit protocol**:
  1. Write all dirty COW B+Tree pages strictly at `pgid >= oldHighWater` (never overwriting live pages).
  2. `Sync()` data pages to stable storage.
  3. Write the alternate meta slot (`txid % 2`) and `Sync()`.
  4. Publish the new `Meta` in memory.
- **Recovery protocol**: On `Open` / `Reopen`, read both Page 0 and Page 1, validate magic/version/checksum, and select the valid meta page with the higher `TxID`. A torn write to the newer meta page automatically falls back to the previous committed generation.

## 2. Append-Only COW B+Tree & MVCC

- **Single writer, multiple concurrent readers**:
  - `BeginRead()` / `View()` pins a private copy of the current committed `Meta`. Because write transactions never mutate committed pages in place, readers traverse an immutable page graph with zero locks on data pages.
  - `BeginWrite()` / `Update()` enforces a single active writer. Mutations (`Put`, `Delete`) copy modified leaf and ancestor branch nodes into an in-memory `dirty` map keyed by strictly increasing `PageID`s.
  - `Rollback()` on a write transaction simply drops the in-memory `dirty` map — zero disk I/O and zero leakage to readers or post-crash recovery.
- **Configurable fanout caps (`MaxLeafKeys`, `MaxBranchKeys`)**:
  - In production, nodes split when serialized entries exceed `PageSize`.
  - In tests and FizzBee MBT, setting `MaxLeafKeys: 2, MaxBranchKeys: 2` forces real leaf splits, branch splits, root promotions, and empty-node collapses using just 2–3 distinct keys!

## 3. FizzBee MBT as a First-Class Citizen

GroundKV separates **what** the public KV API guarantees (black-box contract) from **how** the storage internals uphold crash safety and MVCC reclamation (white-box protocol), with zero tautological ghost booleans and zero adapter shadow state:

| Spec | Layer | Roles & Execution | Verified Safety Invariants & Checks |
|------|-------|-------------------|-------------------------------------|
| `models/contract/kv_contract.fizz` | **Black-Box Contract** (zero storage internals) | `Writer#0`, `Reader#0`, `Reader#1`, `SnapshotReader#0` — **parallel MBT (`max-parallel-runs: 200`) under `-race`** | Linearizable `Put`/`Delete`, multi-key atomic `BatchSwap`, isolated `AbortedPut`, concurrent lock-free `Get`/`ScanAll`, and pinned `SnapshotReader` isolation |
| `models/storage_protocol/storage_protocol.fizz` | **White-Box Protocol** (real abstract `pgid`s + volatile/durable `Disk`) | `Disk#0` + `Engine#0` — 4-phase `CommitStep`, `Put`/`Delete`/`RollbackWrite`, MVCC reader, `Disk.InspectMeta`, and `Disk.Crash(DROP\|REORDERED\|TORN)` | `AtLeastOneValidMeta`, `ValidMetaRootsMatchDurablePages`, `NoLivePageInFreelist`, `ReaderSeesIntactDurablePage`, `CommittedTxIDNeverRegresses` — verified against real `MemStorage.CloneDurable()`, `InspectMeta()`, and `ReachablePages()` with zero adapter shadow state |
| `models/btree_ops/btree_ops.fizz` | **Data-Structure & Range Query** | `BTree#0` with `MaxLeafKeys=2`, `MaxBranchKeys=2`, and **freelist enabled** | Multi-level splits, deletes, root collapses, empty-tree transitions, and half-open `[start, end)` `ScanRange` correctness + `db.Validate()` |

### 4. Verification Pyramid: Dafny Verified Codegen + Lean 4 Proofs + FizzBee MBT + TLA+ Trace Validation + DST + OS Crash/Linearizability + Go Fuzzing

GroundKV combines seven complementary verification layers:
1. **Dafny Verified Implementation Compiled to Go (`formal/dafny/` → `verified/`, `make dafny`)**: Executable, Z3-verified Dafny modules (zero `assume`, zero `{:verify false}`, zero `{:axiom}`) compiled to Go via `dafny translate go` and wired into the sibling `verified/` package alongside the unmodified pure-Go `engine/` package:
   - **Phase 1 (`Types.dfy`, `PageCodec.dfy`, `BTreeNode.dfy`, `Freelist.dfy`)**: strict total-order byte comparison, little-endian page codec round-trip bijections, sorted leaf KV-map refinement, BoltDB `BranchChildIndex`, byte-aware 2-way + greedy multi-way splits, MVCC `ReleaseBound`, and exact `ComputeFreelistPageCount` sufficiency & minimality.
   - **Phase 2 (`BTree.dfy`, `StorageEngine.dfy`)**: recursive COW B+Tree (`TreeGet`/`TreeScan`/`TreePut`/`TreeDelete`/`TreeValidate`) with end-to-end KV-map refinement & COW snapshot immutability, plus the 4-phase dual-meta commit protocol, `dirtyAltMeta` double-fault fence, and crash-refinement theorems (`CrashRecoveryRefinesAtomicKV`, `DoubleFaultRollbackFenceSafety`, `SelectRecoveredMeta`).
   - Verified for bit-for-bit reproducibility in CI (`make dafny`) and cross-engine on-disk interoperability against `engine/` (`verified/verified_test.go`).
2. **Unbounded Machine-Checked Lean 4 Proofs (`formal/lean/`, `make prove`)**: 13 machine-checked theorems in pure Lean 4 stdlib (zero `sorry`, zero custom axioms, audited via `#print axioms`) proving unbounded dual-meta crash atomicity/durability & POSIX `FailSync`/`dirtyAltMeta` fencing (`DualMeta.lean`), multi-reader MVCC freelist zero use-after-free (`FreelistMVCC.lean`), exact `prepareFreelistPages()` sufficiency/minimality & `perPage >= 3` convergence vs `perPage <= 2` divergence (`FreelistSizing.lean`), and byte-aware 2-way + greedy multi-way B+Tree split correctness (`BTreeSplit.lean`).
3. **FizzBee MBT (`models/`, `fizztests/`)**: Exhaustive finite-state safety & temporal liveness (`always eventually` + `fair`) verification (including COW freelist `pgid` tracking, realistic POSIX `FailSync`, and double-fault `dirty_alt_meta` fencing) + parallel `-race` execution + **5 formal spec mutation checks** (`make check-mutations`) + **4 Go-side code mutation checks** (`make check-code-mutations`).
4. **TLA+ Protocol Spec & Formal Trace Validation (`formal/tla/`, `engine/trace.go`)**: Unbounded TLA+ specification (`GroundKVProtocol.tla`) plus TLC Trace Validation (`GroundKVTrace.tla`, `make trace-validate`) replaying thousands of real Go protocol transitions emitted by `engine.JSONLineTracer` during DST runs back against the formal spec (plus negative-control corrupt trace rejection).
5. **Deterministic Simulation Testing (`dst/`)**: Single-threaded, 100% seed-reproducible Swarm Testing across 7 randomized storage profiles (`hot-churn-crash`, `wide-deep-btree`, `binary-byte-splits`, `chaos-io-faults`, `extreme-3way-splits`, `double-fault-meta-sync`, `hybrid-swarm`) with 64-byte sector-level `SimDisk` power-loss faults, silent bit-rot (LSE) verification, probabilistic `ReadAt`/`WriteAt`/`Sync` I/O errors, multi-reader MVCC pools, exact shadow-oracle verification, and **19 mandatory "sometimes" scenario coverage assertions**.
6. **Real OS Crash & Concurrent Linearizability (`dst/oscrash_test.go`, `dst/linearizability_test.go`)**: Subprocess `SIGKILL` crash-restart cycles on real `OSFileStorage` (`flock` + `fsync`) and real-time interval linearizability checking across concurrent readers, writers, and long-lived MVCC snapshots under `-race`.
7. **Go Native Fuzzing (`engine/fuzz_test.go`)**: Mutation fuzzing of raw page decoders (`FuzzDecodePages`) and corrupted/cyclic/truncated full DB images (`FuzzOpenCorruptStorage`).

### Evolution Roadmap

Natural next specs & engine milestones to add using the same Spec -> Scaffold -> Implement -> Verify loop:
1. **Buckets / Nested B+Trees** (`models/bucket/bucket.fizz`): BoltDB-style named buckets with inline sub-trees.
2. **Compaction / Online Snapshot Backup**: Streaming consistent snapshot copy via MVCC read transactions.
