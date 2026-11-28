# GroundKV Architecture

GroundKV is a BoltDB-inspired embedded key-value store built around a **copy-on-write (COW) B+Tree** with **MVCC reader-aware freelist reclamation**, **zero third-party Go dependencies**, and a strictly unidirectional **Functional Core, Imperative Shell** architecture verified across six complementary formal & simulation layers.

## 1. On-Disk Layout

Every GroundKV database is a single file partitioned into fixed-size pages (`PageSize`, default `4096` bytes, power of two >= `256`).

```text
+------------------+------------------+------------------+------------------+-----+
| Page 0: Meta 0   | Page 1: Meta 1   | Page 2: B+Tree / | Page 3: B+Tree / | ... |
| (even TxIDs)     | (odd TxIDs)      | Freelist         | Freelist         |     |
+------------------+------------------+------------------+------------------+-----+
```

### Page Header (24 bytes)

Every page (meta, branch, leaf, freelist) begins with a uniform header protected by a full-page **CRC32-IEEE** checksum:

| Offset | Size | Field      | Description |
|--------|------|------------|-------------|
| 0      | 8    | `pgid`     | Page ID (verified against read offset) |
| 8      | 2    | `flags`    | `0x01` Meta, `0x02` Branch, `0x04` Leaf, `0x08` Freelist |
| 10     | 2    | `count`    | Number of entries in the page |
| 12     | 4    | `checksum` | CRC32-IEEE over entire page with bytes `[12:16]` zeroed |
| 16     | 8    | `txid`     | Transaction ID that allocated/wrote this page |

### Dual Meta Pages (Atomic Commit & Crash Recovery)

- **Page 0** stores commits with even `TxID` (`txid % 2 == 0`).
- **Page 1** stores commits with odd `TxID` (`txid % 2 == 1`).
- Each `Meta` payload stores `Magic ("GRKV")`, `Version`, `PageSize`, `Root` (`PageID`), `HighWater`, `TxID`, and `Freelist` (`PageID` of the COW freelist chain head).
- **4-Phase Commit protocol**:
  1. `WriteDataPages`: write all dirty COW B+Tree pages + COW freelist chain pages (never overwriting any page reachable from either meta slot or any open MVCC reader).
  2. `SyncDataPages`: neutralize any prior `dirtyAltMeta` slot if a double-fault occurred on an earlier rollback, then `Sync()` data pages to stable storage.
  3. `WriteMetaPage`: write the alternate meta slot (`(txid + 1) % 2`).
  4. `SyncMetaPage`: `Sync()` the meta page, update freelist `pending`/`free` pools under `releaseBound = min(max(0, txid - 1), minActiveReaderTxID)`, and publish the new `Meta` in memory.
- **Recovery protocol**: On `Open` / `Reopen`, read both Page 0 and Page 1, validate magic/version/checksum/parity, and select the valid meta page with the higher `TxID`. A torn write to the newer meta page automatically falls back to the previous committed generation.

## 2. Functional Core, Imperative Shell (`engine/core`)

GroundKV enforces a **compiler-checked one-way dependency DAG**:

```text
engine/core     ──▶ [errors]                      (pure types, constants, errors, 4 interfaces)
engine/gocore   ──▶ [engine/core + Go stdlib]     (pure-Go Core implementation)
engine          ──▶ [engine/core, engine/gocore]  (imperative I/O, locking, CRC32, lazy walkers)
engine/verified ──▶ [engine/core, dafnygen]       (Z3-verified Dafny Core implementation)
```

`engine/core.Core` composes four domain interfaces aligned 1:1 with `formal/dafny/`:
- **`Codec`** (`PageCodec.dfy`): page header, meta, leaf, branch, and freelist wire codecs & sizing.
- **`BTreeNode`** (`BTreeNode.dfy`): sorted node lookup/mutation (`LeafGet`/`LeafPut`/`LeafDelete`), `BranchChildIndex`, and byte-aware 2-way + greedy multi-way splitting (`SplitLeafEntries`/`SplitBranchEntries`).
- **`Freelist` + `FreelistPool`** (`Freelist.dfy`): MVCC `ReleaseBound`, `ComputeFreelistPageCount`, and `pending`/`free` page pool.
- **`Protocol`** (`StorageEngine.dfy`): `SelectRecoveredMeta` and 4-phase `ValidNextPhase`.

## 3. Verification Pyramid (Three Orthogonal Formal Pillars + Simulation & Fuzzing)

1. **Dafny Verified Kernel Compiled to Go (`formal/dafny/` → `engine/verified/`, `make dafny`)**: Executable, Z3-verified Dafny modules (1,027 verified, zero `assume`, zero `{:verify false}`, zero `{:axiom}`) compiled to Go via `dafny translate go`, encapsulated behind `engine/verified/internal/dafnygen/`, and plugged into GroundKV's imperative shell via `engine/core.Core`:
   - **Phase 1 (`Types.dfy`, `PageCodec.dfy`, `BTreeNode.dfy`, `Freelist.dfy`)**: strict total-order byte comparison, little-endian page codec round-trip bijections, sorted leaf KV-map refinement, BoltDB `BranchChildIndex`, byte-aware 2-way + greedy multi-way splits, MVCC `ReleaseBound`, and exact `ComputeFreelistPageCount` sufficiency & minimality.
   - **Phase 2 (`BTree.dfy`, `StorageEngine.dfy`)**: recursive COW B+Tree (`TreeGet`/`TreeScan`/`TreePut`/`TreeDelete`/`TreeValidate`) with end-to-end KV-map refinement & COW snapshot immutability, plus the 4-phase dual-meta commit protocol, `dirtyAltMeta` double-fault fence, and crash-refinement theorems.
   - Verified for bit-for-bit reproducibility in CI (`make dafny`) and cross-kernel differential/on-disk interoperability against `engine.GoCore()`.
2. **Unbounded Machine-Checked Lean 4 Proofs (`formal/lean/`, `make prove`)**: 13 machine-checked theorems in pure Lean 4 stdlib (zero `sorry`, zero custom axioms, audited via `#print axioms`) proving unbounded dual-meta crash atomicity/durability & POSIX `FailSync`/`dirtyAltMeta` fencing (`DualMeta.lean`), multi-reader MVCC freelist zero use-after-free (`FreelistMVCC.lean`), exact `prepareFreelistPages()` sufficiency/minimality & `perPage >= 3` convergence vs `perPage <= 2` divergence (`FreelistSizing.lean`), and byte-aware 2-way + greedy multi-way B+Tree split correctness (`BTreeSplit.lean`).
3. **Quint Formal Specs, Spec Mutations, Trace Validation & ITF MBT (`formal/quint/`, `verify/mbt/`)**:
   - **`storage_protocol.qnt` & `btree_ops.qnt`**: Strongly-typed Quint specs verified via `quint typecheck`, `quint test`, `quint run`, and **5 negative-control formal spec mutation checks** (`make check-mutations`).
   - **Quint ITF Model-Based Testing (`verify/mbt/`, `make mbt`)**: Zero-dependency Go stdlib ITF driver replaying generated Quint traces against **both** `engine.GoCore()` and `verified.NewCore()` under `-race` with zero adapter shadow state (`MemStorage.CloneDurable()`, `InspectMeta()`, `ReachablePages()`, `FreelistState()`, `Validate()`, `Scan()`).
   - **Quint Trace Validation (`trace_protocol.qnt`, `make trace-validate`)**: Replays ~4,500 real Go protocol transitions emitted by `engine.JSONLineTracer` across 8 DST swarm seeds (plus 2 negative-control corrupt traces that must be rejected).
4. **Deterministic Simulation Testing (`verify/dst/`, `make dst`)**: Single-threaded, 100% seed-reproducible Swarm Testing across 7 randomized storage profiles (`profiles.go`, `sim.go`, `oracle.go`) with 64-byte sector-level `SimDisk` power-loss faults, silent bit-rot (LSE) verification, probabilistic `ReadAt`/`WriteAt`/`Sync` I/O errors, multi-reader MVCC pools, exact shadow-oracle verification, **19 mandatory "sometimes" scenario coverage assertions**, and **4 Go-side code mutation proofs** (`make check-code-mutations`).
5. **Real OS Crash & Concurrent Linearizability (`verify/dst/oscrash_test.go`, `verify/dst/linearizability_test.go`)**: Subprocess `SIGKILL` crash-restart cycles on real `OSFileStorage` (`flock` + `fsync`) and real-time interval linearizability checking across concurrent readers, writers, and long-lived MVCC snapshots under `-race`.
6. **Go Native Fuzzing (`engine/fuzz_test.go`, `make fuzz`)**: Deterministic mutation fuzzing of raw page decoders (`FuzzDecodePages`) and corrupted/cyclic/truncated full DB images (`FuzzOpenCorruptStorage`).
