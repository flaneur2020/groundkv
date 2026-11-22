#!/usr/bin/env bash
set -euo pipefail

# Runs a single FizzBee MBT Go test suite against fizzbee-mbt-server + fizzbee-mbt-runner.
# Usage: scripts/run_mbt.sh <suite-name> <go-pkg> <test-func> <states-dir>

if [ "$#" -ne 4 ]; then
  echo "Usage: $0 <suite-name> <go-pkg> <test-func> <states-dir>" >&2
  exit 2
fi

SUITE_NAME="$1"
GO_PKG="$2"
TEST_FUNC="$3"
STATES_DIR="$4"

MBT_SERVER="${FIZZBEE_MBT_SERVER:-fizzbee-mbt-server}"
MBT_RUNNER="${FIZZBEE_MBT_BIN:-fizzbee-mbt-runner}"

for bin in "$MBT_SERVER" "$MBT_RUNNER"; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "ERROR: '$bin' not found in PATH." >&2
    echo "Download from: https://github.com/fizzbee-io/fizzbee-mbt-releases/releases" >&2
    exit 1
  fi
done

if [ ! -d "$STATES_DIR" ]; then
  echo "ERROR: state graph directory '$STATES_DIR' not found. Run 'make check-specs' first." >&2
  exit 1
fi

SERVER_LOG="$(mktemp "/tmp/fizzbee-mbt-${SUITE_NAME}-XXXXXX.log")"
SERVER_PID=""

cleanup() {
  if [ -n "$SERVER_PID" ] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  rm -f "$SERVER_LOG"
}
trap cleanup EXIT

echo "==> Starting fizzbee-mbt-server for ${SUITE_NAME} (${STATES_DIR})"
"$MBT_SERVER" --states_file "$STATES_DIR" > "$SERVER_LOG" 2>&1 &
SERVER_PID=$!

# Wait up to 15s for FizzMo server to listen on :50051
ready=0
for _ in $(seq 1 30); do
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    echo "ERROR: fizzbee-mbt-server exited prematurely:" >&2
    cat "$SERVER_LOG" >&2
    exit 1
  fi
  if grep -q "listening on" "$SERVER_LOG" 2>/dev/null; then
    ready=1
    break
  fi
  sleep 0.5
done

if [ "$ready" -ne 1 ]; then
  echo "ERROR: fizzbee-mbt-server did not become ready in time:" >&2
  cat "$SERVER_LOG" >&2
  exit 1
fi

echo "==> Running FizzBee MBT suite: ${SUITE_NAME} (${GO_PKG} -run ^${TEST_FUNC}$)"
set +e
FIZZBEE_MBT_BIN="$MBT_RUNNER" go test -v -count=1 -run "^${TEST_FUNC}$" "$GO_PKG"
RC=$?
set -e

if [ "$RC" -ne 0 ]; then
  echo "--- fizzbee-mbt-server log tail (${SUITE_NAME}) ---" >&2
  tail -n 60 "$SERVER_LOG" >&2
  exit "$RC"
fi
