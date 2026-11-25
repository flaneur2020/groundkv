package dst

import (
	"flag"
	"fmt"
	"testing"

	"github.com/flaneur2020/groundkv/engine"
)

var (
	dstSeedFlag  = flag.Int64("dst-seed", 0, "Specific seed to replay in TestDSTReplay")
	dstStepsFlag = flag.Int("dst-steps", 300, "Number of simulation steps per seed")
	dstSeedsFlag = flag.Int("dst-seeds", 25, "Number of swarm seeds to run in TestDSTSwarm")
)

// TestDSTSwarm runs a deterministic Swarm Testing campaign across varied
// storage profiles, asserting zero invariant violations, exact seed
// reproducibility, and 100% hit rate across all 16 required "sometimes" events.
func TestDSTSwarm(t *testing.T) {
	agg := NewCoverageTracker()

	var firstDigest string
	for s := int64(1); s <= int64(*dstSeedsFlag); s++ {
		res, err := RunSimulation(s, *dstStepsFlag)
		if err != nil {
			t.Fatalf("DST failure: %v\nReplay with: go test -v ./dst -run TestDSTReplay -dst-seed=%d -dst-steps=%d",
				err, s, *dstStepsFlag)
		}
		agg.Merge(res.Coverage)
		if s == 1 {
			firstDigest = res.StateDigest
		}
	}

	// Prove 100% determinism: replaying seed=1 must yield the exact same digest.
	replay, err := RunSimulation(1, *dstStepsFlag)
	if err != nil {
		t.Fatalf("DST determinism replay(seed=1) failed: %v", err)
	}
	if replay.StateDigest != firstDigest {
		t.Fatalf("NON-DETERMINISM DETECTED on seed=1: first=%s replay=%s", firstDigest, replay.StateDigest)
	}

	t.Log("\n" + agg.Summary(AllRequiredEvents))

	// Antithesis-style "sometimes" assertion: every critical storage scenario
	// must actually be exercised across the swarm!
	if missing := agg.Missing(AllRequiredEvents); len(missing) > 0 {
		t.Fatalf("DST swarm missed required coverage scenarios: %v", missing)
	}
}

// TestDSTSteadyStateReclamation checks the liveness / anti-leak property:
// over long steady-state churn without permanently pinned readers, HighWater
// must plateau via freelist reclamation (zero asymptotic page leak) even under
// extreme multi-page freelist chaining (MaxFreelistIDs=3).
func TestDSTSteadyStateReclamation(t *testing.T) {
	profile := Profile{
		Name: "steady-reclamation",
		Options: engine.Options{
			PageSize:       256,
			MaxLeafKeys:    2,
			MaxBranchKeys:  2,
			MaxFreelistIDs: 3,
		},
		Keys:         makeASCIIKeys(6),
		ValMinLen:    8,
		ValMaxLen:    16,
		MaxReaders:   0,
		CrashProb:    0.0,
		StepwiseProb: 0.0,
		RollbackProb: 0.0,
	}

	warmup, err := RunSimulationWithProfile(777, profile, 150)
	if err != nil {
		t.Fatalf("steady-state warmup failed: %v", err)
	}
	longRun, err := RunSimulationWithProfile(777, profile, 450)
	if err != nil {
		t.Fatalf("steady-state long run failed: %v", err)
	}
	if longRun.Reclaimed <= warmup.Reclaimed {
		t.Fatalf("expected continued freelist page reclamation between step 150 and 450, got %d -> %d",
			warmup.Reclaimed, longRun.Reclaimed)
	}
	// After warmup, 300 additional steps (~140+ commits) must NOT grow HighWater
	// beyond a small burst constant (exact plateau proves zero page leak).
	if longRun.HighWater > warmup.HighWater+6 {
		t.Fatalf("potential page leak: HighWater grew from %d (txid=%d) to %d (txid=%d), reclaimed=%d",
			warmup.HighWater, warmup.FinalTxID, longRun.HighWater, longRun.FinalTxID, longRun.Reclaimed)
	}
	t.Logf("Steady-state reclamation plateau OK: step 150 (txid=%d, hw=%d, reclaimed=%d) -> step 450 (txid=%d, hw=%d, reclaimed=%d)",
		warmup.FinalTxID, warmup.HighWater, warmup.Reclaimed,
		longRun.FinalTxID, longRun.HighWater, longRun.Reclaimed)
}

// TestDSTReplay replays a single seed passed via -dst-seed=<N>.
func TestDSTReplay(t *testing.T) {
	if *dstSeedFlag == 0 {
		t.Skip("pass -dst-seed=<N> to replay a specific DST seed")
	}
	res, err := RunSimulation(*dstSeedFlag, *dstStepsFlag)
	if err != nil {
		t.Fatalf("replay failed: %v", err)
	}
	fmt.Printf("Replay seed=%d (%s) PASSED: txid=%d keys=%d hw=%d reclaimed=%d digest=%s\n%s",
		res.Seed, res.ProfileName, res.FinalTxID, res.FinalKeys, res.HighWater,
		res.Reclaimed, res.StateDigest, res.Coverage.Summary(AllRequiredEvents))
}
