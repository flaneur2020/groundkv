// Source: ../../models/kv/kv.fizz
// FizzBee MBT adapter wiring models/kv/kv.fizz to the real GroundKV engine.
package kv_mbt

import (
	"errors"
	"fmt"
	"sync"

	mbt "github.com/fizzbee-io/fizzbee/mbt/lib/go"
	"github.com/flaneur2020/groundkv/engine"
)

const notFoundSentinel = "NOT_FOUND"

var errInvalidArg = errors.New("invalid argument from fizzbee mbt runner")

// DBRoleAdapter connects the formal DB role in specs/kv/kv.fizz to a live
// GroundKV engine backed by crash-simulatable MemStorage.
type DBRoleAdapter struct {
	mu      sync.Mutex
	storage *engine.MemStorage
	db      *engine.DB
	opts    *engine.Options

	readTx  *engine.Tx
	writeTx *engine.Tx
}

// Assert that DBRoleAdapter satisfies DBRole and mbt.StateGetter.
var (
	_ DBRole          = (*DBRoleAdapter)(nil)
	_ mbt.StateGetter = (*DBRoleAdapter)(nil)
)

func newDBRoleAdapter() (*DBRoleAdapter, error) {
	st := engine.NewMemStorage()
	opts := &engine.Options{
		PageSize:      512, // Small pages + low fanout caps exercise real splits on tiny MBT domains
		MaxLeafKeys:   2,
		MaxBranchKeys: 2,
	}
	db, err := engine.OpenStorage(st, opts)
	if err != nil {
		return nil, err
	}
	return &DBRoleAdapter{
		storage: st,
		db:      db,
		opts:    opts,
	}, nil
}

func (a *DBRoleAdapter) close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.readTx != nil {
		_ = a.readTx.Rollback()
		a.readTx = nil
	}
	if a.writeTx != nil {
		_ = a.writeTx.Rollback()
		a.writeTx = nil
	}
	if a.db != nil {
		err := a.db.Close()
		a.db = nil
		return err
	}
	return nil
}

func (a *DBRoleAdapter) ActionPut(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.writeTx != nil {
		return "BUSY", nil
	}
	k, v, err := parseKVArgs(args)
	if err != nil {
		return nil, err
	}
	if err := a.db.Put([]byte(k), []byte(v)); err != nil {
		return nil, err
	}
	if err := a.db.Validate(); err != nil {
		return nil, fmt.Errorf("btree validation failed after Put: %w", err)
	}
	return "OK", nil
}

func (a *DBRoleAdapter) ActionDelete(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.writeTx != nil {
		return "BUSY", nil
	}
	k, err := parseKeyArg(args)
	if err != nil {
		return nil, err
	}
	if err := a.db.Delete([]byte(k)); err != nil {
		return nil, err
	}
	if err := a.db.Validate(); err != nil {
		return nil, fmt.Errorf("btree validation failed after Delete: %w", err)
	}
	return "OK", nil
}

func (a *DBRoleAdapter) ActionGet(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	k, err := parseKeyArg(args)
	if err != nil {
		return nil, err
	}
	val, err := a.db.Get([]byte(k))
	if errors.Is(err, engine.ErrNotFound) {
		return notFoundSentinel, nil
	}
	if err != nil {
		return nil, err
	}
	return string(val), nil
}

func (a *DBRoleAdapter) ActionBeginRead(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.readTx != nil {
		return "BUSY", nil
	}
	tx, err := a.db.BeginRead()
	if err != nil {
		return nil, err
	}
	a.readTx = tx
	return "OK", nil
}

func (a *DBRoleAdapter) ActionSnapshotGet(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.readTx == nil {
		return notFoundSentinel, nil
	}
	k, err := parseKeyArg(args)
	if err != nil {
		return nil, err
	}
	val, err := a.readTx.Get([]byte(k))
	if errors.Is(err, engine.ErrNotFound) {
		return notFoundSentinel, nil
	}
	if err != nil {
		return nil, err
	}
	return string(val), nil
}

func (a *DBRoleAdapter) ActionEndRead(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.readTx == nil {
		return "NONE", nil
	}
	err := a.readTx.Rollback()
	a.readTx = nil
	if err != nil {
		return nil, err
	}
	return "OK", nil
}

func (a *DBRoleAdapter) ActionBeginWrite(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.writeTx != nil {
		return "BUSY", nil
	}
	tx, err := a.db.BeginWrite()
	if errors.Is(err, engine.ErrWriteTxActive) {
		return "BUSY", nil
	}
	if err != nil {
		return nil, err
	}
	a.writeTx = tx
	return "OK", nil
}

func (a *DBRoleAdapter) ActionStagePut(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.writeTx == nil {
		return "NO_TX", nil
	}
	k, v, err := parseKVArgs(args)
	if err != nil {
		return nil, err
	}
	if err := a.writeTx.Put([]byte(k), []byte(v)); err != nil {
		return nil, err
	}
	return "OK", nil
}

func (a *DBRoleAdapter) ActionCommitWrite(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.writeTx == nil {
		return "NO_TX", nil
	}
	tx := a.writeTx
	a.writeTx = nil
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if err := a.db.Validate(); err != nil {
		return nil, fmt.Errorf("btree validation failed after CommitWrite: %w", err)
	}
	return "OK", nil
}

func (a *DBRoleAdapter) ActionRollbackWrite(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.writeTx == nil {
		return "NO_TX", nil
	}
	tx := a.writeTx
	a.writeTx = nil
	if err := tx.Rollback(); err != nil {
		return nil, err
	}
	return "OK", nil
}

func (a *DBRoleAdapter) ActionCrashAndReopen(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Simulate an abrupt process crash: discard in-memory transactions and unsynced writes.
	a.readTx = nil
	a.writeTx = nil
	recoveredStorage := a.storage.SimulateCrash()
	a.storage = recoveredStorage

	db, err := engine.OpenStorage(a.storage, a.opts)
	if err != nil {
		return nil, fmt.Errorf("reopen after crash failed: %w", err)
	}
	a.db = db
	if err := a.db.Validate(); err != nil {
		return nil, fmt.Errorf("btree validation failed after CrashAndReopen: %w", err)
	}
	return int64(a.db.Meta().TxID), nil
}

// GetState exposes the real engine's dual meta pages, committed B+Tree view,
// active MVCC snapshot, and staged write transaction back to FizzBee MBT so
// every transition is verified against the formal model state.
func (a *DBRoleAdapter) GetState() (map[string]any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	meta0, meta1, err := a.db.DualMeta()
	if err != nil {
		return nil, err
	}
	meta0Map, err := a.db.ReadTreeAtRoot(meta0.Root)
	if err != nil {
		return nil, err
	}
	meta1Map, err := a.db.ReadTreeAtRoot(meta1.Root)
	if err != nil {
		return nil, err
	}
	activeMeta := a.db.Meta()
	committedMap, err := a.db.ReadTreeAtRoot(activeMeta.Root)
	if err != nil {
		return nil, err
	}

	snapMap := map[string]string{}
	if a.readTx != nil {
		snapMap, err = a.readTx.All()
		if err != nil {
			return nil, err
		}
	}

	stagedMap := map[string]string{}
	if a.writeTx != nil {
		stagedMap, err = a.writeTx.All()
		if err != nil {
			return nil, err
		}
	}

	return map[string]any{
		"txid":         int64(activeMeta.TxID),
		"meta0_txid":   int64(meta0.TxID),
		"meta1_txid":   int64(meta1.TxID),
		"meta0":        toAnyMap(meta0Map),
		"meta1":        toAnyMap(meta1Map),
		"committed":    toAnyMap(committedMap),
		"has_snapshot": a.readTx != nil,
		"snapshot":     toAnyMap(snapMap),
		"in_write_tx":  a.writeTx != nil,
		"staged":       toAnyMap(stagedMap),
	}, nil
}

func toAnyMap(in map[string]string) map[any]any {
	out := make(map[any]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func parseKeyArg(args []mbt.Arg) (string, error) {
	if len(args) < 1 {
		return "", errInvalidArg
	}
	k, ok := args[0].Value.(string)
	if !ok {
		return "", errInvalidArg
	}
	return k, nil
}

func parseKVArgs(args []mbt.Arg) (string, string, error) {
	if len(args) < 2 {
		return "", "", errInvalidArg
	}
	k, ok := args[0].Value.(string)
	if !ok {
		return "", "", errInvalidArg
	}
	v, ok := args[1].Value.(string)
	if !ok {
		return "", "", errInvalidArg
	}
	return k, v, nil
}

// KvModelAdapter wires the top-level specs/kv/kv.fizz model.
type KvModelAdapter struct {
	dbRole *DBRoleAdapter
}

// Assert that KvModelAdapter satisfies KvModel.
var _ KvModel = (*KvModelAdapter)(nil)

// NewKvModel constructs a KvModelAdapter for FizzBee MBT.
func NewKvModel() KvModel {
	return &KvModelAdapter{}
}

func GetTestOptions() map[string]any {
	return map[string]any{
		"max-seq-runs":      500,
		"max-parallel-runs": 0, // Single-writer model; concurrency is modeled via explicit BeginRead/BeginWrite interleavings
		"max-actions":       20,
	}
}

func (m *KvModelAdapter) GetState() (map[string]any, error) {
	// All state is verified on DBRoleAdapter.GetState().
	return nil, mbt.ErrNotImplemented
}

func (m *KvModelAdapter) GetRoles() (map[mbt.RoleId]mbt.Role, error) {
	roles := make(map[mbt.RoleId]mbt.Role)
	if m.dbRole != nil {
		roles[mbt.RoleId{RoleName: "DB", Index: 0}] = m.dbRole
	}
	return roles, nil
}

func (m *KvModelAdapter) Init() error {
	role, err := newDBRoleAdapter()
	if err != nil {
		return err
	}
	m.dbRole = role
	return nil
}

func (m *KvModelAdapter) Cleanup() error {
	if m.dbRole != nil {
		err := m.dbRole.close()
		m.dbRole = nil
		return err
	}
	return nil
}
