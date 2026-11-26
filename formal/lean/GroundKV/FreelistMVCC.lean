/-
  GroundKV Unbounded Formal Proof — Module 2: Unbounded Multi-Reader MVCC
  Freelist Reclamation Safety (Zero Use-After-Free).

  Mirrors:
    - engine/db.go       (releaseBoundLocked, minActiveReaderTxIDLocked)
    - engine/freelist.go (addPending, release, pop, returnToFree)
    - engine/tx.go       (SyncMetaPage, Rollback)

  Proves for ALL txid : Nat, ALL concurrent reader multisets `readers : List Nat`
  (arbitrary count!), and ALL pages `p : Nat`, that NO page live in:
    1. the current committed meta slot (`txid`),
    2. the previous dual-meta slot (`txid - 1`), or
    3. ANY open MVCC read snapshot (`r ∈ readers`)
  can EVER enter `free` or be recycled!
-/
namespace GroundKV.FreelistMVCC

abbrev PageID := Nat

/-- Exact fold computing `min(default, min(readers))`. -/
def minList (default : Nat) : List Nat → Nat
  | [] => default
  | r :: rs => Nat.min r (minList default rs)

theorem minList_le_default (d : Nat) (rs : List Nat) : minList d rs ≤ d := by
  induction rs with
  | nil => exact Nat.le_refl d
  | cons r rs ih =>
    unfold minList
    exact Nat.le_trans (Nat.min_le_right r _) ih

theorem minList_le_mem (d : Nat) (rs : List Nat) {r : Nat} (h : r ∈ rs) :
    minList d rs ≤ r := by
  induction rs with
  | nil => cases h
  | cons x xs ih =>
    unfold minList
    cases List.mem_cons.mp h with
    | inl heq =>
      rw [heq]
      exact Nat.min_le_left x _
    | inr hmem =>
      exact Nat.le_trans (Nat.min_le_right x _) (ih hmem)

/-- GroundKV's exact MVCC + dual-meta freelist reclamation bound (`engine/db.go:releaseBoundLocked`):
    `releaseBound(txid, readers) = min(max(0, txid - 1), minActiveReaderTxID)`
    (In Lean `Nat`, `0 - 1 = 0`, matching Go's `if txid > 0 { bound = txid - 1 }`). -/
def releaseBound (txid : Nat) (readers : List Nat) : Nat :=
  minList (txid - 1) readers

theorem releaseBound_le_prev_tx (txid : Nat) (readers : List Nat) :
    releaseBound txid readers ≤ txid - 1 :=
  minList_le_default (txid - 1) readers

theorem releaseBound_le_reader (txid : Nat) (readers : List Nat) {r : Nat} (hr : r ∈ readers) :
    releaseBound txid readers ≤ r :=
  minList_le_mem (txid - 1) readers hr

/-- A page `p` is live at snapshot `snap` when it has not yet been retired at or before `snap`:
    if `retired p = some d`, commit `d` replaced `p`, so `p` was live only for snapshots `< d`. -/
def liveAt (retired : PageID → Option Nat) (p : PageID) (snap : Nat) : Prop :=
  ∀ d : Nat, retired p = some d → snap < d

/-- A page `p` is protected in `(txid, readers)` if ANY durable meta slot (`txid` or `txid - 1`)
    or ANY active MVCC reader (`r ∈ readers`) still observes `p` as live. -/
def isProtected (retired : PageID → Option Nat) (txid : Nat) (readers : List Nat) (p : PageID) : Prop :=
  liveAt retired p txid ∨
  liveAt retired p (txid - 1) ∨
  (∃ r ∈ readers, liveAt retired p r)

/-- Key Watermark Safety Lemma: any page retired by a commit `d` with `1 ≤ d ≤ releaseBound txid readers`
    is GUARANTEED NOT live in `txid`, NOT live in `txid - 1`, and NOT live in ANY open reader `r ∈ readers`! -/
theorem retired_le_releaseBound_not_protected
    {retired : PageID → Option Nat} {txid d : Nat} {readers : List Nat} {p : PageID}
    (hdPos : d ≥ 1)
    (hret : retired p = some d)
    (hbound : d ≤ releaseBound txid readers) :
    ¬ isProtected retired txid readers p := by
  have hprev : d ≤ txid - 1 := Nat.le_trans hbound (releaseBound_le_prev_tx txid readers)
  have hcur : d ≤ txid := by omega
  intro hprot
  rcases hprot with hliveCur | hlivePrev | ⟨r, hrMem, hliveR⟩
  · have hlt := hliveCur d hret
    omega
  · have hlt := hlivePrev d hret
    omega
  · have hleR : d ≤ r := Nat.le_trans hbound (releaseBound_le_reader txid readers hrMem)
    have hlt := hliveR d hret
    omega

/-- Unbounded state of GroundKV's MVCC reader-aware freelist allocator. -/
structure State where
  txid    : Nat
  readers : List Nat
  retired : PageID → Option Nat
  pending : List (Nat × PageID)
  free    : List PageID

def initState : State where
  txid    := 0
  readers := []
  retired := fun _ => none
  pending := []
  free    := []

/-- Release all pending `(d, p)` entries with `d ≤ bound` into `free`. -/
def releasePending (bound : Nat) (pending : List (Nat × PageID)) (free : List PageID) :
    List (Nat × PageID) × List PageID :=
  let released := (pending.filter (fun e => decide (e.1 ≤ bound))).map Prod.snd
  let remaining := pending.filter (fun e => decide (¬ (e.1 ≤ bound)))
  (remaining, released ++ free)

def closeReaderState (s : State) (readers' : List Nat) : State :=
  let bound := releaseBound s.txid readers'
  let pair := releasePending bound s.pending s.free
  { s with readers := readers', pending := pair.1, free := pair.2 }

def commitState (s : State) (freedPages : List PageID) : State :=
  let nextTx := s.txid + 1
  let retired' : PageID → Option Nat := fun p =>
    if p ∈ freedPages then some nextTx else s.retired p
  let pendingWithNew := (freedPages.map (fun p => (nextTx, p))) ++ s.pending
  let bound := releaseBound nextTx s.readers
  let pair := releasePending bound pendingWithNew s.free
  { s with txid := nextTx, retired := retired', pending := pair.1, free := pair.2 }

/-- Transitions of the unbounded MVCC freelist protocol. -/
inductive Step : State → State → Prop where
  /-- Open a new concurrent MVCC reader pinned at the current committed `txid`. -/
  | openReader {s : State} :
      Step s { s with readers := s.txid :: s.readers }

  /-- Close any active MVCC reader (removing it from `readers`) and release newly eligible pages. -/
  | closeReader {s : State} (readers' : List Nat) :
      (∀ r ∈ readers', r ∈ s.readers) →
      Step s (closeReaderState s readers')

  /-- Commit `txid + 1`: retire a finite batch of pages `freedPages` at `d = txid + 1`
      (none of which were previously retired), append them to `pending`, and release
      any buckets `≤ releaseBound (txid + 1) readers`. -/
  | commit {s : State} (freedPages : List PageID) :
      (∀ p ∈ freedPages, s.retired p = none) →
      Step s (commitState s freedPages)

  /-- Recycle a page `p ∈ s.free` for a new COW allocation. -/
  | popFree {s : State} (p : PageID) (free' : List PageID) :
      p ∈ s.free →
      (∀ q ∈ free', q ∈ s.free) →
      Step s { s with free := free' }

/-- Unbounded reachability from `initState`. -/
inductive Reachable : State → Prop where
  | init : Reachable initState
  | step {s s' : State} : Reachable s → Step s s' → Reachable s'

/-- Inductive invariant over unbounded commits, readers, pending buckets, and free pages. -/
structure Inv (s : State) : Prop where
  readersBounded : ∀ r ∈ s.readers, r ≤ s.txid
  pendingSound   : ∀ e ∈ s.pending, 1 ≤ e.1 ∧ s.retired e.2 = some e.1
  freeSound      : ∀ p ∈ s.free, ∃ d : Nat, 1 ≤ d ∧ s.retired p = some d ∧
                     d ≤ releaseBound s.txid s.readers

private theorem inv_init : Inv initState where
  readersBounded := by intro _ h; cases h
  pendingSound   := by intro _ h; cases h
  freeSound      := by intro _ h; cases h

private theorem minList_mono_default {d1 d2 : Nat} (hd : d1 ≤ d2) (rs : List Nat) :
    minList d1 rs ≤ minList d2 rs := by
  induction rs with
  | nil => exact hd
  | cons r xs ih =>
    unfold minList
    simp only [Nat.min_def]
    split <;> split <;> omega

private theorem releaseBound_mono_tx {txid : Nat} {readers : List Nat} :
    releaseBound txid readers ≤ releaseBound (txid + 1) readers :=
  minList_mono_default (by omega) readers

private theorem mem_releasePending_rem {bound : Nat} {pending : List (Nat × PageID)}
    {free : List PageID} {e : Nat × PageID}
    (h : e ∈ (releasePending bound pending free).1) : e ∈ pending := by
  unfold releasePending at h
  exact (List.mem_filter.mp h).1

private theorem mem_releasePending_free {bound : Nat} {pending : List (Nat × PageID)}
    {free : List PageID} {p : PageID}
    (h : p ∈ (releasePending bound pending free).2) :
    (∃ d : Nat, (d, p) ∈ pending ∧ d ≤ bound) ∨ p ∈ free := by
  unfold releasePending at h
  rcases List.mem_append.mp h with hrel | hfr
  · rcases List.mem_map.mp hrel with ⟨⟨d, q⟩, hfilt, rfl⟩
    have ⟨hmem, hdec⟩ := List.mem_filter.mp hfilt
    exact Or.inl ⟨d, hmem, of_decide_eq_true hdec⟩
  · exact Or.inr hfr

private theorem releaseBound_mono_readers {txid : Nat} {rs rs' : List Nat}
    (hsub : ∀ r ∈ rs', r ∈ rs) :
    releaseBound txid rs ≤ releaseBound txid rs' := by
  unfold releaseBound
  induction rs' with
  | nil => exact minList_le_default (txid - 1) rs
  | cons r xs ih =>
    change minList (txid - 1) rs ≤ Nat.min r (minList (txid - 1) xs)
    have hr : minList (txid - 1) rs ≤ r :=
      minList_le_mem (txid - 1) rs (hsub r (List.mem_cons_self r xs))
    have hxs : minList (txid - 1) rs ≤ minList (txid - 1) xs :=
      ih (fun x hx => hsub x (List.mem_cons_of_mem r hx))
    exact Nat.le_min.mpr ⟨hr, hxs⟩

private theorem releaseBound_cons_txid {txid : Nat} {readers : List Nat} :
    releaseBound txid (txid :: readers) = releaseBound txid readers := by
  unfold releaseBound
  change Nat.min txid (minList (txid - 1) readers) = minList (txid - 1) readers
  have hle : minList (txid - 1) readers ≤ txid - 1 := minList_le_default (txid - 1) readers
  exact Nat.min_eq_right (by omega)

private theorem inv_step {s s' : State} (hinv : Inv s) (hstep : Step s s') : Inv s' := by
  cases hstep with
  | openReader =>
    refine {
      readersBounded := by
        intro r hr
        cases List.mem_cons.mp hr with
        | inl heq => rw [heq]; exact Nat.le_refl _
        | inr hmem => exact hinv.readersBounded r hmem
      pendingSound := hinv.pendingSound
      freeSound := by
        intro p hp
        obtain ⟨d, hd1, hret, hbd⟩ := hinv.freeSound p hp
        change ∃ d, 1 ≤ d ∧ s.retired p = some d ∧ d ≤ releaseBound s.txid (s.txid :: s.readers)
        rw [releaseBound_cons_txid]
        exact ⟨d, hd1, hret, hbd⟩
    }

  | closeReader readers' hsub =>
    have hmono : releaseBound s.txid s.readers ≤ releaseBound s.txid readers' :=
      releaseBound_mono_readers hsub
    refine {
      readersBounded := fun r hr => hinv.readersBounded r (hsub r hr)
      pendingSound := fun e he => hinv.pendingSound e (mem_releasePending_rem (free := s.free) he)
      freeSound := by
        intro p hp
        rcases mem_releasePending_free hp with ⟨d, hmem, hle⟩ | hmemFree
        · obtain ⟨hd1, hret⟩ := hinv.pendingSound (d, p) hmem
          exact ⟨d, hd1, hret, hle⟩
        · obtain ⟨d, hd1, hret, hbd⟩ := hinv.freeSound p hmemFree
          exact ⟨d, hd1, hret, Nat.le_trans hbd hmono⟩
    }

  | commit freedPages hfresh =>
    have hmono : releaseBound s.txid s.readers ≤ releaseBound (s.txid + 1) s.readers :=
      releaseBound_mono_tx
    have hretPres : ∀ p d, s.retired p = some d →
        (if p ∈ freedPages then some (s.txid + 1) else s.retired p) = some d := by
      intro p d hret
      by_cases hmem : p ∈ freedPages
      · have hnone := hfresh p hmem
        rw [hnone] at hret
        cases hret
      · simp [hmem, hret]
    have hpendAll : ∀ e ∈ (freedPages.map (fun p => (s.txid + 1, p))) ++ s.pending,
        1 ≤ e.1 ∧ (if e.2 ∈ freedPages then some (s.txid + 1) else s.retired e.2) = some e.1 := by
      intro e he
      rcases List.mem_append.mp he with hnew | hold
      · rcases List.mem_map.mp hnew with ⟨p, hpMem, rfl⟩
        exact ⟨by omega, by simp [hpMem]⟩
      · obtain ⟨hd1, hret⟩ := hinv.pendingSound e hold
        exact ⟨hd1, hretPres e.2 e.1 hret⟩
    refine {
      readersBounded := fun r hr => Nat.le_trans (hinv.readersBounded r hr) (Nat.le_succ s.txid)
      pendingSound := fun e he => hpendAll e (mem_releasePending_rem (free := s.free) he)
      freeSound := by
        intro p hp
        rcases mem_releasePending_free hp with ⟨d, hmem, hle⟩ | hmemFree
        · obtain ⟨hd1, hret⟩ := hpendAll (d, p) hmem
          exact ⟨d, hd1, hret, hle⟩
        · obtain ⟨d, hd1, hret, hbd⟩ := hinv.freeSound p hmemFree
          exact ⟨d, hd1, hretPres p d hret, Nat.le_trans hbd hmono⟩
    }

  | popFree _ free' _ hsub =>
    refine {
      readersBounded := hinv.readersBounded
      pendingSound := hinv.pendingSound
      freeSound := fun p hp => hinv.freeSound p (hsub p hp)
    }

/-- Master inductive invariant theorem for the unbounded MVCC freelist. -/
theorem reachable_inv {s : State} (hr : Reachable s) : Inv s := by
  induction hr with
  | init => exact inv_init
  | step _ hstep ih => exact inv_step ih hstep

-- === Main Unbounded MVCC Freelist Safety Theorem ===

/-- Theorem 2: ZERO Use-After-Free — across arbitrary TxIDs, arbitrary commits,
    and arbitrary numbers of concurrent MVCC readers, NO page in `s.free` is
    live in the current meta slot (`s.txid`), the previous meta slot (`s.txid - 1`),
    or ANY open MVCC read snapshot (`r ∈ s.readers`)! -/
theorem no_live_page_in_free {s : State} (hr : Reachable s) {p : PageID} (hp : p ∈ s.free) :
    ¬ isProtected s.retired s.txid s.readers p := by
  obtain ⟨d, hd1, hret, hbd⟩ := (reachable_inv hr).freeSound p hp
  exact retired_le_releaseBound_not_protected hd1 hret hbd

end GroundKV.FreelistMVCC
