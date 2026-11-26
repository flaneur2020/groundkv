# GroundKV

A BoltDB-inspired embedded key-value engine in Go built on a **copy-on-write (COW) B+Tree** with **MVCC reader-aware freelist reclamation**, designed with **FizzBee Model-Based Testing (MBT)** as a first-class citizen.

## Features

- **COW B+Tree** (`engine/`): fixed-size CRC32-checksummed pages, leaf/branch splitting, root promotion & collapse, half-open `[start, end)` range scans, and structural tree validation (`db.Validate()`).
- **Dual Meta Pages & Crash Recovery**: alternating meta pages (Page 0 and Page 1) with 4-phase `Sync`-ordered commits and automatic fallback on torn meta writes.
- **MVCC Snapshot Isolation & Freelist Reclamation**: concurrent lock-free readers pinned to immutable B+Tree roots alongside a single serialized writer; freed pages are persisted as COW freelist chains and recycled strictly when `freeTxID <= min(txid-1, minActiveReaderTxID)`.
- **Pluggable Storage (`engine.Storage`)**: real OS file storage (`OSFileStorage` with exclusive `flock` locking and parent-directory `fsync` on DB creation) plus per-write fault-injecting `MemStorage` (`CrashDropUnsynced`, `CrashReorderedMetaOnly`, `CrashTornMetaWrite`).
- **First-Class FizzBee Specs & Go MBT Harnesses** (`models/`, `fizztests/`):
  - `models/contract/kv_contract.fizz` — **Black-box external contract** (`Writer` + 2 concurrent `Reader`s + `SnapshotReader`), tested with **real parallel MBT under Go `-race`** on real `OSFileStorage`, with formal liveness (`SnapshotEventuallyCloses`). Zero coupling to storage internals.
  - `models/storage_protocol/storage_protocol.fizz` — **Unified white-box storage protocol** (`Disk` + `Engine`), modeling real abstract `pgid`s for BOTH B+Tree roots and COW freelist pages, volatile vs. durable page/meta buffers, realistic POSIX `FailSync` + double-fault `dirty_alt_meta` fence, freelist `pending` → `free` reclamation, `Put`/`Delete`/`RollbackWrite`, MVCC reader pinning, all 3 disk crash modes (`DROP`, `REORDERED`, `TORN`), **6 safety invariants + 3 temporal liveness properties**, backed by **5 formal mutation proofs** (`make check-mutations`).
  - `models/btree_ops/btree_ops.fizz` — **Ordered B+Tree & range-scan contract** exercising multi-level splits, deletes, root collapses, empty-tree transitions, and `[start, end)` range scans with freelist enabled.
- **Unbounded TLA+ Protocol Spec & Formal Trace Validation (`formal/tla/`)**:
  - `formal/tla/GroundKVProtocol.tla` — Unbounded TLA+ specification of GroundKV's 4-phase dual-meta commit, MVCC freelist watermark, `dirtyAltMeta` fence, and crash recovery.
  - `formal/tla/GroundKVTrace.tla` — TLC **Formal Trace Validation** (`make trace-validate`) replaying real NDJSON traces emitted by `engine.JSONLineTracer` during unbounded DST swarm runs (with crashes, I/O faults, and MVCC readers) back against `GroundKVProtocol!Next`.

## Project Layout

```text
groundkv/
├── models/                                  # Formal FizzBee specifications (named models/ to avoid GCC ./specs collision with -race)
│   ├── contract/kv_contract.fizz            # Black-box API contract (multi-role + parallel linearizability)
│   ├── storage_protocol/storage_protocol.fizz # Unified Disk + Engine COW/freelist/MVCC/crash protocol
│   └── btree_ops/btree_ops.fizz             # Ordered B+Tree & range-scan contract
├── formal/tla/            # Unbounded TLA+ protocol spec & TLC Formal Trace Validation
│   ├── GroundKVProtocol.tla # Formal storage protocol (4-phase commit, MVCC freelist, dirtyAltMeta fence, crash recovery)
│   └── GroundKVTrace.tla    # TLA+ Trace Validation checking real Go NDJSON execution traces against GroundKVProtocol
├── engine/                # Core Go storage engine
│   ├── options.go         # PageSize, MaxLeafKeys/MaxBranchKeys, NoFreelist, Tracer, errors
│   ├── storage.go         # Storage interface, OSFileStorage (+ flock), fault-injecting MemStorage
│   ├── trace.go           # Zero-overhead-when-nil structured NDJSON protocol tracer for TLA+ Trace Validation
│   ├── page.go            # Binary page codec (Meta/Branch/Leaf/Freelist) + CRC32 checksums
│   ├── meta.go            # Dual meta page (page 0/1) atomic commit & recovery
│   ├── freelist.go        # On-disk COW freelist chain + pending/free MVCC page reclamation
│   ├── btree.go           # COW B+Tree (Get, Put, Delete, Scan, Validate, freed-page tracking)
│   ├── tx.go              # Read-only MVCC Tx & 4-phase single-writer COW Tx
│   ├── db.go              # DB lifecycle, recovery, active reader tracking, and MBT inspection APIs
│   └── db_test.go         # Unit, concurrency (-race), mid-commit crash, freelist, and COW tests
├── fizztests/             # Generated FizzBee MBT harnesses + zero-shadow-state Go adapters
│   ├── contract/          # kv_contract_{interfaces,test,adapters}.go (parallel + -race on OSFileStorage)
│   ├── storage_protocol/  # storage_protocol_{interfaces,test,adapters}.go
│   └── btree_ops/         # btree_ops_{interfaces,test,adapters}.go
├── dst/                   # Deterministic Simulation Testing (DST), OS crash & linearizability harness
│   ├── simdisk.go         # Sector-level (64B) fault-injecting Storage (subset/reordered/torn crashes, bit-rot & R/W/Sync I/O errors)
│   ├── coverage.go        # 19 Antithesis-style "sometimes" scenario coverage assertions
│   ├── sim.go             # Seed-reproducible 7-profile Swarm simulator, multi-reader MVCC pool & shadow oracle
│   ├── dst_test.go        # TestDSTSwarm, TestDSTSteadyStateReclamation, TestDSTReplay (-dst-seed=<N>)
│   ├── oscrash_test.go    # Real subprocess SIGKILL crash-recovery test on OSFileStorage + flock
│   └── linearizability_test.go # Concurrent real-time linearizability & MVCC snapshot verifier (-race)
├── scripts/               # MBT runner, formal spec & Go code mutation checks, TLA+ trace validator & coverage merger
├── cmd/
│   ├── groundkv/          # Simple CLI (put, get, del, scan, meta, verify)
│   └── groundkv-dst/      # Standalone DST swarm runner (-seed, -seeds, -steps)
├── docs/architecture.md   # Deep dive on disk format, COW B+Tree, freelist, and MBT design
└── Makefile               # Spec-first verification & MBT targets
```

## Quick Start

### 1. Model-check all FizzBee specs

The `Makefile` defaults to `FIZZ=$(HOME)/bin/fizzbee-v0.5.4-linux_x86/fizz`:

```bash
make check-specs
```

### 2. Run Go engine tests (`-race`) and MBT adapter smoke tests

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

### 5. Try the CLI

```bash
make build
./bin/groundkv -db demo.db put hello world
./bin/groundkv -db demo.db get hello
./bin/groundkv -db demo.db meta
./bin/groundkv -db demo.db verify
```
