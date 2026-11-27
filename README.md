# GroundKV

A BoltDB-inspired embedded key-value engine in Go built on a **copy-on-write (COW) B+Tree** with **MVCC reader-aware freelist reclamation**, designed with a **7-layer formal verification pyramid** (Dafny verified Go codegen, Lean 4 unbounded proofs, FizzBee model checking & MBT, TLA+ trace validation, Deterministic Simulation Testing, real `SIGKILL` crash & linearizability checks, and native Go fuzzing).

## Features

- **Shared I/O Shell + Swappable Pure Kernel (`engine.Core`)**:
  - **`engine/` (Pure Go — `engine.GoCore()`)**: zero-dependency Go stdlib COW B+Tree engine with fixed-size CRC32-checksummed pages, byte-aware 2-way & greedy multi-way splitting, root promotion & collapse, half-open `[start, end)` range scans, structural validation (`db.Validate()`), exclusive `flock` locking, and 4-phase crash-safe commits.
  - **`verified/` (Dafny-Verified Go — `verified.NewCore()`, `make dafny`)**: implements `engine.Core` (`PageCodec`, `MetaCore`, `BTreeCore`, `FreelistCore`/`FreelistPool`, `CommitCore`) backed directly by Z3-verified Dafny code translated to Go (encapsulated under `verified/internal/dafnygen/`):
    - **Phase 1 — Verified Pure Core** (`Types.dfy`, `PageCodec.dfy`, `BTreeNode.dfy`, `Freelist.dfy`): total-order byte comparison lemmas, little-endian Meta/Leaf/Branch/Freelist round-trip bijections, sorted `LeafGet`/`LeafPut`/`LeafDelete` KV-map refinement, `BranchChildIndex`, byte-aware 2-way + greedy multi-way splitting, MVCC `ReleaseBound`, and exact `ComputeFreelistPageCount` sufficiency & minimality.
    - **Phase 2 — Verified Full Tree & 4-Phase Storage Engine** (`BTree.dfy`, `StorageEngine.dfy`): end-to-end recursive COW B+Tree (`TreeGet`/`TreeScan`/`TreePut`/`TreeDelete`/`TreeValidate`) proving KV-map refinement & COW snapshot immutability, plus the 4-phase dual-meta commit protocol, `dirtyAltMeta` double-fault rollback fence, and crash-recovery refinement (`SelectRecoveredMeta`, `CrashRecoveryRefinesAtomicKV`, `DoubleFaultRollbackFenceSafety`).
    - Audited in CI (`make dafny`) for zero `assume` / `{:verify false}` / `{:axiom}` and bit-for-bit reproducibility of `verified/internal/dafnygen/`.
    - Select either kernel in Go (`engine.Open(...)` vs `verified.Open(...)` / `engine.Options{Core: verified.NewCore()}`) or in the CLI (`groundkv -engine go|verified`). Both share the exact same I/O/locking shell and bit-compatible on-disk format.
- **Dual Meta Pages & Crash Recovery**: alternating meta pages (Page 0 and Page 1) with 4-phase `Sync`-ordered commits, `dirtyAltMeta` double-fault fencing, and automatic fallback on torn meta writes.
- **MVCC Snapshot Isolation & Freelist Reclamation**: concurrent lock-free readers pinned to immutable B+Tree roots alongside a single serialized writer; freed pages are persisted as COW freelist chains and recycled strictly when `freeTxID <= min(txid-1, minActiveReaderTxID)`.
- **Pluggable Storage (`engine.Storage`)**: real OS file storage (`OSFileStorage` with exclusive `flock` locking and parent-directory `fsync` on DB creation) plus per-write fault-injecting `MemStorage`.
- **Unified Formal Specifications & Proofs (`formal/`)**:
  - **`formal/fizzbee/`** — 3 FizzBee specs (`contract/kv_contract.fizz`, `storage_protocol/storage_protocol.fizz`, `btree_ops/btree_ops.fizz`) verified exhaustively for safety & temporal liveness (`always eventually` + `fair`), backed by **5 formal spec mutation proofs** (`make check-mutations`) and Go MBT harnesses under `verify/mbt/`.
  - **`formal/tla/`** — Unbounded TLA+ protocol spec (`GroundKVProtocol.tla`) & TLC **Formal Trace Validation** (`GroundKVTrace.tla`, `make trace-validate`) replaying real NDJSON traces emitted by `engine.JSONLineTracer` during DST swarm runs.
  - **`formal/lean/`** — 13 unbounded machine-checked Lean 4 theorems (`DualMeta.lean`, `FreelistMVCC.lean`, `FreelistSizing.lean`, `BTreeSplit.lean`; pure Lean 4 stdlib, zero `sorry`, zero custom axioms, verified via `#print axioms` in `make prove`).
  - **`formal/dafny/`** — 6 Dafny modules (1,206 verified assertions, zero `assume`) compiled to Go under `verified/internal/dafnygen/`.

## Project Layout

```text
groundkv/
├── engine/                          # Shared I/O/locking shell + pure-Go Core (zero external deps)
│   ├── core.go                      # Unified Core interface (composing 5 domain interfaces), CompositeCore & GoCore()
│   ├── options.go                   # PageSize, MaxLeafKeys/MaxBranchKeys, NoFreelist, Tracer, Core, errors
│   ├── storage.go                   # Storage interface, OSFileStorage (+ flock), fault-injecting MemStorage
│   ├── trace.go                     # Zero-overhead-when-nil structured NDJSON protocol tracer
│   ├── page.go / meta.go            # PageCodec & MetaCore interfaces + pure-Go implementations & CRC32 helpers
│   ├── freelist.go                  # FreelistCore & FreelistPool interfaces + pure-Go pool & COW chain reader
│   ├── btree.go                     # BTreeCore interface + pure-Go node ops & COW B+Tree traversal
│   ├── tx.go / db.go                # CommitCore interface + MVCC read Tx, 4-phase COW write Tx & DB lifecycle
│   └── db_test.go / fuzz_test.go    # Unit, concurrency (-race), crash, freelist & native fuzz tests
├── verified/                        # Dafny-verified Core package (bit-compatible with engine/)
│   ├── core.go                      # verified.NewCore(), verified.Open/OpenStorage -> composed Dafny Core
│   ├── page.go / meta.go            # Dafny PageCodec & MetaCore adapters (GroundKVCodec + GroundKVEngine)
│   ├── btree.go / freelist.go       # Dafny BTreeCore & FreelistCore/FreelistPool adapters
│   ├── tx.go                        # Dafny CommitCore adapter (4-phase transition guards)
│   ├── internal/dafnygen/           # Generated Go packages translated from formal/dafny/*.dfy (encapsulated)
│   ├── verified_test.go             # End-to-end, MVCC, crash-fence, CompositeCore & cross-engine interop tests
│   └── phase2_test.go               # Direct Phase 2 GroundKVBTree & GroundKVEngine refinement tests
├── formal/                          # Unified formal methods (all 4 formal layers under one root)
│   ├── fizzbee/                     # FizzBee specs (contract, storage_protocol, btree_ops)
│   ├── tla/                         # Unbounded TLA+ protocol spec & TLC Trace Validation
│   ├── lean/                        # Unbounded Lean 4 proofs (13 theorems, 0 sorry)
│   └── dafny/                       # Dafny Phase 1 & Phase 2 proofs -> Go codegen (1,206 verified, 0 assume)
├── verify/                          # Executable verification harnesses
│   ├── dst/                         # Deterministic Simulation Testing, SIGKILL crash & linearizability
│   │   ├── profiles.go              # 7 Swarm Testing profiles
│   │   ├── sim.go                   # Seed-reproducible simulation loop & fault-injecting step dispatch
│   │   ├── oracle.go                # Shadow oracle, MVCC/freelist disjointness & bit-rot invariant verifier
│   │   ├── simdisk.go / coverage.go # 64B sector-level SimDisk & 19 "sometimes" coverage assertions
│   │   └── *_test.go                # Swarm, replay, trace export, SIGKILL & linearizability tests
│   └── mbt/                         # FizzBee MBT harnesses & zero-shadow-state Go adapters
│       ├── contract/                # kv_contract (parallel + -race on OSFileStorage)
│       ├── storage_protocol/        # storage_protocol (Disk + Engine white-box protocol)
│       └── btree_ops/               # btree_ops (splits, deletes, root collapse & range scans)
├── cmd/
│   ├── groundkv/                    # CLI (-engine go|verified: put, get, del, scan, meta, verify)
│   └── groundkv-dst/                # Standalone DST swarm runner (-seed, -seeds, -steps)
├── scripts/                         # Verification, mutation, Dafny codegen, Lean, TLA+ & MBT automation
├── docs/architecture.md             # Deep dive on disk format, Core strategy & 7-layer verification pyramid
└── Makefile                         # Run `make help` to list all verification & build targets
```

## Quick Start

### 1. List all Makefile targets

```bash
make help
```

### 2. Run the full verification suite (`make verify`)

Runs Dafny verification & codegen reproducibility audit, Lean 4 proofs, FizzBee model checking, spec & Go code mutation checks, TLA+ trace validation, `-race` tests (both `engine/` and `verified/`), DST swarm, and MBT adapter smoke tests:

```bash
make verify
```

### 3. Run full FizzBee Model-Based Testing (`make mbt`)

Download `fizzbee-mbt-*-linux_x86.tar.gz` from [fizzbee-io/fizzbee-mbt-releases](https://github.com/fizzbee-io/fizzbee-mbt-releases/releases), place `fizzbee-mbt-server` and `fizzbee-mbt-runner` on your `PATH`, and run:

```bash
make mbt
```

### 4. Regenerate MBT interfaces after editing a `.fizz` spec

```bash
make check-specs
make scaffold-interfaces
```

### 5. Try the CLI (Pure Go or Dafny-Verified Kernel)

```bash
make build
./bin/groundkv -db demo.db -engine go put hello world
./bin/groundkv -db demo.db -engine verified get hello
./bin/groundkv -db demo.db -engine verified meta
./bin/groundkv -db demo.db -engine verified verify
```
