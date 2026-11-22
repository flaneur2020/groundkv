// Source: ../../models/btree/btree.fizz
// FizzBee MBT adapter verifying append-only COW page immutability, multi-level
// B+Tree splits/deletes, and pinned historical root reads in GroundKV.
package btree_mbt

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"

	mbt "github.com/fizzbee-io/fizzbee/mbt/lib/go"
	"github.com/flaneur2020/groundkv/engine"
)

const notFoundSentinel = "NOT_FOUND"

var errInvalidArg = errors.New("invalid argument from fizzbee mbt runner")

// CowBTreeRoleAdapter bridges specs/btree/btree.fizz to GroundKV's real
// append-only COW B+Tree while recording every committed data page's raw bytes
// to verify strict append-only page immutability on every single step.
type CowBTreeRoleAdapter struct {
	mu      sync.Mutex
	storage *engine.MemStorage
	db      *engine.DB

	// genRoots records the committed root PageID for each transaction generation.
	genRoots map[int64]engine.PageID

	// committedPages records the exact raw bytes of every committed B+Tree page
	// (pgid >= 2) when first written, so GetState() can detect any in-place overwrite.
	committedPages map[engine.PageID][]byte
	pagesImmutable bool

	pinnedTx *engine.Tx
}

// Assert that CowBTreeRoleAdapter satisfies CowBTreeRole and mbt.StateGetter.
var (
	_ CowBTreeRole    = (*CowBTreeRoleAdapter)(nil)
	_ mbt.StateGetter = (*CowBTreeRoleAdapter)(nil)
)

func newCowBTreeRoleAdapter() (*CowBTreeRoleAdapter, error) {
	st := engine.NewMemStorage()
	opts := &engine.Options{
		PageSize:      512,
		MaxLeafKeys:   2, // With 3 keys ("a", "b", "c"), Put forces real leaf splits & root promotion!
		MaxBranchKeys: 2,
		NoFreelist:    true, // btree.fizz inspects all unpinned historical roots gen=0..N
	}
	db, err := engine.OpenStorage(st, opts)
	if err != nil {
		return nil, err
	}
	m := db.Meta()
	return &CowBTreeRoleAdapter{
		storage:        st,
		db:             db,
		genRoots:       map[int64]engine.PageID{int64(m.TxID): m.Root},
		committedPages: make(map[engine.PageID][]byte),
		pagesImmutable: true,
	}, nil
}

func (a *CowBTreeRoleAdapter) close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pinnedTx != nil {
		_ = a.pinnedTx.Rollback()
		a.pinnedTx = nil
	}
	if a.db != nil {
		err := a.db.Close()
		a.db = nil
		return err
	}
	return nil
}

// verifyAndRecordPages checks that every previously committed B+Tree page is
// byte-for-byte unchanged, then snapshots any newly appended pages.
func (a *CowBTreeRoleAdapter) verifyAndRecordPages() error {
	meta := a.db.Meta()
	for pgid, saved := range a.committedPages {
		cur, err := a.db.RawPage(pgid)
		if err != nil || !bytes.Equal(saved, cur) {
			a.pagesImmutable = false
			return nil
		}
	}
	for pgid := engine.PageID(2); pgid < meta.HighWater; pgid++ {
		if _, exists := a.committedPages[pgid]; exists {
			continue
		}
		raw, err := a.db.RawPage(pgid)
		if err != nil {
			// Skip unreferenced gaps if any, though append-only allocation is contiguous.
			continue
		}
		cp := make([]byte, len(raw))
		copy(cp, raw)
		a.committedPages[pgid] = cp
	}
	return nil
}

func (a *CowBTreeRoleAdapter) ActionPut(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(args) < 1 {
		return nil, errInvalidArg
	}
	k, ok := args[0].Value.(string)
	if !ok {
		return nil, errInvalidArg
	}
	nextGen := a.db.Meta().TxID + 1
	v := fmt.Sprintf("v%d", nextGen)
	if err := a.db.Put([]byte(k), []byte(v)); err != nil {
		return nil, err
	}
	if err := a.db.Validate(); err != nil {
		return nil, fmt.Errorf("btree validation failed after Put: %w", err)
	}
	meta := a.db.Meta()
	a.genRoots[int64(meta.TxID)] = meta.Root
	if err := a.verifyAndRecordPages(); err != nil {
		return nil, err
	}
	return int64(meta.TxID), nil
}

func (a *CowBTreeRoleAdapter) ActionDelete(args []mbt.Arg) (any, error) {
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
	meta := a.db.Meta()
	a.genRoots[int64(meta.TxID)] = meta.Root
	if err := a.verifyAndRecordPages(); err != nil {
		return nil, err
	}
	return int64(meta.TxID), nil
}

func (a *CowBTreeRoleAdapter) ActionGet(args []mbt.Arg) (any, error) {
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

func (a *CowBTreeRoleAdapter) ActionScanKeys(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	keys := make([]string, 0)
	err := a.db.View(func(tx *engine.Tx) error {
		return tx.Scan(nil, nil, func(k, _ []byte) bool {
			keys = append(keys, string(k))
			return true
		})
	})
	if err != nil {
		return nil, err
	}
	return strings.Join(keys, ","), nil
}

func (a *CowBTreeRoleAdapter) ActionPinSnapshot(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pinnedTx != nil {
		return int64(-1), nil
	}
	tx, err := a.db.BeginRead()
	if err != nil {
		return nil, err
	}
	a.pinnedTx = tx
	return int64(tx.TxID()), nil
}

func (a *CowBTreeRoleAdapter) ActionReadPinned(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pinnedTx == nil {
		return notFoundSentinel, nil
	}
	if len(args) < 1 {
		return nil, errInvalidArg
	}
	k, ok := args[0].Value.(string)
	if !ok {
		return nil, errInvalidArg
	}
	val, err := a.pinnedTx.Get([]byte(k))
	if errors.Is(err, engine.ErrNotFound) {
		return notFoundSentinel, nil
	}
	if err != nil {
		return nil, err
	}
	return string(val), nil
}

// GetState reports every historical generation tree, current root state,
// pinned snapshot state, and byte-level COW page immutability status.
func (a *CowBTreeRoleAdapter) GetState() (map[string]any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if err := a.verifyAndRecordPages(); err != nil {
		return nil, err
	}

	meta := a.db.Meta()
	generations := make(map[any]any, len(a.genRoots))
	for gen, root := range a.genRoots {
		kv, err := a.db.ReadTreeAtRoot(root)
		if err != nil {
			return nil, fmt.Errorf("read generation %d (root %d): %w", gen, root, err)
		}
		// FizzBee encodes Starlark dict keys as JSON strings in state snapshots.
		generations[fmt.Sprintf("%d", gen)] = toAnyMap(kv)
	}

	curRef, err := a.db.ReadTreeAtRoot(meta.Root)
	if err != nil {
		return nil, err
	}

	pinnedGen := int64(-1)
	pinnedRef := map[string]string{}
	if a.pinnedTx != nil {
		pinnedGen = int64(a.pinnedTx.TxID())
		pinnedRef, err = a.pinnedTx.All()
		if err != nil {
			return nil, err
		}
	}

	return map[string]any{
		"generations":     generations,
		"root_gen":        int64(meta.TxID),
		"ref":             toAnyMap(curRef),
		"pinned_gen":      pinnedGen,
		"pinned_ref":      toAnyMap(pinnedRef),
		"pages_immutable": a.pagesImmutable,
	}, nil
}

func toAnyMap(in map[string]string) map[any]any {
	out := make(map[any]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// BtreeModelAdapter wires the top-level specs/btree/btree.fizz model.
type BtreeModelAdapter struct {
	treeRole *CowBTreeRoleAdapter
}

// Assert that BtreeModelAdapter satisfies BtreeModel.
var _ BtreeModel = (*BtreeModelAdapter)(nil)

// NewBtreeModel constructs a BtreeModelAdapter for FizzBee MBT.
func NewBtreeModel() BtreeModel {
	return &BtreeModelAdapter{}
}

func GetTestOptions() map[string]any {
	return map[string]any{
		"max-seq-runs":      500,
		"max-parallel-runs": 0,
		"max-actions":       20,
	}
}

func (m *BtreeModelAdapter) GetState() (map[string]any, error) {
	return nil, mbt.ErrNotImplemented
}

func (m *BtreeModelAdapter) GetRoles() (map[mbt.RoleId]mbt.Role, error) {
	roles := make(map[mbt.RoleId]mbt.Role)
	if m.treeRole != nil {
		roles[mbt.RoleId{RoleName: "CowBTree", Index: 0}] = m.treeRole
	}
	return roles, nil
}

func (m *BtreeModelAdapter) Init() error {
	role, err := newCowBTreeRoleAdapter()
	if err != nil {
		return err
	}
	m.treeRole = role
	return nil
}

func (m *BtreeModelAdapter) Cleanup() error {
	if m.treeRole != nil {
		err := m.treeRole.close()
		m.treeRole = nil
		return err
	}
	return nil
}
