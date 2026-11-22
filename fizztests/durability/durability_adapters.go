// Source: ../../models/durability/durability.fizz
// FizzBee MBT adapter validating GroundKV's 4-phase commit durability protocol
// under drop-unsynced, reordered-meta-flush, and torn-meta-write power losses.
package durability_mbt

import (
	"errors"
	"fmt"
	"sync"

	mbt "github.com/fizzbee-io/fizzbee/mbt/lib/go"
	"github.com/flaneur2020/groundkv/engine"
)

const (
	phaseIdle        = "IDLE"
	phaseStaged      = "STAGED"
	phaseDataWritten = "DATA_WRITTEN"
	phaseDataSynced  = "DATA_SYNCED"
	phaseMetaWritten = "META_WRITTEN"
)

var errInvalidArg = errors.New("invalid argument from fizzbee mbt runner")

// DurabilityDBRoleAdapter drives GroundKV's real 4-phase Tx commit methods
// (WriteDataPages -> SyncDataPages -> WriteMetaPage -> SyncMetaPage) against a
// fault-injecting MemStorage and inspects durable on-disk meta pages after
// every step.
type DurabilityDBRoleAdapter struct {
	mu      sync.Mutex
	storage *engine.MemStorage
	db      *engine.DB
	opts    *engine.Options

	tx              *engine.Tx
	phase           string
	targetTxID      int64
	dataDurable     bool
	metaUnsynced    bool
	lastDurableTxID int64
	lastDurableMap  map[string]string
}

var (
	_ DurabilityDBRole = (*DurabilityDBRoleAdapter)(nil)
	_ mbt.StateGetter  = (*DurabilityDBRoleAdapter)(nil)
)

func newDurabilityDBRoleAdapter() (*DurabilityDBRoleAdapter, error) {
	st := engine.NewMemStorage()
	opts := &engine.Options{
		PageSize:      512,
		MaxLeafKeys:   2,
		MaxBranchKeys: 2,
	}
	db, err := engine.OpenStorage(st, opts)
	if err != nil {
		return nil, err
	}
	return &DurabilityDBRoleAdapter{
		storage:        st,
		db:             db,
		opts:           opts,
		phase:          phaseIdle,
		lastDurableMap: map[string]string{},
	}, nil
}

func (a *DurabilityDBRoleAdapter) close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tx != nil {
		_ = a.tx.Rollback()
		a.tx = nil
	}
	if a.db != nil {
		err := a.db.Close()
		a.db = nil
		return err
	}
	return nil
}

func (a *DurabilityDBRoleAdapter) resetTxState() {
	a.tx = nil
	a.phase = phaseIdle
	a.targetTxID = 0
	a.dataDurable = false
	a.metaUnsynced = false
}

func (a *DurabilityDBRoleAdapter) ActionBeginStage(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.phase != phaseIdle {
		return "BUSY", nil
	}
	if len(args) < 2 {
		return nil, errInvalidArg
	}
	k, ok := args[0].Value.(string)
	if !ok {
		return nil, errInvalidArg
	}
	v, ok := args[1].Value.(string)
	if !ok {
		return nil, errInvalidArg
	}

	tx, err := a.db.BeginWrite()
	if err != nil {
		return nil, err
	}
	if err := tx.Put([]byte(k), []byte(v)); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	a.tx = tx
	a.targetTxID = int64(a.db.Meta().TxID) + 1
	a.phase = phaseStaged
	a.dataDurable = false
	a.metaUnsynced = false
	return "OK", nil
}

func (a *DurabilityDBRoleAdapter) ActionWriteDataPages(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.phase != phaseStaged {
		return "SKIP", nil
	}
	if err := a.tx.WriteDataPages(); err != nil {
		return nil, err
	}
	a.phase = phaseDataWritten
	return "OK", nil
}

func (a *DurabilityDBRoleAdapter) ActionSyncDataPages(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.phase != phaseDataWritten {
		return "SKIP", nil
	}
	if err := a.tx.SyncDataPages(); err != nil {
		return nil, err
	}
	a.dataDurable = true
	a.phase = phaseDataSynced
	return "OK", nil
}

func (a *DurabilityDBRoleAdapter) ActionWriteMetaPage(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.phase != phaseDataSynced {
		return "SKIP", nil
	}
	if err := a.tx.WriteMetaPage(); err != nil {
		return nil, err
	}
	a.metaUnsynced = true
	a.phase = phaseMetaWritten
	return "OK", nil
}

func (a *DurabilityDBRoleAdapter) ActionSyncMetaPage(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.phase != phaseMetaWritten {
		return "SKIP", nil
	}
	if err := a.tx.SyncMetaPage(); err != nil {
		return nil, err
	}
	if err := a.db.Validate(); err != nil {
		return nil, fmt.Errorf("btree validation failed after SyncMetaPage: %w", err)
	}
	meta := a.db.Meta()
	committed, err := a.db.ReadTreeAtRoot(meta.Root)
	if err != nil {
		return nil, err
	}
	a.lastDurableTxID = int64(meta.TxID)
	a.lastDurableMap = committed
	a.resetTxState()
	return int64(meta.TxID), nil
}

func (a *DurabilityDBRoleAdapter) crashAndReopen(mode engine.CrashMode) (any, error) {
	a.resetTxState()
	recovered := a.storage.SimulateCrashWithMode(mode, a.opts.PageSize)
	a.storage = recovered

	db, err := engine.OpenStorage(a.storage, a.opts)
	if err != nil {
		return nil, fmt.Errorf("reopen after crash mode %d failed: %w", mode, err)
	}
	a.db = db
	if err := a.db.Validate(); err != nil {
		return nil, fmt.Errorf("btree validation failed after crash mode %d: %w", mode, err)
	}
	meta := a.db.Meta()
	committed, err := a.db.ReadTreeAtRoot(meta.Root)
	if err != nil {
		return nil, err
	}
	a.lastDurableTxID = int64(meta.TxID)
	a.lastDurableMap = committed
	return int64(meta.TxID), nil
}

func (a *DurabilityDBRoleAdapter) ActionPowerLossDropUnsynced(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.crashAndReopen(engine.CrashDropUnsynced)
}

func (a *DurabilityDBRoleAdapter) ActionPowerLossReorderedFlush(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.crashAndReopen(engine.CrashReorderedMetaOnly)
}

func (a *DurabilityDBRoleAdapter) ActionPowerLossTornMeta(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.crashAndReopen(engine.CrashTornMetaWrite)
}

// GetState inspects the durable on-disk image (via CloneDurable) to verify
// both meta slots, their referenced B+Tree roots, and active tx pipeline state
// against models/durability/durability.fizz.
func (a *DurabilityDBRoleAdapter) GetState() (map[string]any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	durableImg := a.storage.CloneDurable()
	durableDB, err := engine.OpenStorage(durableImg, a.opts)
	if err != nil {
		return nil, fmt.Errorf("durable image failed to open: %w", err)
	}
	defer durableDB.Close()

	slot0, slot1, err := durableDB.InspectMeta()
	if err != nil {
		return nil, err
	}

	meta0Data, meta0Durable := inspectSlotTree(durableDB, slot0)
	meta1Data, meta1Durable := inspectSlotTree(durableDB, slot1)

	activeMeta := a.db.Meta()
	committedMap, err := a.db.ReadTreeAtRoot(activeMeta.Root)
	if err != nil {
		return nil, err
	}

	stagedMap := map[string]string{}
	if a.tx != nil {
		stagedMap, err = a.tx.All()
		if err != nil {
			return nil, err
		}
	}

	return map[string]any{
		"meta0_valid":           slot0.Valid,
		"meta0_txid":            int64(slot0.Meta.TxID),
		"meta0_data":            toAnyMap(meta0Data),
		"meta0_data_durable":    meta0Durable,
		"meta1_valid":           slot1.Valid,
		"meta1_txid":            int64(slot1.Meta.TxID),
		"meta1_data":            toAnyMap(meta1Data),
		"meta1_data_durable":    meta1Durable,
		"txid":                  int64(activeMeta.TxID),
		"committed":             toAnyMap(committedMap),
		"phase":                 a.phase,
		"staged":                toAnyMap(stagedMap),
		"target_txid":           a.targetTxID,
		"data_durable_on_disk":  a.dataDurable,
		"meta_written_unsynced": a.metaUnsynced,
		"last_durable_txid":     a.lastDurableTxID,
		"last_durable_map":      toAnyMap(a.lastDurableMap),
	}, nil
}

func inspectSlotTree(db *engine.DB, slot engine.MetaSlot) (map[string]string, bool) {
	if !slot.Valid {
		return map[string]string{}, false
	}
	data, err := db.ReadTreeAtRoot(slot.Meta.Root)
	if err != nil {
		return map[string]string{}, false
	}
	return data, true
}

func toAnyMap(in map[string]string) map[any]any {
	out := make(map[any]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// DurabilityModelAdapter wires models/durability/durability.fizz to FizzBee MBT.
type DurabilityModelAdapter struct {
	dbRole *DurabilityDBRoleAdapter
}

var _ DurabilityModel = (*DurabilityModelAdapter)(nil)

func NewDurabilityModel() DurabilityModel {
	return &DurabilityModelAdapter{}
}

func GetTestOptions() map[string]any {
	return map[string]any{
		"max-seq-runs":      500,
		"max-parallel-runs": 0,
		"max-actions":       20,
	}
}

func (m *DurabilityModelAdapter) GetState() (map[string]any, error) {
	return nil, mbt.ErrNotImplemented
}

func (m *DurabilityModelAdapter) GetRoles() (map[mbt.RoleId]mbt.Role, error) {
	roles := make(map[mbt.RoleId]mbt.Role)
	if m.dbRole != nil {
		roles[mbt.RoleId{RoleName: "DurabilityDB", Index: 0}] = m.dbRole
	}
	return roles, nil
}

func (m *DurabilityModelAdapter) Init() error {
	role, err := newDurabilityDBRoleAdapter()
	if err != nil {
		return err
	}
	m.dbRole = role
	return nil
}

func (m *DurabilityModelAdapter) Cleanup() error {
	if m.dbRole != nil {
		err := m.dbRole.close()
		m.dbRole = nil
		return err
	}
	return nil
}
