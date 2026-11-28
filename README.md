# GroundKV

A zero-dependency, BoltDB-inspired embedded KV store in Go built on a **copy-on-write (COW) B+Tree** with **MVCC reader-aware freelist reclamation**, dual-meta crash recovery, and multi-layer formal verification (**Quint**, **Lean 4**, **Dafny**).

## Highlights

- **Functional Core, Imperative Shell**: `engine/` handles I/O, `flock`, locking, CRC32 framing, and 4-phase commits, calling strictly one-way into `engine/core.Core`.
- **Swappable Verified Kernel**: Choose between pure-Go `engine.GoCore()` (`engine/gocore/`) and Z3-verified Dafny-compiled `verified.NewCore()` (`engine/verified/`, 1,027 VCs, 0 `assume`) — same I/O shell, bit-compatible on-disk format.
- **Verification Stack**:
  - **Quint (`formal/quint/`, `verify/mbt/`)**: protocol & B+Tree specs, 5 spec mutations, DST trace validation, and ITF MBT against both kernels.
  - **Lean 4 (`formal/lean/`)**: 13 unbounded machine-checked theorems (0 `sorry`).
  - **Dafny (`formal/dafny/`)**: verified codecs, B+Tree, freelist, and engine protocol compiled to Go.
  - **DST & Fuzzing (`verify/dst/`, `engine/fuzz_test.go`)**: 7 Swarm profiles, 64B `SimDisk` crashes, `SIGKILL` recovery, `-race` linearizability, Go code mutations, and native fuzzing.

See [docs/architecture.md](docs/architecture.md) for the on-disk layout, `Core` boundary, and verification details.

## Layout

```text
engine/     # Imperative shell + core/ (interfaces), gocore/ (Go), verified/ (Dafny)
formal/     # quint/ (specs & traces), lean/ (unbounded proofs), dafny/ (Go codegen)
verify/     # dst/ (Swarm DST, SIGKILL, linearizability) & mbt/ (Quint ITF MBT)
cmd/        # groundkv CLI (-engine go|verified) & groundkv-dst runner
```

## Quick Start

```bash
make help        # List all targets
make verify      # Run Dafny, Lean 4, Quint (specs/mutations/trace/MBT), DST, race & code mutations
make fuzz        # Run deterministic Go fuzzers
make build       # Build bin/groundkv and bin/groundkv-dst

./bin/groundkv -db demo.db -engine go put hello world
./bin/groundkv -db demo.db -engine verified get hello
./bin/groundkv -db demo.db -engine verified verify
```
