# GroundKV

A BoltDB-inspired embedded key-value engine in Go built on an **append-only, copy-on-write (COW) B+Tree**, designed with **FizzBee Model-Based Testing (MBT)** as a first-class citizen.

## Features

- **Append-only COW B+Tree** (`engine/`): fixed-size CRC32-checksummed pages, leaf/branch splitting, root promotion & collapse, range scans, and structural tree validation (`db.Validate()`).
- **Dual Meta Pages & Crash Recovery**: alternating meta pages (Page 0 and Page 1) with `Sync`-ordered commit protocol and automatic fallback on torn meta writes.
- **MVCC Snapshot Isolation**: concurrent lock-free readers pinned to immutable B+Tree roots alongside a single serialized writer.
- **Pluggable Storage (`engine.Storage`)**: real OS file storage (`OSFileStorage` with parent-directory `fsync` on DB creation) plus per-write fault-injecting `MemStorage` (`CrashDropUnsynced`, `CrashReorderedMetaOnly`, `CrashTornMetaWrite`).
- **First-Class FizzBee Specs & Go MBT Harnesses** (`models/`, `fizztests/`):
  - `models/kv/kv.fizz` — KV semantics, dual-meta commit protocol, MVCC snapshots, rollback, and crash recovery.
  - `models/btree/btree.fizz` — Append-only COW B+Tree page immutability, sorted scan order, and historical pinned roots.
  - `models/durability/durability.fizz` — 4-phase commit durability (`WriteDataPages` → `SyncDataPages` → `WriteMetaPage` → `SyncMetaPage`) under unsynced-drop, reordered-meta-flush, and torn-meta-write power losses.

## Project Layout

```text
groundkv/
├── models/                    # Formal FizzBee specifications (named models/ to avoid GCC ./specs collision with -race)
│   ├── kv/kv.fizz             # KV + Dual-Meta + MVCC + Crash Recovery spec
│   ├── btree/btree.fizz       # Append-Only COW B+Tree immutability spec
│   └── durability/durability.fizz # 4-phase mid-commit power-loss & write-reordering durability spec
├── engine/                # Core Go storage engine
│   ├── options.go         # PageSize, MaxLeafKeys/MaxBranchKeys, errors
│   ├── storage.go         # Storage interface, OSFileStorage, per-write fault-injecting MemStorage
│   ├── page.go            # Binary page codec + CRC32 checksums
│   ├── meta.go            # Dual meta page (page 0/1) atomic commit & recovery
│   ├── btree.go           # Append-only COW B+Tree (Get, Put, Delete, Scan, Validate)
│   ├── tx.go              # Read-only MVCC Tx & 4-phase single-writer COW Tx
│   ├── db.go              # DB lifecycle, recovery, and MBT state inspection APIs
│   └── db_test.go         # Unit, concurrency (-race), mid-commit crash, and COW property tests
├── fizztests/             # Generated FizzBee MBT harnesses + Go adapters
│   ├── kv/                # kv_interfaces.go, kv_test.go (generated), kv_adapters.go
│   ├── btree/             # btree_interfaces.go, btree_test.go (generated), btree_adapters.go
│   └── durability/        # durability_interfaces.go, durability_test.go (generated), durability_adapters.go
├── cmd/groundkv/          # Simple CLI (put, get, del, scan, meta, verify)
├── docs/architecture.md   # Deep dive on disk format, COW B+Tree, and MBT design
└── Makefile               # Spec-first verification & MBT targets
```

## Quick Start

### 1. Model-check the FizzBee specs

The `Makefile` defaults to `FIZZ=$(HOME)/bin/fizzbee-v0.5.4-linux_x86/fizz`:

```bash
make check-specs
```

### 2. Run Go engine tests (`-race`) and MBT adapter smoke tests

```bash
make verify
```

### 3. Run full FizzBee Model-Based Testing (`go test ./fizztests/...`)

FizzBee MBT executes state-graph traces against the Go adapters via the companion `fizzbee-mbt-runner` binary:

1. Download `fizzbee-mbt-*-linux_x86.tar.gz` from [fizzbee-io/fizzbee-mbt-releases](https://github.com/fizzbee-io/fizzbee-mbt-releases/releases) and place `fizzbee-mbt-runner` on your `PATH` (or pass `FIZZBEE_MBT_BIN=/path/to/fizzbee-mbt-runner`).
2. Run:

```bash
make mbt
```

### 4. Regenerate MBT interfaces after editing a `.fizz` spec

```bash
make check-specs
make scaffold-interfaces
```

(`scaffold-interfaces` intentionally omits `--gen-adapter` so your hand-written `*_adapters.go` files are preserved while `*_interfaces.go` and `*_test.go` stay synced with the spec.)

### 5. Try the CLI

```bash
make build
./bin/groundkv -db demo.db put hello world
./bin/groundkv -db demo.db get hello
./bin/groundkv -db demo.db meta
./bin/groundkv -db demo.db verify
```
