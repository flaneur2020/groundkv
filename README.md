# GroundKV

A BoltDB-inspired embedded key-value engine in Go built on a **copy-on-write (COW) B+Tree** with **MVCC reader-aware freelist reclamation**, designed with **FizzBee Model-Based Testing (MBT)** as a first-class citizen.

## Features

- **COW B+Tree** (`engine/`): fixed-size CRC32-checksummed pages, leaf/branch splitting, root promotion & collapse, half-open `[start, end)` range scans, and structural tree validation (`db.Validate()`).
- **Dual Meta Pages & Crash Recovery**: alternating meta pages (Page 0 and Page 1) with 4-phase `Sync`-ordered commits and automatic fallback on torn meta writes.
- **MVCC Snapshot Isolation & Freelist Reclamation**: concurrent lock-free readers pinned to immutable B+Tree roots alongside a single serialized writer; freed pages are persisted as COW freelist chains and recycled strictly when `freeTxID <= min(txid-1, minActiveReaderTxID)`.
- **Pluggable Storage (`engine.Storage`)**: real OS file storage (`OSFileStorage` with parent-directory `fsync` on DB creation) plus per-write fault-injecting `MemStorage` (`CrashDropUnsynced`, `CrashReorderedMetaOnly`, `CrashTornMetaWrite`).
- **First-Class FizzBee Specs & Go MBT Harnesses** (`models/`, `fizztests/`):
  - `models/contract/kv_contract.fizz` — **Black-box external contract** (`Writer` + 2 concurrent `Reader`s + `SnapshotReader`), tested with **real parallel MBT (`max-parallel-runs: 200`) under Go `-race`**. Zero coupling to storage internals.
  - `models/storage_protocol/storage_protocol.fizz` — **Unified white-box storage protocol** (`Disk` + `Engine`), modeling real abstract `pgid`s, volatile vs. durable page/meta buffers, freelist `pending` → `free` reclamation, `Put`/`Delete`/`RollbackWrite`, MVCC reader pinning, and all 3 disk crash modes (`DROP`, `REORDERED`, `TORN`). Every safety assertion is non-tautological and mutation-tested.
  - `models/btree_ops/btree_ops.fizz` — **Ordered B+Tree & range-scan contract** exercising multi-level splits, deletes, root collapses, empty-tree transitions, and `[start, end)` range scans with freelist enabled.

## Project Layout

```text
groundkv/
├── models/                                  # Formal FizzBee specifications (named models/ to avoid GCC ./specs collision with -race)
│   ├── contract/kv_contract.fizz            # Black-box API contract (multi-role + parallel linearizability)
│   ├── storage_protocol/storage_protocol.fizz # Unified Disk + Engine COW/freelist/MVCC/crash protocol
│   └── btree_ops/btree_ops.fizz             # Ordered B+Tree & range-scan contract
├── engine/                # Core Go storage engine
│   ├── options.go         # PageSize, MaxLeafKeys/MaxBranchKeys, NoFreelist, errors
│   ├── storage.go         # Storage interface, OSFileStorage, per-write fault-injecting MemStorage
│   ├── page.go            # Binary page codec (Meta/Branch/Leaf/Freelist) + CRC32 checksums
│   ├── meta.go            # Dual meta page (page 0/1) atomic commit & recovery
│   ├── freelist.go        # On-disk COW freelist chain + pending/free MVCC page reclamation
│   ├── btree.go           # COW B+Tree (Get, Put, Delete, Scan, Validate, freed-page tracking)
│   ├── tx.go              # Read-only MVCC Tx & 4-phase single-writer COW Tx
│   ├── db.go              # DB lifecycle, recovery, active reader tracking, and MBT inspection APIs
│   └── db_test.go         # Unit, concurrency (-race), mid-commit crash, freelist, and COW tests
├── fizztests/             # Generated FizzBee MBT harnesses + zero-shadow-state Go adapters
│   ├── contract/          # kv_contract_{interfaces,test,adapters}.go (parallel + -race)
│   ├── storage_protocol/  # storage_protocol_{interfaces,test,adapters}.go
│   └── btree_ops/         # btree_ops_{interfaces,test,adapters}.go
├── scripts/run_mbt.sh     # Manages fizzbee-mbt-server + fizzbee-mbt-runner lifecycle
├── cmd/groundkv/          # Simple CLI (put, get, del, scan, meta, verify)
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
