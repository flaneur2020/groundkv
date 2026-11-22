// Source: ../../models/freelist/freelist.fizz
// FizzBee MBT adapter validating BoltDB/LMDB-style MVCC reader-aware freelist
// page reclamation, dual-meta safety, and crash recovery in GroundKV.
package freelist_mbt

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	mbt "github.com/fizzbee-io/fizzbee/mbt/lib/go"
	"github.com/flaneur2020/groundkv/engine"
)

const notFoundSentinel = "NOT_FOUND"

var errInvalidArg = errors.New("invalid argument from fizzbee mbt runner")

// FreelistDBRoleAdapter connects models/freelist/freelist.fizz to a real
// GroundKV engine with freelist page reclamation enabled (default).
type FreelistDBRoleAdapter struct {
	mu      sync.Mutex
	storage *engine.MemStorage
	db      *engine.DB
	opts    *engine.Options

	readers map[string]*engine.Tx
}

var (
	_ FreelistDBRole  = (*FreelistDBRoleAdapter)(nil)
	_ mbt.StateGetter = (*FreelistDBRoleAdapter)(nil)
)

func newFreelistDBRoleAdapter() (*FreelistDBRoleAdapter, error) {
	st := engine.NewMemStorage()
	opts := &engine.Options{
		PageSize:      512,
		MaxLeafKeys:   2,
		MaxBranchKeys: 2,
		NoFreelist:    false, // Explicitly exercise real on-disk freelist reclamation
	}
	db, err := engine.OpenStorage(st, opts)
	if err != nil {
		return nil, err
	}
	return &FreelistDBRoleAdapter{
		storage: st,
		db:      db,
		opts:    opts,
		readers: make(map[string]*engine.Tx),
	}, nil
}

func (a *FreelistDBRoleAdapter) close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for rid, tx := range a.readers {
		_ = tx.Rollback()
		delete(a.readers, rid)
	}
	if a.db != nil {
		err := a.db.Close()
		a.db = nil
		return err
	}
	return nil
}

func (a *FreelistDBRoleAdapter) ActionPut(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(args) < 1 {
		return nil, errInvalidArg
	}
	k, ok := args[0].Value.(string)
	if !ok {
		return nil, errInvalidArg
	}
	nextTxID := a.db.Meta().TxID + 1
	v := fmt.Sprintf("v%d", nextTxID)
	if err := a.db.Put([]byte(k), []byte(v)); err != nil {
		return nil, err
	}
	if err := a.db.Validate(); err != nil {
		return nil, fmt.Errorf("btree validation failed after Put: %w", err)
	}
	return int64(a.db.Meta().TxID), nil
}

func (a *FreelistDBRoleAdapter) ActionOpenReader(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(args) < 1 {
		return nil, errInvalidArg
	}
	rid, ok := args[0].Value.(string)
	if !ok {
		return nil, errInvalidArg
	}
	if _, exists := a.readers[rid]; exists {
		return int64(-1), nil
	}
	tx, err := a.db.BeginRead()
	if err != nil {
		return nil, err
	}
	a.readers[rid] = tx
	return int64(tx.TxID()), nil
}

func (a *FreelistDBRoleAdapter) ActionCloseReader(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(args) < 1 {
		return nil, errInvalidArg
	}
	rid, ok := args[0].Value.(string)
	if !ok {
		return nil, errInvalidArg
	}
	tx, exists := a.readers[rid]
	if !exists {
		return "NONE", nil
	}
	delete(a.readers, rid)
	if err := tx.Rollback(); err != nil {
		return nil, err
	}
	return "OK", nil
}

func (a *FreelistDBRoleAdapter) ActionGet(args []mbt.Arg) (any, error) {
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

func (a *FreelistDBRoleAdapter) ActionReadFromReader(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(args) < 2 {
		return nil, errInvalidArg
	}
	rid, ok := args[0].Value.(string)
	if !ok {
		return nil, errInvalidArg
	}
	k, ok := args[1].Value.(string)
	if !ok {
		return nil, errInvalidArg
	}
	tx, exists := a.readers[rid]
	if !exists {
		return notFoundSentinel, nil
	}
	val, err := tx.Get([]byte(k))
	if errors.Is(err, engine.ErrNotFound) {
		return notFoundSentinel, nil
	}
	if err != nil {
		return nil, err
	}
	return string(val), nil
}

func (a *FreelistDBRoleAdapter) ActionCrashAndReopen(args []mbt.Arg) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Crash drops all in-memory read handles and unsynced writes.
	a.readers = make(map[string]*engine.Tx)
	recovered := a.storage.SimulateCrash()
	a.storage = recovered

	db, err := engine.OpenStorage(a.storage, a.opts)
	if err != nil {
		return nil, fmt.Errorf("reopen after crash failed: %w", err)
	}
	a.db = db
	if err := a.db.Validate(); err != nil {
		return nil, fmt.Errorf("btree validation failed after crash: %w", err)
	}
	return int64(a.db.Meta().TxID), nil
}

// GetState walks every page reachable from meta0, meta1, and every open MVCC
// reader, verifying ZERO overlap with freelist.free, exact releaseBound &
// minActiveReaderTxID tracking, and intact reader snapshots.
func (a *FreelistDBRoleAdapter) GetState() (map[string]any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	meta := a.db.Meta()
	committedMap, err := a.db.ReadTreeAtRoot(meta.Root)
	if err != nil {
		return nil, err
	}

	activeReaders := make(map[any]any, len(a.readers))
	readerViews := make(map[any]any, len(a.readers))
	livePages := make(map[engine.PageID]bool)

	// Collect all live pages from both valid on-disk meta slots (root + freelist chain).
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
			return nil, fmt.Errorf("reachable pages for meta txid=%d: %w", slot.Meta.TxID, err)
		}
		for _, id := range pages {
			livePages[id] = true
		}
	}

	// Collect live pages and point-in-time KV maps for every open MVCC reader.
	for rid, tx := range a.readers {
		activeReaders[rid] = int64(tx.TxID())
		view, err := tx.All()
		if err != nil {
			return nil, fmt.Errorf("reader %s snapshot read failed: %w", rid, err)
		}
		readerViews[rid] = toAnyMap(view)

		pages, err := a.db.ReachablePages(tx.RootPageID(), 0)
		if err != nil {
			return nil, fmt.Errorf("reachable pages for reader %s: %w", rid, err)
		}
		for _, id := range pages {
			livePages[id] = true
		}
	}

	flState := a.db.FreelistState()
	noLivePageInFree := true
	for _, freeID := range flState.FreeIDs {
		if livePages[freeID] {
			noLivePageInFree = false
			break
		}
	}

	releaseBound := a.db.ReleaseBound()
	noPendingBelowBound := true
	pendingParts := make([]string, 0, len(flState.PendingTxIDs))
	for _, pt := range flState.PendingTxIDs {
		if pt <= releaseBound {
			noPendingBelowBound = false
		}
		pendingParts = append(pendingParts, strconv.FormatUint(pt, 10))
	}

	minReader := int64(-1)
	if mr, ok := a.db.MinActiveReaderTxID(); ok {
		minReader = int64(mr)
	}

	return map[string]any{
		"txid":                   int64(meta.TxID),
		"committed":              toAnyMap(committedMap),
		"active_readers":         activeReaders,
		"reader_views":           readerViews,
		"min_reader_txid":        minReader,
		"release_bound":          int64(releaseBound),
		"pending_txids":          strings.Join(pendingParts, ","),
		"no_live_page_in_free":   noLivePageInFree,
		"no_pending_below_bound": noPendingBelowBound,
	}, nil
}

func toAnyMap(in map[string]string) map[any]any {
	out := make(map[any]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// FreelistModelAdapter wires models/freelist/freelist.fizz to FizzBee MBT.
type FreelistModelAdapter struct {
	dbRole *FreelistDBRoleAdapter
}

var _ FreelistModel = (*FreelistModelAdapter)(nil)

func NewFreelistModel() FreelistModel {
	return &FreelistModelAdapter{}
}

func GetTestOptions() map[string]any {
	return map[string]any{
		"max-seq-runs":      500,
		"max-parallel-runs": 0,
		"max-actions":       20,
	}
}

func (m *FreelistModelAdapter) GetState() (map[string]any, error) {
	return nil, mbt.ErrNotImplemented
}

func (m *FreelistModelAdapter) GetRoles() (map[mbt.RoleId]mbt.Role, error) {
	roles := make(map[mbt.RoleId]mbt.Role)
	if m.dbRole != nil {
		roles[mbt.RoleId{RoleName: "FreelistDB", Index: 0}] = m.dbRole
	}
	return roles, nil
}

func (m *FreelistModelAdapter) Init() error {
	role, err := newFreelistDBRoleAdapter()
	if err != nil {
		return err
	}
	m.dbRole = role
	return nil
}

func (m *FreelistModelAdapter) Cleanup() error {
	if m.dbRole != nil {
		err := m.dbRole.close()
		m.dbRole = nil
		return err
	}
	return nil
}
