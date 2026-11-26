/-
  GroundKV Unbounded Formal Proof — Module 3: Exact Freelist Chain Sizing
  and Dual-Meta COW Convergence vs. Divergence Theorem.

  Mirrors:
    - engine/tx.go      (prepareFreelistPages exact page-count formula)
    - engine/options.go (MaxFreelistIDs >= 3 convergence requirement)

  Proves for ALL perPage, freeCount, nonReusable, B, k : Nat:
    1. Sufficiency: `prepareFreelistPages` ALWAYS allocates enough freelist
       pages to encode all remaining IDs after popping `min(C, freeCount)` IDs.
    2. Minimality: NO smaller `C' < C` suffices.
    3. Steady-State Convergence (`perPage >= 3`): chain length stays bounded
       by `ceil(2*B / (perPage - 2))` for ALL commit counts `k : Nat`.
    4. Unbounded Divergence (`perPage <= 2, B >= 1`): chain length grows at
       least linearly (`C₀ + k * B`) without bound, proving `MaxFreelistIDs >= 3`
       is mathematically necessary for dual-meta COW convergence!
-/
namespace GroundKV.FreelistSizing

/-- Exact integer ceiling division `ceil(a / b)` for `b > 0`. -/
def ceilDiv (a b : Nat) : Nat := (a + b - 1) / b

theorem ceilDiv_mul_ge {a b : Nat} (hb : b > 0) : b * ceilDiv a b ≥ a := by
  unfold ceilDiv
  have h1 := Nat.div_add_mod (a + b - 1) b
  have h2 := Nat.mod_lt (a + b - 1) hb
  omega

theorem ceilDiv_le_iff {a b c : Nat} (hb : b > 0) : ceilDiv a b ≤ c ↔ a ≤ b * c := by
  unfold ceilDiv
  constructor
  · intro h
    have hmul : b * ((a + b - 1) / b) ≤ b * c := Nat.mul_le_mul_left b h
    have hge : a ≤ b * ((a + b - 1) / b) := ceilDiv_mul_ge hb
    omega
  · intro h
    have hle : a + b - 1 < b * (c + 1) := by
      have : b * (c + 1) = b * c + b := Nat.mul_succ b c
      omega
    have hlt : (a + b - 1) / b < c + 1 := (Nat.div_lt_iff_lt_mul hb).mpr (by rw [Nat.mul_comm]; exact hle)
    omega

/-- GroundKV's exact `prepareFreelistPages` page-count formula (`engine/tx.go`):
    accounts for popping up to `min(C, reusable)` IDs from `localFree + free`. -/
def prepareFreelistPages (perPage reusable nonReusable : Nat) : Nat :=
  let total := reusable + nonReusable
  if total = 0 then 0
  else
    let c1 := (total + perPage) / (perPage + 1)
    let c2 := if nonReusable > 0 then (nonReusable + perPage - 1) / perPage else 0
    Nat.max 1 (Nat.max c1 c2)

/-- Remaining IDs that must be stored on the `C`-page freelist chain after allocating `C` pages. -/
def remainingIDs (reusable nonReusable C : Nat) : Nat :=
  nonReusable + (reusable - C)

/-- Theorem 3a (Sufficiency): `prepareFreelistPages` ALWAYS has enough capacity (`perPage * C`)
    to store every remaining freed/pending/free ID after allocating `C` chain pages. -/
theorem prepareFreelistPages_sufficient
    {perPage reusable nonReusable : Nat} (hp : perPage ≥ 1) :
    let C := prepareFreelistPages perPage reusable nonReusable
    perPage * C ≥ remainingIDs reusable nonReusable C := by
  dsimp only [prepareFreelistPages, remainingIDs]
  by_cases htot : reusable + nonReusable = 0
  · have hr0 : reusable = 0 := by omega
    have hnr0 : nonReusable = 0 := by omega
    simp [hr0, hnr0]
  · simp only [htot, ↓reduceIte]
    let c1 := (reusable + nonReusable + perPage) / (perPage + 1)
    let c2 := if nonReusable > 0 then (nonReusable + perPage - 1) / perPage else 0
    let C := Nat.max 1 (Nat.max c1 c2)
    change perPage * C ≥ nonReusable + (reusable - C)
    have hC1 : c1 ≤ C := Nat.le_trans (Nat.le_max_left c1 c2) (Nat.le_max_right 1 _)
    have hC2 : c2 ≤ C := Nat.le_trans (Nat.le_max_right c1 c2) (Nat.le_max_right 1 _)
    by_cases hrc : C ≤ reusable
    · have hceil : ceilDiv (reusable + nonReusable) (perPage + 1) = c1 := by
        unfold ceilDiv; dsimp only [c1]
        have : reusable + nonReusable + (perPage + 1) - 1 = reusable + nonReusable + perPage := by omega
        rw [this]
      have hge : (perPage + 1) * C ≥ reusable + nonReusable := by
        have h1 : ceilDiv (reusable + nonReusable) (perPage + 1) ≤ C := by rw [hceil]; exact hC1
        exact (ceilDiv_le_iff (by omega)).mp h1
      have hmul : (perPage + 1) * C = perPage * C + C := by
        rw [Nat.add_mul, Nat.one_mul]
      omega
    · have hzero : reusable - C = 0 := by omega
      rw [hzero, Nat.add_zero]
      by_cases hnr : nonReusable > 0
      · have h2 : ceilDiv nonReusable perPage ≤ C := by
          unfold ceilDiv
          simpa [c2, hnr] using hC2
        exact (ceilDiv_le_iff (by omega)).mp h2
      · omega

/-- Theorem 3b (Minimality): `prepareFreelistPages` returns the STRICTLY MINIMAL valid
    non-zero chain length: ANY `C' < C` overflows capacity (when `total > 0`)! -/
theorem prepareFreelistPages_minimal
    {perPage reusable nonReusable C' : Nat}
    (hp : perPage ≥ 1)
    (htot : reusable + nonReusable > 0)
    (hlt : C' < prepareFreelistPages perPage reusable nonReusable) :
    perPage * C' < remainingIDs reusable nonReusable C' := by
  unfold prepareFreelistPages at hlt
  have hne : reusable + nonReusable ≠ 0 := by omega
  simp only [hne, ↓reduceIte] at hlt
  unfold remainingIDs
  let c1 := (reusable + nonReusable + perPage) / (perPage + 1)
  let c2 := if nonReusable > 0 then (nonReusable + perPage - 1) / perPage else 0
  have hlt' : C' < Nat.max 1 (Nat.max c1 c2) := hlt
  have hcases : C' < 1 ∨ C' < c1 ∨ C' < c2 := by
    simp only [Nat.max_def] at hlt'
    split at hlt' <;> split at hlt' <;> omega
  rcases hcases with h0 | h1 | h2
  · have hC0 : C' = 0 := by omega
    subst hC0
    simp
    omega
  · have hceil : ceilDiv (reusable + nonReusable) (perPage + 1) = c1 := by
      unfold ceilDiv; dsimp only [c1]
      have : reusable + nonReusable + (perPage + 1) - 1 = reusable + nonReusable + perPage := by omega
      rw [this]
    have hnot : ¬ (ceilDiv (reusable + nonReusable) (perPage + 1) ≤ C') := by omega
    have hltMul : ¬ (reusable + nonReusable ≤ (perPage + 1) * C') :=
      fun h => hnot ((ceilDiv_le_iff (by omega)).mpr h)
    have hmul : (perPage + 1) * C' = perPage * C' + C' := by
      rw [Nat.add_mul, Nat.one_mul]
    omega
  · by_cases hnr : nonReusable > 0
    · have hnot : ¬ (ceilDiv nonReusable perPage ≤ C') := by
        unfold ceilDiv; simp [c2, hnr] at h2; omega
      have hltMul : ¬ (nonReusable ≤ perPage * C') :=
        fun h => hnot ((ceilDiv_le_iff (by omega)).mpr h)
      omega
    · simp [c2, hnr] at h2

-- === Dual-Meta COW Freelist Steady-State Convergence vs. Divergence ===

/-- Steady-state recurrence for dual-meta COW freelist chain length `C` with
    per-commit B+Tree churn `B`: dual meta holds 2 pending generations
    (`pending[T]` + `tx.freed`), totaling `2 * (B + C)` non-reusable IDs. -/
def nextChainLen (perPage B C : Nat) : Nat :=
  ceilDiv (2 * (B + C)) perPage

def iterChainLen (perPage B : Nat) : Nat → Nat → Nat
  | C0, 0 => C0
  | C0, k + 1 => nextChainLen perPage B (iterChainLen perPage B C0 k)

/-- Theorem 3c (Convergence for `perPage ≥ 3`):
    When `perPage ≥ 3`, `Cmax = ceil(2*B / (perPage - 2))` is an inductive upper bound:
    starting from ANY `C₀ ≤ Cmax`, `iterChainLen perPage B C₀ k ≤ Cmax` for ALL `k : Nat`! -/
theorem dual_meta_freelist_converges_step
    {perPage B C : Nat} (hp : perPage ≥ 3)
    (hC : C ≤ ceilDiv (2 * B) (perPage - 2)) :
    nextChainLen perPage B C ≤ ceilDiv (2 * B) (perPage - 2) := by
  unfold nextChainLen
  let Cmax := ceilDiv (2 * B) (perPage - 2)
  change ceilDiv (2 * (B + C)) perPage ≤ Cmax
  have hCmaxGe : (perPage - 2) * Cmax ≥ 2 * B := ceilDiv_mul_ge (by omega)
  apply (ceilDiv_le_iff (by omega)).mpr
  have hmul : (perPage - 2) * Cmax + 2 * Cmax = perPage * Cmax := by
    have : (perPage - 2) + 2 = perPage := by omega
    calc (perPage - 2) * Cmax + 2 * Cmax
        = ((perPage - 2) + 2) * Cmax := (Nat.add_mul _ _ _).symm
      _ = perPage * Cmax := by rw [this]
  omega

theorem dual_meta_freelist_converges_all_k
    {perPage B C0 : Nat} (hp : perPage ≥ 3)
    (hC0 : C0 ≤ ceilDiv (2 * B) (perPage - 2)) :
    ∀ k : Nat, iterChainLen perPage B C0 k ≤ ceilDiv (2 * B) (perPage - 2) := by
  intro k
  induction k with
  | zero => exact hC0
  | succ k ih => exact dual_meta_freelist_converges_step hp ih

/-- Theorem 3d (Divergence for `perPage ≤ 2`):
    When `perPage ∈ {1, 2}` and `B ≥ 1`, every commit strictly increases chain length
    by at least `B`, diverging unboundedly (`≥ C₀ + k * B`) — mathematically proving why
    `Options.withDefaults()` in `engine/options.go` MUST enforce `MaxFreelistIDs ≥ 3`! -/
theorem dual_meta_freelist_diverges_step
    {perPage B C : Nat} (hpPos : perPage ≥ 1) (hpLe : perPage ≤ 2) :
    nextChainLen perPage B C ≥ C + B := by
  unfold nextChainLen
  have hge : perPage * ceilDiv (2 * (B + C)) perPage ≥ 2 * (B + C) := ceilDiv_mul_ge (by omega)
  have hle2 : perPage * ceilDiv (2 * (B + C)) perPage ≤ 2 * ceilDiv (2 * (B + C)) perPage :=
    Nat.mul_le_mul_right _ hpLe
  omega

theorem dual_meta_freelist_diverges_all_k
    {perPage B C0 : Nat} (hpPos : perPage ≥ 1) (hpLe : perPage ≤ 2) :
    ∀ k : Nat, iterChainLen perPage B C0 k ≥ C0 + k * B := by
  intro k
  induction k with
  | zero => simp [iterChainLen]
  | succ k ih =>
    change nextChainLen perPage B (iterChainLen perPage B C0 k) ≥ C0 + (k + 1) * B
    have hstep := dual_meta_freelist_diverges_step hpPos hpLe (B := B) (C := iterChainLen perPage B C0 k)
    have hmul : (k + 1) * B = k * B + B := Nat.succ_mul k B
    omega

end GroundKV.FreelistSizing
