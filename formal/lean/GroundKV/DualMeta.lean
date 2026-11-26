/-
  GroundKV Unbounded Formal Proof — Module 1: Dual-Meta Crash Atomicity,
  Durability, and Realistic POSIX FailSync / dirtyAltMeta Fencing.

  Mirrors:
    - engine/tx.go   (4-phase commit, Rollback alt-meta neutralization, SyncDataPages fence)
    - engine/meta.go (dual meta slots Page 0 / Page 1, parity txid % 2)
    - engine/db.go   (recover() selecting max valid durable meta slot)

  Unlike finite model checking (FizzBee / bounded TLC), every theorem below is
  proven by structural induction over arbitrary unbounded execution traces
  (arbitrary TxID : Nat, arbitrary crash counts, arbitrary FailSync sequences).
-/
namespace GroundKV.DualMeta

/-- Commit phase of the single serialized write transaction. -/
inductive Phase where
  | idle
  | staged
  | dataWritten
  | dataSynced
  | metaWritten
  deriving DecidableEq, Repr

/-- Power-loss crash modes modeled against the volatile OS page cache. -/
inductive CrashMode where
  | drop
  | reordered
  | torn
  deriving DecidableEq, Repr

/-- Durable state of one on-disk meta page (Page 0 or Page 1). -/
structure MetaSlot where
  valid       : Bool
  txid        : Nat
  dataDurable : Bool
  deriving DecidableEq, Repr

/-- Unsynced meta-page entry sitting in the volatile OS page cache. -/
inductive PendingMeta where
  | none
  | valid       (slot : Fin 2) (txid : Nat) (dataDurable : Bool)
  | invalidated (slot : Fin 2)
  deriving DecidableEq, Repr

/-- Full unbounded state of the dual-meta storage protocol. -/
structure State where
  txid           : Nat
  lastSyncedTxID : Nat
  phase          : Phase
  dirtyAltMeta   : Bool
  meta0          : MetaSlot
  meta1          : MetaSlot
  pendingMeta    : PendingMeta
  txDataSynced   : Bool
  deriving DecidableEq, Repr

/-- Parity mapping: even TxIDs live in slot 0, odd TxIDs in slot 1. -/
def slotOf (t : Nat) : Fin 2 :=
  ⟨t % 2, Nat.mod_lt t (by omega)⟩

theorem slotOf_succ_ne (t : Nat) : slotOf (t + 1) ≠ slotOf t := by
  intro h
  have hv : (t + 1) % 2 = t % 2 := congrArg Fin.val h
  omega

theorem slotOf_succ_succ (t : Nat) : slotOf (t + 2) = slotOf t := by
  apply Fin.ext
  change (t + 2) % 2 = t % 2
  omega

def getSlot (s : State) (slot : Fin 2) : MetaSlot :=
  if slot.val = 0 then s.meta0 else s.meta1

def setSlot (s : State) (slot : Fin 2) (m : MetaSlot) : State :=
  if slot.val = 0 then { s with meta0 := m } else { s with meta1 := m }

@[simp] theorem getSlot_same_meta (s : State) (tx ltx : Nat) (ph : Phase) (da : Bool)
    (pm : PendingMeta) (ds : Bool) (slot : Fin 2) :
    getSlot ⟨tx, ltx, ph, da, s.meta0, s.meta1, pm, ds⟩ slot = getSlot s slot := rfl

@[simp] theorem setSlot_txid (s : State) (slot : Fin 2) (m : MetaSlot) :
    (setSlot s slot m).txid = s.txid := by
  unfold setSlot; split <;> rfl

@[simp] theorem setSlot_lastSyncedTxID (s : State) (slot : Fin 2) (m : MetaSlot) :
    (setSlot s slot m).lastSyncedTxID = s.lastSyncedTxID := by
  unfold setSlot; split <;> rfl

@[simp] theorem setSlot_dirtyAltMeta (s : State) (slot : Fin 2) (m : MetaSlot) :
    (setSlot s slot m).dirtyAltMeta = s.dirtyAltMeta := by
  unfold setSlot; split <;> rfl

theorem getSlot_setSlot_same (s : State) (slot : Fin 2) (m : MetaSlot) :
    getSlot (setSlot s slot m) slot = m := by
  unfold getSlot setSlot
  cases slot with
  | mk v hv =>
    by_cases h0 : v = 0 <;> simp [h0]

theorem getSlot_setSlot_other (s : State) (a b : Fin 2) (m : MetaSlot) (hne : a ≠ b) :
    getSlot (setSlot s a m) b = getSlot s b := by
  unfold getSlot setSlot
  cases a with
  | mk av hav =>
    cases b with
    | mk bv hbv =>
      have hne' : av ≠ bv := fun h => hne (Fin.ext h)
      have : (av = 0 ∧ bv = 1) ∨ (av = 1 ∧ bv = 0) := by omega
      rcases this with ⟨rfl, rfl⟩ | ⟨rfl, rfl⟩ <;> simp

/-- Initial state after `DB.initEmpty`: both meta slots durably hold TxID 0. -/
def initState : State where
  txid           := 0
  lastSyncedTxID := 0
  phase          := .idle
  dirtyAltMeta   := false
  meta0          := { valid := true, txid := 0, dataDurable := true }
  meta1          := { valid := true, txid := 0, dataDurable := true }
  pendingMeta    := .none
  txDataSynced   := false

/-- Apply any pending OS page-cache meta write to durable disk during `Sync()`. -/
def flushPendingMeta (s : State) : State :=
  match s.pendingMeta with
  | .none => s
  | .valid slot tid dur =>
      { setSlot s slot { valid := true, txid := tid, dataDurable := dur } with
        pendingMeta := .none }
  | .invalidated slot =>
      { setSlot s slot { valid := false, txid := 0, dataDurable := false } with
        pendingMeta := .none }

/-- Apply power-loss crash mode to durable disk before recovery. -/
def applyCrashDisk (s : State) (mode : CrashMode) : State :=
  match mode with
  | .drop => { s with pendingMeta := .none }
  | .reordered => flushPendingMeta s
  | .torn =>
      match s.pendingMeta with
      | .none => { s with pendingMeta := .none }
      | .valid slot _ _ =>
          { setSlot s slot { valid := false, txid := 0, dataDurable := false } with
            pendingMeta := .none }
      | .invalidated slot =>
          { setSlot s slot { valid := false, txid := 0, dataDurable := false } with
            pendingMeta := .none }

/-- `DB.recover()` picks the valid durable meta slot with the higher TxID. -/
def recoverTxID (s : State) : Nat :=
  match s.meta0.valid, s.meta1.valid with
  | true,  true  => Nat.max s.meta0.txid s.meta1.txid
  | true,  false => s.meta0.txid
  | false, true  => s.meta1.txid
  | false, false => 0

def crashAndRecover (s : State) (mode : CrashMode) : State :=
  let d := applyCrashDisk s mode
  let recTx := recoverTxID d
  { d with
    txid           := recTx
    lastSyncedTxID := recTx
    phase          := .idle
    dirtyAltMeta   := false
    pendingMeta    := .none
    txDataSynced   := false }

def syncDataState (s : State) : State :=
  let fenced :=
    if s.dirtyAltMeta then
      { s with pendingMeta := .invalidated (slotOf (s.txid + 1)), dirtyAltMeta := false }
    else s
  let synced := flushPendingMeta fenced
  { synced with phase := .dataSynced, txDataSynced := true }

def syncMetaState (s : State) : State :=
  let synced := flushPendingMeta s
  { synced with
    txid           := s.txid + 1
    lastSyncedTxID := s.txid + 1
    phase          := .idle
    dirtyAltMeta   := false
    txDataSynced   := false }

/-- Single atomic transition of GroundKV's dual-meta protocol. -/
inductive Step : State → State → Prop where
  | beginWrite {s : State} :
      s.phase = .idle →
      Step s { s with phase := .staged, txDataSynced := false }

  | writeData {s : State} :
      s.phase = .staged →
      Step s { s with phase := .dataWritten }

  | syncData {s : State} :
      s.phase = .dataWritten →
      Step s (syncDataState s)

  | writeMeta {s : State} :
      s.phase = .dataSynced →
      Step s { s with
        phase       := .metaWritten
        pendingMeta := .valid (slotOf (s.txid + 1)) (s.txid + 1) s.txDataSynced }

  | syncMeta {s : State} :
      s.phase = .metaWritten →
      Step s (syncMetaState s)

  | rollbackBeforeMeta {s : State} :
      (s.phase = .staged ∨ s.phase = .dataWritten ∨ s.phase = .dataSynced) →
      Step s { s with phase := .idle, txDataSynced := false }

  /-- Failed `SyncDataPages` after neutralizing `dirtyAltMeta` (write succeeded, fsync failed). -/
  | rollbackAfterFenceSyncFail {s : State} :
      s.phase = .dataWritten →
      s.dirtyAltMeta = true →
      Step s { s with
        phase        := .idle
        dirtyAltMeta := false
        pendingMeta  := .invalidated (slotOf (s.txid + 1))
        txDataSynced := false }

  /-- Realistic POSIX `SyncMetaPage` failure:
      `invalOk = true`  -> Rollback neutralizes altSlot with zeros;
      `invalOk = false` -> Rollback zero-write ALSO fails, leaving `dirtyAltMeta = true`. -/
  | failSyncMeta {s : State} (invalOk : Bool) :
      s.phase = .metaWritten →
      Step s (if invalOk then
        { s with
          phase        := .idle
          dirtyAltMeta := false
          pendingMeta  := .invalidated (slotOf (s.txid + 1))
          txDataSynced := false }
      else
        { s with
          phase        := .idle
          dirtyAltMeta := true
          txDataSynced := false })

  | crash {s : State} (mode : CrashMode) :
      Step s (crashAndRecover s mode)

/-- Reflexive-transitive reachability from `initState` (unbounded execution length). -/
inductive Reachable : State → Prop where
  | init : Reachable initState
  | step {s s' : State} : Reachable s → Step s s' → Reachable s'

/-- Inductive invariant capturing the exact protocol invariants at every reachable state. -/
structure Inv (s : State) : Prop where
  activeValid      : (getSlot s (slotOf s.txid)).valid = true
  activeTxID       : (getSlot s (slotOf s.txid)).txid = s.txid
  activeDurable    : (getSlot s (slotOf s.txid)).dataDurable = true
  altBounded       : (getSlot s (slotOf (s.txid + 1))).valid = true →
                       (getSlot s (slotOf (s.txid + 1))).txid ≤ s.txid ∧
                       (getSlot s (slotOf (s.txid + 1))).dataDurable = true
  durabilityBound  : s.txid ≥ s.lastSyncedTxID
  syncedPhaseClean : (s.phase = .dataSynced ∨ s.phase = .metaWritten) →
                       s.txDataSynced = true ∧ s.dirtyAltMeta = false
  pendingShape     :
    match s.phase, s.dirtyAltMeta with
    | .metaWritten, false =>
        s.pendingMeta = .valid (slotOf (s.txid + 1)) (s.txid + 1) true
    | .dataSynced, false =>
        s.pendingMeta = .none
    | _, true =>
        s.pendingMeta = .valid (slotOf (s.txid + 1)) (s.txid + 1) true
    | _, false =>
        s.pendingMeta = .none ∨ s.pendingMeta = .invalidated (slotOf (s.txid + 1))

private theorem inv_init : Inv initState where
  activeValid := by decide
  activeTxID := by decide
  activeDurable := by decide
  altBounded := by decide
  durabilityBound := by decide
  syncedPhaseClean := by intro h; rcases h with h | h <;> cases h
  pendingShape := by simp [initState]

private theorem recoverTxID_eq_of_slots (s : State) (t : Nat) :
    (getSlot s (slotOf t)).valid = true →
    (getSlot s (slotOf t)).txid = t →
    ((getSlot s (slotOf (t + 1))).valid = true →
      (getSlot s (slotOf (t + 1))).txid ≤ t) →
    recoverTxID s = t := by
  intro hactV hactT haltB
  unfold recoverTxID getSlot slotOf at *
  by_cases hp : t % 2 = 0
  · have hp1 : (t + 1) % 2 = 1 := by omega
    simp [hp, hp1] at hactV hactT haltB
    cases hm1 : s.meta1.valid
    · simp [hactV, hactT, hm1]
    · have hle : s.meta1.txid ≤ t := haltB hm1
      simp [hactV, hactT, hm1, Nat.max_eq_left hle]
  · have hp0 : t % 2 = 1 := by omega
    have hp1 : (t + 1) % 2 = 0 := by omega
    simp [hp0, hp1] at hactV hactT haltB
    cases hm0 : s.meta0.valid
    · simp [hactV, hactT, hm0]
    · have hle : s.meta0.txid ≤ t := haltB hm0
      simp [hactV, hactT, hm0, Nat.max_eq_right hle]

private theorem recoverTxID_after_inval_alt (s : State) :
    (getSlot s (slotOf s.txid)).valid = true →
    (getSlot s (slotOf s.txid)).txid = s.txid →
    recoverTxID
      { setSlot s (slotOf (s.txid + 1)) { valid := false, txid := 0, dataDurable := false } with
        pendingMeta := .none } = s.txid := by
  intro hactV hactT
  apply recoverTxID_eq_of_slots _ s.txid
  · change (getSlot (setSlot s (slotOf (s.txid + 1)) _) (slotOf s.txid)).valid = true
    rw [getSlot_setSlot_other _ _ _ _ (slotOf_succ_ne s.txid)]; exact hactV
  · change (getSlot (setSlot s (slotOf (s.txid + 1)) _) (slotOf s.txid)).txid = s.txid
    rw [getSlot_setSlot_other _ _ _ _ (slotOf_succ_ne s.txid)]; exact hactT
  · change (getSlot (setSlot s (slotOf (s.txid + 1)) { valid := false, txid := 0, dataDurable := false }) (slotOf (s.txid + 1))).valid = true → _
    rw [getSlot_setSlot_same]; simp

private theorem recoverTxID_after_valid_alt (s : State) :
    (getSlot s (slotOf s.txid)).valid = true →
    (getSlot s (slotOf s.txid)).txid = s.txid →
    recoverTxID
      { setSlot s (slotOf (s.txid + 1)) { valid := true, txid := s.txid + 1, dataDurable := true } with
        pendingMeta := .none } = s.txid + 1 := by
  intro hactV hactT
  apply recoverTxID_eq_of_slots _ (s.txid + 1)
  · change (getSlot (setSlot s (slotOf (s.txid + 1)) { valid := true, txid := s.txid + 1, dataDurable := true }) (slotOf (s.txid + 1))).valid = true
    rw [getSlot_setSlot_same]
  · change (getSlot (setSlot s (slotOf (s.txid + 1)) { valid := true, txid := s.txid + 1, dataDurable := true }) (slotOf (s.txid + 1))).txid = s.txid + 1
    rw [getSlot_setSlot_same]
  · intro _
    rw [slotOf_succ_succ]
    change (getSlot (setSlot s (slotOf (s.txid + 1)) _) (slotOf s.txid)).txid ≤ s.txid + 1
    rw [getSlot_setSlot_other _ _ _ _ (slotOf_succ_ne s.txid), hactT]
    omega

private theorem inv_step {s s' : State} (hinv : Inv s) (hstep : Step s s') : Inv s' := by
  cases hstep with
  | beginWrite hp =>
    have hps := hinv.pendingShape
    rw [hp] at hps
    refine {
      activeValid := hinv.activeValid
      activeTxID := hinv.activeTxID
      activeDurable := hinv.activeDurable
      altBounded := hinv.altBounded
      durabilityBound := hinv.durabilityBound
      syncedPhaseClean := by intro h; rcases h with h | h <;> cases h
      pendingShape := ?_
    }
    cases hd : s.dirtyAltMeta <;> simp [hd] at hps ⊢ <;> exact hps

  | writeData hp =>
    have hps := hinv.pendingShape
    rw [hp] at hps
    refine {
      activeValid := hinv.activeValid
      activeTxID := hinv.activeTxID
      activeDurable := hinv.activeDurable
      altBounded := hinv.altBounded
      durabilityBound := hinv.durabilityBound
      syncedPhaseClean := by intro h; rcases h with h | h <;> cases h
      pendingShape := ?_
    }
    cases hd : s.dirtyAltMeta <;> simp [hd] at hps ⊢ <;> exact hps

  | syncData hp =>
    have hps := hinv.pendingShape
    rw [hp] at hps
    have hne : slotOf (s.txid + 1) ≠ slotOf s.txid := slotOf_succ_ne s.txid
    cases hd : s.dirtyAltMeta
    · simp [hd] at hps
      rcases hps with hnone | hinvM
      · refine {
          activeValid := by simp [syncDataState, hd, flushPendingMeta, hnone, hinv.activeValid]
          activeTxID := by simp [syncDataState, hd, flushPendingMeta, hnone, hinv.activeTxID]
          activeDurable := by simp [syncDataState, hd, flushPendingMeta, hnone, hinv.activeDurable]
          altBounded := by simp [syncDataState, hd, flushPendingMeta, hnone]; exact hinv.altBounded
          durabilityBound := by simp [syncDataState, hd, flushPendingMeta, hnone]; exact hinv.durabilityBound
          syncedPhaseClean := by intro _; simp [syncDataState, hd, flushPendingMeta, hnone]
          pendingShape := by simp [syncDataState, hd, flushPendingMeta, hnone]
        }
      · refine {
          activeValid := by
            simp only [syncDataState, hd, Bool.false_eq_true, ↓reduceIte, flushPendingMeta, hinvM, setSlot_txid]
            change (getSlot (setSlot s (slotOf (s.txid + 1)) _) (slotOf s.txid)).valid = true
            rw [getSlot_setSlot_other _ _ _ _ hne]; exact hinv.activeValid
          activeTxID := by
            simp only [syncDataState, hd, Bool.false_eq_true, ↓reduceIte, flushPendingMeta, hinvM, setSlot_txid]
            change (getSlot (setSlot s (slotOf (s.txid + 1)) _) (slotOf s.txid)).txid = s.txid
            rw [getSlot_setSlot_other _ _ _ _ hne]; exact hinv.activeTxID
          activeDurable := by
            simp only [syncDataState, hd, Bool.false_eq_true, ↓reduceIte, flushPendingMeta, hinvM, setSlot_txid]
            change (getSlot (setSlot s (slotOf (s.txid + 1)) _) (slotOf s.txid)).dataDurable = true
            rw [getSlot_setSlot_other _ _ _ _ hne]; exact hinv.activeDurable
          altBounded := by
            simp only [syncDataState, hd, Bool.false_eq_true, ↓reduceIte, flushPendingMeta, hinvM, setSlot_txid]
            change (getSlot (setSlot s (slotOf (s.txid + 1)) { valid := false, txid := 0, dataDurable := false }) (slotOf (s.txid + 1))).valid = true → _
            rw [getSlot_setSlot_same]; simp
          durabilityBound := by simp [syncDataState, hd, flushPendingMeta, hinvM]; exact hinv.durabilityBound
          syncedPhaseClean := by intro _; simp [syncDataState, hd, flushPendingMeta, hinvM]
          pendingShape := by simp [syncDataState, hd, flushPendingMeta, hinvM]
        }
    · refine {
        activeValid := by
          simp only [syncDataState, hd, ↓reduceIte, flushPendingMeta, setSlot_txid]
          change (getSlot (setSlot { s with pendingMeta := .invalidated (slotOf (s.txid + 1)), dirtyAltMeta := false } (slotOf (s.txid + 1)) _) (slotOf s.txid)).valid = true
          rw [getSlot_setSlot_other _ _ _ _ hne]; exact hinv.activeValid
        activeTxID := by
          simp only [syncDataState, hd, ↓reduceIte, flushPendingMeta, setSlot_txid]
          change (getSlot (setSlot { s with pendingMeta := .invalidated (slotOf (s.txid + 1)), dirtyAltMeta := false } (slotOf (s.txid + 1)) _) (slotOf s.txid)).txid = s.txid
          rw [getSlot_setSlot_other _ _ _ _ hne]; exact hinv.activeTxID
        activeDurable := by
          simp only [syncDataState, hd, ↓reduceIte, flushPendingMeta, setSlot_txid]
          change (getSlot (setSlot { s with pendingMeta := .invalidated (slotOf (s.txid + 1)), dirtyAltMeta := false } (slotOf (s.txid + 1)) _) (slotOf s.txid)).dataDurable = true
          rw [getSlot_setSlot_other _ _ _ _ hne]; exact hinv.activeDurable
        altBounded := by
          simp only [syncDataState, hd, ↓reduceIte, flushPendingMeta, setSlot_txid]
          change (getSlot (setSlot { s with pendingMeta := .invalidated (slotOf (s.txid + 1)), dirtyAltMeta := false } (slotOf (s.txid + 1)) { valid := false, txid := 0, dataDurable := false }) (slotOf (s.txid + 1))).valid = true → _
          rw [getSlot_setSlot_same]; simp
        durabilityBound := by simp [syncDataState, hd, flushPendingMeta]; exact hinv.durabilityBound
        syncedPhaseClean := by intro _; simp [syncDataState, hd, flushPendingMeta]
        pendingShape := by simp [syncDataState, hd, flushPendingMeta]
      }

  | writeMeta hp =>
    have ⟨hdur, hd⟩ := hinv.syncedPhaseClean (Or.inl hp)
    refine {
      activeValid := hinv.activeValid
      activeTxID := hinv.activeTxID
      activeDurable := hinv.activeDurable
      altBounded := hinv.altBounded
      durabilityBound := hinv.durabilityBound
      syncedPhaseClean := by intro _; exact ⟨hdur, hd⟩
      pendingShape := by simp [hd, hdur]
    }

  | syncMeta hp =>
    have ⟨_, hd⟩ := hinv.syncedPhaseClean (Or.inr hp)
    have hps := hinv.pendingShape
    rw [hp, hd] at hps
    have hne : slotOf (s.txid + 1) ≠ slotOf s.txid := slotOf_succ_ne s.txid
    have hss : slotOf (s.txid + 1 + 1) = slotOf s.txid := slotOf_succ_succ s.txid
    refine {
      activeValid := by
        simp only [syncMetaState, flushPendingMeta, hps]
        change (getSlot (setSlot s (slotOf (s.txid + 1)) { valid := true, txid := s.txid + 1, dataDurable := true }) (slotOf (s.txid + 1))).valid = true
        rw [getSlot_setSlot_same]
      activeTxID := by
        simp only [syncMetaState, flushPendingMeta, hps]
        change (getSlot (setSlot s (slotOf (s.txid + 1)) { valid := true, txid := s.txid + 1, dataDurable := true }) (slotOf (s.txid + 1))).txid = s.txid + 1
        rw [getSlot_setSlot_same]
      activeDurable := by
        simp only [syncMetaState, flushPendingMeta, hps]
        change (getSlot (setSlot s (slotOf (s.txid + 1)) { valid := true, txid := s.txid + 1, dataDurable := true }) (slotOf (s.txid + 1))).dataDurable = true
        rw [getSlot_setSlot_same]
      altBounded := by
        simp only [syncMetaState, flushPendingMeta, hps, hss]
        intro _
        change (getSlot (setSlot s (slotOf (s.txid + 1)) _) (slotOf s.txid)).txid ≤ s.txid + 1 ∧
               (getSlot (setSlot s (slotOf (s.txid + 1)) _) (slotOf s.txid)).dataDurable = true
        rw [getSlot_setSlot_other _ _ _ _ hne]
        exact ⟨by rw [hinv.activeTxID]; omega, hinv.activeDurable⟩
      durabilityBound := by simp [syncMetaState, flushPendingMeta, hps]
      syncedPhaseClean := by intro h; rcases h with h | h <;> cases h
      pendingShape := by simp [syncMetaState, flushPendingMeta, hps]
    }

  | rollbackBeforeMeta hp =>
    have hps := hinv.pendingShape
    refine {
      activeValid := hinv.activeValid
      activeTxID := hinv.activeTxID
      activeDurable := hinv.activeDurable
      altBounded := hinv.altBounded
      durabilityBound := hinv.durabilityBound
      syncedPhaseClean := by intro h; rcases h with h | h <;> cases h
      pendingShape := ?_
    }
    rcases hp with hp | hp | hp <;> rw [hp] at hps
    · cases hd : s.dirtyAltMeta <;> simp [hd] at hps ⊢ <;> exact hps
    · cases hd : s.dirtyAltMeta <;> simp [hd] at hps ⊢ <;> exact hps
    · have ⟨_, hd⟩ := hinv.syncedPhaseClean (Or.inl hp)
      simp [hd] at hps ⊢
      exact Or.inl hps

  | rollbackAfterFenceSyncFail _ _ =>
    refine {
      activeValid := hinv.activeValid
      activeTxID := hinv.activeTxID
      activeDurable := hinv.activeDurable
      altBounded := hinv.altBounded
      durabilityBound := hinv.durabilityBound
      syncedPhaseClean := by intro h; rcases h with h | h <;> cases h
      pendingShape := by simp
    }

  | failSyncMeta invalOk hp =>
    have ⟨_, hd⟩ := hinv.syncedPhaseClean (Or.inr hp)
    have hps := hinv.pendingShape
    rw [hp, hd] at hps
    cases invalOk
    · refine {
        activeValid := hinv.activeValid
        activeTxID := hinv.activeTxID
        activeDurable := hinv.activeDurable
        altBounded := hinv.altBounded
        durabilityBound := hinv.durabilityBound
        syncedPhaseClean := by intro h; rcases h with h | h <;> cases h
        pendingShape := by simp [hps]
      }
    · refine {
        activeValid := hinv.activeValid
        activeTxID := hinv.activeTxID
        activeDurable := hinv.activeDurable
        altBounded := hinv.altBounded
        durabilityBound := hinv.durabilityBound
        syncedPhaseClean := by intro h; rcases h with h | h <;> cases h
        pendingShape := by simp
      }

  | crash mode =>
    have hne : slotOf (s.txid + 1) ≠ slotOf s.txid := slotOf_succ_ne s.txid
    have hss : slotOf (s.txid + 1 + 1) = slotOf s.txid := slotOf_succ_succ s.txid
    have hps := hinv.pendingShape
    have hpmCases :
        s.pendingMeta = .none ∨
        s.pendingMeta = .invalidated (slotOf (s.txid + 1)) ∨
        s.pendingMeta = .valid (slotOf (s.txid + 1)) (s.txid + 1) true := by
      cases hp : s.phase <;> cases hd : s.dirtyAltMeta <;> rw [hp, hd] at hps <;> simp at hps
      all_goals first
        | exact Or.inr (Or.inr hps)
        | exact Or.inl hps
        | (rcases hps with h | h; exact Or.inl h; exact Or.inr (Or.inl h))
    cases mode with
    | drop =>
      have hrec : recoverTxID { s with pendingMeta := .none } = s.txid :=
        recoverTxID_eq_of_slots _ s.txid hinv.activeValid hinv.activeTxID (fun hv => (hinv.altBounded hv).1)
      refine {
        activeValid := by simp [crashAndRecover, applyCrashDisk, hrec, hinv.activeValid]
        activeTxID := by simp [crashAndRecover, applyCrashDisk, hrec, hinv.activeTxID]
        activeDurable := by simp [crashAndRecover, applyCrashDisk, hrec, hinv.activeDurable]
        altBounded := by simp [crashAndRecover, applyCrashDisk, hrec]; exact hinv.altBounded
        durabilityBound := by simp [crashAndRecover, applyCrashDisk, hrec]
        syncedPhaseClean := by intro h; rcases h with h | h <;> cases h
        pendingShape := by simp [crashAndRecover, applyCrashDisk]
      }
    | reordered =>
      rcases hpmCases with hnone | hinvM | hvalM
      · have hrec : recoverTxID s = s.txid :=
          recoverTxID_eq_of_slots s s.txid hinv.activeValid hinv.activeTxID (fun hv => (hinv.altBounded hv).1)
        refine {
          activeValid := by simp [crashAndRecover, applyCrashDisk, flushPendingMeta, hnone, hrec, hinv.activeValid]
          activeTxID := by simp [crashAndRecover, applyCrashDisk, flushPendingMeta, hnone, hrec, hinv.activeTxID]
          activeDurable := by simp [crashAndRecover, applyCrashDisk, flushPendingMeta, hnone, hrec, hinv.activeDurable]
          altBounded := by simp [crashAndRecover, applyCrashDisk, flushPendingMeta, hnone, hrec]; exact hinv.altBounded
          durabilityBound := by simp [crashAndRecover, applyCrashDisk, flushPendingMeta, hnone, hrec]
          syncedPhaseClean := by intro h; rcases h with h | h <;> cases h
          pendingShape := by simp [crashAndRecover, applyCrashDisk, flushPendingMeta, hnone]
        }
      · let d := setSlot s (slotOf (s.txid + 1)) { valid := false, txid := 0, dataDurable := false }
        have hdActV : (getSlot d (slotOf s.txid)).valid = true := by
          rw [getSlot_setSlot_other _ _ _ _ hne]; exact hinv.activeValid
        have hdActT : (getSlot d (slotOf s.txid)).txid = s.txid := by
          rw [getSlot_setSlot_other _ _ _ _ hne]; exact hinv.activeTxID
        have hdActD : (getSlot d (slotOf s.txid)).dataDurable = true := by
          rw [getSlot_setSlot_other _ _ _ _ hne]; exact hinv.activeDurable
        have hdAltB : (getSlot d (slotOf (s.txid + 1))).valid = true →
            (getSlot d (slotOf (s.txid + 1))).txid ≤ s.txid ∧
            (getSlot d (slotOf (s.txid + 1))).dataDurable = true := by
          rw [getSlot_setSlot_same]; simp
        have hrec : recoverTxID { d with pendingMeta := .none } = s.txid :=
          recoverTxID_eq_of_slots _ s.txid hdActV hdActT (fun hv => (hdAltB hv).1)
        refine {
          activeValid := by simp only [crashAndRecover, applyCrashDisk, flushPendingMeta, hinvM]; rw [hrec]; exact hdActV
          activeTxID := by simp only [crashAndRecover, applyCrashDisk, flushPendingMeta, hinvM]; rw [hrec]; exact hdActT
          activeDurable := by simp only [crashAndRecover, applyCrashDisk, flushPendingMeta, hinvM]; rw [hrec]; exact hdActD
          altBounded := by simp only [crashAndRecover, applyCrashDisk, flushPendingMeta, hinvM]; rw [hrec]; exact hdAltB
          durabilityBound := by simp [crashAndRecover, applyCrashDisk, flushPendingMeta, hinvM]
          syncedPhaseClean := by intro h; rcases h with h | h <;> cases h
          pendingShape := by simp [crashAndRecover, applyCrashDisk, flushPendingMeta, hinvM]
        }
      · let d := setSlot s (slotOf (s.txid + 1)) { valid := true, txid := s.txid + 1, dataDurable := true }
        have hrec : recoverTxID { d with pendingMeta := .none } = s.txid + 1 :=
          recoverTxID_after_valid_alt s hinv.activeValid hinv.activeTxID
        refine {
          activeValid := by
            simp only [crashAndRecover, applyCrashDisk, flushPendingMeta, hvalM]
            rw [hrec]
            change (getSlot d (slotOf (s.txid + 1))).valid = true
            rw [getSlot_setSlot_same]
          activeTxID := by
            simp only [crashAndRecover, applyCrashDisk, flushPendingMeta, hvalM]
            rw [hrec]
            change (getSlot d (slotOf (s.txid + 1))).txid = s.txid + 1
            rw [getSlot_setSlot_same]
          activeDurable := by
            simp only [crashAndRecover, applyCrashDisk, flushPendingMeta, hvalM]
            rw [hrec]
            change (getSlot d (slotOf (s.txid + 1))).dataDurable = true
            rw [getSlot_setSlot_same]
          altBounded := by
            simp only [crashAndRecover, applyCrashDisk, flushPendingMeta, hvalM]
            rw [hrec, hss]
            intro _
            change (getSlot d (slotOf s.txid)).txid ≤ s.txid + 1 ∧ (getSlot d (slotOf s.txid)).dataDurable = true
            rw [getSlot_setSlot_other _ _ _ _ hne]
            exact ⟨by rw [hinv.activeTxID]; omega, hinv.activeDurable⟩
          durabilityBound := by simp [crashAndRecover, applyCrashDisk, flushPendingMeta, hvalM]
          syncedPhaseClean := by intro h; rcases h with h | h <;> cases h
          pendingShape := by simp [crashAndRecover, applyCrashDisk, flushPendingMeta, hvalM]
        }
    | torn =>
      rcases hpmCases with hnone | hinvM | hvalM
      · have hrec : recoverTxID { s with pendingMeta := .none } = s.txid :=
          recoverTxID_eq_of_slots _ s.txid hinv.activeValid hinv.activeTxID (fun hv => (hinv.altBounded hv).1)
        refine {
          activeValid := by simp [crashAndRecover, applyCrashDisk, hnone, hrec, hinv.activeValid]
          activeTxID := by simp [crashAndRecover, applyCrashDisk, hnone, hrec, hinv.activeTxID]
          activeDurable := by simp [crashAndRecover, applyCrashDisk, hnone, hrec, hinv.activeDurable]
          altBounded := by simp [crashAndRecover, applyCrashDisk, hnone, hrec]; exact hinv.altBounded
          durabilityBound := by simp [crashAndRecover, applyCrashDisk, hnone, hrec]
          syncedPhaseClean := by intro h; rcases h with h | h <;> cases h
          pendingShape := by simp [crashAndRecover, applyCrashDisk, hnone]
        }
      · let d := setSlot s (slotOf (s.txid + 1)) { valid := false, txid := 0, dataDurable := false }
        have hdActV : (getSlot d (slotOf s.txid)).valid = true := by
          rw [getSlot_setSlot_other _ _ _ _ hne]; exact hinv.activeValid
        have hdActT : (getSlot d (slotOf s.txid)).txid = s.txid := by
          rw [getSlot_setSlot_other _ _ _ _ hne]; exact hinv.activeTxID
        have hdActD : (getSlot d (slotOf s.txid)).dataDurable = true := by
          rw [getSlot_setSlot_other _ _ _ _ hne]; exact hinv.activeDurable
        have hdAltB : (getSlot d (slotOf (s.txid + 1))).valid = true →
            (getSlot d (slotOf (s.txid + 1))).txid ≤ s.txid ∧
            (getSlot d (slotOf (s.txid + 1))).dataDurable = true := by
          rw [getSlot_setSlot_same]; simp
        have hrec : recoverTxID { d with pendingMeta := .none } = s.txid :=
          recoverTxID_eq_of_slots _ s.txid hdActV hdActT (fun hv => (hdAltB hv).1)
        refine {
          activeValid := by simp only [crashAndRecover, applyCrashDisk, hinvM]; rw [hrec]; exact hdActV
          activeTxID := by simp only [crashAndRecover, applyCrashDisk, hinvM]; rw [hrec]; exact hdActT
          activeDurable := by simp only [crashAndRecover, applyCrashDisk, hinvM]; rw [hrec]; exact hdActD
          altBounded := by simp only [crashAndRecover, applyCrashDisk, hinvM]; rw [hrec]; exact hdAltB
          durabilityBound := by simp [crashAndRecover, applyCrashDisk, hinvM]
          syncedPhaseClean := by intro h; rcases h with h | h <;> cases h
          pendingShape := by simp [crashAndRecover, applyCrashDisk, hinvM]
        }
      · let d := setSlot s (slotOf (s.txid + 1)) { valid := false, txid := 0, dataDurable := false }
        have hdActV : (getSlot d (slotOf s.txid)).valid = true := by
          rw [getSlot_setSlot_other _ _ _ _ hne]; exact hinv.activeValid
        have hdActT : (getSlot d (slotOf s.txid)).txid = s.txid := by
          rw [getSlot_setSlot_other _ _ _ _ hne]; exact hinv.activeTxID
        have hdActD : (getSlot d (slotOf s.txid)).dataDurable = true := by
          rw [getSlot_setSlot_other _ _ _ _ hne]; exact hinv.activeDurable
        have hdAltB : (getSlot d (slotOf (s.txid + 1))).valid = true →
            (getSlot d (slotOf (s.txid + 1))).txid ≤ s.txid ∧
            (getSlot d (slotOf (s.txid + 1))).dataDurable = true := by
          rw [getSlot_setSlot_same]; simp
        have hrec : recoverTxID { d with pendingMeta := .none } = s.txid :=
          recoverTxID_eq_of_slots _ s.txid hdActV hdActT (fun hv => (hdAltB hv).1)
        refine {
          activeValid := by simp only [crashAndRecover, applyCrashDisk, hvalM]; rw [hrec]; exact hdActV
          activeTxID := by simp only [crashAndRecover, applyCrashDisk, hvalM]; rw [hrec]; exact hdActT
          activeDurable := by simp only [crashAndRecover, applyCrashDisk, hvalM]; rw [hrec]; exact hdActD
          altBounded := by simp only [crashAndRecover, applyCrashDisk, hvalM]; rw [hrec]; exact hdAltB
          durabilityBound := by simp [crashAndRecover, applyCrashDisk, hvalM]
          syncedPhaseClean := by intro h; rcases h with h | h <;> cases h
          pendingShape := by simp [crashAndRecover, applyCrashDisk, hvalM]
        }

/-- Master inductive invariant theorem: `Inv s` holds for EVERY reachable state. -/
theorem reachable_inv {s : State} (hr : Reachable s) : Inv s := by
  induction hr with
  | init => exact inv_init
  | step _ hstep ih => exact inv_step ih hstep

-- === Main Unbounded Safety Theorems ===

/-- Theorem 1a: At least one durable meta slot is ALWAYS valid (even under unbounded torn crashes). -/
theorem at_least_one_valid_meta {s : State} (hr : Reachable s) :
    s.meta0.valid = true ∨ s.meta1.valid = true := by
  have hact := (reachable_inv hr).activeValid
  unfold getSlot slotOf at hact
  by_cases hp : s.txid % 2 = 0
  · simp [hp] at hact; exact Or.inl hact
  · simp [hp] at hact; exact Or.inr hact

/-- Theorem 1b: Every valid durable meta slot points ONLY to durably synced data pages. -/
theorem all_valid_metas_have_durable_data {s : State} (hr : Reachable s) :
    (s.meta0.valid = true → s.meta0.dataDurable = true) ∧
    (s.meta1.valid = true → s.meta1.dataDurable = true) := by
  have hinv := reachable_inv hr
  have hactD := hinv.activeDurable
  have haltB := hinv.altBounded
  unfold getSlot slotOf at hactD haltB
  by_cases hp : s.txid % 2 = 0
  · have hp1 : (s.txid + 1) % 2 = 1 := by omega
    simp [hp, hp1] at hactD haltB
    exact ⟨fun _ => hactD, fun hv => (haltB hv).2⟩
  · have hp0 : s.txid % 2 = 1 := by omega
    have hp1 : (s.txid + 1) % 2 = 0 := by omega
    simp [hp0, hp1] at hactD haltB
    exact ⟨fun hv => (haltB hv).2, fun _ => hactD⟩

/-- Theorem 1c: No durable meta slot EVER holds a resurrected aborted transaction (`txid > s.txid`). -/
theorem no_resurrected_aborted_meta {s : State} (hr : Reachable s) :
    (s.meta0.valid = true → s.meta0.txid ≤ s.txid) ∧
    (s.meta1.valid = true → s.meta1.txid ≤ s.txid) := by
  have hinv := reachable_inv hr
  have hactT := hinv.activeTxID
  have haltB := hinv.altBounded
  unfold getSlot slotOf at hactT haltB
  by_cases hp : s.txid % 2 = 0
  · have hp1 : (s.txid + 1) % 2 = 1 := by omega
    simp [hp, hp1] at hactT haltB
    exact ⟨fun _ => by omega, fun hv => (haltB hv).1⟩
  · have hp0 : s.txid % 2 = 1 := by omega
    have hp1 : (s.txid + 1) % 2 = 0 := by omega
    simp [hp0, hp1] at hactT haltB
    exact ⟨fun hv => (haltB hv).1, fun _ => by omega⟩

/-- Theorem 1d: Committed TxID never regresses below `lastSyncedTxID` across arbitrary crashes. -/
theorem durability_never_regresses {s : State} (hr : Reachable s) :
    s.txid ≥ s.lastSyncedTxID :=
  (reachable_inv hr).durabilityBound

/-- Theorem 1e: Strict Crash Atomicity — crashing recovers either `s.txid` or (only when
    `metaWritten` or un-neutralized `dirtyAltMeta`) `s.txid + 1`, and NEVER regresses below
    `s.lastSyncedTxID`. -/
theorem crash_atomicity_and_durability {s : State} (hr : Reachable s) (mode : CrashMode) :
    let s' := crashAndRecover s mode
    s'.txid ≥ s.lastSyncedTxID ∧
    (s'.txid = s.txid ∨ s'.txid = s.txid + 1) ∧
    (s.phase ≠ .metaWritten → s.dirtyAltMeta = false → s'.txid = s.txid) := by
  have hinv := reachable_inv hr
  have hne : slotOf (s.txid + 1) ≠ slotOf s.txid := slotOf_succ_ne s.txid
  have hps := hinv.pendingShape
  have hpmCases :
      s.pendingMeta = .none ∨
      s.pendingMeta = .invalidated (slotOf (s.txid + 1)) ∨
      s.pendingMeta = .valid (slotOf (s.txid + 1)) (s.txid + 1) true := by
    cases hp : s.phase <;> cases hd : s.dirtyAltMeta <;> rw [hp, hd] at hps <;> simp at hps
    all_goals first
      | exact Or.inr (Or.inr hps)
      | exact Or.inl hps
      | (rcases hps with h | h; exact Or.inl h; exact Or.inr (Or.inl h))
  have hpmClean : s.phase ≠ .metaWritten → s.dirtyAltMeta = false →
      s.pendingMeta = .none ∨ s.pendingMeta = .invalidated (slotOf (s.txid + 1)) := by
    intro hneM hd
    cases hp : s.phase
    · rw [hp, hd] at hps; exact hps
    · rw [hp, hd] at hps; exact hps
    · rw [hp, hd] at hps; exact hps
    · rw [hp, hd] at hps; exact Or.inl hps
    · exact absurd hp hneM
  have hdur := hinv.durabilityBound
  dsimp only
  cases mode with
  | drop =>
    have hrec : recoverTxID { s with pendingMeta := .none } = s.txid :=
      recoverTxID_eq_of_slots _ s.txid hinv.activeValid hinv.activeTxID (fun hv => (hinv.altBounded hv).1)
    simp [crashAndRecover, applyCrashDisk, hrec, hdur]
  | reordered =>
    rcases hpmCases with hnone | hinvM | hvalM
    · have hrec : recoverTxID s = s.txid :=
        recoverTxID_eq_of_slots _ s.txid hinv.activeValid hinv.activeTxID (fun hv => (hinv.altBounded hv).1)
      simp [crashAndRecover, applyCrashDisk, flushPendingMeta, hnone, hrec, hdur]
    · have hrec := recoverTxID_after_inval_alt s hinv.activeValid hinv.activeTxID
      simp only [crashAndRecover, applyCrashDisk, flushPendingMeta, hinvM]
      rw [hrec]
      exact ⟨hdur, Or.inl rfl, fun _ _ => rfl⟩
    · have hrec := recoverTxID_after_valid_alt s hinv.activeValid hinv.activeTxID
      simp only [crashAndRecover, applyCrashDisk, flushPendingMeta, hvalM]
      rw [hrec]
      refine ⟨by omega, Or.inr rfl, ?_⟩
      intro hneM hd
      rcases hpmClean hneM hd with hc | hc <;> rw [hvalM] at hc <;> cases hc
  | torn =>
    rcases hpmCases with hnone | hinvM | hvalM
    · have hrec : recoverTxID { s with pendingMeta := .none } = s.txid :=
        recoverTxID_eq_of_slots _ s.txid hinv.activeValid hinv.activeTxID (fun hv => (hinv.altBounded hv).1)
      simp [crashAndRecover, applyCrashDisk, hnone, hrec, hdur]
    · have hrec := recoverTxID_after_inval_alt s hinv.activeValid hinv.activeTxID
      simp only [crashAndRecover, applyCrashDisk, hinvM]
      rw [hrec]
      exact ⟨hdur, Or.inl rfl, fun _ _ => rfl⟩
    · have hrec := recoverTxID_after_inval_alt s hinv.activeValid hinv.activeTxID
      simp only [crashAndRecover, applyCrashDisk, hvalM]
      rw [hrec]
      exact ⟨hdur, Or.inl rfl, fun _ _ => rfl⟩

end GroundKV.DualMeta
