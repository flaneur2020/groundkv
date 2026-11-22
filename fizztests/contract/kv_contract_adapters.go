// Source: ../../models/contract/kv_contract.fizz
// Black-box contract adapter exercising GroundKV's public API with real
// concurrent goroutines (1 Writer + 2 lock-free Readers + 1 MVCC SnapshotReader).
package contract_mbt

import (
	"errors"
	"strings"
	"sync"

	mbt "github.com/fizzbee-io/fizzbee/mbt/lib/go"
	"github.com/flaneur2020/groundkv/engine"
)

const notFoundSentinel = "NOT_FOUND"

var errInvalidArg = errors.New("invalid argument from fizzbee mbt runner")

// WriterRoleAdapter implements the single serialized writer role.
type WriterRoleAdapter struct {
	mu sync.Mutex // Serializes Writer#0's own invocations; DB itself is accessed concurrently by Readers.
	db *engine.DB
}

var _ WriterRole = (*WriterRoleAdapter)(nil)

func (a *WriterRoleAdapter) ActionPut(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
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
	if err := a.db.Put([]byte(k), []byte(v)); err != nil {
		return nil, err
	}
	return "OK", nil
}

func (a *WriterRoleAdapter) ActionDelete(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(args) < 1 {
		return nil, errInvalidArg
	}
	k, ok := args[0].Value.(string)
	if !ok {
		return nil, errInvalidArg
	}
	if err := a.db.Delete([]byte(k)); err != nil {
		return nil, err
	}
	return "OK", nil
}

func (a *WriterRoleAdapter) ActionBatchSwap(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(args) < 1 {
		return nil, errInvalidArg
	}
	v, ok := args[0].Value.(string)
	if !ok {
		return nil, errInvalidArg
	}
	err := a.db.Update(func(tx *engine.Tx) error {
		if err := tx.Put([]byte("k1"), []byte(v)); err != nil {
			return err
		}
		return tx.Put([]byte("k2"), []byte(v))
	})
	if err != nil {
		return nil, err
	}
	return "OK", nil
}

func (a *WriterRoleAdapter) ActionAbortedPut(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
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
	if err := tx.Rollback(); err != nil {
		return nil, err
	}
	return "ABORTED", nil
}

// ReaderRoleAdapter executes lock-free concurrent reads directly against DB.
type ReaderRoleAdapter struct {
	db *engine.DB
}

var _ ReaderRole = (*ReaderRoleAdapter)(nil)

func (a *ReaderRoleAdapter) ActionGet(args []mbt.Arg) (any, error) {
	if len(args) < 1 {
		return nil, errInvalidArg
	}
	k, ok := args[0].Value.(string)
	if !ok {
		return nil, errInvalidArg
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

func (a *ReaderRoleAdapter) ActionScanAll(args []mbt.Arg) (any, error) {
	var parts []string
	err := a.db.View(func(tx *engine.Tx) error {
		return tx.Scan(nil, nil, func(k, v []byte) bool {
			parts = append(parts, string(k)+"="+string(v))
			return true
		})
	})
	if err != nil {
		return nil, err
	}
	return strings.Join(parts, ","), nil
}

// SnapshotReaderRoleAdapter pins a long-lived MVCC read snapshot across
// concurrent writer commits.
type SnapshotReaderRoleAdapter struct {
	mu   sync.Mutex // Protects only this reader's local snap handle, never DB.
	db   *engine.DB
	snap *engine.Tx
}

var _ SnapshotReaderRole = (*SnapshotReaderRoleAdapter)(nil)

func (a *SnapshotReaderRoleAdapter) close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.snap != nil {
		_ = a.snap.Rollback()
		a.snap = nil
	}
}

func (a *SnapshotReaderRoleAdapter) ActionBeginSnapshot(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.snap != nil {
		return "BUSY", nil
	}
	tx, err := a.db.BeginRead()
	if err != nil {
		return nil, err
	}
	a.snap = tx
	return "OK", nil
}

func (a *SnapshotReaderRoleAdapter) ActionSnapshotGet(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(args) < 1 {
		return nil, errInvalidArg
	}
	k, ok := args[0].Value.(string)
	if !ok {
		return nil, errInvalidArg
	}
	if a.snap == nil {
		return notFoundSentinel, nil
	}
	val, err := a.snap.Get([]byte(k))
	if errors.Is(err, engine.ErrNotFound) {
		return notFoundSentinel, nil
	}
	if err != nil {
		return nil, err
	}
	return string(val), nil
}

func (a *SnapshotReaderRoleAdapter) ActionEndSnapshot(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.snap == nil {
		return "NONE", nil
	}
	tx := a.snap
	a.snap = nil
	if err := tx.Rollback(); err != nil {
		return nil, err
	}
	return "OK", nil
}

// KvContractModelAdapter wires models/contract/kv_contract.fizz as a pure
// black-box contract (zero internal state coupling, parallel linearizability enabled).
type KvContractModelAdapter struct {
	db         *engine.DB
	writer     *WriterRoleAdapter
	reader0    *ReaderRoleAdapter
	reader1    *ReaderRoleAdapter
	snapReader *SnapshotReaderRoleAdapter
}

var _ KvContractModel = (*KvContractModelAdapter)(nil)

func NewKvContractModel() KvContractModel {
	return &KvContractModelAdapter{}
}

func GetTestOptions() map[string]any {
	return map[string]any{
		"max-seq-runs":      300,
		"max-parallel-runs": 200, // Real concurrent goroutine linearizability testing!
		"max-actions":       15,
	}
}

func (m *KvContractModelAdapter) GetState() (map[string]any, error) {
	// Pure black-box contract: validated strictly via public API return values
	// and concurrent linearizability.
	return nil, mbt.ErrNotImplemented
}

func (m *KvContractModelAdapter) GetRoles() (map[mbt.RoleId]mbt.Role, error) {
	roles := make(map[mbt.RoleId]mbt.Role)
	if m.writer != nil {
		roles[mbt.RoleId{RoleName: "Writer", Index: 0}] = m.writer
	}
	if m.reader0 != nil {
		roles[mbt.RoleId{RoleName: "Reader", Index: 0}] = m.reader0
	}
	if m.reader1 != nil {
		roles[mbt.RoleId{RoleName: "Reader", Index: 1}] = m.reader1
	}
	if m.snapReader != nil {
		roles[mbt.RoleId{RoleName: "SnapshotReader", Index: 0}] = m.snapReader
	}
	return roles, nil
}

func (m *KvContractModelAdapter) Init() error {
	st := engine.NewMemStorage()
	opts := &engine.Options{
		PageSize:      512,
		MaxLeafKeys:   2,
		MaxBranchKeys: 2,
	}
	db, err := engine.OpenStorage(st, opts)
	if err != nil {
		return err
	}
	m.db = db
	m.writer = &WriterRoleAdapter{db: db}
	m.reader0 = &ReaderRoleAdapter{db: db}
	m.reader1 = &ReaderRoleAdapter{db: db}
	m.snapReader = &SnapshotReaderRoleAdapter{db: db}
	return nil
}

func (m *KvContractModelAdapter) Cleanup() error {
	if m.snapReader != nil {
		m.snapReader.close()
		m.snapReader = nil
	}
	m.writer = nil
	m.reader0 = nil
	m.reader1 = nil
	if m.db != nil {
		err := m.db.Close()
		m.db = nil
		return err
	}
	return nil
}
