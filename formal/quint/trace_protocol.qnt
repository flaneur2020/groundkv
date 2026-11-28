// Unbounded Quint protocol specification for GroundKV Trace Validation.
//
// Unbounded Quint protocol specification for GroundKV Trace Validation:
//   1. Direct Quint simulation & invariant checking (`quint run`).
//   2. Formal Trace Validation (`scripts/run_trace_validation.sh`): replays
//      every real NDJSON event emitted by engine.JSONLineTracer during
//      verify/dst swarm runs through these exact transition guards and checks
//      all 5 formal safety invariants at every step.
module trace_protocol {
  pure val PHASES: Set[str] = Set("IDLE", "STAGED", "DATA_WRITTEN", "DATA_SYNCED", "META_WRITTEN")

  type TraceObs = {
    event: str,
    txid: int,
    phase: str,
    root: int,
    freelist: int,
    highWater: int,
    dirtyAltMeta: bool,
    activeReaders: int,
    minReaderTxID: int,
    pendingTxIDs: Set[int],
    releaseBound: int,
  }

  var txid: int
  var phase: str
  var root: int
  var freelist: int
  var highWater: int
  var dirtyAltMeta: bool
  var activeReaders: int
  var minReaderTxID: int
  var pendingTxIDs: Set[int]
  var lastSyncedTxID: int

  pure def min2(a: int, b: int): int = if (a <= b) a else b

  pure def calcReleaseBound(curTx: int, readers: int, minR: int): int = {
    val base = if (curTx > 0) curTx - 1 else 0
    if (readers > 0 and minR < base) minR else base
  }

  // --- Initial State ---

  action init = all {
    txid' = 0,
    phase' = "IDLE",
    root' = 0,
    freelist' = 0,
    highWater' = 2,
    dirtyAltMeta' = false,
    activeReaders' = 0,
    minReaderTxID' = -1,
    pendingTxIDs' = Set(),
    lastSyncedTxID' = 0,
  }

  action traceInit(obs: TraceObs): bool = all {
    obs.event == "Open",
    obs.txid >= 0,
    obs.phase == "IDLE",
    obs.highWater >= 2,
    obs.root == 0 or (obs.root >= 2 and obs.root < obs.highWater),
    obs.freelist == 0 or (obs.freelist >= 2 and obs.freelist < obs.highWater),
    not(obs.dirtyAltMeta),
    obs.activeReaders == 0,
    obs.minReaderTxID == -1,
    obs.releaseBound == calcReleaseBound(obs.txid, obs.activeReaders, obs.minReaderTxID),
    txid' = obs.txid,
    phase' = obs.phase,
    root' = obs.root,
    freelist' = obs.freelist,
    highWater' = obs.highWater,
    dirtyAltMeta' = obs.dirtyAltMeta,
    activeReaders' = obs.activeReaders,
    minReaderTxID' = obs.minReaderTxID,
    pendingTxIDs' = obs.pendingTxIDs,
    lastSyncedTxID' = obs.txid,
  }

  // --- Parameterized Protocol Transitions ---

  action beginReadStep = all {
    activeReaders' = activeReaders + 1,
    minReaderTxID' = if (activeReaders == 0) txid else min2(minReaderTxID, txid),
    txid' = txid,
    phase' = phase,
    root' = root,
    freelist' = freelist,
    highWater' = highWater,
    dirtyAltMeta' = dirtyAltMeta,
    pendingTxIDs' = pendingTxIDs,
    lastSyncedTxID' = lastSyncedTxID,
  }

  action closeReadStep(nextMinR: int): bool = all {
    activeReaders > 0,
    val nextReaders = activeReaders - 1
    val bound = calcReleaseBound(txid, nextReaders, nextMinR)
    all {
      if (nextReaders == 0) nextMinR == -1
      else nextMinR >= minReaderTxID and nextMinR <= txid,
      activeReaders' = nextReaders,
      minReaderTxID' = nextMinR,
      pendingTxIDs' = pendingTxIDs.filter(t => t > bound),
    },
    txid' = txid,
    phase' = phase,
    root' = root,
    freelist' = freelist,
    highWater' = highWater,
    dirtyAltMeta' = dirtyAltMeta,
    lastSyncedTxID' = lastSyncedTxID,
  }

  action beginWriteStep = all {
    phase == "IDLE",
    val bound = calcReleaseBound(txid, activeReaders, minReaderTxID)
    all {
      phase' = "STAGED",
      pendingTxIDs' = pendingTxIDs.filter(t => t > bound),
    },
    txid' = txid,
    root' = root,
    freelist' = freelist,
    highWater' = highWater,
    dirtyAltMeta' = dirtyAltMeta,
    activeReaders' = activeReaders,
    minReaderTxID' = minReaderTxID,
    lastSyncedTxID' = lastSyncedTxID,
  }

  action writeDataPagesStep = all {
    phase == "STAGED",
    phase' = "DATA_WRITTEN",
    txid' = txid,
    root' = root,
    freelist' = freelist,
    highWater' = highWater,
    dirtyAltMeta' = dirtyAltMeta,
    activeReaders' = activeReaders,
    minReaderTxID' = minReaderTxID,
    pendingTxIDs' = pendingTxIDs,
    lastSyncedTxID' = lastSyncedTxID,
  }

  // SyncDataPages neutralizes any lingering dirtyAltMeta BEFORE file-wide fsync!
  action syncDataPagesStep = all {
    phase == "DATA_WRITTEN",
    phase' = "DATA_SYNCED",
    dirtyAltMeta' = false,
    txid' = txid,
    root' = root,
    freelist' = freelist,
    highWater' = highWater,
    activeReaders' = activeReaders,
    minReaderTxID' = minReaderTxID,
    pendingTxIDs' = pendingTxIDs,
    lastSyncedTxID' = lastSyncedTxID,
  }

  action writeMetaPageStep = all {
    phase == "DATA_SYNCED",
    phase' = "META_WRITTEN",
    txid' = txid,
    root' = root,
    freelist' = freelist,
    highWater' = highWater,
    dirtyAltMeta' = dirtyAltMeta,
    activeReaders' = activeReaders,
    minReaderTxID' = minReaderTxID,
    pendingTxIDs' = pendingTxIDs,
    lastSyncedTxID' = lastSyncedTxID,
  }

  action syncMetaPageStep(newRoot: int, newFL: int, newHW: int, newPending: Set[int]): bool = all {
    phase == "META_WRITTEN",
    val nextTx = txid + 1
    val nextBound = calcReleaseBound(nextTx, activeReaders, minReaderTxID)
    all {
      newHW >= highWater,
      newRoot == 0 or (newRoot >= 2 and newRoot < newHW),
      newFL == 0 or (newFL >= 2 and newFL < newHW),
      newPending.subseteq(pendingTxIDs.union(Set(nextTx))),
      newPending.forall(t => t > nextBound),
      txid' = nextTx,
      lastSyncedTxID' = nextTx,
      phase' = "IDLE",
      dirtyAltMeta' = false,
      root' = newRoot,
      freelist' = newFL,
      highWater' = newHW,
      pendingTxIDs' = newPending,
    },
    activeReaders' = activeReaders,
    minReaderTxID' = minReaderTxID,
  }

  action rollbackWriteStep(newHW: int, newDirtyAlt: bool, newPending: Set[int]): bool = all {
    phase != "IDLE",
    val bound = calcReleaseBound(txid, activeReaders, minReaderTxID)
    all {
      if (phase == "META_WRITTEN") all {
        newHW >= highWater,
        newPending.subseteq(pendingTxIDs.union(Set(txid + 1))),
        newPending.forall(t => t > bound),
      } else all {
        newHW == highWater,
        newDirtyAlt == dirtyAltMeta or (phase == "DATA_WRITTEN" and not(newDirtyAlt)),
        newPending == pendingTxIDs,
      },
      phase' = "IDLE",
      highWater' = newHW,
      dirtyAltMeta' = newDirtyAlt,
      pendingTxIDs' = newPending,
    },
    txid' = txid,
    root' = root,
    freelist' = freelist,
    activeReaders' = activeReaders,
    minReaderTxID' = minReaderTxID,
    lastSyncedTxID' = lastSyncedTxID,
  }

  action crashRecoverStep(recTx: int, recRoot: int, recFL: int, recHW: int, recPending: Set[int]): bool = all {
    recTx >= lastSyncedTxID,
    if (phase == "META_WRITTEN" or dirtyAltMeta) (recTx == txid or recTx == txid + 1)
    else recTx == txid,
    recHW >= 2,
    recRoot == 0 or (recRoot >= 2 and recRoot < recHW),
    recFL == 0 or (recFL >= 2 and recFL < recHW),
    recPending.subseteq(Set(recTx)),
    recPending.forall(t => t > calcReleaseBound(recTx, 0, -1)),
    txid' = recTx,
    lastSyncedTxID' = recTx,
    phase' = "IDLE",
    root' = recRoot,
    freelist' = recFL,
    highWater' = recHW,
    dirtyAltMeta' = false,
    activeReaders' = 0,
    minReaderTxID' = -1,
    pendingTxIDs' = recPending,
  }

  // Dispatch a single observed NDJSON event through the formal protocol transition relation.
  action traceDispatch(obs: TraceObs): bool = any {
    all { obs.event == "BeginRead", beginReadStep },
    all { obs.event == "CloseRead", closeReadStep(obs.minReaderTxID) },
    all { obs.event == "BeginWrite", beginWriteStep },
    all { obs.event == "WriteDataPages", writeDataPagesStep },
    all { obs.event == "SyncDataPages", syncDataPagesStep },
    all { obs.event == "WriteMetaPage", writeMetaPageStep },
    all { obs.event == "SyncMetaPage", syncMetaPageStep(obs.root, obs.freelist, obs.highWater, obs.pendingTxIDs) },
    all { obs.event == "RollbackWrite", rollbackWriteStep(obs.highWater, obs.dirtyAltMeta, obs.pendingTxIDs) },
    all { obs.event == "Open", crashRecoverStep(obs.txid, obs.root, obs.freelist, obs.highWater, obs.pendingTxIDs) },
  }

  // Assert exact post-state agreement between the formal state and every observed Go field.
  def matchesObserved(obs: TraceObs): bool = all {
    txid == obs.txid,
    phase == obs.phase,
    root == obs.root,
    freelist == obs.freelist,
    highWater == obs.highWater,
    dirtyAltMeta == obs.dirtyAltMeta,
    activeReaders == obs.activeReaders,
    minReaderTxID == obs.minReaderTxID,
    pendingTxIDs == obs.pendingTxIDs,
    obs.releaseBound == calcReleaseBound(txid, activeReaders, minReaderTxID),
    inv_all,
  }

  // Convenience action for verifying the final state of a trace run.
  action traceCheck(obs: TraceObs): bool = all {
    assert(matchesObserved(obs)),
    txid' = txid,
    phase' = phase,
    root' = root,
    freelist' = freelist,
    highWater' = highWater,
    dirtyAltMeta' = dirtyAltMeta,
    activeReaders' = activeReaders,
    minReaderTxID' = minReaderTxID,
    pendingTxIDs' = pendingTxIDs,
    lastSyncedTxID' = lastSyncedTxID,
  }

  // --- Bounded Simulation Step (for `quint run` invariant checking) ---

  action step = any {
    all { activeReaders < 4, beginReadStep },
    nondet r = (-1).to(txid).oneOf()
    closeReadStep(r),
    all { txid < 8, beginWriteStep },
    writeDataPagesStep,
    syncDataPagesStep,
    writeMetaPageStep,
    nondet nhw = highWater.to(highWater + 3).oneOf()
    nondet nr = 0.to(nhw - 1).oneOf()
    nondet nf = 0.to(nhw - 1).oneOf()
    nondet np = pendingTxIDs.union(Set(txid + 1)).powerset().oneOf()
    syncMetaPageStep(nr, nf, nhw, np),
    nondet nhw = highWater.to(highWater + 2).oneOf()
    nondet nd = Set(true, false).oneOf()
    nondet np = pendingTxIDs.union(Set(txid + 1)).powerset().oneOf()
    rollbackWriteStep(nhw, nd, np),
    nondet rt = txid.to(txid + 1).oneOf()
    nondet rhw = 2.to(highWater + 2).oneOf()
    nondet rr = 0.to(rhw - 1).oneOf()
    nondet rf = 0.to(rhw - 1).oneOf()
    nondet rp = Set(rt).powerset().oneOf()
    crashRecoverStep(rt, rr, rf, rhw, rp),
  }

  // --- 5 Formal Safety Invariants ---

  val inv_valid_meta_bounds: bool = all {
    highWater >= 2,
    root == 0 or (root >= 2 and root < highWater),
    freelist == 0 or (freelist >= 2 and freelist < highWater),
  }

  val inv_release_bound_safe: bool =
    pendingTxIDs.forall(t => t > calcReleaseBound(txid, activeReaders, minReaderTxID))

  val inv_reader_watermark_valid: bool = all {
    activeReaders == 0 implies minReaderTxID == -1,
    activeReaders > 0 implies (minReaderTxID >= 0 and minReaderTxID <= txid),
  }

  val inv_committed_txid_monotonic: bool =
    txid >= lastSyncedTxID

  val inv_no_dirty_alt_meta_after_data_sync: bool =
    (phase == "DATA_SYNCED" or phase == "META_WRITTEN") implies not(dirtyAltMeta)

  val inv_all: bool = all {
    PHASES.contains(phase),
    inv_valid_meta_bounds,
    inv_release_bound_safe,
    inv_reader_watermark_valid,
    inv_committed_txid_monotonic,
    inv_no_dirty_alt_meta_after_data_sync,
  }
}
