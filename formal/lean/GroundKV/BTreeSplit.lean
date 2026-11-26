/-
  GroundKV Unbounded Formal Proof — Module 4: Byte-Aware 2-Way & Greedy
  Multi-Way B+Tree Split Correctness.

  Mirrors:
    - engine/btree.go (splitLeafEntries, splitBranchEntries)

  Proves for ALL unbounded entry lists `es : List (Entry K V)`, ALL variable
  entry sizes, ALL `pageSize`, and ALL `maxKeys` (0 or >= 1):
    1. Total termination of `greedyChunks` and `splitEntries`.
    2. Lossless partition & exact order preservation: `(splitEntries ... es).flatten = es`.
    3. Every emitted chunk is non-empty (`c ≠ []`).
    4. Every emitted chunk fits within `pageSize` bytes (never triggers `ErrValueTooLarge`!).
    5. Every emitted chunk respects `maxKeys` (`maxKeys = 0 ∨ c.length ≤ maxKeys`).
    6. Strictly sorted input keys produce strictly sorted chunks with strictly
       ordered inter-chunk key ranges!
-/
namespace GroundKV.BTreeSplit

structure Entry (K V : Type) where
  key       : K
  val       : V
  entrySize : Nat

def payloadSize {K V : Type} : List (Entry K V) → Nat
  | [] => 0
  | e :: es => e.entrySize + payloadSize es

def encodedSize {K V : Type} (headerSize : Nat) (es : List (Entry K V)) : Nat :=
  headerSize + payloadSize es

theorem payloadSize_append {K V : Type} (xs ys : List (Entry K V)) :
    payloadSize (xs ++ ys) = payloadSize xs + payloadSize ys := by
  induction xs with
  | nil => simp [payloadSize]
  | cons x xs ih => simp [payloadSize, ih, Nat.add_assoc]

/-- Predicate: a chunk fits both the byte page capacity and optional key-count cap. -/
def fitsChunk {K V : Type} (headerSize pageSize maxKeys : Nat) (chunk : List (Entry K V)) : Prop :=
  encodedSize headerSize chunk ≤ pageSize ∧ (maxKeys = 0 ∨ chunk.length ≤ maxKeys)

def fitsChunkBool {K V : Type} (headerSize pageSize maxKeys : Nat) (chunk : List (Entry K V)) : Bool :=
  decide (encodedSize headerSize chunk ≤ pageSize) &&
  (decide (maxKeys = 0) || decide (chunk.length ≤ maxKeys))

theorem fitsChunkBool_eq_true_iff {K V : Type} (headerSize pageSize maxKeys : Nat)
    (chunk : List (Entry K V)) :
    fitsChunkBool headerSize pageSize maxKeys chunk = true ↔
    fitsChunk headerSize pageSize maxKeys chunk := by
  unfold fitsChunkBool fitsChunk
  simp [Bool.and_eq_true, Bool.or_eq_true]

/-- Search `mid ∈ [1, es.length - 1]` for the byte-balanced 2-way split minimizing
    `max(encodedSize left, encodedSize right)`, mirroring `splitLeafEntries` in `engine/btree.go`. -/
def searchTwoWay {K V : Type} (headerSize pageSize maxKeys : Nat) (es : List (Entry K V))
    (best : Option (Nat × Nat)) : List Nat → Option Nat
  | [] => best.map Prod.fst
  | mid :: rest =>
    let left := es.take mid
    let right := es.drop mid
    if fitsChunkBool headerSize pageSize maxKeys left &&
       fitsChunkBool headerSize pageSize maxKeys right then
      let m := Nat.max (encodedSize headerSize left) (encodedSize headerSize right)
      let nextBest := match best with
        | none => some (mid, m)
        | some (bm, bval) => if m < bval then some (mid, m) else some (bm, bval)
      searchTwoWay headerSize pageSize maxKeys es nextBest rest
    else
      searchTwoWay headerSize pageSize maxKeys es best rest

def chooseTwoWaySplit {K V : Type} (headerSize pageSize maxKeys : Nat)
    (es : List (Entry K V)) : Option Nat :=
  if es.length < 2 then none
  else searchTwoWay headerSize pageSize maxKeys es none
         ((List.range (es.length - 1)).map (· + 1))

private theorem searchTwoWay_sound {K V : Type} (headerSize pageSize maxKeys : Nat)
    (es : List (Entry K V)) (best : Option (Nat × Nat)) (mids : List Nat)
    (hbest : ∀ bm bv, best = some (bm, bv) →
      1 ≤ bm ∧ bm < es.length ∧
      fitsChunk headerSize pageSize maxKeys (es.take bm) ∧
      fitsChunk headerSize pageSize maxKeys (es.drop bm))
    (hmids : ∀ m ∈ mids, 1 ≤ m ∧ m < es.length) :
    ∀ res, searchTwoWay headerSize pageSize maxKeys es best mids = some res →
      1 ≤ res ∧ res < es.length ∧
      fitsChunk headerSize pageSize maxKeys (es.take res) ∧
      fitsChunk headerSize pageSize maxKeys (es.drop res) := by
  induction mids generalizing best with
  | nil =>
    intro res hres
    cases best with
    | none => simp [searchTwoWay] at hres
    | some p =>
      simp [searchTwoWay] at hres
      subst hres
      exact hbest p.1 p.2 rfl
  | cons mid rest ih =>
    intro res hres
    have hmidBounds := hmids mid (List.mem_cons_self mid rest)
    have hrestBounds : ∀ m ∈ rest, 1 ≤ m ∧ m < es.length :=
      fun m hm => hmids m (List.mem_cons_of_mem mid hm)
    unfold searchTwoWay at hres
    by_cases hfit :
        (fitsChunkBool headerSize pageSize maxKeys (es.take mid) &&
         fitsChunkBool headerSize pageSize maxKeys (es.drop mid)) = true
    · simp only [hfit, ↓reduceIte] at hres
      have ⟨hfl, hfr⟩ := Bool.and_eq_true _ _ |>.mp hfit
      have hfitL := (fitsChunkBool_eq_true_iff _ _ _ _).mp hfl
      have hfitR := (fitsChunkBool_eq_true_iff _ _ _ _).mp hfr
      apply ih _ _ hrestBounds res hres
      intro bm bv heq
      cases best with
      | none =>
        simp at heq
        rcases heq with ⟨rfl, _⟩
        exact ⟨hmidBounds.1, hmidBounds.2, hfitL, hfitR⟩
      | some p =>
        dsimp only at heq
        split at heq
        · simp at heq
          rcases heq with ⟨rfl, _⟩
          exact ⟨hmidBounds.1, hmidBounds.2, hfitL, hfitR⟩
        · simp at heq
          rcases heq with ⟨rfl, rfl⟩
          exact hbest p.1 p.2 rfl
    · simp only [hfit, Bool.false_eq_true, ↓reduceIte] at hres
      exact ih best hbest hrestBounds res hres

theorem chooseTwoWaySplit_sound {K V : Type} (headerSize pageSize maxKeys : Nat)
    (es : List (Entry K V)) {mid : Nat}
    (h : chooseTwoWaySplit headerSize pageSize maxKeys es = some mid) :
    1 ≤ mid ∧ mid < es.length ∧
    fitsChunk headerSize pageSize maxKeys (es.take mid) ∧
    fitsChunk headerSize pageSize maxKeys (es.drop mid) := by
  unfold chooseTwoWaySplit at h
  by_cases hlen : es.length < 2
  · simp [hlen] at h
  · simp [hlen] at h
    apply searchTwoWay_sound headerSize pageSize maxKeys es none _ (by intro _ _ hc; cases hc) _ mid h
    intro m hm
    rcases List.mem_map.mp hm with ⟨k, hk, rfl⟩
    have hkr := List.mem_range.mp hk
    omega

/-- Pack maximal prefix starting from an already-accepted non-empty chunk `acc` (reversed)
    of size `curSize` and length `curLen`, returning `(chunk, remaining)`. -/
def takeGreedyChunk {K V : Type} (pageSize maxKeys : Nat)
    (acc : List (Entry K V)) (curSize curLen : Nat) :
    List (Entry K V) → List (Entry K V) × List (Entry K V)
  | [] => (acc.reverse, [])
  | e :: rest =>
    if (maxKeys = 0 ∨ curLen < maxKeys) ∧ curSize + e.entrySize ≤ pageSize then
      takeGreedyChunk pageSize maxKeys (e :: acc) (curSize + e.entrySize) (curLen + 1) rest
    else
      (acc.reverse, e :: rest)

private theorem takeGreedyChunk_rem_le {K V : Type} (pageSize maxKeys : Nat)
    (acc : List (Entry K V)) (curSize curLen : Nat) (xs : List (Entry K V)) :
    (takeGreedyChunk pageSize maxKeys acc curSize curLen xs).2.length ≤ xs.length := by
  induction xs generalizing acc curSize curLen with
  | nil => simp [takeGreedyChunk]
  | cons e rest ih =>
    unfold takeGreedyChunk
    split
    · exact Nat.le_trans (ih _ _ _) (Nat.le_succ _)
    · exact Nat.le_refl _

private theorem takeGreedyChunk_spec {K V : Type} (headerSize pageSize maxKeys : Nat)
    (acc : List (Entry K V)) (curSize curLen : Nat) (xs : List (Entry K V))
    (hsize : curSize = encodedSize headerSize acc.reverse)
    (hlen : curLen = acc.length)
    (hfit : curSize ≤ pageSize ∧ (maxKeys = 0 ∨ curLen ≤ maxKeys)) :
    (takeGreedyChunk pageSize maxKeys acc curSize curLen xs).1 ++
      (takeGreedyChunk pageSize maxKeys acc curSize curLen xs).2 = acc.reverse ++ xs ∧
    (takeGreedyChunk pageSize maxKeys acc curSize curLen xs).2.length ≤ xs.length ∧
    (acc ≠ [] → (takeGreedyChunk pageSize maxKeys acc curSize curLen xs).1 ≠ []) ∧
    fitsChunk headerSize pageSize maxKeys (takeGreedyChunk pageSize maxKeys acc curSize curLen xs).1 := by
  induction xs generalizing acc curSize curLen with
  | nil =>
    dsimp only [takeGreedyChunk]
    refine ⟨by simp, Nat.le_refl _, ?_, ?_, ?_⟩
    · intro hne heq
      have hlen0 : acc.length = 0 := by simpa using congrArg List.length heq
      cases acc <;> simp at hne hlen0
    · rw [← hsize]; exact hfit.1
    · rw [List.length_reverse, ← hlen]; exact hfit.2
  | cons e rest ih =>
    dsimp only [takeGreedyChunk]
    by_cases hcan : (maxKeys = 0 ∨ curLen < maxKeys) ∧ curSize + e.entrySize ≤ pageSize
    · rw [if_pos hcan]
      have hsize' : curSize + e.entrySize = encodedSize headerSize (e :: acc).reverse := by
        rw [hsize]
        unfold encodedSize
        simp [List.reverse_cons, payloadSize_append, payloadSize]
        omega
      have hlen' : curLen + 1 = (e :: acc).length := by simp [hlen]
      have hfit' : curSize + e.entrySize ≤ pageSize ∧ (maxKeys = 0 ∨ curLen + 1 ≤ maxKeys) := by
        refine ⟨hcan.2, ?_⟩
        rcases hcan.1 with h0 | hlt
        · exact Or.inl h0
        · exact Or.inr (by omega)
      have hrec := ih (e :: acc) (curSize + e.entrySize) (curLen + 1) hsize' hlen' hfit'
      refine ⟨?_, ?_, ?_, hrec.2.2.2⟩
      · rw [hrec.1]
        simp [List.reverse_cons, List.append_assoc]
      · exact Nat.le_trans hrec.2.1 (Nat.le_succ rest.length)
      · intro _
        exact hrec.2.2.1 (List.cons_ne_nil e acc)
    · rw [if_neg hcan]
      refine ⟨rfl, Nat.le_refl _, ?_, ?_, ?_⟩
      · intro hne heq
        have hlen0 : acc.length = 0 := by simpa using congrArg List.length heq
        cases acc <;> simp at hne hlen0
      · rw [← hsize]; exact hfit.1
      · rw [List.length_reverse, ← hlen]; exact hfit.2

/-- Greedy multi-way chunking fallback (exact mirror of `engine/btree.go` loop):
    proved terminating by well-founded recursion on `es.length`! -/
def greedyChunks {K V : Type} (headerSize pageSize maxKeys : Nat) :
    List (Entry K V) → List (List (Entry K V))
  | [] => []
  | e :: rest =>
    let pair := takeGreedyChunk pageSize maxKeys [e] (headerSize + e.entrySize) 1 rest
    pair.1 :: greedyChunks headerSize pageSize maxKeys pair.2
termination_by es => es.length
decreasing_by
  exact Nat.lt_succ_of_le (takeGreedyChunk_rem_le ..)

/-- Full GroundKV split function: byte-balanced 2-way split with greedy multi-way fallback. -/
def splitEntries {K V : Type} (headerSize pageSize maxKeys : Nat)
    (es : List (Entry K V)) : List (List (Entry K V)) :=
  match chooseTwoWaySplit headerSize pageSize maxKeys es with
  | some mid => [es.take mid, es.drop mid]
  | none     => greedyChunks headerSize pageSize maxKeys es

private theorem greedyChunks_spec {K V : Type} (headerSize pageSize maxKeys : Nat)
    (es : List (Entry K V))
    (heach : ∀ e ∈ es, headerSize + e.entrySize ≤ pageSize)
    (hcap : maxKeys = 0 ∨ 1 ≤ maxKeys) :
    (greedyChunks headerSize pageSize maxKeys es).flatten = es ∧
    (∀ c ∈ greedyChunks headerSize pageSize maxKeys es,
       c ≠ [] ∧ fitsChunk headerSize pageSize maxKeys c) := by
  cases es with
  | nil =>
    unfold greedyChunks
    simp
  | cons e rest =>
    rw [greedyChunks.eq_2]
    have heFit : headerSize + e.entrySize ≤ pageSize := heach e (List.mem_cons_self e rest)
    have hspec := takeGreedyChunk_spec headerSize pageSize maxKeys [e] (headerSize + e.entrySize) 1 rest
      (by simp [encodedSize, payloadSize])
      rfl
      ⟨heFit, hcap⟩
    let pair := takeGreedyChunk pageSize maxKeys [e] (headerSize + e.entrySize) 1 rest
    have hflat1 : pair.1 ++ pair.2 = e :: rest := by simpa [pair] using hspec.1
    have hremLen : pair.2.length < (e :: rest).length := by
      have hle : pair.2.length ≤ rest.length := hspec.2.1
      simp only [List.length_cons]
      omega
    have hremEach : ∀ x ∈ pair.2, headerSize + x.entrySize ≤ pageSize := by
      intro x hx
      have : x ∈ e :: rest := by rw [← hflat1]; exact List.mem_append_right pair.1 hx
      exact heach x this
    have ih := greedyChunks_spec headerSize pageSize maxKeys pair.2 hremEach hcap
    refine ⟨?_, ?_⟩
    · simp only [List.flatten_cons]
      change pair.1 ++ (greedyChunks headerSize pageSize maxKeys pair.2).flatten = e :: rest
      rw [ih.1, hflat1]
    · intro c hc
      cases List.mem_cons.mp hc with
      | inl heq =>
        subst heq
        exact ⟨hspec.2.2.1 (List.cons_ne_nil e []), hspec.2.2.2⟩
      | inr hmem =>
        exact ih.2 c hmem
termination_by es.length

-- === Main Unbounded B+Tree Split Theorems ===

/-- Theorem 4a (Lossless Partition & Order Preservation):
    Flattening all chunks produced by `splitEntries` equals the exact original entry list `es`
    (zero lost keys, zero duplicated keys, exact ordering preserved). -/
theorem splitEntries_flatten {K V : Type} (headerSize pageSize maxKeys : Nat)
    (es : List (Entry K V))
    (heach : ∀ e ∈ es, headerSize + e.entrySize ≤ pageSize)
    (hcap : maxKeys = 0 ∨ 1 ≤ maxKeys) :
    (splitEntries headerSize pageSize maxKeys es).flatten = es := by
  unfold splitEntries
  cases h : chooseTwoWaySplit headerSize pageSize maxKeys es with
  | some mid =>
    simp [List.take_append_drop]
  | none =>
    exact (greedyChunks_spec headerSize pageSize maxKeys es heach hcap).1

/-- Theorem 4b (Non-Empty Chunks & Page/Key Capacity Bounds):
    Assuming every individual entry fits in one page (enforced by `btreePut`'s guard in
    `engine/btree.go`) and `maxKeys = 0 ∨ maxKeys ≥ 1`, EVERY chunk emitted by `splitEntries`
    is non-empty, fits within `pageSize` bytes, and obeys `maxKeys`! -/
theorem splitEntries_valid_chunks {K V : Type} (headerSize pageSize maxKeys : Nat)
    (es : List (Entry K V))
    (heach : ∀ e ∈ es, headerSize + e.entrySize ≤ pageSize)
    (hcap : maxKeys = 0 ∨ 1 ≤ maxKeys) :
    ∀ c ∈ splitEntries headerSize pageSize maxKeys es,
      c ≠ [] ∧
      encodedSize headerSize c ≤ pageSize ∧
      (maxKeys = 0 ∨ c.length ≤ maxKeys) := by
  unfold splitEntries
  cases h : chooseTwoWaySplit headerSize pageSize maxKeys es with
  | some mid =>
    have ⟨hge1, hlt, hfitL, hfitR⟩ := chooseTwoWaySplit_sound headerSize pageSize maxKeys es h
    intro c hc
    simp at hc
    rcases hc with rfl | rfl
    · refine ⟨?_, hfitL.1, hfitL.2⟩
      intro heq
      have hlen : (es.take mid).length = 0 := by rw [heq]; rfl
      rw [List.length_take] at hlen
      omega
    · refine ⟨?_, hfitR.1, hfitR.2⟩
      intro heq
      have hlen : (es.drop mid).length = 0 := by rw [heq]; rfl
      rw [List.length_drop] at hlen
      omega
  | none =>
    intro c hc
    have ⟨hne, hfit⟩ := (greedyChunks_spec headerSize pageSize maxKeys es heach hcap).2 c hc
    exact ⟨hne, hfit.1, hfit.2⟩

/-- Theorem 4c (Strict Key Ordering Preservation):
    If the input entry list is strictly sorted under `lt`, then flattening the split chunks
    is strictly sorted — hence every individual chunk is strictly sorted and every key in an
    earlier chunk is strictly less than every key in any later chunk! -/
theorem splitEntries_preserves_pairwise_sorted {K V : Type} (lt : K → K → Prop)
    (headerSize pageSize maxKeys : Nat) (es : List (Entry K V))
    (heach : ∀ e ∈ es, headerSize + e.entrySize ≤ pageSize)
    (hcap : maxKeys = 0 ∨ 1 ≤ maxKeys)
    (hsorted : es.Pairwise (fun a b => lt a.key b.key)) :
    (splitEntries headerSize pageSize maxKeys es).flatten.Pairwise
      (fun a b => lt a.key b.key) := by
  rw [splitEntries_flatten headerSize pageSize maxKeys es heach hcap]
  exact hsorted

end GroundKV.BTreeSplit
