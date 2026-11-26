package verified_test

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/flaneur2020/groundkv/engine"
	"github.com/flaneur2020/groundkv/verified"
	"github.com/flaneur2020/groundkv/verified/dafnygen/GroundKVBTree"
	"github.com/flaneur2020/groundkv/verified/dafnygen/GroundKVEngine"
	"github.com/flaneur2020/groundkv/verified/dafnygen/GroundKVTypes"
	_dafny "github.com/flaneur2020/groundkv/verified/dafnygen/dafny"
)

func TestVerifiedBasicCRUDAndPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "verified.db")
	opts := &verified.Options{
		PageSize:      256,
		MaxLeafKeys:   3,
		MaxBranchKeys: 3,
	}

	db, err := verified.Open(path, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	expected := make(map[string]string)
	for i := 0; i < 60; i++ {
		k := fmt.Sprintf("k%03d", i)
		v := fmt.Sprintf("val-%03d", i)
		if err := db.Put([]byte(k), []byte(v)); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
		expected[k] = v
	}
	if err := db.Validate(); err != nil {
		t.Fatalf("Validate after puts: %v", err)
	}

	// Overwrite and delete half the keys
	for i := 0; i < 60; i += 2 {
		k := fmt.Sprintf("k%03d", i)
		if i%4 == 0 {
			if err := db.Delete([]byte(k)); err != nil {
				t.Fatalf("Delete %s: %v", k, err)
			}
			delete(expected, k)
		} else {
			v := fmt.Sprintf("updated-%03d", i)
			if err := db.Put([]byte(k), []byte(v)); err != nil {
				t.Fatalf("Update %s: %v", k, err)
			}
			expected[k] = v
		}
	}
	if err := db.Validate(); err != nil {
		t.Fatalf("Validate after updates/deletes: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen with nil Options to test PageSize auto-detection + dual-meta recovery
	db2, err := verified.Open(path, nil)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer db2.Close()

	if err := db2.Validate(); err != nil {
		t.Fatalf("Validate after reopen: %v", err)
	}
	var got map[string]string
	err = db2.View(func(tx *verified.Tx) error {
		var allErr error
		got, allErr = tx.All()
		return allErr
	})
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("reopened map mismatch: got %d keys, want %d keys", len(got), len(expected))
	}

	// Verify range scan [k010, k025)
	var scanned []string
	err = db2.View(func(tx *verified.Tx) error {
		return tx.Scan([]byte("k010"), []byte("k025"), func(k, v []byte) bool {
			if expected[string(k)] != string(v) {
				t.Errorf("scan value mismatch for %s: got %s, want %s", k, v, expected[string(k)])
			}
			scanned = append(scanned, string(k))
			return true
		})
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	var wantScan []string
	for k := range expected {
		if k >= "k010" && k < "k025" {
			wantScan = append(wantScan, k)
		}
	}
	sort.Strings(wantScan)
	if !reflect.DeepEqual(scanned, wantScan) {
		t.Fatalf("scan keys mismatch:\ngot:  %v\nwant: %v", scanned, wantScan)
	}
}

func TestVerifiedMVCCSnapshotIsolationAndFreelistReclamation(t *testing.T) {
	st := engine.NewMemStorage()
	opts := &verified.Options{
		PageSize:      256,
		MaxLeafKeys:   3,
		MaxBranchKeys: 3,
	}
	db, err := verified.OpenStorage(st, opts)
	if err != nil {
		t.Fatalf("OpenStorage: %v", err)
	}
	defer db.Close()

	for i := 0; i < 15; i++ {
		if err := db.Put([]byte(fmt.Sprintf("k%02d", i)), []byte("v1")); err != nil {
			t.Fatalf("seed put: %v", err)
		}
	}

	// Pin an MVCC snapshot reader at txid = 15
	snapTx, err := db.BeginRead()
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}
	snapTxID := snapTx.TxID()
	snapExpected, err := snapTx.All()
	if err != nil {
		t.Fatalf("snap All: %v", err)
	}

	// Perform 25 writes while the reader is pinned
	for round := 0; round < 25; round++ {
		k := fmt.Sprintf("k%02d", round%15)
		v := fmt.Sprintf("v2-round-%02d", round)
		if err := db.Put([]byte(k), []byte(v)); err != nil {
			t.Fatalf("round %d put: %v", round, err)
		}
		if rb := db.ReleaseBound(); rb > snapTxID {
			t.Fatalf("ReleaseBound %d exceeded pinned reader txid %d", rb, snapTxID)
		}
		// Assert no page reachable from the pinned snapshot is ever in freelist.free
		reach, err := db.ReachablePages(snapTx.RootPageID(), 0)
		if err != nil {
			t.Fatalf("ReachablePages: %v", err)
		}
		freeSet := make(map[verified.PageID]bool)
		for _, id := range db.FreelistState().FreeIDs {
			freeSet[id] = true
		}
		for _, id := range reach {
			if freeSet[id] {
				t.Fatalf("round %d: pinned reader page %d leaked into reusable free pool!", round, id)
			}
		}
	}

	// Pinned snapshot must still read exact v1 data
	snapAfter, err := snapTx.All()
	if err != nil {
		t.Fatalf("snap All after writes: %v", err)
	}
	if !reflect.DeepEqual(snapAfter, snapExpected) {
		t.Fatalf("MVCC snapshot isolation violated")
	}
	_ = snapTx.Rollback()

	// After closing the reader, subsequent writes should recycle pages!
	for i := 0; i < 10; i++ {
		if err := db.Put([]byte(fmt.Sprintf("k%02d", i)), []byte("v3")); err != nil {
			t.Fatalf("post-close put: %v", err)
		}
	}
	if fl := db.FreelistState(); fl.ReclaimedHits == 0 {
		t.Fatalf("expected reclaimed page hits after closing pinned reader, got 0")
	}
	if err := db.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

type failingWriteStorage struct {
	engine.Storage
	failNextSync  bool
	failNextWrite bool
}

func (f *failingWriteStorage) WriteAt(p []byte, off int64) (int, error) {
	if f.failNextWrite {
		f.failNextWrite = false
		return 0, errors.New("injected write failure")
	}
	return f.Storage.WriteAt(p, off)
}

func (f *failingWriteStorage) Sync() error {
	if f.failNextSync {
		f.failNextSync = false
		return errors.New("injected sync failure")
	}
	return f.Storage.Sync()
}

func TestVerifiedCrashRecoveryAndDoubleFaultRollbackFence(t *testing.T) {
	mem := engine.NewMemStorage()
	fs := &failingWriteStorage{Storage: mem}
	opts := &verified.Options{PageSize: 256, MaxLeafKeys: 3, MaxBranchKeys: 3}

	db, err := verified.OpenStorage(fs, opts)
	if err != nil {
		t.Fatalf("OpenStorage: %v", err)
	}
	if err := db.Put([]byte("alpha"), []byte("committed-v1")); err != nil {
		t.Fatalf("seed Put: %v", err)
	}

	// Trigger double fault: SyncMetaPage fails AND Rollback's alt-meta invalidation write fails!
	tx, err := db.BeginWrite()
	if err != nil {
		t.Fatalf("BeginWrite: %v", err)
	}
	if err := tx.Put([]byte("alpha"), []byte("aborted-v2")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := tx.WriteDataPages(); err != nil {
		t.Fatalf("WriteDataPages: %v", err)
	}
	if err := tx.SyncDataPages(); err != nil {
		t.Fatalf("SyncDataPages: %v", err)
	}
	if err := tx.WriteMetaPage(); err != nil {
		t.Fatalf("WriteMetaPage: %v", err)
	}
	fs.failNextSync = true
	if err := tx.SyncMetaPage(); err == nil {
		t.Fatalf("expected SyncMetaPage to fail")
	}
	fs.failNextWrite = true
	_ = tx.Rollback()

	if !db.HasDirtyAltMeta() {
		t.Fatalf("expected HasDirtyAltMeta() == true after double-fault rollback")
	}

	// Next transaction's SyncDataPages MUST neutralize alt-meta BEFORE syncing!
	tx2, err := db.BeginWrite()
	if err != nil {
		t.Fatalf("BeginWrite tx2: %v", err)
	}
	if err := tx2.Put([]byte("beta"), []byte("v3")); err != nil {
		t.Fatalf("Put tx2: %v", err)
	}
	if err := tx2.WriteDataPages(); err != nil {
		t.Fatalf("WriteDataPages tx2: %v", err)
	}
	if err := tx2.SyncDataPages(); err != nil {
		t.Fatalf("SyncDataPages tx2: %v", err)
	}
	if db.HasDirtyAltMeta() {
		t.Fatalf("expected HasDirtyAltMeta() == false after SyncDataPages fence")
	}
	_ = tx2.Rollback()
	crashed := mem.SimulateCrash()
	_ = db.Close()

	// Reopen from crashed storage: aborted-v2 MUST NOT have been resurrected!
	db2, err := verified.OpenStorage(crashed, opts)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer db2.Close()

	got, err := db2.Get([]byte("alpha"))
	if err != nil {
		t.Fatalf("Get alpha: %v", err)
	}
	if string(got) != "committed-v1" {
		t.Fatalf("double-fault fence failed: got %q, want %q", got, "committed-v1")
	}
}

// TestCrossEngineDifferentialAndOnDiskInterop verifies that the original Go
// engine (`engine.DB`) and the Dafny-verified engine (`verified.DB`) produce
// bit-compatible on-disk databases and identical KV/Meta/Freelist state across
// randomized splits, deletes, variable-sized values, and cross-engine reopens.
func TestCrossEngineDifferentialAndOnDiskInterop(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	opts := &engine.Options{
		PageSize:       256,
		MaxLeafKeys:    3,
		MaxBranchKeys:  3,
		MaxFreelistIDs: 3,
	}

	goMem := engine.NewMemStorage()
	dafnyMem := engine.NewMemStorage()

	goDB, err := engine.OpenStorage(goMem, opts)
	if err != nil {
		t.Fatalf("engine.OpenStorage: %v", err)
	}
	defer goDB.Close()

	dafnyDB, err := verified.OpenStorage(dafnyMem, opts)
	if err != nil {
		t.Fatalf("verified.OpenStorage: %v", err)
	}
	defer dafnyDB.Close()

	ref := make(map[string]string)

	for step := 0; step < 250; step++ {
		k := fmt.Sprintf("key-%02d", rng.Intn(35))
		switch rng.Intn(3) {
		case 0, 1:
			// Variable value lengths (1..90 bytes) to trigger 2-way & 3-way splits
			vLen := 1 + rng.Intn(90)
			val := bytes.Repeat([]byte{byte('a' + rng.Intn(26))}, vLen)
			errGo := goDB.Put([]byte(k), val)
			errDafny := dafnyDB.Put([]byte(k), val)
			if (errGo == nil) != (errDafny == nil) {
				t.Fatalf("step %d Put error mismatch: go=%v dafny=%v", step, errGo, errDafny)
			}
			if errGo == nil {
				ref[k] = string(val)
			}
		case 2:
			errGo := goDB.Delete([]byte(k))
			errDafny := dafnyDB.Delete([]byte(k))
			if (errGo == nil) != (errDafny == nil) {
				t.Fatalf("step %d Delete error mismatch: go=%v dafny=%v", step, errGo, errDafny)
			}
			if errGo == nil {
				delete(ref, k)
			}
		}

		if step%25 == 0 || step == 249 {
			if err := goDB.Validate(); err != nil {
				t.Fatalf("step %d goDB.Validate: %v", step, err)
			}
			if err := dafnyDB.Validate(); err != nil {
				t.Fatalf("step %d dafnyDB.Validate: %v", step, err)
			}
			if goDB.Meta() != dafnyDB.Meta() {
				t.Fatalf("step %d Meta mismatch:\ngo:    %+v\ndafny: %+v", step, goDB.Meta(), dafnyDB.Meta())
			}
			if !reflect.DeepEqual(goDB.FreelistState(), dafnyDB.FreelistState()) {
				t.Fatalf("step %d FreelistState mismatch:\ngo:    %+v\ndafny: %+v", step, goDB.FreelistState(), dafnyDB.FreelistState())
			}
			var goAll, dafnyAll map[string]string
			_ = goDB.View(func(tx *engine.Tx) error { goAll, err = tx.All(); return err })
			_ = dafnyDB.View(func(tx *verified.Tx) error { dafnyAll, err = tx.All(); return err })
			if !reflect.DeepEqual(goAll, ref) || !reflect.DeepEqual(dafnyAll, ref) {
				t.Fatalf("step %d KV map mismatch", step)
			}
		}
	}

	// On-disk cross-engine interoperability test:
	// 1. Write file with verified.Open -> reopen with engine.Open -> mutate
	// 2. Reopen with verified.Open -> verify all mutations visible & valid!
	interopPath := filepath.Join(t.TempDir(), "interop.db")
	vdb, err := verified.Open(interopPath, opts)
	if err != nil {
		t.Fatalf("verified.Open interop: %v", err)
	}
	for k, v := range ref {
		if err := vdb.Put([]byte(k), []byte(v)); err != nil {
			t.Fatalf("vdb.Put %s: %v", k, err)
		}
	}
	_ = vdb.Close()

	edb, err := engine.Open(interopPath, nil)
	if err != nil {
		t.Fatalf("engine.Open on verified.db: %v", err)
	}
	if err := edb.Validate(); err != nil {
		t.Fatalf("engine.Validate on verified.db: %v", err)
	}
	var edbAll map[string]string
	_ = edb.View(func(tx *engine.Tx) error { edbAll, err = tx.All(); return err })
	if !reflect.DeepEqual(edbAll, ref) {
		t.Fatalf("engine.Open read mismatch on file created by verified.DB")
	}
	if err := edb.Put([]byte("from-go"), []byte("hello-dafny")); err != nil {
		t.Fatalf("edb.Put: %v", err)
	}
	ref["from-go"] = "hello-dafny"
	_ = edb.Close()

	vdb2, err := verified.Open(interopPath, nil)
	if err != nil {
		t.Fatalf("verified.Open on engine-mutated db: %v", err)
	}
	defer vdb2.Close()
	if err := vdb2.Validate(); err != nil {
		t.Fatalf("verified.Validate on engine-mutated db: %v", err)
	}
	var vdb2All map[string]string
	_ = vdb2.View(func(tx *verified.Tx) error { vdb2All, err = tx.All(); return err })
	if !reflect.DeepEqual(vdb2All, ref) {
		t.Fatalf("verified.Open read mismatch on file mutated by engine.DB")
	}
}

// TestPhase2VerifiedCOWBTreeRefinement directly exercises the Phase 2 Z3-verified
// recursive COW B+Tree (`GroundKVBTree.TreePut`, `TreeGet`, `TreeScan`, `TreeDelete`,
// `TreeValidate`) compiled to Go, checking KV-map refinement and COW snapshot immutability.
func TestPhase2VerifiedCOWBTreeRefinement(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	store := _dafny.EmptyMap
	nextID := uint64(2)
	root := uint64(0)
	pageSize := uint32(256)
	maxLeaf := _dafny.IntOfInt64(3)
	maxBranch := _dafny.IntOfInt64(3)

	type snapshot struct {
		root uint64
		kv   map[string]string
	}
	var snapshots []snapshot
	ref := make(map[string]string)

	for step := 0; step < 300; step++ {
		k := fmt.Sprintf("k%02d", rng.Intn(40))
		kSeq := _dafny.SeqOfBytes([]byte(k))

		if rng.Intn(3) != 0 {
			v := fmt.Sprintf("v-%03d-%s", step, bytes.Repeat([]byte("x"), rng.Intn(30)))
			vSeq := _dafny.SeqOfBytes([]byte(v))
			ok, newStore, newNextID, newRoot := GroundKVBTree.Companion_Default___.TreePut(
				store, nextID, root, kSeq, vSeq, pageSize, maxLeaf, maxBranch,
			)
			if !ok {
				t.Fatalf("step %d: TreePut returned ok=false", step)
			}
			store, nextID, root = newStore, newNextID, newRoot
			ref[k] = v
		} else {
			ok, newStore, newNextID, newRoot, deleted := GroundKVBTree.Companion_Default___.TreeDelete(
				store, nextID, root, kSeq, pageSize,
			)
			if !ok {
				t.Fatalf("step %d: TreeDelete returned ok=false", step)
			}
			_, existed := ref[k]
			if deleted != existed {
				t.Fatalf("step %d: TreeDelete deleted=%v, want %v", step, deleted, existed)
			}
			store, nextID, root = newStore, newNextID, newRoot
			delete(ref, k)
		}

		if !GroundKVBTree.Companion_Default___.TreeValidate(store, root, nextID) {
			t.Fatalf("step %d: TreeValidate failed on root=%d nextID=%d", step, root, nextID)
		}

		// Record periodic snapshots to verify COW immutability later
		if step%50 == 49 {
			cp := make(map[string]string, len(ref))
			for rk, rv := range ref {
				cp[rk] = rv
			}
			snapshots = append(snapshots, snapshot{root: root, kv: cp})
		}

		// Verify TreeGet + TreeScan against reference map
		if step%20 == 0 || step == 299 {
			for i := 0; i < 40; i++ {
				qk := fmt.Sprintf("k%02d", i)
				want, exists := ref[qk]
				if root == 0 {
					if exists {
						t.Fatalf("step %d: root==0 but key %s exists", step, qk)
					}
					continue
				}
				opt := GroundKVBTree.Companion_Default___.TreeGet(store, root, _dafny.SeqOfBytes([]byte(qk)))
				if exists {
					if !opt.Is_Some() || string(_dafny.ToByteArray(opt.Dtor_value().(_dafny.Sequence))) != want {
						t.Fatalf("step %d: TreeGet(%s) mismatch", step, qk)
					}
				} else if opt.Is_Some() {
					t.Fatalf("step %d: TreeGet(%s) unexpectedly found value", step, qk)
				}
			}

			if root != 0 {
				scanned := GroundKVBTree.Companion_Default___.TreeScan(
					store, root, _dafny.SeqOfBytes([]byte("k10")), _dafny.SeqOfBytes([]byte("k30")),
				)
				gotScan := make(map[string]string)
				for i := uint32(0); i < scanned.Cardinality(); i++ {
					e := scanned.Select(i).(GroundKVTypes.LeafEntry)
					gotScan[string(_dafny.ToByteArray(e.Dtor_key()))] = string(_dafny.ToByteArray(e.Dtor_value()))
				}
				wantScan := make(map[string]string)
				for rk, rv := range ref {
					if rk >= "k10" && rk < "k30" {
						wantScan[rk] = rv
					}
				}
				if !reflect.DeepEqual(gotScan, wantScan) {
					t.Fatalf("step %d: TreeScan [k10,k30) mismatch: got %v, want %v", step, gotScan, wantScan)
				}
			}
		}
	}

	// Verify EVERY historical snapshot is still 100% intact in the final COW PageStore!
	for idx, snap := range snapshots {
		if snap.root == 0 {
			if len(snap.kv) != 0 {
				t.Fatalf("snapshot %d had root=0 with non-empty map", idx)
			}
			continue
		}
		scanned := GroundKVBTree.Companion_Default___.TreeScan(store, snap.root, _dafny.EmptySeq, _dafny.EmptySeq)
		got := make(map[string]string)
		for i := uint32(0); i < scanned.Cardinality(); i++ {
			e := scanned.Select(i).(GroundKVTypes.LeafEntry)
			got[string(_dafny.ToByteArray(e.Dtor_key()))] = string(_dafny.ToByteArray(e.Dtor_value()))
		}
		if !reflect.DeepEqual(got, snap.kv) {
			t.Fatalf("COW immutability violated on historical snapshot %d", idx)
		}
	}
}

// TestPhase2VerifiedStorageEngineProtocol directly exercises the Phase 2
// Z3-verified 4-phase dual-meta commit & crash-recovery state machine
// (`GroundKVEngine`) compiled to Go.
func TestPhase2VerifiedStorageEngineProtocol(t *testing.T) {
	eng := GroundKVEngine.Companion_Default___

	s := eng.InitEmpty(uint32(256))
	if !eng.EngineInv(s) {
		t.Fatalf("InitEmpty violated EngineInv")
	}

	kv1 := _dafny.NewMapBuilder().Add(_dafny.SeqOfBytes([]byte("a")), _dafny.SeqOfBytes([]byte("1"))).ToMap()
	pages1 := _dafny.SetOf(uint64(2))

	// Commit 1: BeginWrite -> WriteDataPages -> SyncDataPages -> WriteMetaPage -> SyncMetaPage
	s = eng.BeginWrite(s)
	s = eng.WriteDataPages(s, uint64(2), kv1, pages1, uint64(3), _dafny.EmptySet, _dafny.EmptySet)
	s = eng.SyncDataPages(s)
	s = eng.WriteMetaPage(s, uint64(0))
	s = eng.SyncMetaPage(s)
	if !eng.EngineInv(s) || s.Dtor_meta().Dtor_txid() != 1 {
		t.Fatalf("Commit 1 failed EngineInv or txid != 1")
	}

	// Start Commit 2, reach PhaseMetaWritten, then double-fault RollbackWrite(invalidateOk=false)
	kv2 := _dafny.NewMapBuilder().Add(_dafny.SeqOfBytes([]byte("a")), _dafny.SeqOfBytes([]byte("aborted"))).ToMap()
	pages2 := _dafny.SetOf(uint64(3))
	s = eng.BeginWrite(s)
	s = eng.WriteDataPages(s, uint64(3), kv2, pages2, uint64(4), _dafny.SetOf(uint64(2)), _dafny.EmptySet)
	s = eng.SyncDataPages(s)
	s = eng.WriteMetaPage(s, uint64(0))
	s = eng.RollbackWrite(s, false)
	if !eng.EngineInv(s) || !s.Dtor_dirtyAltMeta() {
		t.Fatalf("double-fault RollbackWrite should set dirtyAltMeta=true")
	}

	// Next tx's SyncDataPages clears dirtyAltMeta BEFORE syncing; crash recovers Commit 1!
	kv3 := _dafny.NewMapBuilder().Add(_dafny.SeqOfBytes([]byte("a")), _dafny.SeqOfBytes([]byte("3"))).ToMap()
	pages3 := _dafny.SetOf(uint64(4))
	s = eng.BeginWrite(s)
	s = eng.WriteDataPages(s, uint64(4), kv3, pages3, uint64(5), _dafny.SetOf(uint64(2)), _dafny.EmptySet)
	s = eng.SyncDataPages(s)
	if s.Dtor_dirtyAltMeta() {
		t.Fatalf("SyncDataPages failed to clear dirtyAltMeta")
	}

	modes := []GroundKVEngine.CrashMode{
		GroundKVEngine.Companion_CrashMode_.Create_CrashDropVolatile_(),
		GroundKVEngine.Companion_CrashMode_.Create_CrashFlushDataSubset_(_dafny.SetOf(uint64(4))),
		GroundKVEngine.Companion_CrashMode_.Create_CrashFlushAltMeta_(true),
		GroundKVEngine.Companion_CrashMode_.Create_CrashTornAltMeta_(),
	}
	for i, mode := range modes {
		tup := eng.ApplyCrash(s, mode)
		d0 := (*(tup.IndexInt(0))).(GroundKVEngine.MetaSlotState)
		d1 := (*(tup.IndexInt(1))).(GroundKVEngine.MetaSlotState)
		rec := eng.SelectRecoveredMeta(eng.SlotToOption(d0), eng.SlotToOption(d1))
		if !rec.Is_Ok() {
			t.Fatalf("mode %d: recovery failed", i)
		}
		m := rec.Dtor_value().(GroundKVTypes.Meta)
		if m.Dtor_txid() != 1 || m.Dtor_root() != 2 {
			t.Fatalf("mode %d: recovered txid=%d root=%d, want txid=1 root=2", i, m.Dtor_txid(), m.Dtor_root())
		}
	}
}

func TestVerifiedConcurrentReadersAndWriterRace(t *testing.T) {
	st := engine.NewMemStorage()
	db, err := verified.OpenStorage(st, &verified.Options{PageSize: 256, MaxLeafKeys: 3, MaxBranchKeys: 3})
	if err != nil {
		t.Fatalf("OpenStorage: %v", err)
	}
	for i := 0; i < 10; i++ {
		if err := db.Put([]byte(fmt.Sprintf("k%d", i)), []byte("init")); err != nil {
			t.Fatalf("seed Put: %v", err)
		}
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = db.View(func(tx *verified.Tx) error {
						_, _ = tx.All()
						return nil
					})
				}
			}
		}()
	}

	for w := 0; w < 30; w++ {
		if err := db.Put([]byte(fmt.Sprintf("k%d", w%10)), []byte(fmt.Sprintf("v%d", w))); err != nil {
			t.Fatalf("Put %d: %v", w, err)
		}
	}
	close(stop)
	wg.Wait()

	if err := db.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	_ = db.Close()
}
