// Source: ../../models/contract/kv_contract.fizz
// Black-box contract adapter exercising GroundKV's public API on real POSIX
// OSFileStorage with real concurrent goroutines (1 Writer + 2 lock-free
// Readers + 1 MVCC SnapshotReader), binary keys, near-page-size values
// (triggering byte-capacity splits), and multi-page freelist chains.
package contract_mbt

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	mbt "github.com/fizzbee-io/fizzbee/mbt/lib/go"
	"github.com/flaneur2020/groundkv/engine"
)

const notFoundSentinel = "NOT_FOUND"

var errInvalidArg = errors.New("invalid argument from fizzbee mbt runner")

// Domain mapping: maps abstract model keys/values to binary keys (including
// NUL/0xFF bytes) and near-page-size values (80B and 120B on PageSize=256)
// so MBT exercises byte-capacity page splits and binary key ordering!
var (
	keyToBytes = map[string][]byte{
		"k1": []byte("\x00key-alpha"),
		"k2": []byte("key-\xff-beta"),
	}
	bytesToKey = map[string]string{
		"\x00key-alpha": "k1",
		"key-\xff-beta": "k2",
	}
	valToBytes = map[string][]byte{
		"v1": []byte("v1:" + strings.Repeat("A", 77)),
		"v2": []byte("v2:" + strings.Repeat("B", 117)),
	}
	bytesToVal = map[string]string{
		"v1:" + strings.Repeat("A", 77):  "v1",
		"v2:" + strings.Repeat("B", 117): "v2",
	}
)

func encodeKey(k string) ([]byte, error) {
	b, ok := keyToBytes[k]
	if !ok {
		return nil, fmt.Errorf("%w: unknown key %q", errInvalidArg, k)
	}
	return b, nil
}

func encodeVal(v string) ([]byte, error) {
	b, ok := valToBytes[v]
	if !ok {
		return nil, fmt.Errorf("%w: unknown value %q", errInvalidArg, v)
	}
	return b, nil
}

func decodeVal(b []byte) (string, error) {
	v, ok := bytesToVal[string(b)]
	if !ok {
		return "", fmt.Errorf("unexpected decoded value length %d", len(b))
	}
	return v, nil
}

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
	kb, err := encodeKey(k)
	if err != nil {
		return nil, err
	}
	vb, err := encodeVal(v)
	if err != nil {
		return nil, err
	}
	if err := a.db.Put(kb, vb); err != nil {
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
	kb, err := encodeKey(k)
	if err != nil {
		return nil, err
	}
	if err := a.db.Delete(kb); err != nil {
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
	k1, _ := encodeKey("k1")
	k2, _ := encodeKey("k2")
	vb, err := encodeVal(v)
	if err != nil {
		return nil, err
	}
	tmpVal := []byte("v1:" + strings.Repeat("A", 77))

	// Exercise same-tx overwrite (dirty-page replacement) + read-your-own-writes
	// inside one atomic multi-key Update() transaction!
	err = a.db.Update(func(tx *engine.Tx) error {
		if err := tx.Put(k1, tmpVal); err != nil {
			return err
		}
		if err := tx.Put(k1, vb); err != nil {
			return err
		}
		got, err := tx.Get(k1)
		if err != nil || string(got) != string(vb) {
			return fmt.Errorf("read-your-own-writes mismatch in BatchSwap: got=%q err=%v", got, err)
		}
		return tx.Put(k2, vb)
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
	kb, err := encodeKey(k)
	if err != nil {
		return nil, err
	}
	vb, err := encodeVal(v)
	if err != nil {
		return nil, err
	}
	tx, err := a.db.BeginWrite()
	if err != nil {
		return nil, err
	}
	if err := tx.Put(kb, vb); err != nil {
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
	kb, err := encodeKey(k)
	if err != nil {
		return nil, err
	}
	val, err := a.db.Get(kb)
	if errors.Is(err, engine.ErrNotFound) {
		return notFoundSentinel, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeVal(val)
}

func (a *ReaderRoleAdapter) ActionScanAll(args []mbt.Arg) (any, error) {
	var parts []string
	err := a.db.View(func(tx *engine.Tx) error {
		return tx.Scan(nil, nil, func(k, v []byte) bool {
			ak, okK := bytesToKey[string(k)]
			av, okV := bytesToVal[string(v)]
			if okK && okV {
				parts = append(parts, ak+"="+av)
			}
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
	kb, err := encodeKey(k)
	if err != nil {
		return nil, err
	}
	val, err := a.snap.Get(kb)
	if errors.Is(err, engine.ErrNotFound) {
		return notFoundSentinel, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeVal(val)
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
// black-box contract on real OSFileStorage with parallel linearizability enabled.
type KvContractModelAdapter struct {
	dir        string
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
		"max-seq-runs":      150,
		"max-parallel-runs": 100, // Real concurrent goroutine linearizability on synced OSFileStorage!
		"max-actions":       15,
	}
}

func (m *KvContractModelAdapter) GetState() (map[string]any, error) {
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
	dir, err := os.MkdirTemp("", "groundkv-contract-*")
	if err != nil {
		return err
	}
	opts := &engine.Options{
		PageSize:       256, // Small pages + 80B/120B values trigger byte-capacity splits!
		MaxFreelistIDs: 3,   // Force multi-page linked freelist chains
		NoSync:         false,
	}
	db, err := engine.Open(filepath.Join(dir, "contract.db"), opts)
	if err != nil {
		_ = os.RemoveAll(dir)
		return err
	}
	m.dir = dir
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
	var closeErr error
	if m.db != nil {
		closeErr = m.db.Close()
		m.db = nil
	}
	if m.dir != "" {
		_ = os.RemoveAll(m.dir)
		m.dir = ""
	}
	return closeErr
}
