#!/usr/bin/env bash
set -euo pipefail

# Builds and audits all unbounded Lean 4 formal proofs under formal/lean/.
# Verifies:
#   1. Full `lake build` succeeds with zero errors across all 4 proof modules.
#   2. Every audited theorem in GroundKV.lean is checked by Lean's kernel via
#      `#print axioms`, and depends STRICTLY on Lean's standard logical axioms
#      {propext, Classical.choice, Quot.sound} (ZERO sorryAx, ZERO custom axioms).

export PATH="$HOME/.elan/bin:$PATH"

if ! command -v lake >/dev/null 2>&1; then
  echo "ERROR: 'lake' (Lean 4) not found on PATH or ~/.elan/bin." >&2
  echo "Install Lean 4 via elan: https://github.com/leanprover/elan" >&2
  exit 1
fi

echo "==> Building & machine-checking Lean 4 proofs ($(lean --version))"
LOG="$(mktemp /tmp/groundkv-lean-XXXXXX.log)"
trap 'rm -f "$LOG"' EXIT

(cd formal/lean && lake build GroundKV.DualMeta GroundKV.FreelistMVCC GroundKV.FreelistSizing GroundKV.BTreeSplit)
# Always run `lake env lean GroundKV.lean` so `#print axioms` output is audited even on warm builds.
(cd formal/lean && lake env lean GroundKV.lean) | tee "$LOG"

python3 - "$LOG" <<'PY'
import re, sys

log = open(sys.argv[1]).read()
allowed = {"propext", "Classical.choice", "Quot.sound"}
expected = [
    "GroundKV.DualMeta.at_least_one_valid_meta",
    "GroundKV.DualMeta.all_valid_metas_have_durable_data",
    "GroundKV.DualMeta.no_resurrected_aborted_meta",
    "GroundKV.DualMeta.durability_never_regresses",
    "GroundKV.DualMeta.crash_atomicity_and_durability",
    "GroundKV.FreelistMVCC.no_live_page_in_free",
    "GroundKV.FreelistSizing.prepareFreelistPages_sufficient",
    "GroundKV.FreelistSizing.prepareFreelistPages_minimal",
    "GroundKV.FreelistSizing.dual_meta_freelist_converges_all_k",
    "GroundKV.FreelistSizing.dual_meta_freelist_diverges_all_k",
    "GroundKV.BTreeSplit.splitEntries_flatten",
    "GroundKV.BTreeSplit.splitEntries_valid_chunks",
    "GroundKV.BTreeSplit.splitEntries_preserves_pairwise_sorted",
]

found = {}
for line in log.splitlines():
    m = re.match(r"^'([^']+)' depends on axioms: \[([^\]]*)\]\s*$", line.strip())
    if m:
        thm = m.group(1)
        axioms = {a.strip() for a in m.group(2).split(",") if a.strip()}
        found[thm] = axioms

for thm in expected:
    if thm not in found:
        sys.exit(f"ERROR: theorem '{thm}' missing from Lean 4 axiom audit!")
    extra = found[thm] - allowed
    if extra:
        sys.exit(f"ERROR: theorem '{thm}' uses forbidden axioms: {sorted(extra)}")

print(f"==> All {len(expected)} unbounded Lean 4 theorems verified with ZERO sorry and ZERO custom axioms!")
PY
