// Source: ../../formal/quint/storage_protocol.qnt
// White-box Quint ITF Model-Based Testing suite driving GroundKV's 4-phase
// commit protocol, MVCC reader-aware freelist reclamation, POSIX FailSync
// double-fault rollback fencing, and 3 crash modes (DROP / REORDERED / TORN)
// against BOTH engine.GoCore() and verified.NewCore() with zero shadow state.
package mbt

import (
	"errors"
	"fmt"
	"testing"

	"github.com/flaneur2020/groundkv/engine"
	"github.com/flaneur2020/groundkv/engine/verified"
)

const (
	notFoundSentinel = "NOT_FOUND"
	protocolKey      = "k1"
)

type quintMetaSlot struct {
	Valid    bool   `json:"valid"`
	TxID     ITFInt `json:"txid"`
	Expected string `json:"expected"`
}

type quintStepRecord struct {
	ActionName string `json:"actionName"`
	ArgStr     string `json:"argStr"`
	ArgBool    bool   `json:"argBool"`
	Result     string `json:"result"`
}

type storageProtocolState struct {
	TxID           ITFInt          `json:"txid"`
	Phase          string          `json:"phase"`
	CommittedVal   string          `json:"committedVal"`
	DirtyAltMeta   bool            `json:"dirtyAltMeta"`
	HasReader      bool            `json:"hasReader"`
	ReaderTxID     ITFInt          `json:"readerTxid"`
	ReaderExpected string          `json:"readerExpected"`
	Meta0          quintMetaSlot   `json:"meta0"`
	Meta1          quintMetaSlot   `json:"meta1"`
	LastStep       quintStepRecord `json:"lastStep"`
}

type protocolHarness struct {
	storage *engine.MemStorage
	db      *engine.DB
	opts    *engine.Options
	writeTx *engine.Tx
	readTx  *engine.Tx
}

func newProtocolHarness(core engine.Core) (*protocolHarness, error) {
	st := engine.NewMemStorage()
	opts := &engine.Options{
		PageSize:       512,
		MaxLeafKeys:    2,
		MaxBranchKeys:  2,
		MaxFreelistIDs: 3, // Force multi-page linked freelist chains (next != 0)
		NoFreelist:     false,
		Core:           core,
	}
	db, err := engine.OpenStorage(st, opts)
	if err != nil {
		return nil, err
	}
	return &protocolHarness{
		storage: st,
		db:      db,
		opts:    opts,
	}, nil
}

func (h *protocolHarness) close() {
	if h.readTx != nil {
		_ = h.readTx.Rollback()
		h.readTx = nil
	}
	if h.writeTx != nil {
		_ = h.writeTx.Rollback()
		h.writeTx = nil
	}
	if h.db != nil {
		_ = h.db.Close()
		h.db = nil
	}
}

func (h *protocolHarness) applyAction(step quintStepRecord) error {
	switch step.ActionName {
	case "BeginPut":
		tx, err := h.db.BeginWrite()
		if err != nil {
			return fmt.Errorf("BeginWrite: %w", err)
		}
		if err := tx.Put([]byte(protocolKey), []byte(step.Result)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("Put(%s): %w", step.Result, err)
		}
		h.writeTx = tx
		return nil

	case "StageOverwrite":
		if h.writeTx == nil {
			return fmt.Errorf("StageOverwrite with nil writeTx")
		}
		if err := h.writeTx.Put([]byte(protocolKey), []byte(step.Result)); err != nil {
			return fmt.Errorf("StageOverwrite Put(%s): %w", step.Result, err)
		}
		got, err := h.writeTx.Get([]byte(protocolKey))
		if err != nil || string(got) != step.Result {
			return fmt.Errorf("read-your-own-writes mismatch: got=%q want=%q err=%v", got, step.Result, err)
		}
		return nil

	case "BeginDelete":
		tx, err := h.db.BeginWrite()
		if err != nil {
			return fmt.Errorf("BeginWrite: %w", err)
		}
		if err := tx.Delete([]byte(protocolKey)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("Delete: %w", err)
		}
		h.writeTx = tx
		return nil

	case "CommitStep":
		if h.writeTx == nil {
			return fmt.Errorf("CommitStep with nil writeTx")
		}
		switch step.Result {
		case "DATA_WRITTEN":
			return h.writeTx.WriteDataPages()
		case "DATA_SYNCED":
			return h.writeTx.SyncDataPages()
		case "META_WRITTEN":
			return h.writeTx.WriteMetaPage()
		case "COMMITTED":
			tx := h.writeTx
			h.writeTx = nil
			return tx.SyncMetaPage()
		default:
			return fmt.Errorf("unknown CommitStep result %q", step.Result)
		}

	case "RollbackWrite":
		if h.writeTx == nil {
			return fmt.Errorf("RollbackWrite with nil writeTx")
		}
		tx := h.writeTx
		h.writeTx = nil
		return tx.Rollback()

	case "OpenReader":
		tx, err := h.db.BeginRead()
		if err != nil {
			return fmt.Errorf("BeginRead: %w", err)
		}
		h.readTx = tx
		return nil

	case "CloseReader":
		if h.readTx == nil {
			return fmt.Errorf("CloseReader with nil readTx")
		}
		tx := h.readTx
		h.readTx = nil
		return tx.Rollback()

	case "FailSync":
		if h.writeTx == nil {
			return fmt.Errorf("FailSync with nil writeTx")
		}
		phase := h.writeTx.CommitPhase()
		h.storage.SetFailSync(true)
		var syncErr error
		switch phase {
		case "DATA_WRITTEN":
			syncErr = h.writeTx.SyncDataPages()
		case "META_WRITTEN":
			syncErr = h.writeTx.SyncMetaPage()
		default:
			h.storage.SetFailSync(false)
			return fmt.Errorf("FailSync in unexpected phase %q", phase)
		}
		h.storage.SetFailSync(false)
		if phase == "META_WRITTEN" && syncErr == nil {
			return fmt.Errorf("expected SyncMetaPage to fail when SetFailSync(true)")
		}
		tx := h.writeTx
		h.writeTx = nil
		if phase == "META_WRITTEN" && !step.ArgBool {
			h.storage.SetFailWrite(true)
		}
		rbErr := tx.Rollback()
		h.storage.SetFailWrite(false)
		return rbErr

	case "Crash":
		var mode engine.CrashMode
		switch step.ArgStr {
		case "DROP":
			mode = engine.CrashDropUnsynced
		case "REORDERED":
			mode = engine.CrashReorderedMetaOnly
		case "TORN":
			mode = engine.CrashTornMetaWrite
		default:
			return fmt.Errorf("unknown crash mode %q", step.ArgStr)
		}
		h.writeTx = nil
		h.readTx = nil
		h.storage = h.storage.SimulateCrashWithMode(mode, h.opts.PageSize)
		db, err := engine.OpenStorage(h.storage, h.opts)
		if err != nil {
			return fmt.Errorf("reopen after crash (%s): %w", step.ArgStr, err)
		}
		h.db = db
		return nil

	default:
		return fmt.Errorf("unhandled Quint action %q", step.ActionName)
	}
}

func (h *protocolHarness) assertMatchesAndInvariants(want storageProtocolState) error {
	// 1. Live B+Tree structural & checksum validation.
	if err := h.db.Validate(); err != nil {
		return fmt.Errorf("live Validate(): %w", err)
	}

	// 2. Exact protocol state agreement with Quint.
	if got := int64(h.db.Meta().TxID); got != int64(want.TxID) {
		return fmt.Errorf("txid mismatch: got=%d want=%d", got, want.TxID)
	}
	gotPhase := "IDLE"
	if h.writeTx != nil {
		gotPhase = h.writeTx.CommitPhase()
	}
	if gotPhase != want.Phase {
		return fmt.Errorf("phase mismatch: got=%q want=%q", gotPhase, want.Phase)
	}
	if got := h.db.HasDirtyAltMeta(); got != want.DirtyAltMeta {
		return fmt.Errorf("dirtyAltMeta mismatch: got=%v want=%v", got, want.DirtyAltMeta)
	}

	// 3. Committed KV value matches Quint.
	gotVal, err := readKeyOrNotFound(h.db, protocolKey)
	if err != nil {
		return fmt.Errorf("Get(%s): %w", protocolKey, err)
	}
	if gotVal != want.CommittedVal {
		return fmt.Errorf("committedVal mismatch: got=%q want=%q", gotVal, want.CommittedVal)
	}

	// 4. Active MVCC reader snapshot isolation matches Quint.
	if (h.readTx != nil) != want.HasReader {
		return fmt.Errorf("hasReader mismatch: got=%v want=%v", h.readTx != nil, want.HasReader)
	}
	if h.readTx != nil {
		if int64(h.readTx.TxID()) != int64(want.ReaderTxID) {
			return fmt.Errorf("readerTxID mismatch: got=%d want=%d", h.readTx.TxID(), want.ReaderTxID)
		}
		snapVal, err := readTxKeyOrNotFound(h.readTx, protocolKey)
		if err != nil {
			return fmt.Errorf("reader Get(%s): %w", protocolKey, err)
		}
		if snapVal != want.ReaderExpected {
			return fmt.Errorf("reader snapshot mismatch: got=%q want=%q", snapVal, want.ReaderExpected)
		}
	}

	// 5. Inspect durable disk image (CloneDurable) & verify BOTH durable meta slots match Quint!
	durableImg := h.storage.CloneDurable()
	durableDB, err := engine.OpenStorage(durableImg, h.opts)
	if err != nil {
		return fmt.Errorf("durable OpenStorage: %w", err)
	}
	defer durableDB.Close()

	if err := durableDB.Validate(); err != nil {
		return fmt.Errorf("durable Validate(): %w", err)
	}
	slot0, slot1, err := durableDB.InspectMeta()
	if err != nil {
		return fmt.Errorf("durable InspectMeta(): %w", err)
	}
	if err := checkDurableSlot(durableDB, "meta0", slot0, want.Meta0); err != nil {
		return err
	}
	if err := checkDurableSlot(durableDB, "meta1", slot1, want.Meta1); err != nil {
		return err
	}

	// 6. Zero use-after-free: live pages reachable from meta0, meta1, and open reader
	// must be strictly disjoint from freelist.free, and all pending buckets > ReleaseBound().
	livePages := make(map[engine.PageID]bool)
	liveSlot0, liveSlot1, err := h.db.InspectMeta()
	if err != nil {
		return err
	}
	for _, slot := range []engine.MetaSlot{liveSlot0, liveSlot1} {
		if !slot.Valid {
			continue
		}
		pages, err := h.db.ReachablePages(slot.Meta.Root, slot.Meta.Freelist)
		if err != nil {
			return fmt.Errorf("ReachablePages(txid=%d): %w", slot.Meta.TxID, err)
		}
		for _, id := range pages {
			livePages[id] = true
		}
	}
	if h.readTx != nil {
		pages, err := h.db.ReachablePages(h.readTx.RootPageID(), 0)
		if err != nil {
			return fmt.Errorf("ReachablePages(reader): %w", err)
		}
		for _, id := range pages {
			livePages[id] = true
		}
	}

	fl := h.db.FreelistState()
	for _, freeID := range fl.FreeIDs {
		if livePages[freeID] {
			return fmt.Errorf("MVCC/freelist violation: live page %d in freelist.free", freeID)
		}
	}
	bound := h.db.ReleaseBound()
	for _, pt := range fl.PendingTxIDs {
		if pt <= bound {
			return fmt.Errorf("freelist watermark violation: pending txid %d <= releaseBound %d", pt, bound)
		}
	}
	return nil
}

func readKeyOrNotFound(db *engine.DB, key string) (string, error) {
	val, err := db.Get([]byte(key))
	if errors.Is(err, engine.ErrNotFound) {
		return notFoundSentinel, nil
	}
	if err != nil {
		return "", err
	}
	return string(val), nil
}

func readTxKeyOrNotFound(tx *engine.Tx, key string) (string, error) {
	val, err := tx.Get([]byte(key))
	if errors.Is(err, engine.ErrNotFound) {
		return notFoundSentinel, nil
	}
	if err != nil {
		return "", err
	}
	return string(val), nil
}

func checkDurableSlot(db *engine.DB, name string, got engine.MetaSlot, want quintMetaSlot) error {
	if got.Valid != want.Valid {
		return fmt.Errorf("durable %s valid mismatch: got=%v want=%v", name, got.Valid, want.Valid)
	}
	if !want.Valid {
		return nil
	}
	if int64(got.Meta.TxID) != int64(want.TxID) {
		return fmt.Errorf("durable %s txid mismatch: got=%d want=%d", name, got.Meta.TxID, want.TxID)
	}
	m, err := db.ReadTreeAtRoot(got.Meta.Root)
	if err != nil {
		return fmt.Errorf("durable %s ReadTreeAtRoot: %w", name, err)
	}
	gotVal := notFoundSentinel
	if v, ok := m[protocolKey]; ok {
		gotVal = v
	}
	if gotVal != want.Expected {
		return fmt.Errorf("durable %s value mismatch: got=%q want=%q", name, gotVal, want.Expected)
	}
	return nil
}

func TestQuintStorageProtocolMBT(t *testing.T) {
	traces := generateITFTraces[storageProtocolState](t, "formal/quint/storage_protocol.qnt", 30, 40, "0x2026")

	kernels := []struct {
		name string
		core engine.Core
	}{
		{name: "GoCore", core: engine.GoCore()},
		{name: "VerifiedDafnyCore", core: verified.NewCore()},
	}

	for _, k := range kernels {
		t.Run(k.name, func(t *testing.T) {
			actionCounts := make(map[string]int)
			for traceIdx, tr := range traces {
				h, err := newProtocolHarness(k.core)
				if err != nil {
					t.Fatalf("trace %d init: %v", traceIdx, err)
				}
				if err := h.assertMatchesAndInvariants(tr.States[0]); err != nil {
					h.close()
					t.Fatalf("trace %d state 0: %v", traceIdx, err)
				}
				for stepIdx := 1; stepIdx < len(tr.States); stepIdx++ {
					st := tr.States[stepIdx]
					actionCounts[st.LastStep.ActionName]++
					if err := h.applyAction(st.LastStep); err != nil {
						h.close()
						t.Fatalf("trace %d step %d (%s arg=%q/%v res=%q): %v",
							traceIdx, stepIdx, st.LastStep.ActionName, st.LastStep.ArgStr, st.LastStep.ArgBool, st.LastStep.Result, err)
					}
					if err := h.assertMatchesAndInvariants(st); err != nil {
						h.close()
						t.Fatalf("trace %d step %d (%s arg=%q/%v res=%q): %v",
							traceIdx, stepIdx, st.LastStep.ActionName, st.LastStep.ArgStr, st.LastStep.ArgBool, st.LastStep.Result, err)
					}
				}
				h.close()
			}
			t.Logf("%s replayed %d Quint ITF traces cleanly: %v", k.name, len(traces), actionCounts)
		})
	}
}
