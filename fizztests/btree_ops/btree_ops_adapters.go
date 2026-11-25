// Source: ../../models/btree_ops/btree_ops.fizz
// Ordered B+Tree & range-scan contract adapter with freelist enabled and small
// node fanout caps (MaxLeafKeys=2, MaxBranchKeys=2) to exercise multi-level
// splits, deletes, root collapse, and [start, end) scans.
package btree_ops_mbt

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	mbt "github.com/fizzbee-io/fizzbee/mbt/lib/go"
	"github.com/flaneur2020/groundkv/engine"
)

const notFoundSentinel = "NOT_FOUND"

var errInvalidArg = errors.New("invalid argument from fizzbee mbt runner")

type BTreeRoleAdapter struct {
	mu sync.Mutex
	db *engine.DB
}

var (
	_ BTreeRole       = (*BTreeRoleAdapter)(nil)
	_ mbt.StateGetter = (*BTreeRoleAdapter)(nil)
)

func newBTreeRoleAdapter() (*BTreeRoleAdapter, error) {
	st := engine.NewMemStorage()
	opts := &engine.Options{
		PageSize:       512,
		MaxLeafKeys:    2,
		MaxBranchKeys:  2,
		MaxFreelistIDs: 3,     // Force multi-page linked freelist chains alongside 3-level B+Tree ops
		NoFreelist:     false, // Exercise splits, deletes & collapses with real freelist enabled
	}
	db, err := engine.OpenStorage(st, opts)
	if err != nil {
		return nil, err
	}
	return &BTreeRoleAdapter{db: db}, nil
}

func (a *BTreeRoleAdapter) close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.db != nil {
		err := a.db.Close()
		a.db = nil
		return err
	}
	return nil
}

func (a *BTreeRoleAdapter) ActionBulkLoad(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	tag := fmt.Sprintf("b%d", a.db.Meta().TxID+1)
	err := a.db.Update(func(tx *engine.Tx) error {
		for _, k := range []string{"a", "b", "c", "d", "e"} {
			if err := tx.Put([]byte(k), []byte(tag+"_"+k)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := a.db.Validate(); err != nil {
		return nil, fmt.Errorf("btree validation failed after BulkLoad: %w", err)
	}
	return tag, nil
}

func (a *BTreeRoleAdapter) ActionPut(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(args) < 1 {
		return nil, errInvalidArg
	}
	k, ok := args[0].Value.(string)
	if !ok {
		return nil, errInvalidArg
	}
	v := fmt.Sprintf("v%d", a.db.Meta().TxID+1)
	if err := a.db.Put([]byte(k), []byte(v)); err != nil {
		return nil, err
	}
	if err := a.db.Validate(); err != nil {
		return nil, fmt.Errorf("btree validation failed after Put: %w", err)
	}
	return v, nil
}

func (a *BTreeRoleAdapter) ActionDelete(args []mbt.Arg) (any, error) {
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
	if err := a.db.Validate(); err != nil {
		return nil, fmt.Errorf("btree validation failed after Delete: %w", err)
	}
	return "OK", nil
}

func (a *BTreeRoleAdapter) ActionGet(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
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

func (a *BTreeRoleAdapter) ActionScanRange(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(args) < 2 {
		return nil, errInvalidArg
	}
	startStr, ok := args[0].Value.(string)
	if !ok {
		return nil, errInvalidArg
	}
	endStr, ok := args[1].Value.(string)
	if !ok {
		return nil, errInvalidArg
	}
	var start, end []byte
	if startStr != "" {
		start = []byte(startStr)
	}
	if endStr != "" {
		end = []byte(endStr)
	}
	var parts []string
	err := a.db.View(func(tx *engine.Tx) error {
		return tx.Scan(start, end, func(k, v []byte) bool {
			parts = append(parts, string(k)+"="+string(v))
			return true
		})
	})
	if err != nil {
		return nil, err
	}
	return strings.Join(parts, ","), nil
}

func (a *BTreeRoleAdapter) GetState() (map[string]any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	meta := a.db.Meta()

	// Walk multi-level branch & freelist pages via ReachablePages and verify
	// zero overlap with freelist.free on 3-level B+Tree structures.
	livePages := make(map[engine.PageID]bool)
	slot0, slot1, err := a.db.InspectMeta()
	if err != nil {
		return nil, err
	}
	for _, slot := range []engine.MetaSlot{slot0, slot1} {
		if !slot.Valid {
			continue
		}
		pages, err := a.db.ReachablePages(slot.Meta.Root, slot.Meta.Freelist)
		if err != nil {
			return nil, fmt.Errorf("ReachablePages(txid=%d): %w", slot.Meta.TxID, err)
		}
		for _, id := range pages {
			livePages[id] = true
		}
	}
	for _, freeID := range a.db.FreelistState().FreeIDs {
		if livePages[freeID] {
			return nil, fmt.Errorf("freelist violation on multi-level tree: live page %d in freelist.free", freeID)
		}
	}

	all, err := a.db.ReadTreeAtRoot(meta.Root)
	if err != nil {
		return nil, err
	}
	entries := make(map[any]any, len(all))
	for k, v := range all {
		entries[k] = v
	}
	return map[string]any{
		"txid":    int64(meta.TxID),
		"entries": entries,
	}, nil
}

type BtreeOpsModelAdapter struct {
	treeRole *BTreeRoleAdapter
}

var _ BtreeOpsModel = (*BtreeOpsModelAdapter)(nil)

func NewBtreeOpsModel() BtreeOpsModel {
	return &BtreeOpsModelAdapter{}
}

func GetTestOptions() map[string]any {
	return map[string]any{
		"max-seq-runs":      500,
		"max-parallel-runs": 0,
		"max-actions":       20,
	}
}

func (m *BtreeOpsModelAdapter) GetState() (map[string]any, error) {
	return nil, mbt.ErrNotImplemented
}

func (m *BtreeOpsModelAdapter) GetRoles() (map[mbt.RoleId]mbt.Role, error) {
	roles := make(map[mbt.RoleId]mbt.Role)
	if m.treeRole != nil {
		roles[mbt.RoleId{RoleName: "BTree", Index: 0}] = m.treeRole
	}
	return roles, nil
}

func (m *BtreeOpsModelAdapter) Init() error {
	role, err := newBTreeRoleAdapter()
	if err != nil {
		return err
	}
	m.treeRole = role
	return nil
}

func (m *BtreeOpsModelAdapter) Cleanup() error {
	if m.treeRole != nil {
		err := m.treeRole.close()
		m.treeRole = nil
		return err
	}
	return nil
}
