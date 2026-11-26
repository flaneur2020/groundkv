/-
  GroundKV — Unbounded Machine-Checked Protocol & Algorithm Proofs (Lean 4).

  Imports all 4 proof modules and audits that every core theorem relies ONLY on
  Lean's standard logical kernel (ZERO `sorry`, ZERO custom axioms).
-/
import GroundKV.DualMeta
import GroundKV.FreelistMVCC
import GroundKV.FreelistSizing
import GroundKV.BTreeSplit

-- Axiom audit: CI verifies none of these print `sorryAx` or custom axioms!
#print axioms GroundKV.DualMeta.at_least_one_valid_meta
#print axioms GroundKV.DualMeta.all_valid_metas_have_durable_data
#print axioms GroundKV.DualMeta.no_resurrected_aborted_meta
#print axioms GroundKV.DualMeta.durability_never_regresses
#print axioms GroundKV.DualMeta.crash_atomicity_and_durability
#print axioms GroundKV.FreelistMVCC.no_live_page_in_free
#print axioms GroundKV.FreelistSizing.prepareFreelistPages_sufficient
#print axioms GroundKV.FreelistSizing.prepareFreelistPages_minimal
#print axioms GroundKV.FreelistSizing.dual_meta_freelist_converges_all_k
#print axioms GroundKV.FreelistSizing.dual_meta_freelist_diverges_all_k
#print axioms GroundKV.BTreeSplit.splitEntries_flatten
#print axioms GroundKV.BTreeSplit.splitEntries_valid_chunks
#print axioms GroundKV.BTreeSplit.splitEntries_preserves_pairwise_sorted
