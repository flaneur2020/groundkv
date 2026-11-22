#!/usr/bin/env bash
set -euo pipefail

# Negative-control mutation tests for models/storage_protocol/storage_protocol.fizz.
# Ensures formal safety invariants genuinely catch protocol bugs and never drift into tautologies.

FIZZ="${FIZZ:-$(command -v fizz || echo "$HOME/bin/fizzbee-v0.5.4-linux_x86/fizz")}"
SPEC="models/storage_protocol/storage_protocol.fizz"

TMPDIR="$(mktemp -d /tmp/groundkv-mutations-XXXXXX)"
trap 'rm -rf "$TMPDIR"' EXIT

python3 - "$SPEC" "$TMPDIR" <<'PY'
import os, sys

spec_path, out_dir = sys.argv[1], sys.argv[2]
src = open(spec_path).read()

# Mutation 1: Omit disk.Sync() barrier between DATA_WRITTEN and DATA_SYNCED.
m1_target = (
    "        if self.phase == PHASE_DATA_WRITTEN:\n"
    "            if self.tx_dirty_written:\n"
    "                disk.Sync()"
)
m1_replace = (
    "        if self.phase == PHASE_DATA_WRITTEN:\n"
    "            pass"
)
assert m1_target in src, "Mutation 1 target snippet not found in spec"
os.makedirs(os.path.join(out_dir, "m1"), exist_ok=True)
open(os.path.join(out_dir, "m1", "mut1.fizz"), "w").write(src.replace(m1_target, m1_replace, 1))

# Mutation 2: Ignore active MVCC reader txid in freelist release_bound().
m2_target = (
    "        if self.has_reader and self.reader_txid < bound:\n"
    "            bound = self.reader_txid"
)
m2_replace = "        pass"
assert m2_target in src, "Mutation 2 target snippet not found in spec"
os.makedirs(os.path.join(out_dir, "m2"), exist_ok=True)
open(os.path.join(out_dir, "m2", "mut2.fizz"), "w").write(src.replace(m2_target, m2_replace, 1))
PY

check_mutation() {
  local name="$1" file="$2" expected_inv="$3" out="$4"
  echo "==> Running mutation check: ${name} (expecting ${expected_inv} to FAIL)"
  mkdir -p "$out"
  local log="${out}/run.log"
  "$FIZZ" --output-dir "$out" "$file" > "$log" 2>&1 || true
  if grep -q "FAILED: Model checker failed. Invariant:.*${expected_inv}" "$log"; then
    echo "    OK: caught expected invariant violation (${expected_inv})"
  else
    echo "    ERROR: mutation '${name}' was NOT caught by ${expected_inv}!" >&2
    tail -n 30 "$log" >&2
    exit 1
  fi
}

check_mutation "omit-data-sync-barrier" "$TMPDIR/m1/mut1.fizz" "ValidMetaRootsMatchDurablePages" "$TMPDIR/m1/out"
check_mutation "ignore-mvcc-reader-watermark" "$TMPDIR/m2/mut2.fizz" "NoLivePageInFreelist" "$TMPDIR/m2/out"
echo "==> All formal spec mutation checks passed!"
