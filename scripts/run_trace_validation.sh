#!/usr/bin/env bash
set -euo pipefail

# Runs TLA+ formal model checking of GroundKVProtocol.tla AND TLA+ Trace Validation
# (GroundKVTrace.tla) against real NDJSON execution traces exported from GroundKV's
# Go engine during DST swarm simulations (plus a negative-control corrupt trace).

TLA_TOOLS_DIR="${TLA_TOOLS_DIR:-$HOME/.local/tla-tools}"
TLA_VERSION="${TLA_VERSION:-1.8.0}"
TLA_JAR="${TLA_TOOLS_DIR}/tla2tools.jar"
CM_JAR="${TLA_TOOLS_DIR}/CommunityModules-deps.jar"

mkdir -p "$TLA_TOOLS_DIR"
if [ ! -f "$TLA_JAR" ]; then
  echo "==> Downloading tla2tools.jar (v${TLA_VERSION})"
  curl -fsSL --retry 3 --retry-delay 2 --retry-all-errors \
    "https://github.com/tlaplus/tlaplus/releases/download/v${TLA_VERSION}/tla2tools.jar" -o "${TLA_JAR}.tmp"
  mv "${TLA_JAR}.tmp" "$TLA_JAR"
fi
if [ ! -f "$CM_JAR" ]; then
  echo "==> Downloading CommunityModules-deps.jar"
  curl -fsSL --retry 3 --retry-delay 2 --retry-all-errors \
    "https://github.com/tlaplus/CommunityModules/releases/latest/download/CommunityModules-deps.jar" -o "${CM_JAR}.tmp"
  mv "${CM_JAR}.tmp" "$CM_JAR"
fi

CP="${TLA_JAR}:${CM_JAR}"
WORK_DIR="$(mktemp -d /tmp/groundkv-tla-trace-XXXXXX)"
trap 'rm -rf "$WORK_DIR"' EXIT

echo "==> 1. Model-checking unbounded TLA+ protocol spec (formal/tla/GroundKVProtocol.tla)"
java -XX:+UseParallelGC -cp "$CP" tlc2.TLC \
  -workers auto -cleanup -metadir "${WORK_DIR}/mc-meta" \
  -config formal/tla/GroundKVProtocol_MC.cfg \
  formal/tla/GroundKVProtocol.tla

echo "==> 2. Exporting real Go execution traces from DST swarm seeds"
TRACE_DIR="${WORK_DIR}/traces"
mkdir -p "$TRACE_DIR"
GROUNDKV_TRACE_DIR="$TRACE_DIR" go test -v ./dst -run '^TestExportDSTTraces$'

echo "==> 3. Running TLA+ Trace Validation against every real Go execution trace"
count=0
for trace in "${TRACE_DIR}"/*.ndjson; do
  base="$(basename "$trace")"
  lines="$(wc -l < "$trace")"
  echo "    Validating ${base} (${lines} protocol transitions)..."
  TRACE_PATH="$trace" java -XX:+UseParallelGC -cp "$CP" tlc2.TLC \
    -workers 1 -cleanup -metadir "${WORK_DIR}/meta-${base}" \
    -config formal/tla/GroundKVTrace.cfg \
    formal/tla/GroundKVTrace.tla > "${WORK_DIR}/${base}.log" 2>&1 || {
      echo "    ERROR: TLA+ Trace Validation FAILED on ${base}!" >&2
      tail -n 40 "${WORK_DIR}/${base}.log" >&2
      exit 1
    }
  echo "    OK: ${base} accepted by GroundKVProtocol"
  count=$((count + 1))
done
if [ "$count" -eq 0 ]; then
  echo "ERROR: no traces exported!" >&2
  exit 1
fi

echo "==> 4. Negative-control checks: corrupted traces must be REJECTED by TLA+"
FIRST_TRACE="$(ls "${TRACE_DIR}"/*.ndjson | head -n 1)"
BAD_RESURRECT="${WORK_DIR}/bad_resurrected.ndjson"
BAD_WATERMARK="${WORK_DIR}/bad_watermark.ndjson"
python3 - "$FIRST_TRACE" "$BAD_RESURRECT" "$BAD_WATERMARK" <<'PY'
import json, sys
lines = [json.loads(l) for l in open(sys.argv[1]) if l.strip()]

# Negative control 1: illegal txid jump (resurrected aborted commit).
bad1 = dict(lines[-1])
bad1["event"] = "BeginRead"
bad1["txid"] = bad1["txid"] + 2
with open(sys.argv[2], "w") as f:
    for ev in lines + [bad1]:
        f.write(json.dumps(ev) + "\n")

# Negative control 2: MVCC/freelist watermark violation (pending txid <= release_bound).
bad2 = dict(lines[-1])
bad2["event"] = "BeginRead"
bad2["active_readers"] = bad2["active_readers"] + 1
if bad2["min_reader_txid"] == -1:
    bad2["min_reader_txid"] = bad2["txid"]
bad2["pending_txids"] = [0]
with open(sys.argv[3], "w") as f:
    for ev in lines + [bad2]:
        f.write(json.dumps(ev) + "\n")
PY

for bad_file in "$BAD_RESURRECT" "$BAD_WATERMARK"; do
  bname="$(basename "$bad_file")"
  if TRACE_PATH="$bad_file" java -XX:+UseParallelGC -cp "$CP" tlc2.TLC \
    -workers 1 -cleanup -metadir "${WORK_DIR}/meta-${bname}" \
    -config formal/tla/GroundKVTrace.cfg \
    formal/tla/GroundKVTrace.tla > "${WORK_DIR}/${bname}.log" 2>&1; then
    echo "    ERROR: negative-control trace '${bname}' was NOT rejected by TLA+!" >&2
    exit 1
  else
    echo "    OK: ${bname} rejected by TLA+ Trace Validation"
  fi
done

echo "==> TLA+ model checking & trace validation PASSED (${count} real traces verified)!"
