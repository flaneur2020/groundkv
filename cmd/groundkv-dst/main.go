package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/flaneur2020/groundkv/dst"
)

func main() {
	startSeed := flag.Int64("seed", 1, "starting seed")
	numSeeds := flag.Int("seeds", 50, "number of deterministic seeds to simulate")
	steps := flag.Int("steps", 300, "simulation steps per seed")
	flag.Parse()

	start := time.Now()
	agg := dst.NewCoverageTracker()
	totalSteps := 0

	for i := 0; i < *numSeeds; i++ {
		seed := *startSeed + int64(i)
		res, err := dst.RunSimulation(seed, *steps)
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
			fmt.Fprintf(os.Stderr, "Reproduce with: go test -v ./dst -run TestDSTReplay -dst-seed=%d -dst-steps=%d\n", seed, *steps)
			os.Exit(1)
		}
		agg.Merge(res.Coverage)
		totalSteps += res.Steps
	}

	fmt.Printf("PASSED: %d seeds (%d total steps) in %s\n", *numSeeds, totalSteps, time.Since(start).Round(time.Millisecond))
	fmt.Print(agg.Summary(dst.AllRequiredEvents))
	if missing := agg.Missing(dst.AllRequiredEvents); len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "WARNING: missing scenario coverage events: %v\n", missing)
		os.Exit(1)
	}
}
