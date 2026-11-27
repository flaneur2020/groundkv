#!/usr/bin/env bash
set -euo pipefail

# Go-side engine code mutation proofs.
# Copies the workspace into an isolated temp directory, injects 4 realistic
# storage-engine regressions directly into Go source files, and proves that
# GroundKV's engine tests and DST swarm catch every single one.

ROOT_DIR="$(pwd)"
TMPDIR="$(mktemp -d /tmp/groundkv-code-mutations-XXXXXX)"
trap 'rm -rf "$TMPDIR"' EXIT

setup_copy() {
  local dest="$1"
  mkdir -p "$dest/verify"
  cp "$ROOT_DIR/go.mod" "$ROOT_DIR/go.sum" "$dest/"
  cp -r "$ROOT_DIR/engine" "$dest/"
  cp -r "$ROOT_DIR/verify/dst" "$dest/verify/"
}

run_code_mutation() {
  local name="$1" file="$2" target="$3" repl="$4" test_cmd="$5"
  local work="${TMPDIR}/${name}"
  setup_copy "$work"

  python3 - "${work}/${file}" "$target" "$repl" <<'PY'
import sys
path, target, repl = sys.argv[1], sys.argv[2], sys.argv[3]
src = open(path).read()
assert target in src, f"Mutation target not found in {path}:\n{target}"
open(path, "w").write(src.replace(target, repl, 1))
PY

  echo "==> Running Go code mutation check: ${name} (expecting test suite to FAIL)"
  local log="${work}/test.log"
  if (cd "$work" && eval "$test_cmd") > "$log" 2>&1; then
    echo "    ERROR: code mutation '${name}' was NOT caught by tests!" >&2
    tail -n 30 "$log" >&2
    exit 1
  else
    echo "    OK: caught code mutation (${name})"
  fi
}

# Code Mutation 1: Omit storage.Sync() barrier inside SyncDataPages()
run_code_mutation \
  "omit-sync-data-pages" \
  "engine/tx_commit.go" \
  '	if len(tx.dirty) > 0 && !tx.db.opts.NoSync {
		if err := tx.db.storage.Sync(); err != nil {
			return fmt.Errorf("groundkv: sync data pages: %w", err)
		}
	}' \
  '	// MUTATED: data sync barrier omitted' \
  "go test ./engine -run '^TestMidCommitCrashDurabilityAndWriteReordering$'"

# Code Mutation 2: Ignore minActiveReaderTxID in releaseBoundLocked()
run_code_mutation \
  "ignore-mvcc-reader-watermark" \
  "engine/db.go" \
  '	minR, hasReader := db.minActiveReaderTxIDLocked()
	return db.core.ReleaseBound(db.meta.TxID, hasReader, minR)' \
  '	// MUTATED: MVCC reader watermark ignored
	return db.core.ReleaseBound(db.meta.TxID, false, 0)' \
  "go test ./engine -run '^TestFreelistReclamationAndMVCCReaderBarrier$'"

# Code Mutation 3: Omit alternate meta slot neutralization on Rollback after failed SyncMetaPage
run_code_mutation \
  "omit-rollback-alt-meta-neutralize" \
  "engine/tx_commit.go" \
  '	if tx.writable && tx.phase >= CommitPhaseMetaWritten {
		// WriteMetaPage already wrote nextMeta into the alternate meta slot in
		// the OS page cache before SyncMetaPage failed. Overwrite that slot with
		// an invalid (zeroed) page so a subsequent transaction'\''s file-wide
		// SyncDataPages() fsync cannot resurrect this rolled-back transaction.
		// The active committed slot (db.meta.TxID % 2) remains durable and untouched.
		slot := tx.db.core.MetaSlot(tx.nextMeta.TxID)
		zeros := make([]byte, tx.db.opts.PageSize)
		if _, err := tx.db.storage.WriteAt(zeros, int64(slot)*int64(tx.db.opts.PageSize)); err != nil {
			altMetaDirty = true
		}
	}' \
  '	// MUTATED: alt-meta neutralization on Rollback omitted' \
  "go test ./verify/dst -run '^TestDSTSwarm$' -dst-seeds=14 -dst-steps=200"

# Code Mutation 4: Replace byte-aware splitLeafEntries with naive midpoint split
run_code_mutation \
  "midpoint-only-leaf-split" \
  "engine/gocore/btree.go" \
  'func splitLeafEntries(entries []core.LeafEntry, pageSize, maxKeys int) [][]core.LeafEntry {
	return splitEntries(entries, pageSize, maxKeys, func(e core.LeafEntry) int {
		return core.LeafEntryHeaderSize + len(e.Key) + len(e.Value)
	})
}' \
  'func splitLeafEntries(entries []core.LeafEntry, pageSize, maxKeys int) [][]core.LeafEntry {
	if len(entries) >= 2 {
		mid := len(entries) / 2
		return [][]core.LeafEntry{entries[:mid], entries[mid:]}
	}
	return [][]core.LeafEntry{entries}
}' \
  "go test ./verify/dst -run '^TestDSTSwarm$' -dst-seeds=14 -dst-steps=200"

echo "==> All 4 Go engine code mutation checks passed!"
