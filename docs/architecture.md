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

Rather than treating formal methods as documentation written after the fact, GroundKV keeps four executable `.fizz` specifications tightly bound to Go MBT harnesses:

| Spec | Focus | Verified Safety Invariants | Adapter Checks |
|------|-------|----------------------------|----------------|
| `models/kv/kv.fizz` | End-to-end KV, MVCC snapshots, explicit write tx commit/rollback, crash & reopen | `DualMetaConsistency`, `NoWriteTxLeak`, `CleanSnapshotState` | Compares real on-disk `DualMeta()`, committed B+Tree, open snapshot `Tx`, staged write `Tx`, and `MemStorage.SimulateCrash()` |
| `models/btree/btree.fizz` | Append-only COW page immutability, sorted scans, historical pinned snapshots | `AppendOnlyImmutability`, `RootMatchesOracle`, `PinnedSnapshotIntact` | Snapshots raw bytes of every committed page (`pgid >= 2`) and asserts byte-for-byte immutability across all transitions + `db.Validate()` |
| `models/durability/durability.fizz` | 4-phase mid-commit durability (`WriteDataPages` → `SyncDataPages` → `WriteMetaPage` → `SyncMetaPage`) under unsynced-drop, reordered-meta-flush, and torn-meta-write power losses | `AtLeastOneValidMeta`, `ValidMetaHasDurableData`, `CommittedTxIDNeverRegresses`, `ActiveViewMatchesHighestValidMeta` | Drives `Tx`'s production 4-phase commit methods against per-write fault-injecting `MemStorage`, clones the durable disk image (`CloneDurable`) to verify both meta slots & B+Tree roots after every step |
| `models/freelist/freelist.fizz` | MVCC reader-aware freelist & page reclamation (`releaseBound = min(txid-1, minActiveReaderTxID)`), on-disk COW freelist chain, crash recovery | `NoLivePageInFreelist`, `PendingStrictlyAboveBound`, `ReleaseBoundRespectsReadersAndDualMeta`, `ReadersConsistent` | Walks every reachable page from `meta0`, `meta1`, and every open MVCC reader (`ReachablePages`) to verify zero overlap with `freelist.free`, exact `minActiveReaderTxID` / `releaseBound`, and intact snapshots |

### Evolution Roadmap

Natural next specs & engine milestones to add using the same Spec -> Scaffold -> Implement -> Verify loop:
1. **Buckets / Nested B+Trees** (`models/bucket/bucket.fizz`): BoltDB-style named buckets with inline sub-trees.
2. **Compaction / Online Snapshot Backup**: Streaming consistent snapshot copy via MVCC read transactions.
