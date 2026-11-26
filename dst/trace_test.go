package dst

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/flaneur2020/groundkv/engine"
)

// TestExportDSTTraces runs representative Swarm Testing profiles with
// engine.JSONLineTracer enabled and exports NDJSON protocol execution traces
// for formal TLA+ Trace Validation (formal/tla/GroundKVTrace.tla).
// When GROUNDKV_TRACE_DIR is unset, it writes to t.TempDir() to smoke-test
// tracing cleanly during regular go test runs.
func TestExportDSTTraces(t *testing.T) {
	outDir := os.Getenv("GROUNDKV_TRACE_DIR")
	if outDir == "" {
		outDir = t.TempDir()
	}

	// Export 7 seeds covering all 7 Swarm profiles (including crashes, MVCC
	// readers, multi-page freelists, 3-way splits, and dirtyAltMeta double faults).
	seeds := []int64{1, 2, 3, 4, 5, 6, 7, 12}
	const steps = 150

	for _, seed := range seeds {
		profile := SwarmProfile(seed)
		path := filepath.Join(outDir, fmt.Sprintf("seed_%02d_%s.ndjson", seed, profile.Name))
		f, err := os.Create(path)
		if err != nil {
			t.Fatalf("create trace file %s: %v", path, err)
		}

		profile.Options.Tracer = engine.NewJSONLineTracer(f)
		res, runErr := RunSimulationWithProfile(seed, profile, steps)
		closeErr := f.Close()
		if runErr != nil {
			t.Fatalf("traced simulation seed=%d (%s) failed: %v", seed, profile.Name, runErr)
		}
		if closeErr != nil {
			t.Fatalf("close trace file %s: %v", path, closeErr)
		}

		fi, err := os.Stat(path)
		if err != nil || fi.Size() == 0 {
			t.Fatalf("expected non-empty trace file %s (stat err=%v)", path, err)
		}
		t.Logf("Exported trace %s (%d bytes, txid=%d, hw=%d, reclaimed=%d)",
			filepath.Base(path), fi.Size(), res.FinalTxID, res.HighWater, res.Reclaimed)
	}
}
