package dst

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"

	"github.com/flaneur2020/groundkv/engine"
)

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

func (s *Simulator) pickReaderID() int {
	ids := make([]int, 0, len(s.readers))
	for id := range s.readers {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids[s.rng.Intn(len(ids))]
}

func isInjectedErr(err error) bool {
	return errors.Is(err, ErrInjectedRead) ||
		errors.Is(err, ErrInjectedWrite) ||
		errors.Is(err, ErrInjectedSync)
}
