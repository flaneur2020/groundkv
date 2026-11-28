#!/usr/bin/env bash
set -euo pipefail

# Runs Quint formal checking of formal/quint/trace_protocol.qnt AND Quint Trace
# Validation against real NDJSON execution traces exported from GroundKV's Go
# engine during DST swarm simulations (plus 2 negative-control corrupt traces).

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
QUINT="${QUINT:-$(command -v quint || echo "$HOME/.local/bin/quint")}"
QUINT_BACKEND="${QUINT_BACKEND:-typescript}"
SPEC="${ROOT_DIR}/formal/quint/trace_protocol.qnt"

WORK_DIR="$(mktemp -d /tmp/groundkv-quint-trace-XXXXXX)"
trap 'rm -rf "$WORK_DIR"' EXIT

echo "==> 1. Typechecking & simulating formal/quint/trace_protocol.qnt"
"$QUINT" typecheck "$SPEC"
"$QUINT" run --backend="$QUINT_BACKEND" \
  --invariant=inv_all \
  --max-samples=500 \
  --max-steps=25 \
  --seed=0x2026 \
  --verbosity=1 \
  "$SPEC"

echo "==> 2. Exporting real Go execution traces from DST swarm seeds"
TRACE_DIR="${WORK_DIR}/traces"
mkdir -p "$TRACE_DIR"
GROUNDKV_TRACE_DIR="$TRACE_DIR" go test -v ./verify/dst -run '^TestExportDSTTraces$'

# Copy trace_protocol.qnt into WORK_DIR so generated trace modules can import it cleanly.
cp "$SPEC" "${WORK_DIR}/trace_protocol.qnt"

generate_trace_qnt() {
  local ndjson_path="$1"
  local out_qnt="$2"
  python3 - "$ndjson_path" "$out_qnt" <<'PY'
import json, sys

ndjson_path, out_qnt = sys.argv[1], sys.argv[2]
events = [json.loads(line) for line in open(ndjson_path) if line.strip()]
assert len(events) >= 1, f"empty trace: {ndjson_path}"

def fmt_obs(ev):
    pending = ", ".join(str(x) for x in ev.get("pending_txids", []))
    dirty = "true" if ev["dirty_alt_meta"] else "false"
    return (
        f'{{ event: "{ev["event"]}", '
        f'txid: {ev["txid"]}, '
        f'phase: "{ev["phase"]}", '
        f'root: {ev["root"]}, '
        f'freelist: {ev["freelist"]}, '
        f'highWater: {ev["high_water"]}, '
        f'dirtyAltMeta: {dirty}, '
        f'activeReaders: {ev["active_readers"]}, '
        f'minReaderTxID: {ev["min_reader_txid"]}, '
        f'pendingTxIDs: Set({pending}), '
        f'releaseBound: {ev["release_bound"]} }}'
    )

lines = [
    "module trace_check {",
    '  import trace_protocol.* from "./trace_protocol"',
    "",
    "  var traceIdx: int",
    "",
    "  pure val traceEvents: List[TraceObs] = [",
]
for ev in events:
    lines.append(f"    {fmt_obs(ev)},")
lines += [
    "  ]",
    "",
    "  action initCursor = all {",
    "    traceInit(traceEvents[0]),",
    "    traceIdx' = 1,",
    "  }",
    "",
    "  action stepCursor = all {",
    "    traceIdx < traceEvents.length(),",
    "    traceDispatch(traceEvents[traceIdx]),",
    "    traceIdx' = traceIdx + 1,",
    "  }",
    "",
    "  run validateTraceTest = {",
    "    initCursor",
    "      .expect(matchesObserved(traceEvents[0]))",
    f"      .then(reps({len(events) - 1}, _ => stepCursor.expect(matchesObserved(traceEvents[traceIdx - 1]))))",
    "      .expect(traceIdx == traceEvents.length())",
    "  }",
    "}",
]

with open(out_qnt, "w") as f:
    f.write("\n".join(lines) + "\n")
PY
}

echo "==> 3. Running Quint Trace Validation against every real Go execution trace"
count=0
for trace in "${TRACE_DIR}"/*.ndjson; do
  base="$(basename "$trace")"
  lines="$(wc -l < "$trace")"
  qnt_file="${WORK_DIR}/check_${count}.qnt"
  generate_trace_qnt "$trace" "$qnt_file"
  echo "    Validating ${base} (${lines} protocol transitions)..."
  "$QUINT" test --backend="$QUINT_BACKEND" --max-samples=1 "$qnt_file" > "${WORK_DIR}/${base}.log" 2>&1 || {
    echo "    ERROR: Quint Trace Validation FAILED on ${base}!" >&2
    tail -n 40 "${WORK_DIR}/${base}.log" >&2
    exit 1
  }
  echo "    OK: ${base} accepted by trace_protocol.qnt"
  count=$((count + 1))
done
if [ "$count" -eq 0 ]; then
  echo "ERROR: no traces exported!" >&2
  exit 1
fi

echo "==> 4. Negative-control checks: corrupted traces must be REJECTED by Quint"
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
  bad_qnt="${WORK_DIR}/check_${bname}.qnt"
  generate_trace_qnt "$bad_file" "$bad_qnt"
  if "$QUINT" test --backend="$QUINT_BACKEND" --max-samples=1 "$bad_qnt" > "${WORK_DIR}/${bname}.log" 2>&1; then
    echo "    ERROR: negative-control trace '${bname}' was NOT rejected by Quint!" >&2
    exit 1
  else
    echo "    OK: ${bname} rejected by Quint Trace Validation"
  fi
done

echo "==> Quint protocol simulation & trace validation PASSED (${count} real traces verified)!"
