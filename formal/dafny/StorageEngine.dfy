// GroundKV Phase 2 — Verified 4-Phase Dual-Meta Commit Protocol, MVCC Freelist
// Reclamation, and Crash-Recovery State Machine.
//
// Proves end-to-end crash-refinement against an atomic key-value map across:
//   - 4-phase commit (WriteDataPages -> SyncDataPages -> WriteMetaPage -> SyncMetaPage)
//   - Rollback with alternate-meta neutralization + dirtyAltMeta double-fault fence
//   - MVCC concurrent readers & reader-aware freelist page reclamation
//   - Power-loss crashes (drop volatile, arbitrary reordered subset flush, torn alt-meta)
//   - Dual-meta crash recovery (SelectRecoveredMeta compiled to Go & called by engine/db.go)

include "Types.dfy"
include "PageCodec.dfy"
include "Freelist.dfy"

module GroundKVEngine {
  import opened GroundKVTypes
  import opened GroundKVCodec
  import opened GroundKVFreelist

  // =========================================================================
  // Executable Dual-Meta Selection & Phase Transition Guards (Compiled to Go)
  // =========================================================================

  datatype CommitPhase =
    | PhaseIdle
    | PhaseActive
    | PhaseDataWritten
    | PhaseDataSynced
    | PhaseMetaWritten

  // Selects the authoritative meta page from slots 0 and 1 on crash recovery.
  // Matches engine/db.go recover(): picks the valid slot with higher TxID,
  // or falls back to the surviving valid slot if one slot was torn/corrupted.
  function SelectRecoveredMeta(slot0: Option<Meta>, slot1: Option<Meta>): Result<Meta, ErrCode>
    ensures slot0.Some? && slot1.Some? ==>
      SelectRecoveredMeta(slot0, slot1) ==
        Ok(if slot0.value.txid >= slot1.value.txid then slot0.value else slot1.value)
    ensures slot0.Some? && slot1.None? ==> SelectRecoveredMeta(slot0, slot1) == Ok(slot0.value)
    ensures slot0.None? && slot1.Some? ==> SelectRecoveredMeta(slot0, slot1) == Ok(slot1.value)
    ensures slot0.None? && slot1.None? ==> SelectRecoveredMeta(slot0, slot1) == Err(ErrCorruptedMeta)
  {
    match (slot0, slot1)
      case (Some(m0), Some(m1)) =>
        if m0.txid >= m1.txid then Ok(m0) else Ok(m1)
      case (Some(m0), None) => Ok(m0)
      case (None, Some(m1)) => Ok(m1)
      case (None, None) => Err(ErrCorruptedMeta)
  }

  function ValidNextPhase(current: CommitPhase, next: CommitPhase): bool
  {
    (current == PhaseIdle && next == PhaseActive) ||
    (current == PhaseActive && next == PhaseDataWritten) ||
    (current == PhaseDataWritten && next == PhaseDataSynced) ||
    (current == PhaseDataSynced && next == PhaseMetaWritten) ||
    (current == PhaseMetaWritten && next == PhaseIdle) ||
    (current != PhaseIdle && next == PhaseIdle) // Rollback
  }

  // =========================================================================
  // Abstract Disk & Crash Model for Formal Protocol Refinement
  // =========================================================================

  type KVMap = map<Bytes, Bytes>

  // A committed tree snapshot stored on disk at a root PageID, together with
  // its full set of reachable page IDs (B+Tree + freelist chain).
  datatype DiskSnapshot = DiskSnapshot(
    kv: KVMap,
    pages: set<PageID>
  )

  datatype MetaSlotState =
    | EmptySlot
    | CorruptSlot
    | ValidSlot(meta: Meta)

  datatype CrashMode =
    | CrashDropVolatile
    | CrashFlushDataSubset(flushedRoots: set<PageID>)
    | CrashFlushAltMeta(flushAlt: bool)
    | CrashTornAltMeta

  datatype EngineState = EngineState(
    pageSize: uint32,
    // Dual-meta slots: durable (on platter/NVMe) vs volatile (OS page cache)
    durableMeta0: MetaSlotState,
    durableMeta1: MetaSlotState,
    volatileMeta0: MetaSlotState,
    volatileMeta1: MetaSlotState,
    // Committed snapshots indexed by root PageID (root == 0 represents empty KV map)
    durableRoots: map<PageID, DiskSnapshot>,
    volatileRoots: map<PageID, DiskSnapshot>,
    // History of all committed snapshots by TxID (ghost/specification reference)
    history: map<uint64, Meta>,
    // In-memory active committed state
    meta: Meta,
    committedKV: KVMap,
    dirtyAltMeta: bool,
    // Active write transaction state
    phase: CommitPhase,
    stagedRoot: PageID,
    stagedKV: KVMap,
    stagedPages: set<PageID>,
    stagedHighWater: PageID,
    stagedFreed: set<PageID>,
    stagedRecycled: set<PageID>,
    nextMeta: Meta,
    // Active MVCC readers: snapTxID -> count > 0
    readers: map<uint64, nat>,
    // MVCC-aware freelist
    freelist: FreelistState
  )

  function GetMetaSlot(m0: MetaSlotState, m1: MetaSlotState, slot: PageID): MetaSlotState
  {
    if slot == 0 then m0 else m1
  }

  function RootKV(roots: map<PageID, DiskSnapshot>, root: PageID): KVMap
  {
    if root == 0 then map[]
    else if root in roots then roots[root].kv
    else map[]
  }

  function RootPages(roots: map<PageID, DiskSnapshot>, root: PageID): set<PageID>
  {
    if root == 0 then {}
    else if root in roots then roots[root].pages
    else {}
  }

  predicate SlotValidDurable(s: EngineState, m: Meta, slot: PageID)
  {
    ValidMeta(m, slot) &&
    m.pageSize == s.pageSize &&
    (m.root == 0 || (m.root in s.durableRoots && m.root in s.durableRoots[m.root].pages))
  }

  predicate SlotValidVolatile(s: EngineState, m: Meta, slot: PageID)
  {
    ValidMeta(m, slot) &&
    m.pageSize == s.pageSize &&
    (m.root == 0 || (m.root in s.volatileRoots && m.root in s.volatileRoots[m.root].pages))
  }

  // All pages currently reachable by either durable meta slot or any active MVCC reader.
  function LivePages(s: EngineState): set<PageID>
  {
    var p0 := if s.durableMeta0.ValidSlot? then RootPages(s.durableRoots, s.durableMeta0.meta.root) else {};
    var p1 := if s.durableMeta1.ValidSlot? then RootPages(s.durableRoots, s.durableMeta1.meta.root) else {};
    var pActive := RootPages(s.durableRoots, s.meta.root);
    var pReaders := set txid, id |
      txid in s.readers && s.readers[txid] > 0 && txid in s.history &&
      id in RootPages(s.durableRoots, s.history[txid].root) :: id;
    p0 + p1 + pActive + pReaders
  }

  predicate PendingDisjointFromSnapshots(
    pending: map<uint64, set<PageID>>,
    durableRoots: map<PageID, DiskSnapshot>,
    history: map<uint64, Meta>,
    d0: MetaSlotState,
    d1: MetaSlotState
  )
  {
    (forall freeTx, id, snapTx |
      freeTx in pending && id in pending[freeTx] && snapTx in history && snapTx >= freeTx ::
      id !in RootPages(durableRoots, history[snapTx].root)) &&
    (forall freeTx, id |
      freeTx in pending && id in pending[freeTx] && d0.ValidSlot? && d0.meta.txid >= freeTx ::
      id !in RootPages(durableRoots, d0.meta.root)) &&
    (forall freeTx, id |
      freeTx in pending && id in pending[freeTx] && d1.ValidSlot? && d1.meta.txid >= freeTx ::
      id !in RootPages(durableRoots, d1.meta.root))
  }

  // Core inductive safety & crash-refinement invariant over EngineState.
  predicate EngineInv(s: EngineState)
  {
    var activeSlot := MetaSlot(s.meta.txid);
    var altSlot := if activeSlot == 0 then 1 as PageID else 0 as PageID;

    (s.pageSize as nat) >= 256 &&
    ValidFreelist(s.freelist) &&

    // 1. Active meta is valid, durable in its parity slot, and never overwritten in volatile cache.
    SlotValidDurable(s, s.meta, activeSlot) &&
    GetMetaSlot(s.durableMeta0, s.durableMeta1, activeSlot) == ValidSlot(s.meta) &&
    GetMetaSlot(s.volatileMeta0, s.volatileMeta1, activeSlot) == ValidSlot(s.meta) &&
    RootKV(s.durableRoots, s.meta.root) == s.committedKV &&

    // 2. Durable roots are a subset of volatile roots (writes never delete durable roots before reclamation).
    (forall r | r in s.durableRoots :: r in s.volatileRoots && s.volatileRoots[r] == s.durableRoots[r]) &&

    // 3. Both durable slots (when valid) reference durable roots and have txid <= s.meta.txid.
    (s.durableMeta0.ValidSlot? ==>
      SlotValidDurable(s, s.durableMeta0.meta, 0) &&
      s.durableMeta0.meta.txid <= s.meta.txid) &&
    (s.durableMeta1.ValidSlot? ==>
      SlotValidDurable(s, s.durableMeta1.meta, 1) &&
      s.durableMeta1.meta.txid <= s.meta.txid) &&

    // 4. Alternate meta slot safety (prevents resurrecting aborted commits!):
    //    - Outside PhaseMetaWritten and !dirtyAltMeta:
    //      volatile alt slot is either CorruptSlot (neutralized) or identical to durable alt slot!
    (s.phase != PhaseMetaWritten && !s.dirtyAltMeta ==>
      var vAlt := GetMetaSlot(s.volatileMeta0, s.volatileMeta1, altSlot);
      var dAlt := GetMetaSlot(s.durableMeta0, s.durableMeta1, altSlot);
      vAlt == dAlt || vAlt == CorruptSlot) &&

    // 5. Phase-specific invariants:
    (s.phase != PhaseIdle ==>
      s.stagedRecycled * LivePages(s) == {} &&
      s.stagedHighWater >= 2) &&

    (s.phase == PhaseDataWritten ==>
      s.stagedPages * s.freelist.free == {} &&
      (forall freeTx, id | freeTx in s.freelist.pending && id in s.freelist.pending[freeTx] :: id !in s.stagedPages) &&
      (s.stagedRoot == 0 ==> s.stagedKV == map[] && s.stagedPages == {}) &&
      (s.stagedRoot != 0 ==>
        2 <= s.stagedRoot < s.stagedHighWater &&
        s.stagedRoot in s.volatileRoots &&
        s.volatileRoots[s.stagedRoot] == DiskSnapshot(s.stagedKV, s.stagedPages) &&
        s.stagedRoot in s.stagedPages)) &&

    (s.phase == PhaseDataSynced ==>
      !s.dirtyAltMeta &&
      s.stagedPages * s.freelist.free == {} &&
      (forall freeTx, id | freeTx in s.freelist.pending && id in s.freelist.pending[freeTx] :: id !in s.stagedPages) &&
      (s.stagedRoot == 0 ==> s.stagedKV == map[] && s.stagedPages == {}) &&
      (s.stagedRoot != 0 ==>
        2 <= s.stagedRoot < s.stagedHighWater &&
        s.stagedRoot in s.durableRoots &&
        s.durableRoots[s.stagedRoot] == DiskSnapshot(s.stagedKV, s.stagedPages) &&
        s.stagedRoot in s.stagedPages)) &&

    (s.phase == PhaseMetaWritten ==>
      !s.dirtyAltMeta &&
      s.stagedPages * s.freelist.free == {} &&
      (forall freeTx, id | freeTx in s.freelist.pending && id in s.freelist.pending[freeTx] :: id !in s.stagedPages) &&
      (s.meta.txid as int) + 1 < 0x1_0000_0000_0000_0000 &&
      s.nextMeta.txid == s.meta.txid + 1 &&
      MetaSlot(s.nextMeta.txid) == altSlot &&
      SlotValidDurable(s, s.nextMeta, altSlot) &&
      RootKV(s.durableRoots, s.nextMeta.root) == s.stagedKV &&
      RootPages(s.durableRoots, s.nextMeta.root) == s.stagedPages &&
      GetMetaSlot(s.volatileMeta0, s.volatileMeta1, altSlot) == ValidSlot(s.nextMeta)) &&

    // 6. History & active readers consistency:
    s.meta.txid in s.history &&
    s.history[s.meta.txid] == s.meta &&
    (forall t | t in s.history ::
      t <= s.meta.txid &&
      s.history[t].txid == t &&
      (s.history[t].root == 0 || s.history[t].root in s.durableRoots)) &&
    (forall t | t in s.readers && s.readers[t] > 0 :: t in s.history) &&

    // 7. MVCC + dual-meta freelist disjointness:
    (s.freelist.free * LivePages(s) == {}) &&
    PendingDisjointFromSnapshots(s.freelist.pending, s.durableRoots, s.history, s.durableMeta0, s.durableMeta1)
  }

  // =========================================================================
  // Protocol Transitions
  // =========================================================================

  function InitEmpty(pageSize: uint32): EngineState
    requires (pageSize as nat) >= 256
    ensures EngineInv(InitEmpty(pageSize))
    ensures InitEmpty(pageSize).committedKV == map[]
    ensures InitEmpty(pageSize).meta.txid == 0
  {
    var m := Meta(MetaMagic, MetaVersion, pageSize, 0, 2, 0, 0);
    EngineState(
      pageSize,
      ValidSlot(m), ValidSlot(m),
      ValidSlot(m), ValidSlot(m),
      map[], map[],
      map[0 := m],
      m, map[], false,
      PhaseIdle,
      0, map[], {}, 2, {}, {}, m,
      map[],
      EmptyFreelist()
    )
  }

  ghost function MinReaderInfo(readers: map<uint64, nat>): (bool, uint64)
  {
    var active := set t | t in readers && readers[t] > 0;
    if active == {} then (false, 0)
    else
      (true, MinSet(active))
  }

  ghost function MinSet(s: set<uint64>): uint64
    requires s != {}
    ensures MinSet(s) in s
    ensures forall x | x in s :: MinSet(s) <= x
    decreases |s|
  {
    var x :| x in s;
    var rest := s - {x};
    if rest == {} then
      assert forall y | y in s :: y == x by {
        forall y | y in s ensures y == x {
          if y != x {
            assert y in rest;
          }
        }
      }
      x
    else
      var m := MinSet(rest);
      assert forall y | y in s :: (if x < m then x else m) <= y by {
        forall y | y in s ensures (if x < m then x else m) <= y {
          if y != x {
            assert y in rest;
          }
        }
      }
      if x < m then x else m
  }

  ghost function CurrentReleaseBound(s: EngineState): uint64
  {
    var (hasR, minR) := MinReaderInfo(s.readers);
    ReleaseBound(s.meta.txid, hasR, minR)
  }

  // Lemma: releasing pending pages with freeTxID <= CurrentReleaseBound(s)
  // NEVER puts any live page (from either durable meta slot or any active reader)
  // into the reusable free pool!
  lemma ReleasePreservesLiveDisjoint(s: EngineState)
    requires EngineInv(s)
    requires s.durableMeta0.ValidSlot? ==> s.durableMeta0.meta.txid >= (if s.meta.txid > 0 then s.meta.txid - 1 else 0)
    requires s.durableMeta1.ValidSlot? ==> s.durableMeta1.meta.txid >= (if s.meta.txid > 0 then s.meta.txid - 1 else 0)
    ensures ReleaseEligible(s.freelist, CurrentReleaseBound(s)).free * LivePages(s) == {}
  {
    var bound := CurrentReleaseBound(s);
    var newFl := ReleaseEligible(s.freelist, bound);
    var live := LivePages(s);
    var (hasR, minR) := MinReaderInfo(s.readers);

    forall id | id in newFl.free
      ensures id !in live
    {
      if id in s.freelist.free {
        assert id !in live;
      } else {
        // id came from pending[freeTx] with freeTx <= bound
        var freeTx :| freeTx in s.freelist.pending && freeTx <= bound && id in s.freelist.pending[freeTx];
        // Active meta has txid >= bound >= freeTx
        assert s.meta.txid >= bound;
        assert id !in RootPages(s.durableRoots, s.meta.root);
        // Durable meta 0 & 1 have txid >= max(0, s.meta.txid - 1) >= bound >= freeTx
        if s.durableMeta0.ValidSlot? {
          assert s.durableMeta0.meta.txid >= freeTx;
          assert id !in RootPages(s.durableRoots, s.durableMeta0.meta.root);
        }
        if s.durableMeta1.ValidSlot? {
          assert s.durableMeta1.meta.txid >= freeTx;
          assert id !in RootPages(s.durableRoots, s.durableMeta1.meta.root);
        }
        // Every active reader has snapTx >= minR >= bound >= freeTx
        forall snapTx | snapTx in s.readers && s.readers[snapTx] > 0 && snapTx in s.history
          ensures id !in RootPages(s.durableRoots, s.history[snapTx].root)
        {
          var active := set t | t in s.readers && s.readers[t] > 0;
          assert snapTx in active;
          assert hasR && minR <= snapTx;
          assert freeTx <= bound <= minR <= snapTx;
        }
      }
    }
  }

  function BeginRead(s: EngineState): EngineState
    requires EngineInv(s)
    ensures EngineInv(BeginRead(s))
  {
    var c := if s.meta.txid in s.readers then s.readers[s.meta.txid] else 0;
    s.(readers := s.readers[s.meta.txid := c + 1])
  }

  function CloseRead(s: EngineState, snapTxID: uint64): EngineState
    requires EngineInv(s)
    requires snapTxID in s.readers && s.readers[snapTxID] > 0
    ensures EngineInv(CloseRead(s, snapTxID))
  {
    var newReaders :=
      if s.readers[snapTxID] <= 1 then map t | t in s.readers && t != snapTxID :: s.readers[t]
      else s.readers[snapTxID := s.readers[snapTxID] - 1];
    s.(readers := newReaders)
  }

  function BeginWrite(s: EngineState): EngineState
    requires EngineInv(s)
    requires s.phase == PhaseIdle
    ensures EngineInv(BeginWrite(s))
  {
    s.(
      phase := PhaseActive,
      stagedRoot := s.meta.root,
      stagedKV := s.committedKV,
      stagedPages := RootPages(s.durableRoots, s.meta.root),
      stagedHighWater := s.meta.highWater,
      stagedFreed := {},
      stagedRecycled := {}
    )
  }

  // Phase 1: Write COW data pages into the volatile page cache (never touching meta slots).
  function WriteDataPages(
    s: EngineState,
    newRoot: PageID,
    newKV: KVMap,
    newPages: set<PageID>,
    newHighWater: PageID,
    freedPages: set<PageID>,
    recycledPages: set<PageID>
  ): EngineState
    requires EngineInv(s)
    requires s.phase == PhaseActive
    requires newHighWater >= s.meta.highWater
    requires newRoot == 0 ==> newKV == map[] && newPages == {}
    requires newRoot != 0 ==>
      2 <= newRoot < newHighWater &&
      newRoot in newPages &&
      (newRoot in s.durableRoots ==> s.durableRoots[newRoot] == DiskSnapshot(newKV, newPages))
    requires newPages * (s.freelist.free - recycledPages) == {}
    requires newPages * s.stagedRecycled == {}
    requires forall freeTx, id | freeTx in s.freelist.pending && id in s.freelist.pending[freeTx] :: id !in newPages
    requires freedPages * newPages == {}
    requires recycledPages <= s.freelist.free
    ensures EngineInv(WriteDataPages(s, newRoot, newKV, newPages, newHighWater, freedPages, recycledPages))
  {
    var vRoots :=
      if newRoot == 0 then s.volatileRoots
      else s.volatileRoots[newRoot := DiskSnapshot(newKV, newPages)];
    s.(
      phase := PhaseDataWritten,
      volatileRoots := vRoots,
      stagedRoot := newRoot,
      stagedKV := newKV,
      stagedPages := newPages,
      stagedHighWater := newHighWater,
      stagedFreed := freedPages,
      stagedRecycled := recycledPages,
      freelist := s.freelist.(free := s.freelist.free - recycledPages)
    )
  }

  // Phase 2: Fence dirtyAltMeta BEFORE file-wide fsync, then fsync all volatile data pages.
  function SyncDataPages(s: EngineState): EngineState
    requires EngineInv(s)
    requires s.phase == PhaseDataWritten
    ensures EngineInv(SyncDataPages(s))
    ensures !SyncDataPages(s).dirtyAltMeta
  {
    var activeSlot := MetaSlot(s.meta.txid);
    // If dirtyAltMeta was set by an earlier double-fault rollback, neutralize the
    // alternate meta slot BEFORE flushing volatile buffers to durable storage!
    var vM0 := if s.dirtyAltMeta && activeSlot == 1 then CorruptSlot else s.volatileMeta0;
    var vM1 := if s.dirtyAltMeta && activeSlot == 0 then CorruptSlot else s.volatileMeta1;
    // File-wide fsync promotes volatile data roots and (now-safe) volatile meta slots.
    var s' := s.(
      durableMeta0 := vM0,
      durableMeta1 := vM1,
      volatileMeta0 := vM0,
      volatileMeta1 := vM1,
      durableRoots := s.volatileRoots,
      dirtyAltMeta := false,
      phase := PhaseDataSynced
    );
    assert LivePages(s') <= LivePages(s) by {
      assert vM0 == s.durableMeta0 || vM0 == CorruptSlot;
      assert vM1 == s.durableMeta1 || vM1 == CorruptSlot;
      forall r | r in s.durableRoots
        ensures RootPages(s'.durableRoots, r) == RootPages(s.durableRoots, r)
      {}
    }
    assert PendingDisjointFromSnapshots(s'.freelist.pending, s'.durableRoots, s'.history, s'.durableMeta0, s'.durableMeta1) by {
      assert PendingDisjointFromSnapshots(s.freelist.pending, s.durableRoots, s.history, s.durableMeta0, s.durableMeta1);
      assert s'.freelist.pending == s.freelist.pending;
      assert s'.history == s.history;
      assert vM0 == s.durableMeta0 || vM0 == CorruptSlot;
      assert vM1 == s.durableMeta1 || vM1 == CorruptSlot;
      forall freeTx, id | freeTx in s'.freelist.pending && id in s'.freelist.pending[freeTx]
        ensures (forall snapTx | snapTx in s'.history && snapTx >= freeTx ::
          id !in RootPages(s'.durableRoots, s'.history[snapTx].root))
        ensures (s'.durableMeta0.ValidSlot? && s'.durableMeta0.meta.txid >= freeTx ==>
          id !in RootPages(s'.durableRoots, s'.durableMeta0.meta.root))
        ensures (s'.durableMeta1.ValidSlot? && s'.durableMeta1.meta.txid >= freeTx ==>
          id !in RootPages(s'.durableRoots, s'.durableMeta1.meta.root))
      {
        assert freeTx in s.freelist.pending && id in s.freelist.pending[freeTx];
        forall snapTx | snapTx in s'.history && snapTx >= freeTx
          ensures id !in RootPages(s'.durableRoots, s'.history[snapTx].root)
        {
          assert snapTx in s.history && snapTx >= freeTx;
          assert id !in RootPages(s.durableRoots, s.history[snapTx].root);
          var r := s.history[snapTx].root;
          if r != 0 {
            assert r in s.durableRoots;
            assert s'.durableRoots[r] == s.durableRoots[r];
          }
        }
        if s'.durableMeta0.ValidSlot? && s'.durableMeta0.meta.txid >= freeTx {
          assert s'.durableMeta0 == s.durableMeta0;
          assert id !in RootPages(s.durableRoots, s.durableMeta0.meta.root);
          var r := s.durableMeta0.meta.root;
          if r != 0 {
            assert r in s.durableRoots;
            assert s'.durableRoots[r] == s.durableRoots[r];
          }
        }
        if s'.durableMeta1.ValidSlot? && s'.durableMeta1.meta.txid >= freeTx {
          assert s'.durableMeta1 == s.durableMeta1;
          assert id !in RootPages(s.durableRoots, s.durableMeta1.meta.root);
          var r := s.durableMeta1.meta.root;
          if r != 0 {
            assert r in s.durableRoots;
            assert s'.durableRoots[r] == s.durableRoots[r];
          }
        }
      }
    }
    s'
  }

  // Phase 3: Write the alternate meta slot (nextTxID % 2) into the volatile page cache.
  // Data pages are ALREADY durable (PhaseDataSynced) and active meta slot is untouched!
  function WriteMetaPage(s: EngineState, newFreelistHead: PageID): EngineState
    requires EngineInv(s)
    requires s.phase == PhaseDataSynced
    requires (s.meta.txid as int) + 1 < 0x1_0000_0000_0000_0000
    requires newFreelistHead == 0 || (2 <= newFreelistHead < s.stagedHighWater)
    ensures EngineInv(WriteMetaPage(s, newFreelistHead))
  {
    var nextTxID := s.meta.txid + 1;
    var nextSlot := MetaSlot(nextTxID);
    var nm := Meta(
      MetaMagic,
      MetaVersion,
      s.pageSize,
      s.stagedRoot,
      s.stagedHighWater,
      nextTxID,
      newFreelistHead
    );
    var vM0 := if nextSlot == 0 then ValidSlot(nm) else s.volatileMeta0;
    var vM1 := if nextSlot == 1 then ValidSlot(nm) else s.volatileMeta1;
    s.(
      volatileMeta0 := vM0,
      volatileMeta1 := vM1,
      nextMeta := nm,
      phase := PhaseMetaWritten
    )
  }

  // Phase 4: Fsync the alternate meta page, record freed pages into pending[nextTxID],
  // and publish the newly committed Meta & KV state.
  function SyncMetaPage(s: EngineState): EngineState
    requires EngineInv(s)
    requires s.phase == PhaseMetaWritten
    requires s.stagedFreed * RootPages(s.durableRoots, s.nextMeta.root) == {}
    ensures EngineInv(SyncMetaPage(s))
    ensures SyncMetaPage(s).committedKV == s.stagedKV
    ensures SyncMetaPage(s).meta == s.nextMeta
  {
    var nextSlot := MetaSlot(s.nextMeta.txid);
    var dM0 := if nextSlot == 0 then ValidSlot(s.nextMeta) else s.durableMeta0;
    var dM1 := if nextSlot == 1 then ValidSlot(s.nextMeta) else s.durableMeta1;
    var fl1 := AddPending(s.freelist, s.nextMeta.txid, s.stagedFreed);
    var s' := s.(
      durableMeta0 := dM0,
      durableMeta1 := dM1,
      history := s.history[s.nextMeta.txid := s.nextMeta],
      meta := s.nextMeta,
      committedKV := s.stagedKV,
      phase := PhaseIdle,
      freelist := fl1
    );
    assert PendingDisjointFromSnapshots(s'.freelist.pending, s'.durableRoots, s'.history, s'.durableMeta0, s'.durableMeta1) by {
      forall freeTx, id | freeTx in s'.freelist.pending && id in s'.freelist.pending[freeTx]
        ensures (forall snapTx | snapTx in s'.history && snapTx >= freeTx ::
          id !in RootPages(s'.durableRoots, s'.history[snapTx].root))
        ensures (s'.durableMeta0.ValidSlot? && s'.durableMeta0.meta.txid >= freeTx ==>
          id !in RootPages(s'.durableRoots, s'.durableMeta0.meta.root))
        ensures (s'.durableMeta1.ValidSlot? && s'.durableMeta1.meta.txid >= freeTx ==>
          id !in RootPages(s'.durableRoots, s'.durableMeta1.meta.root))
      {
        if freeTx == s.nextMeta.txid {
          assert id in s.stagedFreed || (freeTx in s.freelist.pending && id in s.freelist.pending[freeTx]);
          assert id !in s.stagedPages;
          assert RootPages(s'.durableRoots, s.nextMeta.root) == s.stagedPages;
        } else {
          assert freeTx in s.freelist.pending && id in s.freelist.pending[freeTx];
          assert id !in s.stagedPages;
        }
      }
    }
    s'
  }

  // Rollback a write transaction from any phase.
  // If called from PhaseMetaWritten (e.g. SyncMetaPage failed), neutralizes the
  // alternate meta slot (or sets dirtyAltMeta = true if the invalidation write
  // itself failed!) and holds recycled pages in pending[nextMeta.txid].
  function RollbackWrite(s: EngineState, invalidateOk: bool): EngineState
    requires EngineInv(s)
    requires s.phase != PhaseIdle
    ensures EngineInv(RollbackWrite(s, invalidateOk))
    ensures RollbackWrite(s, invalidateOk).committedKV == s.committedKV
    ensures RollbackWrite(s, invalidateOk).meta == s.meta
    ensures RollbackWrite(s, invalidateOk).phase == PhaseIdle
  {
    if s.phase == PhaseMetaWritten then
      var altSlot := MetaSlot(s.nextMeta.txid);
      var vM0 := if invalidateOk && altSlot == 0 then CorruptSlot else s.volatileMeta0;
      var vM1 := if invalidateOk && altSlot == 1 then CorruptSlot else s.volatileMeta1;
      var fl1 := AddPending(s.freelist, s.nextMeta.txid, s.stagedRecycled);
      var s' := s.(
        volatileMeta0 := vM0,
        volatileMeta1 := vM1,
        dirtyAltMeta := !invalidateOk,
        phase := PhaseIdle,
        freelist := fl1
      );
      assert PendingDisjointFromSnapshots(s'.freelist.pending, s'.durableRoots, s'.history, s'.durableMeta0, s'.durableMeta1) by {
        assert PendingDisjointFromSnapshots(s.freelist.pending, s.durableRoots, s.history, s.durableMeta0, s.durableMeta1);
        assert s.nextMeta.txid > s.meta.txid;
        assert s'.durableRoots == s.durableRoots;
        assert s'.history == s.history;
        assert s'.durableMeta0 == s.durableMeta0;
        assert s'.durableMeta1 == s.durableMeta1;
        forall freeTx, id | freeTx in s'.freelist.pending && id in s'.freelist.pending[freeTx]
          ensures (forall snapTx | snapTx in s'.history && snapTx >= freeTx ::
            id !in RootPages(s'.durableRoots, s'.history[snapTx].root))
          ensures (s'.durableMeta0.ValidSlot? && s'.durableMeta0.meta.txid >= freeTx ==>
            id !in RootPages(s'.durableRoots, s'.durableMeta0.meta.root))
          ensures (s'.durableMeta1.ValidSlot? && s'.durableMeta1.meta.txid >= freeTx ==>
            id !in RootPages(s'.durableRoots, s'.durableMeta1.meta.root))
        {
          if freeTx == s.nextMeta.txid {
            forall snapTx | snapTx in s'.history && snapTx >= freeTx
              ensures id !in RootPages(s'.durableRoots, s'.history[snapTx].root)
            {
              assert snapTx <= s.meta.txid;
              assert false;
            }
            if s'.durableMeta0.ValidSlot? && s'.durableMeta0.meta.txid >= freeTx {
              assert s.durableMeta0.meta.txid <= s.meta.txid;
              assert false;
            }
            if s'.durableMeta1.ValidSlot? && s'.durableMeta1.meta.txid >= freeTx {
              assert s.durableMeta1.meta.txid <= s.meta.txid;
              assert false;
            }
          } else {
            assert freeTx in s.freelist.pending && s'.freelist.pending[freeTx] == s.freelist.pending[freeTx];
            assert id in s.freelist.pending[freeTx];
          }
        }
      }
      s'
    else
      // Before PhaseMetaWritten, neither meta slot was touched. Recycled pages
      // are disjoint from LivePages(s) because they came from s.freelist.free.
      var fl1 := ReturnToFree(s.freelist, s.stagedRecycled);
      s.(
        phase := PhaseIdle,
        freelist := fl1
      )
  }

  // =========================================================================
  // Crash & Dual-Meta Recovery Model + End-to-End Crash-Refinement Theorem
  // =========================================================================

  function SlotToOption(ms: MetaSlotState): Option<Meta>
  {
    match ms
      case ValidSlot(m) => Some(m)
      case _ => None
  }

  // Models an abrupt power loss from state s under CrashMode, producing the
  // surviving durable meta slots & durable data roots on disk.
  function ApplyCrash(s: EngineState, mode: CrashMode): (MetaSlotState, MetaSlotState, map<PageID, DiskSnapshot>)
    requires EngineInv(s)
  {
    var activeSlot := MetaSlot(s.meta.txid);
    match mode
      case CrashDropVolatile =>
        (s.durableMeta0, s.durableMeta1, s.durableRoots)
      case CrashFlushDataSubset(flushed) =>
        var dRoots := map r | r in s.volatileRoots && (r in s.durableRoots || r in flushed) ::
          s.volatileRoots[r];
        (s.durableMeta0, s.durableMeta1, dRoots)
      case CrashFlushAltMeta(flushAlt) =>
        // Only possible when volatile alt meta was written (e.g. PhaseMetaWritten
        // or dirtyAltMeta); active slot is NEVER overwritten!
        if flushAlt && s.phase == PhaseMetaWritten then
          var dM0 := if activeSlot == 1 then s.volatileMeta0 else s.durableMeta0;
          var dM1 := if activeSlot == 0 then s.volatileMeta1 else s.durableMeta1;
          (dM0, dM1, s.durableRoots)
        else
          (s.durableMeta0, s.durableMeta1, s.durableRoots)
      case CrashTornAltMeta =>
        // Power loss mid-write to the alternate meta slot tears its CRC32 checksum.
        var dM0 := if activeSlot == 1 then CorruptSlot else s.durableMeta0;
        var dM1 := if activeSlot == 0 then CorruptSlot else s.durableMeta1;
        (dM0, dM1, s.durableRoots)
  }

  // Theorem (End-to-End Crash Atomicity & Durability Refinement):
  // From ANY reachable state satisfying EngineInv(s) and ANY crash mode:
  //   1. Dual-meta recovery ALWAYS succeeds (at least one durable meta slot is valid).
  //   2. The recovered meta's root page is ALWAYS durable and intact.
  //   3. Outside PhaseMetaWritten, recovery ALWAYS restores the exact last committed KV map
  //      (zero lost commits, zero resurrected uncommitted/aborted writes).
  //   4. During PhaseMetaWritten, recovery atomically yields EITHER the exact pre-commit
  //      KV map OR the exact post-commit KV map — NEVER a torn or partial state!
  lemma CrashRecoveryRefinesAtomicKV(s: EngineState, mode: CrashMode)
    requires EngineInv(s)
    ensures var (d0, d1, dRoots) := ApplyCrash(s, mode);
            var rec := SelectRecoveredMeta(SlotToOption(d0), SlotToOption(d1));
            rec.Ok? &&
            (rec.value.root == 0 || rec.value.root in dRoots) &&
            (s.phase != PhaseMetaWritten ==>
              rec.value == s.meta &&
              RootKV(dRoots, rec.value.root) == s.committedKV) &&
            (s.phase == PhaseMetaWritten ==>
              (rec.value == s.meta && RootKV(dRoots, rec.value.root) == s.committedKV) ||
              (rec.value == s.nextMeta && RootKV(dRoots, rec.value.root) == s.stagedKV))
  {
    var activeSlot := MetaSlot(s.meta.txid);
    var (d0, d1, dRoots) := ApplyCrash(s, mode);
    var opt0 := SlotToOption(d0);
    var opt1 := SlotToOption(d1);

    // Active meta slot is NEVER corrupted or overwritten in any crash mode!
    if activeSlot == 0 {
      assert d0 == ValidSlot(s.meta);
      assert opt0 == Some(s.meta);
    } else {
      assert d1 == ValidSlot(s.meta);
      assert opt1 == Some(s.meta);
    }

    var rec := SelectRecoveredMeta(opt0, opt1);
    assert rec.Ok?;

    match mode
      case CrashDropVolatile =>
        if s.durableMeta0.ValidSlot? && s.durableMeta1.ValidSlot? {
          if activeSlot == 0 {
            assert s.durableMeta1.meta.txid <= s.meta.txid;
          } else {
            assert s.durableMeta0.meta.txid <= s.meta.txid;
            if s.durableMeta0.meta.txid == s.meta.txid {
              // Both slots can only have the same txid when txid == 0 (InitEmpty)
              assert s.meta.txid == 0;
              assert MetaSlot(0) == 0;
              assert false;
            }
          }
        }
      case CrashFlushDataSubset(flushed) =>
        if s.durableMeta0.ValidSlot? && s.durableMeta1.ValidSlot? {
          if activeSlot == 0 {
            assert s.durableMeta1.meta.txid <= s.meta.txid;
          } else {
            assert s.durableMeta0.meta.txid <= s.meta.txid;
            if s.durableMeta0.meta.txid == s.meta.txid {
              assert s.meta.txid == 0;
              assert false;
            }
          }
        }
      case CrashFlushAltMeta(flushAlt) =>
        if flushAlt && s.phase == PhaseMetaWritten {
          if activeSlot == 0 {
            assert d0 == ValidSlot(s.meta);
            assert d1 == ValidSlot(s.nextMeta);
            assert s.nextMeta.txid > s.meta.txid;
            assert rec.value == s.nextMeta;
          } else {
            assert d1 == ValidSlot(s.meta);
            assert d0 == ValidSlot(s.nextMeta);
            assert s.nextMeta.txid > s.meta.txid;
            assert rec.value == s.nextMeta;
          }
        } else {
          if s.durableMeta0.ValidSlot? && s.durableMeta1.ValidSlot? {
            if activeSlot == 0 {
              assert s.durableMeta1.meta.txid <= s.meta.txid;
            } else {
              assert s.durableMeta0.meta.txid <= s.meta.txid;
              if s.durableMeta0.meta.txid == s.meta.txid {
                assert s.meta.txid == 0;
                assert false;
              }
            }
          }
        }
      case CrashTornAltMeta =>
        if activeSlot == 0 {
          assert d0 == ValidSlot(s.meta) && d1 == CorruptSlot;
          assert rec.value == s.meta;
        } else {
          assert d1 == ValidSlot(s.meta) && d0 == CorruptSlot;
          assert rec.value == s.meta;
        }
  }

  // Theorem (No Resurrection After Double-Fault Rollback + SyncDataPages):
  // Even if SyncMetaPage failed AND RollbackWrite's invalidation write failed
  // (leaving dirtyAltMeta = true), the next transaction's SyncDataPages
  // neutralizes the alternate meta slot BEFORE fsyncing, so any subsequent
  // crash recovers s.meta (NOT the aborted transaction).
  lemma DoubleFaultRollbackFenceSafety(
    s: EngineState,
    newRoot: PageID,
    newKV: KVMap,
    newPages: set<PageID>,
    newHighWater: PageID,
    freedPages: set<PageID>,
    recycledPages: set<PageID>,
    mode: CrashMode
  )
    requires EngineInv(s)
    requires s.phase == PhaseMetaWritten
    requires newHighWater >= s.meta.highWater
    requires newRoot == 0 ==> newKV == map[] && newPages == {}
    requires newRoot != 0 ==>
      2 <= newRoot < newHighWater &&
      newRoot in newPages &&
      (newRoot in s.durableRoots ==> s.durableRoots[newRoot] == DiskSnapshot(newKV, newPages))
    requires newPages * (s.freelist.free - recycledPages) == {}
    requires newPages * s.stagedRecycled == {}
    requires forall freeTx, id | freeTx in s.freelist.pending && id in s.freelist.pending[freeTx] :: id !in newPages
    requires freedPages * newPages == {}
    requires recycledPages <= s.freelist.free
    ensures var s1 := RollbackWrite(s, false); // double-fault: invalidate write ALSO failed!
            var s2 := BeginWrite(s1);
            var s3 := WriteDataPages(s2, newRoot, newKV, newPages, newHighWater, freedPages, recycledPages);
            var s4 := SyncDataPages(s3);
            var (d0, d1, dRoots) := ApplyCrash(s4, mode);
            var rec := SelectRecoveredMeta(SlotToOption(d0), SlotToOption(d1));
            s1.dirtyAltMeta &&
            !s4.dirtyAltMeta &&
            rec.Ok? &&
            rec.value == s.meta &&
            RootKV(dRoots, rec.value.root) == s.committedKV
  {
    var s1 := RollbackWrite(s, false);
    assert s1.dirtyAltMeta;
    assert s1.freelist.free == s.freelist.free;
    assert LivePages(s1) == LivePages(s);
    var s2 := BeginWrite(s1);
    assert LivePages(s2) == LivePages(s);
    var s3 := WriteDataPages(s2, newRoot, newKV, newPages, newHighWater, freedPages, recycledPages);
    var s4 := SyncDataPages(s3);
    CrashRecoveryRefinesAtomicKV(s4, mode);
  }
}
