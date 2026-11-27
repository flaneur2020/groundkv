package dst

import (
	"fmt"
	"math/rand"

	"github.com/flaneur2020/groundkv/engine"
)

// Profile configures a Swarm Testing simulation scenario. Varying profiles
// across seeds exposes vastly more state-space shapes than a single fixed config.
type Profile struct {
	Name         string
	Options      engine.Options
	Keys         [][]byte
	ValMinLen    int
	ValMaxLen    int
	BimodalVals  bool // Alternates tiny and near-page-capacity values to trigger 3-way splits
	MaxReaders   int
	CrashProb    float64
	StepwiseProb float64
	RollbackProb float64
	Faults       FaultConfig
}

// SwarmProfile deterministically selects and parameterizes a Swarm Testing
// profile from seed so each seed explores a distinct region of the design space.
func SwarmProfile(seed int64) Profile {
	r := rand.New(rand.NewSource(seed ^ 0x517cc1b727220a95))
	kind := int((uint64(seed) % 7))

	switch kind {
	case 0: // Hot-key churn + multi-page freelist + heavy crashes
		return Profile{
			Name: "hot-churn-crash",
			Options: engine.Options{
				PageSize:       256,
				MaxLeafKeys:    2,
				MaxBranchKeys:  2,
				MaxFreelistIDs: 3,
			},
			Keys:         makeASCIIKeys(5),
			ValMinLen:    4,
			ValMaxLen:    24,
			MaxReaders:   3,
			CrashProb:    0.16,
			StepwiseProb: 0.45,
			RollbackProb: 0.25,
			Faults:       FaultConfig{FailWriteProb: 0.04, FailSyncProb: 0.06, FailReadProb: 0.02},
		}

	case 1: // Wide 3-4 level B+Tree splits, deletions & root collapses
		return Profile{
			Name: "wide-deep-btree",
			Options: engine.Options{
				PageSize:       512,
				MaxLeafKeys:    2,
				MaxBranchKeys:  2,
				MaxFreelistIDs: 6,
			},
			Keys:         makeASCIIKeys(24),
			ValMinLen:    4,
			ValMaxLen:    20,
			MaxReaders:   4,
			CrashProb:    0.08,
			StepwiseProb: 0.30,
			RollbackProb: 0.15,
			Faults:       FaultConfig{FailWriteProb: 0.02, FailSyncProb: 0.03, FailReadProb: 0.01},
		}

	case 2: // Binary keys (\x00/\xff) + large values triggering byte-capacity splits
		return Profile{
			Name: "binary-byte-splits",
			Options: engine.Options{
				PageSize:       256,
				MaxLeafKeys:    0, // Split purely by encoded byte capacity!
				MaxBranchKeys:  0,
				MaxFreelistIDs: 6,
			},
			Keys:         makeBinaryKeys(10),
			ValMinLen:    40,
			ValMaxLen:    110,
			MaxReaders:   3,
			CrashProb:    0.10,
			StepwiseProb: 0.35,
			RollbackProb: 0.20,
			Faults:       FaultConfig{FailWriteProb: 0.02, FailSyncProb: 0.04, FailReadProb: 0.01},
		}

	case 3: // Extreme I/O fault & mid-commit crash chaos
		return Profile{
			Name: "chaos-io-faults",
			Options: engine.Options{
				PageSize:       256,
				MaxLeafKeys:    2,
				MaxBranchKeys:  2,
				MaxFreelistIDs: 4,
			},
			Keys:         makeASCIIKeys(8),
			ValMinLen:    4,
			ValMaxLen:    32,
			MaxReaders:   4,
			CrashProb:    0.22,
			StepwiseProb: 0.65,
			RollbackProb: 0.30,
			Faults:       FaultConfig{FailWriteProb: 0.08, FailSyncProb: 0.12, FailReadProb: 0.03},
		}

	case 4: // Bimodal tiny/medium/near-max values forcing greedy 3-way leaf splits on 256B pages
		return Profile{
			Name: "extreme-3way-splits",
			Options: engine.Options{
				PageSize:       256,
				MaxLeafKeys:    0,
				MaxBranchKeys:  0,
				MaxFreelistIDs: 6,
			},
			Keys:         makeASCIIKeys(6),
			ValMinLen:    4,
			ValMaxLen:    195,
			BimodalVals:  true,
			MaxReaders:   2,
			CrashProb:    0.06,
			StepwiseProb: 0.15,
			RollbackProb: 0.05,
			Faults:       FaultConfig{FailWriteProb: 0.01, FailSyncProb: 0.02, FailReadProb: 0.01},
		}

	case 5: // High Sync + Write fault overlap exercising dirtyAltMeta fence & retry
		return Profile{
			Name: "double-fault-meta-sync",
			Options: engine.Options{
				PageSize:       256,
				MaxLeafKeys:    2,
				MaxBranchKeys:  2,
				MaxFreelistIDs: 5,
			},
			Keys:         makeASCIIKeys(5),
			ValMinLen:    4,
			ValMaxLen:    24,
			MaxReaders:   2,
			CrashProb:    0.04,
			StepwiseProb: 0.70,
			RollbackProb: 0.0,
			Faults:       FaultConfig{FailWriteProb: 0.12, FailSyncProb: 0.45, FailReadProb: 0.01},
		}

	default: // Randomized hybrid swarm profile
		pageSizes := []int{256, 512, 1024}
		leafCaps := []int{0, 2, 3}
		branchCaps := []int{0, 2, 3}
		flCaps := []int{0, 6, 8}
		return Profile{
			Name: "hybrid-swarm",
			Options: engine.Options{
				PageSize:       pageSizes[r.Intn(len(pageSizes))],
				MaxLeafKeys:    leafCaps[r.Intn(len(leafCaps))],
				MaxBranchKeys:  branchCaps[r.Intn(len(branchCaps))],
				MaxFreelistIDs: flCaps[r.Intn(len(flCaps))],
			},
			Keys:         makeASCIIKeys(4 + r.Intn(14)),
			ValMinLen:    2,
			ValMaxLen:    48,
			MaxReaders:   2 + r.Intn(3),
			CrashProb:    0.05 + r.Float64()*0.15,
			StepwiseProb: 0.35,
			RollbackProb: 0.20,
			Faults: FaultConfig{
				FailWriteProb: r.Float64() * 0.05,
				FailSyncProb:  r.Float64() * 0.08,
				FailReadProb:  r.Float64() * 0.02,
			},
		}
	}
}

func makeASCIIKeys(n int) [][]byte {
	keys := make([][]byte, n)
	for i := 0; i < n; i++ {
		keys[i] = []byte(fmt.Sprintf("k%02d", i))
	}
	return keys
}

func makeBinaryKeys(n int) [][]byte {
	keys := make([][]byte, n)
	for i := 0; i < n; i++ {
		keys[i] = []byte{byte(i % 3), 0x00, byte('a' + i), 0xff}
	}
	return keys
}
