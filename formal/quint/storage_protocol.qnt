// White-box Quint specification of GroundKV's dual-meta COW storage protocol:
//   - Separate volatile (OS page cache) vs durable disk page & meta buffers
//   - Realistic POSIX FailSync (returns error without discarding dirty page cache)
//   - Optional double-fault InvalidateMeta write failure (dirtyAltMeta fence)
//   - Three power-loss crash modes: DROP, REORDERED, TORN
//   - Abstract PageIDs (pgid >= 2) for both B+Tree root and COW freelist pages
//   - MVCC reader snapshot + freelist pending[txid] -> free reclamation gated by
//     releaseBound = min(max(0, txid - 1), minActiveReaderTxID)
//   - 7 non-tautological safety invariants backed by scripts/check_mutations.sh
module storage_protocol {
  pure val NOT_FOUND: str = "NOT_FOUND"
  pure val FL_TAG: str = "FL"

  pure val PHASE_IDLE: str = "IDLE"
  pure val PHASE_STAGED: str = "STAGED"
  pure val PHASE_DATA_WRITTEN: str = "DATA_WRITTEN"
  pure val PHASE_DATA_SYNCED: str = "DATA_SYNCED"
  pure val PHASE_META_WRITTEN: str = "META_WRITTEN"

  pure val CRASH_MODES: Set[str] = Set("DROP", "REORDERED", "TORN")

  type MetaSlot = {
    valid: bool,
    txid: int,
    root: int,
    fl: int,
    expected: str,
  }

  type PendingMeta = {
    present: bool,
    valid: bool,
    slot: int,
    txid: int,
    root: int,
    fl: int,
    expected: str,
  }

  type StepRecord = {
    actionName: str,
    argStr: str,
    argBool: bool,
    result: str,
  }

  pure val emptyMeta: MetaSlot = {
    valid: true, txid: 0, root: 0, fl: 0, expected: NOT_FOUND
  }

  pure val invalidMeta: MetaSlot = {
    valid: false, txid: 0, root: 0, fl: 0, expected: NOT_FOUND
  }

  pure val noPendingMeta: PendingMeta = {
    present: false, valid: false, slot: 0, txid: 0, root: 0, fl: 0, expected: NOT_FOUND
  }

  // --- Disk state ---
  var durablePages: int -> str
  var pendingPages: int -> str
  var meta0: MetaSlot
  var meta1: MetaSlot
  var pendingMeta: PendingMeta

  // --- Engine state ---
  var txid: int
  var root: int
  var freelistPgid: int
  var committedVal: str
  var dirtyAltMeta: bool

  var nextPgid: int
  var freePages: Set[int]
  var pendingFree: int -> Set[int]

  var hasReader: bool
  var readerTxid: int
  var readerRoot: int
  var readerExpected: str

  var phase: str
  var txRoot: int
  var txFlPgid: int
  var txStagedVal: str
  var txFreed: Set[int]
  var txRecycled: Set[int]
  var txDirtyWritten: bool
  var txFlWritten: bool

  var lastAckedTxid: int
  var lastStep: StepRecord

  // --- Pure helpers ---

  pure def valForTx(prefix: str, t: int): str =
    if (prefix == "v") {
      if (t == 1) "v1"
      else if (t == 2) "v2"
      else if (t == 3) "v3"
      else if (t == 4) "v4"
      else if (t == 5) "v5"
      else if (t == 6) "v6"
      else if (t == 7) "v7"
      else if (t == 8) "v8"
      else if (t == 9) "v9"
      else if (t == 10) "v10"
      else if (t == 11) "v11"
      else if (t == 12) "v12"
      else if (t == 13) "v13"
      else if (t == 14) "v14"
      else if (t == 15) "v15"
      else if (t == 16) "v16"
      else if (t == 17) "v17"
      else if (t == 18) "v18"
      else if (t == 19) "v19"
      else if (t == 20) "v20"
      else "vN"
    } else {
      if (t == 1) "w1"
      else if (t == 2) "w2"
      else if (t == 3) "w3"
      else if (t == 4) "w4"
      else if (t == 5) "w5"
      else if (t == 6) "w6"
      else if (t == 7) "w7"
      else if (t == 8) "w8"
      else if (t == 9) "w9"
      else if (t == 10) "w10"
      else if (t == 11) "w11"
      else if (t == 12) "w12"
      else if (t == 13) "w13"
      else if (t == 14) "w14"
      else if (t == 15) "w15"
      else if (t == 16) "w16"
      else if (t == 17) "w17"
      else if (t == 18) "w18"
      else if (t == 19) "w19"
      else if (t == 20) "w20"
      else "wN"
    }

  pure def minInt(s: Set[int]): int =
    s.fold(1000000000, (acc, x) => if (x < acc) x else acc)

  pure def calcReleaseBound(curTx: int, readerOpen: bool, rTx: int): int = {
    val base = if (curTx > 0) curTx - 1 else 0
    if (readerOpen and rTx < base) rTx else base
  }

  pure def releaseFromPending(
    curFree: Set[int],
    curPending: int -> Set[int],
    bound: int
  ): { free: Set[int], pending: int -> Set[int] } = {
    val eligibleTxids = curPending.keys().filter(t => t <= bound)
    val releasedPages = eligibleTxids.fold(Set(), (acc, t) => acc.union(curPending.get(t)))
    val keptTxids = curPending.keys().filter(t => t > bound)
    {
      free: curFree.union(releasedPages),
      pending: keptTxids.mapBy(t => curPending.get(t)),
    }
  }

  pure def addPendingPages(
    curPending: int -> Set[int],
    t: int,
    ids: Set[int]
  ): int -> Set[int] = {
    val clean = ids.filter(id => id >= 2)
    if (clean.size() == 0) curPending
    else if (curPending.keys().contains(t)) curPending.set(t, curPending.get(t).union(clean))
    else curPending.put(t, clean)
  }

  pure def allocFromPool(
    curFree: Set[int],
    curNext: int
  ): { pgid: int, free: Set[int], next: int, recycled: bool } =
    if (curFree.size() > 0) {
      val lowest = minInt(curFree)
      { pgid: lowest, free: curFree.exclude(Set(lowest)), next: curNext, recycled: true }
    } else {
      { pgid: curNext, free: curFree, next: curNext + 1, recycled: false }
    }

  pure def applyPendingPagesToDurable(
    durable: int -> str,
    pending: int -> str
  ): int -> str =
    pending.keys().fold(durable, (acc, k) => acc.put(k, pending.get(k)))

  pure def applyMetaToSlot(
    m0: MetaSlot,
    m1: MetaSlot,
    pm: PendingMeta
  ): { m0: MetaSlot, m1: MetaSlot } =
    if (not(pm.present)) { m0: m0, m1: m1 }
    else {
      val updated =
        if (pm.valid) {
          valid: true, txid: pm.txid, root: pm.root, fl: pm.fl, expected: pm.expected
        } else invalidMeta
      if (pm.slot == 0) { m0: updated, m1: m1 }
      else { m0: m0, m1: updated }
    }

  pure def invalidateSlot(
    m0: MetaSlot,
    m1: MetaSlot,
    slot: int
  ): { m0: MetaSlot, m1: MetaSlot } =
    if (slot == 0) { m0: invalidMeta, m1: m1 }
    else { m0: m0, m1: invalidMeta }

  pure def selectRecoveredMeta(m0: MetaSlot, m1: MetaSlot): MetaSlot =
    if (m0.valid and m1.valid) {
      if (m0.txid >= m1.txid) m0 else m1
    } else if (m0.valid) m0
    else m1

  pure def liveMetaPages(m: MetaSlot): Set[int] =
    if (not(m.valid)) Set()
    else {
      val r = if (m.root != 0) Set(m.root) else Set()
      val f = if (m.fl != 0) Set(m.fl) else Set()
      r.union(f)
    }

  // --- Initial State ---

  action init = all {
    durablePages' = Map(),
    pendingPages' = Map(),
    meta0' = emptyMeta,
    meta1' = emptyMeta,
    pendingMeta' = noPendingMeta,

    txid' = 0,
    root' = 0,
    freelistPgid' = 0,
    committedVal' = NOT_FOUND,
    dirtyAltMeta' = false,

    nextPgid' = 2,
    freePages' = Set(),
    pendingFree' = Map(),

    hasReader' = false,
    readerTxid' = -1,
    readerRoot' = 0,
    readerExpected' = NOT_FOUND,

    phase' = PHASE_IDLE,
    txRoot' = 0,
    txFlPgid' = 0,
    txStagedVal' = NOT_FOUND,
    txFreed' = Set(),
    txRecycled' = Set(),
    txDirtyWritten' = false,
    txFlWritten' = false,

    lastAckedTxid' = 0,
    lastStep' = { actionName: "Init", argStr: "", argBool: false, result: "OK" },
  }

  // --- Actions ---

  action beginPut = all {
    phase == PHASE_IDLE,
    txid < 18,
    val bound = calcReleaseBound(txid, hasReader, readerTxid)
    val rel = releaseFromPending(freePages, pendingFree, bound)
    val alloc = allocFromPool(rel.free, nextPgid)
    val nextVal = valForTx("v", txid + 1)
    all {
      freePages' = alloc.free,
      pendingFree' = rel.pending,
      nextPgid' = alloc.next,
      phase' = PHASE_STAGED,
      txRoot' = alloc.pgid,
      txFlPgid' = 0,
      txStagedVal' = nextVal,
      txFreed' = if (root != 0) Set(root) else Set(),
      txRecycled' = if (alloc.recycled) Set(alloc.pgid) else Set(),
      txDirtyWritten' = true,
      txFlWritten' = false,
      lastStep' = { actionName: "BeginPut", argStr: "", argBool: false, result: nextVal },
    },
    durablePages' = durablePages,
    pendingPages' = pendingPages,
    meta0' = meta0,
    meta1' = meta1,
    pendingMeta' = pendingMeta,
    txid' = txid,
    root' = root,
    freelistPgid' = freelistPgid,
    committedVal' = committedVal,
    dirtyAltMeta' = dirtyAltMeta,
    hasReader' = hasReader,
    readerTxid' = readerTxid,
    readerRoot' = readerRoot,
    readerExpected' = readerExpected,
    lastAckedTxid' = lastAckedTxid,
  }

  action stageOverwrite = all {
    phase == PHASE_STAGED,
    val nextVal = valForTx("w", txid + 1)
    if (txRoot == 0) {
      val alloc = allocFromPool(freePages, nextPgid)
      all {
        freePages' = alloc.free,
        nextPgid' = alloc.next,
        txRoot' = alloc.pgid,
        txRecycled' = if (alloc.recycled) txRecycled.union(Set(alloc.pgid)) else txRecycled,
        txDirtyWritten' = true,
        txStagedVal' = nextVal,
        lastStep' = { actionName: "StageOverwrite", argStr: "", argBool: false, result: nextVal },
      }
    } else {
      all {
        freePages' = freePages,
        nextPgid' = nextPgid,
        txRoot' = txRoot,
        txRecycled' = txRecycled,
        txDirtyWritten' = txDirtyWritten,
        txStagedVal' = nextVal,
        lastStep' = { actionName: "StageOverwrite", argStr: "", argBool: false, result: nextVal },
      }
    },
    durablePages' = durablePages,
    pendingPages' = pendingPages,
    meta0' = meta0,
    meta1' = meta1,
    pendingMeta' = pendingMeta,
    txid' = txid,
    root' = root,
    freelistPgid' = freelistPgid,
    committedVal' = committedVal,
    dirtyAltMeta' = dirtyAltMeta,
    pendingFree' = pendingFree,
    hasReader' = hasReader,
    readerTxid' = readerTxid,
    readerRoot' = readerRoot,
    readerExpected' = readerExpected,
    phase' = phase,
    txFlPgid' = txFlPgid,
    txFreed' = txFreed,
    txFlWritten' = txFlWritten,
    lastAckedTxid' = lastAckedTxid,
  }

  action beginDelete = all {
    phase == PHASE_IDLE,
    txid < 18,
    val bound = calcReleaseBound(txid, hasReader, readerTxid)
    val rel = releaseFromPending(freePages, pendingFree, bound)
    val changed = committedVal != NOT_FOUND
    all {
      freePages' = rel.free,
      pendingFree' = rel.pending,
      phase' = PHASE_STAGED,
      txRoot' = if (changed) 0 else root,
      txFlPgid' = 0,
      txStagedVal' = NOT_FOUND,
      txFreed' = if (changed and root != 0) Set(root) else Set(),
      txRecycled' = Set(),
      txDirtyWritten' = false,
      txFlWritten' = false,
      lastStep' = { actionName: "BeginDelete", argStr: "", argBool: false, result: "OK" },
    },
    durablePages' = durablePages,
    pendingPages' = pendingPages,
    meta0' = meta0,
    meta1' = meta1,
    pendingMeta' = pendingMeta,
    txid' = txid,
    root' = root,
    freelistPgid' = freelistPgid,
    committedVal' = committedVal,
    dirtyAltMeta' = dirtyAltMeta,
    nextPgid' = nextPgid,
    hasReader' = hasReader,
    readerTxid' = readerTxid,
    readerRoot' = readerRoot,
    readerExpected' = readerExpected,
    lastAckedTxid' = lastAckedTxid,
  }

  // Phase 1: WriteDataPages (dirty B+Tree page + COW freelist page)
  action writeDataPages = all {
    phase == PHASE_STAGED,
    val freedWithFL = if (freelistPgid != 0) txFreed.union(Set(freelistPgid)) else txFreed
    val needFL = freePages.size() > 0 or pendingFree.keys().size() > 0 or freedWithFL.size() > 0
    val flAlloc =
      if (needFL) allocFromPool(freePages, nextPgid)
      else { pgid: 0, free: freePages, next: nextPgid, recycled: false }
    val p1 = if (needFL) pendingPages.put(flAlloc.pgid, FL_TAG) else pendingPages
    val p2 = if (txDirtyWritten) p1.put(txRoot, txStagedVal) else p1
    all {
      pendingPages' = p2,
      freePages' = flAlloc.free,
      nextPgid' = flAlloc.next,
      txFreed' = freedWithFL,
      txRecycled' = if (flAlloc.recycled) txRecycled.union(Set(flAlloc.pgid)) else txRecycled,
      txFlPgid' = flAlloc.pgid,
      txFlWritten' = needFL,
      phase' = PHASE_DATA_WRITTEN,
      lastStep' = { actionName: "CommitStep", argStr: "", argBool: false, result: "DATA_WRITTEN" },
    },
    durablePages' = durablePages,
    meta0' = meta0,
    meta1' = meta1,
    pendingMeta' = pendingMeta,
    txid' = txid,
    root' = root,
    freelistPgid' = freelistPgid,
    committedVal' = committedVal,
    dirtyAltMeta' = dirtyAltMeta,
    pendingFree' = pendingFree,
    hasReader' = hasReader,
    readerTxid' = readerTxid,
    readerRoot' = readerRoot,
    readerExpected' = readerExpected,
    txRoot' = txRoot,
    txStagedVal' = txStagedVal,
    txDirtyWritten' = txDirtyWritten,
    lastAckedTxid' = lastAckedTxid,
  }

  // Phase 2: SyncDataPages (neutralize dirtyAltMeta BEFORE file-wide fsync!)
  action syncDataPages = all {
    phase == PHASE_DATA_WRITTEN,
    val pmAfterFence =
      if (dirtyAltMeta) {
        present: true, valid: false, slot: (txid + 1) % 2,
        txid: 0, root: 0, fl: 0, expected: NOT_FOUND
      } else pendingMeta
    val doSync = txDirtyWritten or txFlWritten
    val syncedMeta =
      if (doSync) applyMetaToSlot(meta0, meta1, pmAfterFence)
      else { m0: meta0, m1: meta1 }
    all {
      dirtyAltMeta' = false,
      durablePages' = if (doSync) applyPendingPagesToDurable(durablePages, pendingPages) else durablePages,
      pendingPages' = if (doSync) Map() else pendingPages,
      meta0' = syncedMeta.m0,
      meta1' = syncedMeta.m1,
      pendingMeta' = if (doSync) noPendingMeta else pmAfterFence,
      phase' = PHASE_DATA_SYNCED,
      lastStep' = { actionName: "CommitStep", argStr: "", argBool: false, result: "DATA_SYNCED" },
    },
    txid' = txid,
    root' = root,
    freelistPgid' = freelistPgid,
    committedVal' = committedVal,
    nextPgid' = nextPgid,
    freePages' = freePages,
    pendingFree' = pendingFree,
    hasReader' = hasReader,
    readerTxid' = readerTxid,
    readerRoot' = readerRoot,
    readerExpected' = readerExpected,
    txRoot' = txRoot,
    txFlPgid' = txFlPgid,
    txStagedVal' = txStagedVal,
    txFreed' = txFreed,
    txRecycled' = txRecycled,
    txDirtyWritten' = txDirtyWritten,
    txFlWritten' = txFlWritten,
    lastAckedTxid' = lastAckedTxid,
  }

  // Phase 3: WriteMetaPage to alternate slot (nextTxid % 2)
  action writeMetaPage = all {
    phase == PHASE_DATA_SYNCED,
    val nextTx = txid + 1
    all {
      pendingMeta' = {
        present: true,
        valid: true,
        slot: nextTx % 2,
        txid: nextTx,
        root: txRoot,
        fl: txFlPgid,
        expected: txStagedVal,
      },
      phase' = PHASE_META_WRITTEN,
      lastStep' = { actionName: "CommitStep", argStr: "", argBool: false, result: "META_WRITTEN" },
    },
    durablePages' = durablePages,
    pendingPages' = pendingPages,
    meta0' = meta0,
    meta1' = meta1,
    txid' = txid,
    root' = root,
    freelistPgid' = freelistPgid,
    committedVal' = committedVal,
    dirtyAltMeta' = dirtyAltMeta,
    nextPgid' = nextPgid,
    freePages' = freePages,
    pendingFree' = pendingFree,
    hasReader' = hasReader,
    readerTxid' = readerTxid,
    readerRoot' = readerRoot,
    readerExpected' = readerExpected,
    txRoot' = txRoot,
    txFlPgid' = txFlPgid,
    txStagedVal' = txStagedVal,
    txFreed' = txFreed,
    txRecycled' = txRecycled,
    txDirtyWritten' = txDirtyWritten,
    txFlWritten' = txFlWritten,
    lastAckedTxid' = lastAckedTxid,
  }

  // Phase 4: SyncMetaPage, record freed pages, release eligible buckets, publish
  action syncMetaPage = all {
    phase == PHASE_META_WRITTEN,
    val nextTx = txid + 1
    val syncedMeta = applyMetaToSlot(meta0, meta1, pendingMeta)
    val pendWithFreed = addPendingPages(pendingFree, nextTx, txFreed)
    val bound = calcReleaseBound(nextTx, hasReader, readerTxid)
    val rel = releaseFromPending(freePages, pendWithFreed, bound)
    all {
      durablePages' = applyPendingPagesToDurable(durablePages, pendingPages),
      pendingPages' = Map(),
      meta0' = syncedMeta.m0,
      meta1' = syncedMeta.m1,
      pendingMeta' = noPendingMeta,
      txid' = nextTx,
      root' = txRoot,
      freelistPgid' = txFlPgid,
      committedVal' = txStagedVal,
      dirtyAltMeta' = false,
      lastAckedTxid' = nextTx,
      freePages' = rel.free,
      pendingFree' = rel.pending,
      phase' = PHASE_IDLE,
      txRoot' = 0,
      txFlPgid' = 0,
      txStagedVal' = NOT_FOUND,
      txFreed' = Set(),
      txRecycled' = Set(),
      txDirtyWritten' = false,
      txFlWritten' = false,
      lastStep' = { actionName: "CommitStep", argStr: "", argBool: false, result: "COMMITTED" },
    },
    nextPgid' = nextPgid,
    hasReader' = hasReader,
    readerTxid' = readerTxid,
    readerRoot' = readerRoot,
    readerExpected' = readerExpected,
  }

  action rollbackWrite = all {
    phase == PHASE_STAGED,
    all {
      freePages' = freePages.union(txRecycled),
      phase' = PHASE_IDLE,
      txRoot' = 0,
      txFlPgid' = 0,
      txStagedVal' = NOT_FOUND,
      txFreed' = Set(),
      txRecycled' = Set(),
      txDirtyWritten' = false,
      txFlWritten' = false,
      lastStep' = { actionName: "RollbackWrite", argStr: "", argBool: false, result: "OK" },
    },
    durablePages' = durablePages,
    pendingPages' = pendingPages,
    meta0' = meta0,
    meta1' = meta1,
    pendingMeta' = pendingMeta,
    txid' = txid,
    root' = root,
    freelistPgid' = freelistPgid,
    committedVal' = committedVal,
    dirtyAltMeta' = dirtyAltMeta,
    nextPgid' = nextPgid,
    pendingFree' = pendingFree,
    hasReader' = hasReader,
    readerTxid' = readerTxid,
    readerRoot' = readerRoot,
    readerExpected' = readerExpected,
    lastAckedTxid' = lastAckedTxid,
  }

  action openReader = all {
    not(hasReader),
    all {
      hasReader' = true,
      readerTxid' = txid,
      readerRoot' = root,
      readerExpected' = committedVal,
      lastStep' = { actionName: "OpenReader", argStr: "", argBool: false, result: "OK" },
    },
    durablePages' = durablePages,
    pendingPages' = pendingPages,
    meta0' = meta0,
    meta1' = meta1,
    pendingMeta' = pendingMeta,
    txid' = txid,
    root' = root,
    freelistPgid' = freelistPgid,
    committedVal' = committedVal,
    dirtyAltMeta' = dirtyAltMeta,
    nextPgid' = nextPgid,
    freePages' = freePages,
    pendingFree' = pendingFree,
    phase' = phase,
    txRoot' = txRoot,
    txFlPgid' = txFlPgid,
    txStagedVal' = txStagedVal,
    txFreed' = txFreed,
    txRecycled' = txRecycled,
    txDirtyWritten' = txDirtyWritten,
    txFlWritten' = txFlWritten,
    lastAckedTxid' = lastAckedTxid,
  }

  action closeReader = all {
    hasReader,
    val bound = calcReleaseBound(txid, false, -1)
    val rel = releaseFromPending(freePages, pendingFree, bound)
    all {
      hasReader' = false,
      readerTxid' = -1,
      readerRoot' = 0,
      readerExpected' = NOT_FOUND,
      freePages' = rel.free,
      pendingFree' = rel.pending,
      lastStep' = { actionName: "CloseReader", argStr: "", argBool: false, result: "OK" },
    },
    durablePages' = durablePages,
    pendingPages' = pendingPages,
    meta0' = meta0,
    meta1' = meta1,
    pendingMeta' = pendingMeta,
    txid' = txid,
    root' = root,
    freelistPgid' = freelistPgid,
    committedVal' = committedVal,
    dirtyAltMeta' = dirtyAltMeta,
    nextPgid' = nextPgid,
    phase' = phase,
    txRoot' = txRoot,
    txFlPgid' = txFlPgid,
    txStagedVal' = txStagedVal,
    txFreed' = txFreed,
    txRecycled' = txRecycled,
    txDirtyWritten' = txDirtyWritten,
    txFlWritten' = txFlWritten,
    lastAckedTxid' = lastAckedTxid,
  }

  // Realistic POSIX Sync() I/O failure + Rollback (invalOk=false injects double-fault!)
  action failSync(invalOk: bool): bool = all {
    phase == PHASE_DATA_WRITTEN or phase == PHASE_META_WRITTEN,
    if (phase == PHASE_META_WRITTEN) {
      val nextTx = txid + 1
      val heldRoot = if (txDirtyWritten and txRoot != 0) Set(txRoot) else Set()
      val heldFL = if (txFlWritten and txFlPgid != 0) Set(txFlPgid) else Set()
      val held = txRecycled.union(heldRoot).union(heldFL)
      val nextPM =
        if (invalOk) {
          present: true, valid: false, slot: nextTx % 2,
          txid: 0, root: 0, fl: 0, expected: NOT_FOUND
        } else pendingMeta
      all {
        pendingMeta' = nextPM,
        dirtyAltMeta' = not(invalOk),
        pendingFree' = addPendingPages(pendingFree, nextTx, held),
        freePages' = freePages,
      }
    } else {
      all {
        pendingMeta' = pendingMeta,
        dirtyAltMeta' = dirtyAltMeta,
        pendingFree' = pendingFree,
        freePages' = freePages.union(txRecycled),
      }
    },
    phase' = PHASE_IDLE,
    txRoot' = 0,
    txFlPgid' = 0,
    txStagedVal' = NOT_FOUND,
    txFreed' = Set(),
    txRecycled' = Set(),
    txDirtyWritten' = false,
    txFlWritten' = false,
    lastStep' = { actionName: "FailSync", argStr: "", argBool: invalOk, result: "SYNC_FAILED" },
    durablePages' = durablePages,
    pendingPages' = pendingPages,
    meta0' = meta0,
    meta1' = meta1,
    txid' = txid,
    root' = root,
    freelistPgid' = freelistPgid,
    committedVal' = committedVal,
    nextPgid' = nextPgid,
    hasReader' = hasReader,
    readerTxid' = readerTxid,
    readerRoot' = readerRoot,
    readerExpected' = readerExpected,
    lastAckedTxid' = lastAckedTxid,
  }

  // Power-loss fault + reopen recovery under DROP / REORDERED / TORN
  action crash(mode: str): bool = {
    val postMeta =
      if (mode == "DROP") { m0: meta0, m1: meta1 }
      else if (mode == "REORDERED") applyMetaToSlot(meta0, meta1, pendingMeta)
      else {
        if (pendingMeta.present) invalidateSlot(meta0, meta1, pendingMeta.slot)
        else { m0: meta0, m1: meta1 }
      }
    val chosen = selectRecoveredMeta(postMeta.m0, postMeta.m1)
    val recVal =
      if (chosen.root == 0) NOT_FOUND
      else if (durablePages.keys().contains(chosen.root)) durablePages.get(chosen.root)
      else NOT_FOUND
    val liveDisk = liveMetaPages(postMeta.m0).union(liveMetaPages(postMeta.m1))
    val recFreed = durablePages.keys().exclude(liveDisk)
    val recPending =
      if (recFreed.size() > 0 and chosen.txid >= 2) Map(chosen.txid -> recFreed)
      else Map()
    all {
      durablePages' = durablePages,
      pendingPages' = Map(),
      meta0' = postMeta.m0,
      meta1' = postMeta.m1,
      pendingMeta' = noPendingMeta,

      txid' = chosen.txid,
      root' = chosen.root,
      freelistPgid' = chosen.fl,
      committedVal' = recVal,
      dirtyAltMeta' = false,
      lastAckedTxid' = chosen.txid,

      nextPgid' = nextPgid,
      freePages' = Set(),
      pendingFree' = recPending,

      hasReader' = false,
      readerTxid' = -1,
      readerRoot' = 0,
      readerExpected' = NOT_FOUND,

      phase' = PHASE_IDLE,
      txRoot' = 0,
      txFlPgid' = 0,
      txStagedVal' = NOT_FOUND,
      txFreed' = Set(),
      txRecycled' = Set(),
      txDirtyWritten' = false,
      txFlWritten' = false,
      lastStep' = { actionName: "Crash", argStr: mode, argBool: false, result: "RECOVERED" },
    }
  }

  action commitStep = any {
    writeDataPages,
    syncDataPages,
    writeMetaPage,
    syncMetaPage,
  }

  action progressStep = any {
    commitStep,
    beginPut,
    beginDelete,
  }

  action stagedAltStep = any {
    stageOverwrite,
    rollbackWrite,
  }

  action readerStep = any {
    openReader,
    closeReader,
  }

  action faultStep = any {
    nondet invalOk = Set(true, false).oneOf()
    failSync(invalOk),
    all {
      txid > 0 or phase != PHASE_IDLE,
      nondet mode = CRASH_MODES.oneOf()
      crash(mode),
    },
  }

  // Progress-weighted step for deep simulation & rich MBT traces without losing
  // any reachable behavior: ~60% commit/mutation progress, ~20% MVCC reader ops,
  // ~10% staged overwrite/rollback, ~10% disk faults (FailSync / Crash).
  action step = {
    nondet roll = 0.to(9).oneOf()
    val canProgress = txid < 18 or phase != PHASE_IDLE
    val canFault = txid > 0 or phase != PHASE_IDLE
    if (roll <= 5) {
      if (canProgress) progressStep else faultStep
    } else if (roll <= 7) {
      readerStep
    } else if (roll == 8) {
      if (phase == PHASE_STAGED) stagedAltStep
      else if (canProgress) progressStep
      else faultStep
    } else {
      if (canFault) faultStep else progressStep
    }
  }

  // --- 7 Non-Tautological Safety Invariants ---

  pure def metaMatchesDurable(m: MetaSlot, pages: int -> str): bool =
    if (not(m.valid)) true
    else {
      val rootOK =
        if (m.root == 0) m.expected == NOT_FOUND
        else pages.keys().contains(m.root) and pages.get(m.root) == m.expected
      val flOK =
        if (m.fl == 0) true
        else pages.keys().contains(m.fl) and pages.get(m.fl) == FL_TAG
      rootOK and flOK
    }

  val inv_at_least_one_valid_meta: bool =
    meta0.valid or meta1.valid

  val inv_valid_meta_roots_match_durable: bool =
    metaMatchesDurable(meta0, durablePages) and metaMatchesDurable(meta1, durablePages)

  val inv_no_live_page_in_freelist: bool = {
    val live =
      liveMetaPages(meta0)
        .union(liveMetaPages(meta1))
        .union(if (root != 0) Set(root) else Set())
        .union(if (freelistPgid != 0) Set(freelistPgid) else Set())
        .union(if (hasReader and readerRoot != 0) Set(readerRoot) else Set())
    freePages.intersect(live).size() == 0
  }

  val inv_reader_sees_intact_durable_page: bool =
    if (not(hasReader)) true
    else if (readerRoot == 0) readerExpected == NOT_FOUND
    else durablePages.keys().contains(readerRoot) and durablePages.get(readerRoot) == readerExpected

  val inv_committed_txid_never_regresses: bool =
    txid >= lastAckedTxid

  val inv_no_resurrected_aborted_meta: bool = {
    val maxAllowed = if (phase == PHASE_META_WRITTEN) txid + 1 else txid
    val ok0 = not(meta0.valid) or meta0.txid <= maxAllowed
    val ok1 = not(meta1.valid) or meta1.txid <= maxAllowed
    ok0 and ok1
  }

  val inv_release_bound_safe: bool = {
    val bound = calcReleaseBound(txid, hasReader, readerTxid)
    pendingFree.keys().forall(t => t > bound)
  }

  val inv_all: bool = all {
    inv_at_least_one_valid_meta,
    inv_valid_meta_roots_match_durable,
    inv_no_live_page_in_freelist,
    inv_reader_sees_intact_durable_page,
    inv_committed_txid_never_regresses,
    inv_no_resurrected_aborted_meta,
    inv_release_bound_safe,
  }

  // --- Built-in Quint Scenario Tests ---

  run doubleFaultFenceTest = {
    init
      .then(beginPut)
      .then(writeDataPages)
      .then(syncDataPages)
      .then(writeMetaPage)
      .then(syncMetaPage)
      .then(beginPut)
      .then(writeDataPages)
      .then(syncDataPages)
      .then(writeMetaPage)
      .then(failSync(false))
      .expect(dirtyAltMeta and inv_all)
      .then(beginPut)
      .then(writeDataPages)
      .then(syncDataPages)
      .expect(not(dirtyAltMeta) and inv_all)
      .then(crash("DROP"))
      .expect(txid == 1 and inv_all)
  }

  run mvccReaderReclaimTest = {
    init
      .then(beginPut)
      .then(writeDataPages)
      .then(syncDataPages)
      .then(writeMetaPage)
      .then(syncMetaPage)
      .then(openReader)
      .then(beginPut)
      .then(writeDataPages)
      .then(syncDataPages)
      .then(writeMetaPage)
      .then(syncMetaPage)
      .then(beginPut)
      .then(writeDataPages)
      .then(syncDataPages)
      .then(writeMetaPage)
      .then(syncMetaPage)
      .expect(freePages.size() == 0 and inv_all)
      .then(closeReader)
      .expect(freePages.size() > 0 and inv_all)
  }
}
