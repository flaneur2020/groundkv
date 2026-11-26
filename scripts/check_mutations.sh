#!/usr/bin/env bash
set -euo pipefail

# Negative-control mutation tests for models/storage_protocol/storage_protocol.fizz.
# Ensures formal safety AND liveness invariants genuinely catch protocol bugs and never drift into tautologies.

FIZZ="${FIZZ:-$(command -v fizz || echo "$HOME/bin/fizzbee-v0.5.4-linux_x86/fizz")}"
SPEC="models/storage_protocol/storage_protocol.fizz"

TMPDIR="$(mktemp -d /tmp/groundkv-mutations-XXXXXX)"
trap 'rm -rf "$TMPDIR"' EXIT

python3 - "$SPEC" "$TMPDIR" <<'PY'
import os, sys

spec_path, out_dir = sys.argv[1], sys.argv[2]
src = open(spec_path).read()

mutations = [
    # Mutation 1 (Safety): Omit disk.Sync() barrier between DATA_WRITTEN and DATA_SYNCED.
    (
        "m1",
        "            if self.tx_dirty_written or self.tx_fl_written:\n                disk.Sync()",
        "            pass",
    ),
    # Mutation 2 (Safety): Ignore active MVCC reader txid in freelist release_bound().
    (
        "m2",
        "        if self.has_reader and self.reader_txid < bound:\n            bound = self.reader_txid",
        "        pass",
    ),
    # Mutation 3 (Safety): Omit disk.InvalidateMeta() on failed SyncMetaPage rollback.
    (
        "m3",
        "            if inval_ok:\n                disk.InvalidateMeta(next_txid % 2)\n                self.dirty_alt_meta = False",
        "            if inval_ok:\n                self.dirty_alt_meta = False",
    ),
    # Mutation 4 (Safety): Omit SyncDataPages() dirty_alt_meta fence before disk.Sync().
    (
        "m4",
        "            if self.dirty_alt_meta:\n                disk.InvalidateMeta((self.txid + 1) % 2)\n                self.dirty_alt_meta = False",
        "            pass",
    ),
    # Mutation 5 (Liveness): Omit release_eligible() when closing an MVCC reader.
    (
        "m5",
        "        self.reader_expected = NOT_FOUND\n        self.release_eligible()",
        "        self.reader_expected = NOT_FOUND",
    ),
]

for key, target, repl in mutations:
    assert target in src, f"Mutation {key} target snippet not found in {spec_path}"
    sub = os.path.join(out_dir, key)
    os.makedirs(sub, exist_ok=True)
    open(os.path.join(sub, f"{key}.fizz"), "w").write(src.replace(target, repl, 1))
PY

check_mutation() {
  local name="$1" file="$2" expected_inv="$3" out="$4"
  echo "==> Running mutation check: ${name} (expecting ${expected_inv} to FAIL)"
  mkdir -p "$out"
  local log="${out}/run.log"
  "$FIZZ" --output-dir "$out" "$file" > "$log" 2>&1 || true
  if grep -Eq "(FAILED: Model checker failed\. Invariant:.*${expected_inv}|Invariant: ${expected_inv})" "$log"; then
    echo "    OK: caught expected violation (${expected_inv})"
  else
    echo "    ERROR: mutation '${name}' was NOT caught by ${expected_inv}!" >&2
    tail -n 30 "$log" >&2
    exit 1
  fi
}

check_mutation "omit-data-sync-barrier" "$TMPDIR/m1/m1.fizz" "ValidMetaRootsMatchDurablePages" "$TMPDIR/m1/out"
check_mutation "ignore-mvcc-reader-watermark" "$TMPDIR/m2/m2.fizz" "NoLivePageInFreelist" "$TMPDIR/m2/out"
check_mutation "omit-alt-meta-invalidation-on-sync-fail" "$TMPDIR/m3/m3.fizz" "NoResurrectedAbortedMeta" "$TMPDIR/m3/out"
check_mutation "omit-dirty-alt-meta-fence" "$TMPDIR/m4/m4.fizz" "NoResurrectedAbortedMeta" "$TMPDIR/m4/out"
check_mutation "omit-release-on-close-reader" "$TMPDIR/m5/m5.fizz" "EventuallyReclaimsEligibleFreelist" "$TMPDIR/m5/out"
echo "==> All 5 formal spec mutation checks (4 safety + 1 liveness) passed!"
