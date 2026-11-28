# GroundKV

A BoltDB-inspired embedded key-value engine in Go built on a **copy-on-write (COW) B+Tree** with **MVCC reader-aware freelist reclamation**, **zero third-party Go dependencies**, and a **6-layer formal verification pyramid** (Dafny verified Go codegen, Lean 4 unbounded proofs, Quint formal specs / mutations / trace validation / ITF MBT, Deterministic Simulation Testing, real `SIGKILL` crash & linearizability checks, and native Go fuzzing).

## Features

- **Functional Core, Imperative Shell (Compiler-Enforced One-Way `engine ──▶ engine/core`)**:
  - **`engine/core/` (Pure Contract)**: zero-I/O data types, wire constants, sentinel errors, and four domain interfaces aligned 1:1 with `formal/dafny/` (`Codec`, `BTreeNode`, `Freelist`/`FreelistPool`, `Protocol`) composed into a unified `Core` (+ `CompositeCore`).
  - **`engine/gocore/` (Pure Go — `engine.GoCore()`)**: zero-dependency Go stdlib `core.Core` implementation importing only `engine/core` + stdlib.
  - **`engine/verified/` (Dafny-Verified Go — `verified.NewCore()`, `make dafny`)**: symmetric peer `core.Core` implementation backed directly by Z3-verified Dafny code translated to Go (encapsulated under `engine/verified/internal/dafnygen/`):
    - **Phase 1 — Verified Pure Core** (`Types.dfy`, `PageCodec.dfy`, `BTreeNode.dfy`, `Freelist.dfy`): total-order byte comparison lemmas, little-endian Meta/Leaf/Branch/Freelist round-trip bijections, sorted `LeafGet`/`LeafPut`/`LeafDelete` KV-map refinement, `BranchChildIndex`, byte-aware 2-way + greedy multi-way splitting, MVCC `ReleaseBound`, and exact `ComputeFreelistPageCount` sufficiency & minimality.
    - **Phase 2 — Verified Full Tree & 4-Phase Storage Engine** (`BTree.dfy`, `StorageEngine.dfy`): end-to-end recursive COW B+Tree (`TreeGet`/`TreeScan`/`TreePut`/`TreeDelete`/`TreeValidate`) proving KV-map refinement & COW snapshot immutability, plus the 4-phase dual-meta commit protocol, `dirtyAltMeta` double-fault rollback fence, and crash-recovery refinement (`SelectRecoveredMeta`, `CrashRecoveryRefinesAtomicKV`, `DoubleFaultRollbackFenceSafety`).
    - Audited in CI (`make dafny`) for zero `assume` / `{:verify false}` / `{:axiom}` and bit-for-bit reproducibility of `engine/verified/internal/dafnygen/`.
    - Select either kernel in Go (`engine.Open(...)` vs `verified.Open(...)` / `engine.Options{Core: verified.NewCore()}`) or in the CLI (`groundkv -engine go|verified`). Both share the exact same I/O/locking shell (`engine/`) and bit-compatible on-disk format.
- **Dual Meta Pages & Crash Recovery**: alternating meta pages (Page 0 and Page 1) with 4-phase `Sync`-ordered commits, `dirtyAltMeta` double-fault fencing, and automatic fallback on torn meta writes.
- **MVCC Snapshot Isolation & Freelist Reclamation**: concurrent lock-free readers pinned to immutable B+Tree roots alongside a single serialized writer; freed pages are persisted as COW freelist chains and recycled strictly when `freeTxID <= min(txid-1, minActiveReaderTxID)`.
- **Pluggable Storage (`engine.Storage`)**: real OS file storage (`OSFileStorage` with exclusive `flock` locking and parent-directory `fsync` on DB creation) plus per-write fault-injecting `MemStorage`.
- **Three Strictly Orthogonal Formal Layers (`formal/`)**:
  - **`formal/quint/`** — 3 strongly-typed Quint specs (`storage_protocol.qnt`, `btree_ops.qnt`, `trace_protocol.qnt`) covering invariant simulation, **5 formal spec mutation proofs** (`make check-mutations`), **Quint Trace Validation** replaying ~4,500 real Go DST NDJSON transitions (`make trace-validate`), and **zero-dependency Go ITF Model-Based Testing** (`verify/mbt/`, `make mbt`) driving BOTH `GoCore` and `verified.NewCore` under `-race`.
  - **`formal/lean/`** — 13 unbounded machine-checked Lean 4 theorems (`DualMeta.lean`, `FreelistMVCC.lean`, `FreelistSizing.lean`, `BTreeSplit.lean`; pure Lean 4 stdlib, zero `sorry`, zero custom axioms, verified via `#print axioms` in `make prove`).
  - **`formal/dafny/`** — 6 Dafny modules (1,027 verified assertions, zero `assume`) compiled to Go under `engine/verified/internal/dafnygen/`.

## Project Layout

```text
groundkv/
├── engine/                          # Imperative storage shell (one-way calls -> engine/core)
│   ├── core/                        # Pure contract: types, errors, constants & 4 domain interfaces (zero impl/I/O)
│   │   ├── types.go                 # PageID, PageHeader, Meta, LeafEntry, BranchEntry, CommitPhase, errors
│   │   └── core.go                  # Codec, BTreeNode, Freelist/FreelistPool, Protocol, Core, CompositeCore
│   ├── gocore/                      # Pure-Go core.Core peer implementation (imports only engine/core + stdlib)
│   │   ├── core.go                  # gocore.New() composed pure-Go Core
│   │   ├── codec.go / btree.go      # Pure-Go Codec & BTreeNode implementations
│   │   └── freelist.go / protocol.go# Pure-Go Freelist/FreelistPool & Protocol implementations
│   ├── verified/                    # Dafny-verified core.Core peer implementation (1:1 mirror of gocore/)
│   │   ├── core.go                  # verified.NewCore(), verified.Open/OpenStorage
│   │   ├── codec.go / btree.go      # Dafny Codec (PageCodec.dfy) & BTreeNode (BTreeNode.dfy) adapters
│   │   ├── freelist.go / protocol.go# Dafny Freelist (Freelist.dfy) & Protocol (StorageEngine.dfy) adapters
│   │   ├── internal/dafnygen/       # Generated Go packages translated from formal/dafny/*.dfy (encapsulated)
│   │   ├── verified_test.go         # End-to-end, MVCC, crash-fence, CompositeCore & cross-engine interop tests
│   │   └── phase2_test.go           # Direct Phase 2 GroundKVBTree & GroundKVEngine refinement tests
│   ├── core.go / options.go / doc.go# Public type aliases, Options, errors & package overview
│   ├── page.go / btree.go           # Zero-alloc CRC32 page framing & lazy on-disk COW B+Tree walker
│   ├── db.go / inspect.go           # DB lifecycle/recovery & MBT/diagnostic inspection helpers
│   ├── tx.go / tx_commit.go         # Tx KV API, COW page allocator, 4-phase commit pipeline & rollback fence
│   ├── storage.go / memstorage.go   # Storage interface, OSFileStorage (+ flock) & fault-injecting MemStorage
│   ├── trace.go                     # Zero-overhead-when-nil structured NDJSON protocol tracer
│   └── db_test.go / fuzz_test.go    # Unit, concurrency (-race), crash, freelist & native fuzz tests
├── formal/                          # 3 strictly orthogonal formal verification layers
│   ├── quint/                       # Quint specs (storage_protocol, btree_ops, trace_protocol) -> MBT & trace validation
│   ├── lean/                        # Unbounded Lean 4 proofs (13 theorems, 0 sorry)
│   └── dafny/                       # Dafny Phase 1 & Phase 2 proofs -> Go codegen (1,027 verified, 0 assume)
├── verify/                          # Executable verification harnesses (zero external Go deps)
│   ├── dst/                         # Deterministic Simulation Testing, SIGKILL crash & linearizability
│   │   ├── profiles.go              # 7 Swarm Testing profiles
│   │   ├── sim.go                   # Seed-reproducible simulation loop & fault-injecting step dispatch
│   │   ├── oracle.go                # Shadow oracle, MVCC/freelist disjointness & bit-rot invariant verifier
│   │   ├── simdisk.go / coverage.go # 64B sector-level SimDisk & 19 "sometimes" coverage assertions
│   │   └── *_test.go                # Swarm, replay, trace export, SIGKILL & linearizability tests
│   └── mbt/                         # Pure-Go stdlib Quint ITF MBT driver (GoCore + verified.NewCore)
│       ├── itf.go                   # Minimal ITF JSON parser & Quint trace generator
│       ├── storage_protocol_test.go # White-box dual-meta, MVCC, FailSync & 3-mode crash MBT
│       └── btree_ops_test.go        # 3-level B+Tree splits, deletes, collapses & [start, end) scan MBT
├── cmd/
│   ├── groundkv/                    # CLI (-engine go|verified: put, get, del, scan, meta, verify)
│   └── groundkv-dst/                # Standalone DST swarm runner (-seed, -seeds, -steps)
├── scripts/                         # Verification, mutation, Dafny codegen, Lean & Quint trace automation
├── docs/architecture.md             # Deep dive on disk format, Core strategy & verification pyramid
└── Makefile                         # Run `make help` to list all verification & build targets
```

## Quick Start

### 1. List all Makefile targets

```bash
make help
```

### 2. Run the full verification suite (`make verify`)

Runs Dafny verification & codegen reproducibility audit, Lean 4 proofs, Quint specs, spec & Go code mutation checks, Quint trace validation on real DST traces, `-race` tests (both `engine/` and `engine/verified/`), DST swarm, and Quint ITF MBT:

```bash
make verify
```

### 3. Run Go native fuzzing

```bash
make fuzz
```

### 4. Build & use the CLI (`-engine go` or `-engine verified`)

```bash
make build
./bin/groundkv -db demo.db -engine go put hello world
./bin/groundkv -db demo.db -engine verified get hello
./bin/groundkv -db demo.db -engine verified meta
./bin/groundkv -db demo.db -engine verified verify
```
