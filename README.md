# GroundKV

A zero-dependency, BoltDB-inspired embedded append only B+Tree KV store in Go, designed with **Model-Based Testing (MBT) as a first-class citizen**.

- **MBT-First Design**: Fault-injectable `Storage`, configurable fanout caps, and white-box inspection APIs let executable **Quint** specs (`formal/quint/`) directly drive the real engine via ITF MBT (`verify/mbt/`) and validate real DST traces with zero shadow state.
- **Swappable Core**: `engine/` (I/O, `flock`, CRC32, 4-phase commits) calls one-way into `engine/core.Core` — choose pure-Go `engine.GoCore()` or Dafny-verified `verified.NewCore()`.
- **Deep Verification**: Quint MBT/trace validation/mutations, Swarm DST (`SimDisk`, `SIGKILL`, `-race` linearizability), native fuzzing, plus Dafny & Lean proofs. See [docs/architecture.md](docs/architecture.md).

```text
engine/   # Shell + core/ (interfaces), gocore/ (Go), verified/ (Dafny)
formal/   # quint/ (MBT & trace specs), dafny/ (Go codegen), lean/ (proofs)
verify/   # mbt/ (Quint ITF MBT) & dst/ (Swarm DST, SIGKILL, linearizability)
cmd/      # groundkv CLI (-engine go|verified) & groundkv-dst
```

```bash
make mbt && make verify   # Run Quint MBT and full verification suite (`make help` for all targets)
make build && ./bin/groundkv -db demo.db -engine verified put hello world
```
