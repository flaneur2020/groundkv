// Source: ../../../formal/fizzbee/storage_protocol/storage_protocol.fizz
// Unified white-box storage protocol adapter driving real 4-phase commits,
// MVCC reader-aware freelist reclamation, and 3 disk crash modes with ZERO
// shadow state on the adapter (every checked invariant is observed directly
// from engine.DB, engine.Tx, and MemStorage.CloneDurable).
package storage_protocol_mbt

import (
	"errors"
	"fmt"
	"sync"

	mbt "github.com/fizzbee-io/fizzbee/mbt/lib/go"
	"github.com/flaneur2020/groundkv/engine"
)

const (
	notFoundSentinel = "NOT_FOUND"
	protocolKey      = "k1"
)

var errInvalidArg = errors.New("invalid argument from fizzbee mbt runner")

// sharedHarness holds ONLY live SUT handles (no shadow bookkeeping variables).
type sharedHarness struct {
	mu      sync.Mutex
	storage *engine.MemStorage
	db      *engine.DB
	opts    *engine.Options
	writeTx *engine.Tx
	readTx  *engine.Tx
}

func newSharedHarness() (*sharedHarness, error) {
	st := engine.NewMemStorage()
	opts := &engine.Options{
		PageSize:       512,
		MaxLeafKeys:    2,
		MaxBranchKeys:  2,
		MaxFreelistIDs: 3,     // Force multi-page linked freelist chains (next != 0)
		NoFreelist:     false, // Exercise real on-disk freelist + MVCC reclamation under crashes
	}
	db, err := engine.OpenStorage(st, opts)
	if err != nil {
		return nil, err
	}
	return &sharedHarness{
		storage: st,
		db:      db,
		opts:    opts,
	}, nil
}

func (h *sharedHarness) close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.readTx != nil {
		_ = h.readTx.Rollback()
		h.readTx = nil
	}
	if h.writeTx != nil {
		_ = h.writeTx.Rollback()
		h.writeTx = nil
	}
	if h.db != nil {
		err := h.db.Close()
		h.db = nil
		return err
	}
	return nil
}

// verifyEngineInvariantsLocked asserts directly against the real engine and
// durable storage that:
//  1. The live B+Tree passes structural & checksum Validate().
//  2. The durable disk image opens cleanly and passes Validate().
//  3. NO page reachable from meta0, meta1, active root, or open MVCC reader
//     appears in freelist.free, and no pending bucket sits <= ReleaseBound().
func (h *sharedHarness) verifyEngineInvariantsLocked() error {
	if err := h.db.Validate(); err != nil {
		return fmt.Errorf("live btree Validate() failed: %w", err)
	}

	durableImg := h.storage.CloneDurable()
	durableDB, err := engine.OpenStorage(durableImg, h.opts)
	if err != nil {
		return fmt.Errorf("durable disk image failed to open: %w", err)
	}
	if err := durableDB.Validate(); err != nil {
		_ = durableDB.Close()
		return fmt.Errorf("durable disk image failed Validate(): %w", err)
	}
	_ = durableDB.Close()

	livePages := make(map[engine.PageID]bool)
	slot0, slot1, err := h.db.InspectMeta()
	if err != nil {
		return err
	}
	for _, slot := range []engine.MetaSlot{slot0, slot1} {
		if !slot.Valid {
			continue
		}
		pages, err := h.db.ReachablePages(slot.Meta.Root, slot.Meta.Freelist)
		if err != nil {
			return fmt.Errorf("reachable pages for meta txid=%d: %w", slot.Meta.TxID, err)
		}
		for _, id := range pages {
			livePages[id] = true
		}
	}
	if h.readTx != nil {
		pages, err := h.db.ReachablePages(h.readTx.RootPageID(), 0)
		if err != nil {
			return fmt.Errorf("reachable pages for open reader: %w", err)
		}
		for _, id := range pages {
			livePages[id] = true
		}
	}

	fl := h.db.FreelistState()
	for _, freeID := range fl.FreeIDs {
		if livePages[freeID] {
			return fmt.Errorf("MVCC/freelist violation: live page %d found in freelist.free", freeID)
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

// DiskRoleAdapter injects power-loss faults on MemStorage and inspects durable
// meta slots directly from MemStorage.CloneDurable().
type DiskRoleAdapter struct {
	h *sharedHarness
}

var _ DiskRole = (*DiskRoleAdapter)(nil)

func (a *DiskRoleAdapter) ActionCrash(args []mbt.Arg) (any, error) {
	a.h.mu.Lock()
	defer a.h.mu.Unlock()
	if len(args) < 1 {
		return nil, errInvalidArg
	}
	modeStr, ok := args[0].Value.(string)
	if !ok {
		return nil, errInvalidArg
	}

	var mode engine.CrashMode
	switch modeStr {
	case "DROP":
		mode = engine.CrashDropUnsynced
	case "REORDERED":
		mode = engine.CrashReorderedMetaOnly
	case "TORN":
		mode = engine.CrashTornMetaWrite
	default:
		return nil, fmt.Errorf("%w: unknown crash mode %q", errInvalidArg, modeStr)
	}

	// Abrupt crash discards in-memory handles and unsynced writes per CrashMode.
	a.h.writeTx = nil
	a.h.readTx = nil
	a.h.storage = a.h.storage.SimulateCrashWithMode(mode, a.h.opts.PageSize)

	db, err := engine.OpenStorage(a.h.storage, a.h.opts)
	if err != nil {
		return nil, fmt.Errorf("reopen after crash (%s) failed: %w", modeStr, err)
	}
	a.h.db = db
	if err := a.h.verifyEngineInvariantsLocked(); err != nil {
		return nil, err
	}
	return int64(a.h.db.Meta().TxID), nil
}

func (a *DiskRoleAdapter) ActionFailSync(args []mbt.Arg) (any, error) {
	a.h.mu.Lock()
	defer a.h.mu.Unlock()
	invalOK := true
	if len(args) >= 1 {
		b, ok := args[0].Value.(bool)
		if !ok {
			return nil, errInvalidArg
		}
		invalOK = b
	}
	if a.h.writeTx == nil {
		return "SKIP", nil
	}
	phase := a.h.writeTx.CommitPhase()
	if phase != "DATA_WRITTEN" && phase != "META_WRITTEN" {
		return "SKIP", nil
	}

	// Inject a real Sync() I/O failure on the live process, verify Sync returns
	// an error, then rollback cleanly (optionally injecting a double-fault
	// WriteAt failure during Rollback neutralization when invalOK=false so
	// db.HasDirtyAltMeta() and the SyncDataPages() fence are exercised by MBT!).
	a.h.storage.SetFailSync(true)
	var syncErr error
	if phase == "DATA_WRITTEN" {
		syncErr = a.h.writeTx.SyncDataPages()
	} else {
		syncErr = a.h.writeTx.SyncMetaPage()
	}
	a.h.storage.SetFailSync(false)

	if phase == "META_WRITTEN" && syncErr == nil {
		return nil, fmt.Errorf("expected SyncMetaPage to fail when SetFailSync(true)")
	}
	tx := a.h.writeTx
	a.h.writeTx = nil
	if phase == "META_WRITTEN" && !invalOK {
		a.h.storage.SetFailWrite(true)
	}
	rollbackErr := tx.Rollback()
	a.h.storage.SetFailWrite(false)
	if rollbackErr != nil {
		return nil, fmt.Errorf("rollback after sync failure failed: %w", rollbackErr)
	}
	if phase == "META_WRITTEN" && !invalOK && !a.h.db.HasDirtyAltMeta() {
		return nil, fmt.Errorf("expected db.HasDirtyAltMeta()=true after double-fault FailSync(inval_ok=false)")
	}
	if err := a.h.verifyEngineInvariantsLocked(); err != nil {
		return nil, err
	}
	return "SYNC_FAILED", nil
}

func (a *DiskRoleAdapter) ActionInspectMeta(args []mbt.Arg) (any, error) {
	a.h.mu.Lock()
	defer a.h.mu.Unlock()

	durableImg := a.h.storage.CloneDurable()
	durableDB, err := engine.OpenStorage(durableImg, a.h.opts)
	if err != nil {
		return nil, fmt.Errorf("durable disk image failed to open: %w", err)
	}
	defer durableDB.Close()

	if err := durableDB.Validate(); err != nil {
		return nil, fmt.Errorf("durable disk image failed Validate(): %w", err)
	}

	slot0, slot1, err := durableDB.InspectMeta()
	if err != nil {
		return nil, err
	}
	meta0Val, err := readSlotKey(durableDB, slot0, protocolKey)
	if err != nil {
		return nil, fmt.Errorf("durable meta0 root unreadable: %w", err)
	}
	meta1Val, err := readSlotKey(durableDB, slot1, protocolKey)
	if err != nil {
		return nil, fmt.Errorf("durable meta1 root unreadable: %w", err)
	}

	m0v, m1v := 0, 0
	if slot0.Valid {
		m0v = 1
	}
	if slot1.Valid {
		m1v = 1
	}
	return fmt.Sprintf("m0=%d:%d:%s|m1=%d:%d:%s",
		m0v, slot0.Meta.TxID, meta0Val,
		m1v, slot1.Meta.TxID, meta1Val,
	), nil
}

func readSlotKey(db *engine.DB, slot engine.MetaSlot, key string) (string, error) {
	if !slot.Valid {
		return notFoundSentinel, nil
	}
	m, err := db.ReadTreeAtRoot(slot.Meta.Root)
	if err != nil {
		return "", err
	}
	if v, ok := m[key]; ok {
		return v, nil
	}
	return notFoundSentinel, nil
}

// EngineRoleAdapter drives GroundKV's real 4-phase commit, rollback, and MVCC
// snapshot operations directly.
type EngineRoleAdapter struct {
	h *sharedHarness
}

var _ EngineRole = (*EngineRoleAdapter)(nil)

func (a *EngineRoleAdapter) ActionBeginPut(args []mbt.Arg) (any, error) {
	a.h.mu.Lock()
	defer a.h.mu.Unlock()
	if a.h.writeTx != nil {
		return "BUSY", nil
	}
	tx, err := a.h.db.BeginWrite()
	if err != nil {
		return nil, err
	}
	nextVal := fmt.Sprintf("v%d", a.h.db.Meta().TxID+1)
	if err := tx.Put([]byte(protocolKey), []byte(nextVal)); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	a.h.writeTx = tx
	return "OK", nil
}

func (a *EngineRoleAdapter) ActionStageOverwrite(args []mbt.Arg) (any, error) {
	a.h.mu.Lock()
	defer a.h.mu.Unlock()
	if a.h.writeTx == nil || a.h.writeTx.CommitPhase() != "STAGED" {
		return "NO_TX", nil
	}
	nextVal := fmt.Sprintf("w%d", a.h.db.Meta().TxID+1)
	if err := a.h.writeTx.Put([]byte(protocolKey), []byte(nextVal)); err != nil {
		return nil, err
	}
	// Verify read-your-own-writes inside the uncommitted write tx.
	got, err := a.h.writeTx.Get([]byte(protocolKey))
	if err != nil || string(got) != nextVal {
		return nil, fmt.Errorf("read-your-own-writes mismatch in StageOverwrite: got=%q want=%q err=%v", got, nextVal, err)
	}
	return nextVal, nil
}

func (a *EngineRoleAdapter) ActionBeginDelete(args []mbt.Arg) (any, error) {
	a.h.mu.Lock()
	defer a.h.mu.Unlock()
	if a.h.writeTx != nil {
		return "BUSY", nil
	}
	tx, err := a.h.db.BeginWrite()
	if err != nil {
		return nil, err
	}
	if err := tx.Delete([]byte(protocolKey)); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	a.h.writeTx = tx
	return "OK", nil
}

func (a *EngineRoleAdapter) ActionCommitStep(args []mbt.Arg) (any, error) {
	a.h.mu.Lock()
	defer a.h.mu.Unlock()
	if a.h.writeTx == nil {
		return "IDLE", nil
	}
	switch a.h.writeTx.CommitPhase() {
	case "STAGED":
		if err := a.h.writeTx.WriteDataPages(); err != nil {
			return nil, err
		}
		if err := a.h.verifyEngineInvariantsLocked(); err != nil {
			return nil, err
		}
		return "DATA_WRITTEN", nil
	case "DATA_WRITTEN":
		if err := a.h.writeTx.SyncDataPages(); err != nil {
			return nil, err
		}
		if err := a.h.verifyEngineInvariantsLocked(); err != nil {
			return nil, err
		}
		return "DATA_SYNCED", nil
	case "DATA_SYNCED":
		if err := a.h.writeTx.WriteMetaPage(); err != nil {
			return nil, err
		}
		if err := a.h.verifyEngineInvariantsLocked(); err != nil {
			return nil, err
		}
		return "META_WRITTEN", nil
	case "META_WRITTEN":
		tx := a.h.writeTx
		a.h.writeTx = nil
		if err := tx.SyncMetaPage(); err != nil {
			return nil, err
		}
		if err := a.h.verifyEngineInvariantsLocked(); err != nil {
			return nil, err
		}
		return "COMMITTED", nil
	default:
		return "IDLE", nil
	}
}

func (a *EngineRoleAdapter) ActionRollbackWrite(args []mbt.Arg) (any, error) {
	a.h.mu.Lock()
	defer a.h.mu.Unlock()
	if a.h.writeTx == nil || a.h.writeTx.CommitPhase() != "STAGED" {
		return "NO_TX", nil
	}
	tx := a.h.writeTx
	a.h.writeTx = nil
	if err := tx.Rollback(); err != nil {
		return nil, err
	}
	if err := a.h.verifyEngineInvariantsLocked(); err != nil {
		return nil, err
	}
	return "OK", nil
}

func (a *EngineRoleAdapter) ActionOpenReader(args []mbt.Arg) (any, error) {
	a.h.mu.Lock()
	defer a.h.mu.Unlock()
	if a.h.readTx != nil {
		return int64(-1), nil
	}
	tx, err := a.h.db.BeginRead()
	if err != nil {
		return nil, err
	}
	a.h.readTx = tx
	return int64(tx.TxID()), nil
}

func (a *EngineRoleAdapter) ActionCloseReader(args []mbt.Arg) (any, error) {
	a.h.mu.Lock()
	defer a.h.mu.Unlock()
	if a.h.readTx == nil {
		return "NONE", nil
	}
	tx := a.h.readTx
	a.h.readTx = nil
	if err := tx.Rollback(); err != nil {
		return nil, err
	}
	if err := a.h.verifyEngineInvariantsLocked(); err != nil {
		return nil, err
	}
	return "OK", nil
}

func (a *EngineRoleAdapter) ActionRead(args []mbt.Arg) (any, error) {
	a.h.mu.Lock()
	defer a.h.mu.Unlock()
	val, err := a.h.db.Get([]byte(protocolKey))
	if errors.Is(err, engine.ErrNotFound) {
		return notFoundSentinel, nil
	}
	if err != nil {
		return nil, err
	}
	return string(val), nil
}

func (a *EngineRoleAdapter) ActionReadSnapshot(args []mbt.Arg) (any, error) {
	a.h.mu.Lock()
	defer a.h.mu.Unlock()
	if a.h.readTx == nil {
		return notFoundSentinel, nil
	}
	val, err := a.h.readTx.Get([]byte(protocolKey))
	if errors.Is(err, engine.ErrNotFound) {
		return notFoundSentinel, nil
	}
	if err != nil {
		return nil, err
	}
	return string(val), nil
}

// StorageProtocolModelAdapter wires formal/fizzbee/storage_protocol/storage_protocol.fizz.
type StorageProtocolModelAdapter struct {
	harness    *sharedHarness
	diskRole   *DiskRoleAdapter
	engineRole *EngineRoleAdapter
}

var _ StorageProtocolModel = (*StorageProtocolModelAdapter)(nil)

func NewStorageProtocolModel() StorageProtocolModel {
	return &StorageProtocolModelAdapter{}
}

func GetTestOptions() map[string]any {
	return map[string]any{
		"max-seq-runs":      500,
		"max-parallel-runs": 0, // Deterministic fault-injection ordering across Disk & Engine
		"max-actions":       20,
	}
}

func (m *StorageProtocolModelAdapter) GetState() (map[string]any, error) {
	return nil, mbt.ErrNotImplemented
}

func (m *StorageProtocolModelAdapter) GetRoles() (map[mbt.RoleId]mbt.Role, error) {
	roles := make(map[mbt.RoleId]mbt.Role)
	if m.diskRole != nil {
		roles[mbt.RoleId{RoleName: "Disk", Index: 0}] = m.diskRole
	}
	if m.engineRole != nil {
		roles[mbt.RoleId{RoleName: "Engine", Index: 0}] = m.engineRole
	}
	return roles, nil
}

func (m *StorageProtocolModelAdapter) Init() error {
	h, err := newSharedHarness()
	if err != nil {
		return err
	}
	m.harness = h
	m.diskRole = &DiskRoleAdapter{h: h}
	m.engineRole = &EngineRoleAdapter{h: h}
	return nil
}

func (m *StorageProtocolModelAdapter) Cleanup() error {
	m.diskRole = nil
	m.engineRole = nil
	if m.harness != nil {
		err := m.harness.close()
		m.harness = nil
		return err
	}
	return nil
}
