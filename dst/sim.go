package dst

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"

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

type simReader struct {
	id       int
	tx       *engine.Tx
	pinnedTx uint64
	expected map[string]string
}

type stagedTxOracle struct {
	baseTxID        uint64
	staged          map[string]string
	mutatedKeys     map[string]int
	hadRecycledFree bool
}

// RunResult summarizes a completed deterministic simulation run.
type RunResult struct {
	Seed        int64
	ProfileName string
	Steps       int
	FinalTxID   uint64
	FinalKeys   int
	HighWater   engine.PageID
	Reclaimed   uint64
	StateDigest string
	Coverage    *CoverageTracker
}

// Simulator executes a single-threaded, 100% seed-reproducible simulation of
// GroundKV under concurrent MVCC readers, 4-phase commits, sector-level power
// losses, and transient disk I/O errors.
type Simulator struct {
	seed    int64
	rng     *rand.Rand
	profile Profile

	disk *SimDisk
	db   *engine.DB

	writeTx      *engine.Tx
	stagedOracle *stagedTxOracle

	readers    map[int]*simReader
	nextReadID int

	committed          map[string]string
	lastSyncedTxID     uint64
	indeterminateTxID  uint64
	indeterminateState map[string]string
	prevTreeHeight     int

	cov     *CoverageTracker
	history []string
	step    int
}

// RunSimulation executes steps deterministic ticks for seed using its SwarmProfile.
func RunSimulation(seed int64, steps int) (*RunResult, error) {
	return RunSimulationWithProfile(seed, SwarmProfile(seed), steps)
}

// RunSimulationWithProfile runs a deterministic simulation with an explicit Profile.
func RunSimulationWithProfile(seed int64, profile Profile, steps int) (*RunResult, error) {
	rng := rand.New(rand.NewSource(seed))
	disk := NewSimDisk(rng, profile.Faults)
	// Mute faults during initial DB creation so we always start from an initialized DB.
	disk.SetFaultsMuted(true)
	db, err := engine.OpenStorage(disk, &profile.Options)
	if err != nil {
		return nil, fmt.Errorf("seed %d: initial OpenStorage failed: %w", seed, err)
	}
	disk.SetFaultsMuted(false)

	s := &Simulator{
		seed:      seed,
		rng:       rng,
		profile:   profile,
		disk:      disk,
		db:        db,
		readers:   make(map[int]*simReader),
		committed: make(map[string]string),
		cov:       NewCoverageTracker(),
	}
	defer s.cleanup()

	for i := 0; i < steps; i++ {
		s.step = i
		if err := s.tick(); err != nil {
			return nil, fmt.Errorf("seed=%d profile=%s step=%d: %w\nRecent trace:\n%s",
				seed, profile.Name, i, err, s.formatHistory())
		}
		if err := s.verifyInvariants(); err != nil {
			return nil, fmt.Errorf("seed=%d profile=%s step=%d invariant violation: %w\nRecent trace:\n%s",
				seed, profile.Name, i, err, s.formatHistory())
		}
	}

	s.disk.SetFaultsMuted(true)
	meta := s.db.Meta()
	fl := s.db.FreelistState()
	digest := s.computeDigest(meta)

	return &RunResult{
		Seed:        seed,
		ProfileName: profile.Name,
		Steps:       steps,
		FinalTxID:   meta.TxID,
		FinalKeys:   len(s.committed),
		HighWater:   meta.HighWater,
		Reclaimed:   fl.ReclaimedHits,
		StateDigest: digest,
		Coverage:    s.cov,
	}, nil
}

func (s *Simulator) cleanup() {
	if s.disk != nil {
		s.disk.SetFaultsMuted(true)
	}
	for id, r := range s.readers {
		_ = r.tx.Rollback()
		delete(s.readers, id)
	}
	if s.writeTx != nil {
		_ = s.writeTx.Rollback()
		s.writeTx = nil
	}
	if s.db != nil {
		_ = s.db.Close()
		s.db = nil
	}
}

func (s *Simulator) record(msg string) {
	entry := fmt.Sprintf("  [%04d] %s", s.step, msg)
	s.history = append(s.history, entry)
	if len(s.history) > 40 {
		s.history = s.history[len(s.history)-40:]
	}
}

func (s *Simulator) formatHistory() string {
	return strings.Join(s.history, "\n")
}

func (s *Simulator) randKey() []byte {
	return s.profile.Keys[s.rng.Intn(len(s.profile.Keys))]
}

func (s *Simulator) randVal() []byte {
	var n int
	if s.profile.BimodalVals {
		// Trimodal distribution tailored for 256B pages (232B payload, 11B entry overhead):
		//   - medium (76..84B -> 87..95B entry): [med, tiny, med] <= 209B fits in 1 leaf!
		//   - tiny   (4..8B   -> 15..19B entry)
		//   - huge   (165..195B -> 176..206B entry): med + huge >= 263B > 232B, so
		//     overwriting/inserting a huge middle key ALWAYS forces a 3-way split!
		roll := s.rng.Intn(20)
		switch {
		case roll < 9:
			n = 76 + s.rng.Intn(9)
		case roll < 14:
			n = 4 + s.rng.Intn(5)
		default:
			n = 165 + s.rng.Intn(31)
		}
	} else {
		span := s.profile.ValMaxLen - s.profile.ValMinLen + 1
		if span < 1 {
			span = 1
		}
		n = s.profile.ValMinLen + s.rng.Intn(span)
	}
	buf := make([]byte, n)
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	for i := range buf {
		buf[i] = alphabet[s.rng.Intn(len(alphabet))]
	}
	return buf
}

func (s *Simulator) tick() error {
	// Crash injection can fire at ANY step (idle OR mid-4-phase commit!).
	if s.rng.Float64() < s.profile.CrashProb {
		return s.doCrash()
	}

	// If a stepwise write transaction is in flight, advance it, stage more ops, or rollback.
	if s.writeTx != nil {
		roll := s.rng.Float64()
		switch {
		case s.writeTx.CommitPhase() == "STAGED" && roll < 0.30:
			return s.doStageMore()
		case s.writeTx.CommitPhase() == "STAGED" && roll < 0.30+s.profile.RollbackProb:
			return s.doRollbackWrite()
		case roll < 0.85:
			return s.doCommitStep()
		default:
			return s.doReaderAction()
		}
	}

	// Otherwise schedule among: stepwise write, atomic batch/put/delete, reader ops, range scans.
	roll := s.rng.Float64()
	switch {
	case roll < s.profile.StepwiseProb:
		return s.doBeginStepwiseWrite()
	case roll < s.profile.StepwiseProb+0.25:
		return s.doAtomicWrite()
	case roll < s.profile.StepwiseProb+0.40:
		return s.doAtomicDeleteBatch()
	default:
		return s.doReaderAction()
	}
}

func (s *Simulator) doBeginStepwiseWrite() error {
	freeBefore := len(s.db.FreelistState().FreeIDs)
	tx, err := s.db.BeginWrite()
	if err != nil {
		return fmt.Errorf("BeginWrite: %w", err)
	}
	staged := cloneMap(s.committed)
	muts := make(map[string]int)

	// Stage 1-2 initial ops.
	nOps := 1 + s.rng.Intn(2)
	for i := 0; i < nOps; i++ {
		k := s.randKey()
		if s.rng.Intn(3) == 0 {
			if err := tx.Delete(k); err != nil {
				if isInjectedErr(err) {
					_ = tx.Rollback()
					s.record("BeginStepwiseWrite aborted by injected read error")
					return nil
				}
				_ = tx.Rollback()
				return fmt.Errorf("tx.Delete: %w", err)
			}
			delete(staged, string(k))
		} else {
			v := s.randVal()
			if err := tx.Put(k, v); err != nil {
				if isInjectedErr(err) {
					_ = tx.Rollback()
					s.record("BeginStepwiseWrite aborted by injected read error")
					return nil
				}
				_ = tx.Rollback()
				return fmt.Errorf("tx.Put: %w", err)
			}
			staged[string(k)] = string(v)
		}
		muts[string(k)]++
		if muts[string(k)] >= 2 {
			s.cov.Hit(EventSameTxDirtyOverwrite)
		}
	}

	freeAfter := len(s.db.FreelistState().FreeIDs)
	s.writeTx = tx
	s.stagedOracle = &stagedTxOracle{
		baseTxID:        s.db.Meta().TxID,
		staged:          staged,
		mutatedKeys:     muts,
		hadRecycledFree: freeAfter < freeBefore,
	}
	s.record(fmt.Sprintf("BeginStepwiseWrite (ops=%d, recycled=%v)", nOps, freeAfter < freeBefore))
	return nil
}

func (s *Simulator) doStageMore() error {
	k := s.randKey()
	// Bias toward re-mutating an already-staged key to exercise same-tx dirty overwrite.
	if len(s.stagedOracle.mutatedKeys) > 0 && s.rng.Intn(2) == 0 {
		mkeys := make([]string, 0, len(s.stagedOracle.mutatedKeys))
		for mk := range s.stagedOracle.mutatedKeys {
			mkeys = append(mkeys, mk)
		}
		sort.Strings(mkeys)
		k = []byte(mkeys[s.rng.Intn(len(mkeys))])
	}
	v := s.randVal()
	if err := s.writeTx.Put(k, v); err != nil {
		if isInjectedErr(err) {
			_ = s.writeTx.Rollback()
			s.writeTx = nil
			s.stagedOracle = nil
			s.record("StageMore aborted by injected I/O error")
			return nil
		}
		return fmt.Errorf("StageMore Put: %w", err)
	}
	s.stagedOracle.staged[string(k)] = string(v)
	s.stagedOracle.mutatedKeys[string(k)]++
	if s.stagedOracle.mutatedKeys[string(k)] >= 2 {
		s.cov.Hit(EventSameTxDirtyOverwrite)
	}
	// Verify read-your-own-writes inside the uncommitted tx (with reads temporarily unmuted).
	s.disk.SetFaultsMuted(true)
	got, err := s.writeTx.Get(k)
	s.disk.SetFaultsMuted(false)
	if err != nil || !bytes.Equal(got, v) {
		return fmt.Errorf("read-your-own-writes mismatch: got=(%q, %v), want=%q", got, err, v)
	}
	s.record(fmt.Sprintf("StageMore Put(%x)", k))
	return nil
}

func (s *Simulator) doRollbackWrite() error {
	recycled := s.stagedOracle.hadRecycledFree
	if err := s.writeTx.Rollback(); err != nil {
		return fmt.Errorf("RollbackWrite: %w", err)
	}
	if recycled {
		s.cov.Hit(EventTxRollbackRecycled)
	}
	s.writeTx = nil
	s.stagedOracle = nil
	s.record(fmt.Sprintf("RollbackWrite (restoredRecycled=%v)", recycled))
	return nil
}

func (s *Simulator) doCommitStep() error {
	phase := s.writeTx.CommitPhase()
	switch phase {
	case "STAGED":
		if err := s.writeTx.WriteDataPages(); err != nil {
			if isInjectedErr(err) {
				s.cov.Hit(EventIOFailWrite)
				_ = s.writeTx.Rollback()
				s.writeTx = nil
				s.stagedOracle = nil
				s.record("WriteDataPages injected write error -> rolled back")
				return nil
			}
			return fmt.Errorf("WriteDataPages: %w", err)
		}
		s.record("CommitStep -> DATA_WRITTEN")

	case "DATA_WRITTEN":
		wasDirtyAlt := s.db.HasDirtyAltMeta()
		if err := s.writeTx.SyncDataPages(); err != nil {
			if isInjectedErr(err) {
				if errors.Is(err, ErrInjectedWrite) {
					s.cov.Hit(EventIOFailWrite)
				} else {
					s.cov.Hit(EventIOFailSync)
				}
				_ = s.writeTx.Rollback()
				s.writeTx = nil
				s.stagedOracle = nil
				s.record("SyncDataPages injected I/O error -> rolled back")
				return nil
			}
			return fmt.Errorf("SyncDataPages: %w", err)
		}
		if wasDirtyAlt && !s.db.HasDirtyAltMeta() {
			s.cov.Hit(EventDirtyAltMetaFence)
			s.indeterminateState = nil
		}
		s.record("CommitStep -> DATA_SYNCED")

	case "DATA_SYNCED":
		if err := s.writeTx.WriteMetaPage(); err != nil {
			if isInjectedErr(err) {
				s.cov.Hit(EventIOFailWrite)
				_ = s.writeTx.Rollback()
				s.writeTx = nil
				s.stagedOracle = nil
				s.record("WriteMetaPage injected write error -> rolled back")
				return nil
			}
			return fmt.Errorf("WriteMetaPage: %w", err)
		}
		s.record("CommitStep -> META_WRITTEN")

	case "META_WRITTEN":
		if err := s.writeTx.SyncMetaPage(); err != nil {
			if isInjectedErr(err) {
				s.cov.Hit(EventIOFailSync)
				nextTxID := s.stagedOracle.baseTxID + 1
				nextState := s.stagedOracle.staged
				_ = s.writeTx.Rollback()
				if s.db.HasDirtyAltMeta() {
					s.indeterminateTxID = nextTxID
					s.indeterminateState = nextState
				}
				s.writeTx = nil
				s.stagedOracle = nil
				s.record("SyncMetaPage injected sync error -> rolled back")
				return nil
			}
			return fmt.Errorf("SyncMetaPage: %w", err)
		}
		s.committed = s.stagedOracle.staged
		s.lastSyncedTxID = s.db.Meta().TxID
		s.indeterminateState = nil
		s.writeTx = nil
		s.stagedOracle = nil
		s.record(fmt.Sprintf("CommitStep -> COMMITTED (txid=%d)", s.lastSyncedTxID))
	}
	return nil
}

func (s *Simulator) doAtomicWrite() error {
	// Multi-key atomic Update (1..4 mutations in one Update transaction;
	// use crisp 1-op writes on BimodalVals profiles so 3-way split leaf deltas are exact).
	nOps := 1 + s.rng.Intn(4)
	if s.profile.BimodalVals {
		nOps = 1
	}
	next := cloneMap(s.committed)
	muts := make(map[string]int)
	wasDirtyAlt := s.db.HasDirtyAltMeta()

	var leavesBefore int
	if nOps == 1 && s.profile.BimodalVals {
		s.disk.SetFaultsMuted(true)
		leavesBefore = s.countLeafPages(s.db.Meta().Root)
		s.disk.SetFaultsMuted(false)
	}

	err := s.db.Update(func(tx *engine.Tx) error {
		for i := 0; i < nOps; i++ {
			k := s.randKey()
			v := s.randVal()
			if err := tx.Put(k, v); err != nil {
				return err
			}
			next[string(k)] = string(v)
			muts[string(k)]++
			if muts[string(k)] >= 2 {
				s.cov.Hit(EventSameTxDirtyOverwrite)
			}
		}
		return nil
	})
	if err != nil {
		if isInjectedErr(err) {
			if errors.Is(err, ErrInjectedWrite) {
				s.cov.Hit(EventIOFailWrite)
			} else if errors.Is(err, ErrInjectedSync) {
				s.cov.Hit(EventIOFailSync)
			}
			if !wasDirtyAlt && s.db.HasDirtyAltMeta() {
				s.indeterminateTxID = s.db.Meta().TxID + 1
				s.indeterminateState = next
			} else if wasDirtyAlt && !s.db.HasDirtyAltMeta() {
				s.cov.Hit(EventDirtyAltMetaFence)
				s.indeterminateState = nil
			}
			s.record(fmt.Sprintf("AtomicUpdate(%d ops) aborted by injected I/O err: %v", nOps, err))
			return nil
		}
		return fmt.Errorf("AtomicUpdate: %w", err)
	}
	if wasDirtyAlt && !s.db.HasDirtyAltMeta() {
		s.cov.Hit(EventDirtyAltMetaFence)
	}
	if nOps == 1 && s.profile.BimodalVals && leavesBefore > 0 {
		s.disk.SetFaultsMuted(true)
		leavesAfter := s.countLeafPages(s.db.Meta().Root)
		s.disk.SetFaultsMuted(false)
		// A 2-way leaf split always adds +1 leaf; a 3-way split adds >= +2 leaves!
		if leavesAfter-leavesBefore >= 2 {
			s.cov.Hit(EventThreeWaySplit)
		}
	}
	s.committed = next
	s.lastSyncedTxID = s.db.Meta().TxID
	s.indeterminateState = nil
	s.record(fmt.Sprintf("AtomicUpdate(%d ops) -> txid=%d", nOps, s.lastSyncedTxID))
	return nil
}

func (s *Simulator) doAtomicDeleteBatch() error {
	// Occasionally clear all keys to force root collapse & empty-tree transitions!
	clearAll := len(s.committed) > 0 && s.rng.Intn(5) == 0
	next := cloneMap(s.committed)
	wasDirtyAlt := s.db.HasDirtyAltMeta()

	err := s.db.Update(func(tx *engine.Tx) error {
		if clearAll {
			keys := sortedMapKeys(next)
			for _, k := range keys {
				if err := tx.Delete([]byte(k)); err != nil {
					return err
				}
				delete(next, k)
			}
			return nil
		}
		nOps := 1 + s.rng.Intn(3)
		for i := 0; i < nOps; i++ {
			k := s.randKey()
			if err := tx.Delete(k); err != nil {
				return err
			}
			delete(next, string(k))
		}
		return nil
	})
	if err != nil {
		if isInjectedErr(err) {
			if errors.Is(err, ErrInjectedWrite) {
				s.cov.Hit(EventIOFailWrite)
			} else if errors.Is(err, ErrInjectedSync) {
				s.cov.Hit(EventIOFailSync)
			}
			if !wasDirtyAlt && s.db.HasDirtyAltMeta() {
				s.indeterminateTxID = s.db.Meta().TxID + 1
				s.indeterminateState = next
			} else if wasDirtyAlt && !s.db.HasDirtyAltMeta() {
				s.cov.Hit(EventDirtyAltMetaFence)
				s.indeterminateState = nil
			}
			s.record(fmt.Sprintf("AtomicDelete aborted by injected I/O err: %v", err))
			return nil
		}
		return fmt.Errorf("AtomicDelete: %w", err)
	}
	if wasDirtyAlt && !s.db.HasDirtyAltMeta() {
		s.cov.Hit(EventDirtyAltMetaFence)
	}
	s.committed = next
	s.lastSyncedTxID = s.db.Meta().TxID
	s.indeterminateState = nil
	s.record(fmt.Sprintf("AtomicDelete(clearAll=%v) -> txid=%d keys=%d", clearAll, s.lastSyncedTxID, len(s.committed)))
	return nil
}

func (s *Simulator) doReaderAction() error {
	roll := s.rng.Intn(4)
	switch {
	case roll == 0 && len(s.readers) < s.profile.MaxReaders:
		tx, err := s.db.BeginRead()
		if err != nil {
			return fmt.Errorf("BeginRead: %w", err)
		}
		id := s.nextReadID
		s.nextReadID++
		s.readers[id] = &simReader{
			id:       id,
			tx:       tx,
			pinnedTx: tx.TxID(),
			expected: cloneMap(s.committed),
		}
		s.record(fmt.Sprintf("OpenReader#%d (pinnedTx=%d)", id, tx.TxID()))

	case roll == 1 && len(s.readers) > 0:
		id := s.pickReaderID()
		r := s.readers[id]
		delete(s.readers, id)
		if err := r.tx.Rollback(); err != nil {
			return fmt.Errorf("CloseReader#%d: %w", id, err)
		}
		s.record(fmt.Sprintf("CloseReader#%d (pinnedTx=%d)", id, r.pinnedTx))

	case roll == 2 && len(s.readers) > 0:
		id := s.pickReaderID()
		r := s.readers[id]
		k := s.randKey()
		got, err := r.tx.Get(k)
		if isInjectedErr(err) {
			s.record(fmt.Sprintf("Reader#%d.Get injected read err (expected)", id))
			return nil
		}
		want, ok := r.expected[string(k)]
		if !ok {
			if !errors.Is(err, engine.ErrNotFound) {
				return fmt.Errorf("Reader#%d.Get(%x) = (%q, %v), want ErrNotFound", id, k, got, err)
			}
		} else if err != nil || string(got) != want {
			return fmt.Errorf("Reader#%d.Get(%x) = (%q, %v), want %q", id, k, got, err, want)
		}

	default:
		// Range scan check on latest committed view.
		k1 := s.randKey()
		k2 := s.randKey()
		if bytes.Compare(k1, k2) > 0 {
			k1, k2 = k2, k1
		}
		var start, end []byte
		if s.rng.Intn(3) != 0 {
			start = k1
		}
		if s.rng.Intn(3) != 0 {
			end = k2
		}
		got := make([]string, 0)
		err := s.db.View(func(tx *engine.Tx) error {
			return tx.Scan(start, end, func(k, v []byte) bool {
				got = append(got, fmt.Sprintf("%x=%s", k, v))
				return true
			})
		})
		if isInjectedErr(err) {
			s.record("ScanRange injected read err (expected)")
			return nil
		}
		if err != nil {
			return fmt.Errorf("ScanRange: %w", err)
		}
		want := expectedRangeScan(s.committed, start, end)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			return fmt.Errorf("ScanRange[%x,%x) mismatch: got=%v want=%v", start, end, got, want)
		}
	}
	return nil
}

func (s *Simulator) doCrash() error {
	phase := "IDLE"
	var postCommitMap map[string]string
	var postCommitTxID uint64
	if s.writeTx != nil {
		phase = s.writeTx.CommitPhase()
		postCommitMap = s.stagedOracle.staged
		postCommitTxID = s.stagedOracle.baseTxID + 1
	}

	preCommitMap := s.committed
	preCommitTxID := s.db.Meta().TxID
	hadDirtyAlt := s.db.HasDirtyAltMeta()
	indetTxID := s.indeterminateTxID
	indetMap := s.indeterminateState

	// Choose crash style deterministically to thoroughly cover drop, reordered,
	// torn-meta, and arbitrary sector-subset power losses!
	styles := []PowerLossStyle{
		PowerLossDropAll,
		PowerLossRandomSubset,
		PowerLossReorderedMeta,
		PowerLossTornMeta,
	}
	style := styles[s.rng.Intn(len(styles))]

	switch phase {
	case "DATA_WRITTEN":
		s.cov.Hit(EventCrashAfterDataWrite)
	case "DATA_SYNCED":
		s.cov.Hit(EventCrashAfterDataSync)
	}

	// Abrupt crash discards all in-memory tx & reader handles.
	s.writeTx = nil
	s.stagedOracle = nil
	s.readers = make(map[int]*simReader)

	nextDisk, rep := s.disk.PowerLoss(style, s.profile.Options.PageSize)
	s.disk = nextDisk

	// Reopen cleanly (temporarily muting transient read errors during boot recovery).
	s.disk.SetFaultsMuted(true)
	db2, err := engine.OpenStorage(s.disk, &s.profile.Options)
	if err != nil {
		return fmt.Errorf("OpenStorage after crash(phase=%s, style=%d) failed: %w", phase, style, err)
	}
	s.db = db2

	if err := s.db.Validate(); err != nil {
		return fmt.Errorf("Validate() failed after crash(phase=%s, style=%d): %w", phase, style, err)
	}

	gotMap, err := s.db.ReadTreeAtRoot(s.db.Meta().Root)
	if err != nil {
		return fmt.Errorf("ReadTreeAtRoot after crash failed: %w", err)
	}
	gotTxID := s.db.Meta().TxID
	slot0, slot1, err := s.db.InspectMeta()
	if err != nil {
		return fmt.Errorf("InspectMeta after crash failed: %w", err)
	}
	s.disk.SetFaultsMuted(false)

	if !slot0.Valid || !slot1.Valid {
		s.cov.Hit(EventCrashTornMetaFallback)
	}

	// Durability invariant: never regress below last durably synced txid!
	if gotTxID < s.lastSyncedTxID {
		return fmt.Errorf("DURABILITY VIOLATION: txid regressed from %d to %d after crash(phase=%s, style=%d)",
			s.lastSyncedTxID, gotTxID, phase, style)
	}

	// Atomicity invariant:
	// - Before META_WRITTEN, recovered state MUST equal exact preCommitMap
	//   (or indetMap only if a prior SyncMetaPage + rollback invalidation double-fault
	//   left an un-neutralizable valid meta page in volatile cache).
	// - At META_WRITTEN, recovered state MUST equal EITHER preCommitMap OR postCommitMap!
	switch {
	case gotTxID == preCommitTxID && mapsEqual(gotMap, preCommitMap):
		if phase == "META_WRITTEN" {
			s.cov.Hit(EventCrashAfterMetaWritePre)
		}
		s.committed = preCommitMap
		s.lastSyncedTxID = gotTxID

	case phase == "META_WRITTEN" && gotTxID == postCommitTxID && mapsEqual(gotMap, postCommitMap):
		s.cov.Hit(EventCrashAfterMetaWritePost)
		s.committed = postCommitMap
		s.lastSyncedTxID = gotTxID

	case hadDirtyAlt && indetMap != nil && gotTxID == indetTxID && mapsEqual(gotMap, indetMap):
		s.cov.Hit(EventCrashAfterMetaWritePost)
		s.committed = indetMap
		s.lastSyncedTxID = gotTxID

	default:
		return fmt.Errorf("ATOMICITY VIOLATION after crash(phase=%s, style=%d, rep=%+v): gotTxID=%d (pre=%d, post=%d), gotKeys=%d preKeys=%d",
			phase, style, rep, gotTxID, preCommitTxID, postCommitTxID, len(gotMap), len(preCommitMap))
	}
	s.indeterminateState = nil

	s.record(fmt.Sprintf("Crash(phase=%s, style=%d, torn=%v) -> recovered txid=%d", phase, style, rep.MetaTorn, gotTxID))
	return nil
}

func (s *Simulator) probeOptions() *engine.Options {
	cp := s.profile.Options
	cp.Tracer = nil
	return &cp
}

func (s *Simulator) verifyInvariants() error {
	s.disk.SetFaultsMuted(true)
	defer s.disk.SetFaultsMuted(false)

	// 1. Live B+Tree structural & CRC32 validation.
	if err := s.db.Validate(); err != nil {
		return fmt.Errorf("live db.Validate(): %w", err)
	}

	// 2. Live committed map matches exact shadow oracle.
	meta := s.db.Meta()
	if meta.TxID < s.lastSyncedTxID {
		return fmt.Errorf("meta.TxID %d < lastSyncedTxID %d", meta.TxID, s.lastSyncedTxID)
	}
	gotCommitted, err := s.db.ReadTreeAtRoot(meta.Root)
	if err != nil {
		return fmt.Errorf("ReadTreeAtRoot(active): %w", err)
	}
	if !mapsEqual(gotCommitted, s.committed) {
		return fmt.Errorf("committed state mismatch: got %d keys, oracle has %d keys", len(gotCommitted), len(s.committed))
	}

	// 3. Durable disk image MUST always open and validate cleanly (even mid-commit!).
	durableDisk := s.disk.CloneDurable()
	durableDB, err := engine.OpenStorage(durableDisk, s.probeOptions())
	if err != nil {
		return fmt.Errorf("durable image OpenStorage failed: %w", err)
	}
	if err := durableDB.Validate(); err != nil {
		_ = durableDB.Close()
		return fmt.Errorf("durable image Validate failed: %w", err)
	}
	_ = durableDB.Close()

	// 4. Every open MVCC reader sees its exact immutable point-in-time snapshot.
	livePages := make(map[engine.PageID]bool)
	slot0, slot1, err := s.db.InspectMeta()
	if err != nil {
		return fmt.Errorf("InspectMeta: %w", err)
	}
	if !slot0.Valid && !slot1.Valid {
		return errors.New("both meta slots invalid")
	}
	for _, slot := range []engine.MetaSlot{slot0, slot1} {
		if !slot.Valid {
			continue
		}
		pages, err := s.db.ReachablePages(slot.Meta.Root, slot.Meta.Freelist)
		if err != nil {
			return fmt.Errorf("ReachablePages(meta txid=%d): %w", slot.Meta.TxID, err)
		}
		for _, id := range pages {
			livePages[id] = true
		}
	}

	for id, r := range s.readers {
		gotSnap, err := r.tx.All()
		if err != nil {
			return fmt.Errorf("Reader#%d.All() failed: %w", id, err)
		}
		if !mapsEqual(gotSnap, r.expected) {
			return fmt.Errorf("MVCC VIOLATION on Reader#%d (txid=%d): snapshot drifted from oracle", id, r.pinnedTx)
		}
		pages, err := s.db.ReachablePages(r.tx.RootPageID(), 0)
		if err != nil {
			return fmt.Errorf("ReachablePages(Reader#%d): %w", id, err)
		}
		for _, pid := range pages {
			livePages[pid] = true
		}
	}

	// 5. Freelist & MVCC disjointness: NO live page may appear in freelist.free,
	//    and NO pending txid may sit <= ReleaseBound().
	fl := s.db.FreelistState()
	for _, freeID := range fl.FreeIDs {
		if livePages[freeID] {
			return fmt.Errorf("FREELIST USE-AFTER-FREE: live page %d present in freelist.free", freeID)
		}
	}
	bound := s.db.ReleaseBound()
	for _, pt := range fl.PendingTxIDs {
		if pt <= bound {
			return fmt.Errorf("FREELIST WATERMARK VIOLATION: pending txid %d <= releaseBound %d", pt, bound)
		}
	}

	// 6. Periodic non-destructive silent bit-rot (LSE) verification on durable image.
	if s.step > 0 && s.step%30 == 0 {
		if err := s.verifyBitRotDetectionLocked(); err != nil {
			return err
		}
	}

	// 7. Track "sometimes" scenario coverage metrics from real engine state!
	s.observeCoverageLocked(meta, fl)
	return nil
}

// verifyBitRotDetectionLocked clones the durable disk image, flips a bit in a
// live page (both meta slots, active root, freelist chain, or interior leaf),
// and asserts GroundKV always surfaces ErrCorruptedMeta/ErrCorruptedPage cleanly
// rather than silently returning corrupted data or rolling back committed data.
func (s *Simulator) verifyBitRotDetectionLocked() error {
	ps := s.profile.Options.PageSize

	// Case A: Corrupting both meta slots must always fail OpenStorage with ErrCorruptedMeta.
	bothBad, ok := s.disk.CloneDurableWithBitRot(ps, 0, 1)
	if ok {
		if dbBad, err := engine.OpenStorage(bothBad, s.probeOptions()); err == nil {
			_ = dbBad.Close()
			return errors.New("BIT-ROT VIOLATION: OpenStorage succeeded when both meta slots were corrupted")
		} else if !errors.Is(err, engine.ErrCorruptedMeta) {
			return fmt.Errorf("BIT-ROT VIOLATION: expected ErrCorruptedMeta, got %v", err)
		}
		s.cov.Hit(EventBitRotDetected)
	}

	// Case B: Corrupting the durable active root page (when non-empty) must fail loudly
	// on OpenStorage rather than silently reverting to an older meta slot.
	durableProbe := s.disk.CloneDurable()
	probeDB, err := engine.OpenStorage(durableProbe, s.probeOptions())
	if err != nil {
		return fmt.Errorf("bit-rot probe OpenStorage: %w", err)
	}
	dMeta := probeDB.Meta()
	var nonRootPages []engine.PageID
	if dMeta.Root != 0 {
		if pages, pErr := probeDB.ReachablePages(dMeta.Root, 0); pErr == nil {
			for _, pid := range pages {
				if pid != dMeta.Root {
					nonRootPages = append(nonRootPages, pid)
				}
			}
		}
	}
	_ = probeDB.Close()

	if dMeta.Root != 0 {
		rootBad, flipped := s.disk.CloneDurableWithBitRot(ps, dMeta.Root)
		if flipped {
			if dbBad, err := engine.OpenStorage(rootBad, s.probeOptions()); err == nil {
				_ = dbBad.Close()
				return fmt.Errorf("BIT-ROT VIOLATION: OpenStorage succeeded despite corrupt active root page %d", dMeta.Root)
			} else if !errors.Is(err, engine.ErrCorruptedPage) {
				return fmt.Errorf("BIT-ROT VIOLATION: expected ErrCorruptedPage on corrupt root, got %v", err)
			}
			s.cov.Hit(EventBitRotDetected)
		}
	}

	// Case C: Corrupting a non-root reachable page must be caught by Validate().
	if len(nonRootPages) > 0 {
		victim := nonRootPages[s.step%len(nonRootPages)]
		leafBad, flipped := s.disk.CloneDurableWithBitRot(ps, victim)
		if flipped {
			dbBad, err := engine.OpenStorage(leafBad, s.probeOptions())
			if err == nil {
				valErr := dbBad.Validate()
				_ = dbBad.Close()
				if !errors.Is(valErr, engine.ErrCorruptedPage) {
					return fmt.Errorf("BIT-ROT VIOLATION: Validate() did not catch corrupt page %d (err=%v)", victim, valErr)
				}
				s.cov.Hit(EventBitRotDetected)
			}
		}
	}
	return nil
}

func (s *Simulator) observeCoverageLocked(meta engine.Meta, fl engine.FreelistSnapshot) {
	if meta.Root == 0 {
		if s.prevTreeHeight > 0 {
			s.cov.Hit(EventEmptyTree)
		}
		s.prevTreeHeight = 0
	} else {
		height := s.measureTreeHeight(meta.Root)
		if height >= 2 {
			s.cov.Hit(EventLeafSplit)
		}
		if height >= 3 {
			s.cov.Hit(EventBranchSplit)
		}
		if s.prevTreeHeight > height && height > 0 {
			s.cov.Hit(EventRootCollapse)
		}
		s.prevTreeHeight = height
	}

	if meta.Freelist != 0 {
		flPages, err := s.db.ReachablePages(0, meta.Freelist)
		if err == nil && len(flPages) >= 2 {
			s.cov.Hit(EventMultiPageFreelist)
		}
	}
	if fl.ReclaimedHits > 0 {
		s.cov.Hit(EventFreelistReclaim)
	}
	if minR, ok := s.db.MinActiveReaderTxID(); ok && meta.TxID > 1 && minR < meta.TxID-1 && len(fl.PendingTxIDs) > 0 {
		s.cov.Hit(EventReaderBlockedReclaim)
	}
}

func (s *Simulator) countLeafPages(root engine.PageID) int {
	if root == 0 {
		return 0
	}
	pages, err := s.db.ReachablePages(root, 0)
	if err != nil {
		return 0
	}
	leaves := 0
	for _, pid := range pages {
		buf, err := s.db.RawPage(pid)
		if err != nil || len(buf) < 10 {
			continue
		}
		flags := uint16(buf[8]) | uint16(buf[9])<<8
		if flags == engine.PageFlagLeaf {
			leaves++
		}
	}
	return leaves
}

func (s *Simulator) measureTreeHeight(root engine.PageID) int {
	// Measure height by inspecting raw page flags from root down leftmost child.
	height := 0
	curr := root
	for curr != 0 && height < 16 {
		buf, err := s.db.RawPage(curr)
		if err != nil || len(buf) < 24 {
			break
		}
		flags := uint16(buf[8]) | uint16(buf[9])<<8
		height++
		if flags == engine.PageFlagLeaf {
			break
		}
		if flags != engine.PageFlagBranch || len(buf) < 24+12 {
			break
		}
		// First branch entry: [24:28]=klen, [28:36]=child PageID
		var child uint64
		for i := 0; i < 8; i++ {
			child |= uint64(buf[28+i]) << (8 * i)
		}
		curr = engine.PageID(child)
	}
	return height
}

func (s *Simulator) pickReaderID() int {
	ids := make([]int, 0, len(s.readers))
	for id := range s.readers {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids[s.rng.Intn(len(ids))]
}

func (s *Simulator) computeDigest(meta engine.Meta) string {
	h := sha256.New()
	fmt.Fprintf(h, "txid=%d;hw=%d;", meta.TxID, meta.HighWater)
	for _, k := range sortedMapKeys(s.committed) {
		fmt.Fprintf(h, "%x=%s;", k, s.committed[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func isInjectedErr(err error) bool {
	return errors.Is(err, ErrInjectedRead) ||
		errors.Is(err, ErrInjectedWrite) ||
		errors.Is(err, ErrInjectedSync)
}

func cloneMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func sortedMapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func expectedRangeScan(m map[string]string, start, end []byte) []string {
	out := make([]string, 0)
	for _, k := range sortedMapKeys(m) {
		kb := []byte(k)
		if len(start) > 0 && bytes.Compare(kb, start) < 0 {
			continue
		}
		if len(end) > 0 && bytes.Compare(kb, end) >= 0 {
			break
		}
		out = append(out, fmt.Sprintf("%x=%s", kb, m[k]))
	}
	return out
}
