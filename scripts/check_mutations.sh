#!/usr/bin/env bash
set -euo pipefail

# Negative-control mutation proofs for formal/quint/storage_protocol.qnt.
# Injects 5 real storage-protocol bugs into temporary copies of the Quint spec
# and proves that Quint's invariants catch EVERY single violation.

QUINT="${QUINT:-$(command -v quint || echo "$HOME/.local/bin/quint")}"
QUINT_BACKEND="${QUINT_BACKEND:-typescript}"
SPEC="formal/quint/storage_protocol.qnt"

TMPDIR="$(mktemp -d /tmp/groundkv-quint-mutations-XXXXXX)"
trap 'rm -rf "$TMPDIR"' EXIT

python3 - "$SPEC" "$TMPDIR" <<'PY'
import os, sys

spec_path, out_dir = sys.argv[1], sys.argv[2]
src = open(spec_path).read()

mutations = [
    # Mutation 1: Omit disk.Sync() barrier in syncDataPages
    (
        "m1",
        "    val doSync = txDirtyWritten or txFlWritten",
        "    val doSync = false",
    ),
    # Mutation 2: Ignore active MVCC reader watermark in calcReleaseBound
    (
        "m2",
        "    if (readerOpen and rTx < base) rTx else base",
        "    base",
    ),
    # Mutation 3: Omit InvalidateMeta on failed SyncMetaPage rollback
    (
        "m3",
        "      val nextPM =\n        if (invalOk) {\n          present: true, valid: false, slot: nextTx % 2,\n          txid: 0, root: 0, fl: 0, expected: NOT_FOUND\n        } else pendingMeta",
        "      val nextPM = pendingMeta",
    ),
    # Mutation 4: Omit SyncDataPages dirtyAltMeta fence before fsync
    (
        "m4",
        "    val pmAfterFence =\n      if (dirtyAltMeta) {\n        present: true, valid: false, slot: (txid + 1) % 2,\n        txid: 0, root: 0, fl: 0, expected: NOT_FOUND\n      } else pendingMeta",
        "    val pmAfterFence = pendingMeta",
    ),
    # Mutation 5: Omit releaseFromPending when closing an MVCC reader
    (
        "m5",
        "    val bound = calcReleaseBound(txid, false, -1)\n    val rel = releaseFromPending(freePages, pendingFree, bound)",
        "    val bound = calcReleaseBound(txid, false, -1)\n    val rel = { free: freePages, pending: pendingFree }",
    ),
]

for key, target, repl in mutations:
    assert target in src, f"Mutation {key} target snippet not found in {spec_path}"
    open(os.path.join(out_dir, f"{key}.qnt"), "w").write(src.replace(target, repl, 1))
PY

check_mutation() {
  local name="$1" file="$2" expected_inv="$3"
  echo "==> Running Quint spec mutation check: ${name} (expecting ${expected_inv} to FAIL)"
  local log="${TMPDIR}/${name}.log"
  if "$QUINT" run --backend="$QUINT_BACKEND" \
      --main=storage_protocol \
      --invariant="$expected_inv" \
      --max-samples=3000 \
      --max-steps=60 \
      --seed=0x42 \
      --verbosity=1 \
      "$file" > "$log" 2>&1; then
    echo "    ERROR: mutation '${name}' was NOT caught by ${expected_inv}!" >&2
    tail -n 30 "$log" >&2
    exit 1
  else
    if grep -qiE "(violation|violated|counterexample)" "$log"; then
      echo "    OK: caught expected violation (${expected_inv})"
    else
      echo "    ERROR: '${name}' failed unexpectedly without an invariant violation:" >&2
      tail -n 30 "$log" >&2
      exit 1
    fi
  fi
}

check_mutation "omit-data-sync-barrier" "$TMPDIR/m1.qnt" "inv_valid_meta_roots_match_durable"
check_mutation "ignore-mvcc-reader-watermark" "$TMPDIR/m2.qnt" "inv_no_live_page_in_freelist"
check_mutation "omit-alt-meta-invalidation-on-sync-fail" "$TMPDIR/m3.qnt" "inv_no_resurrected_aborted_meta"
check_mutation "omit-dirty-alt-meta-fence" "$TMPDIR/m4.qnt" "inv_no_resurrected_aborted_meta"
check_mutation "omit-release-on-close-reader" "$TMPDIR/m5.qnt" "inv_release_bound_safe"
echo "==> All 5 Quint formal spec mutation checks passed!"
