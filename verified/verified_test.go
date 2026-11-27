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
)

func TestVerifiedBasicCRUDAndPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "verified.db")
	opts := &engine.Options{
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

	db2, err := verified.Open(path, nil)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer db2.Close()

	if err := db2.Validate(); err != nil {
		t.Fatalf("Validate after reopen: %v", err)
	}
	var got map[string]string
	err = db2.View(func(tx *engine.Tx) error {
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

	var scanned []string
	err = db2.View(func(tx *engine.Tx) error {
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
	opts := &engine.Options{
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

	snapTx, err := db.BeginRead()
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}
	snapTxID := snapTx.TxID()
	snapExpected, err := snapTx.All()
	if err != nil {
		t.Fatalf("snap All: %v", err)
	}

	for round := 0; round < 25; round++ {
		k := fmt.Sprintf("k%02d", round%15)
		v := fmt.Sprintf("v2-round-%02d", round)
		if err := db.Put([]byte(k), []byte(v)); err != nil {
			t.Fatalf("round %d put: %v", round, err)
		}
		if rb := db.ReleaseBound(); rb > snapTxID {
			t.Fatalf("ReleaseBound %d exceeded pinned reader txid %d", rb, snapTxID)
		}
		reach, err := db.ReachablePages(snapTx.RootPageID(), 0)
		if err != nil {
			t.Fatalf("ReachablePages: %v", err)
		}
		freeSet := make(map[engine.PageID]bool)
		for _, id := range db.FreelistState().FreeIDs {
			freeSet[id] = true
		}
		for _, id := range reach {
			if freeSet[id] {
				t.Fatalf("round %d: pinned reader page %d leaked into reusable free pool!", round, id)
			}
		}
	}

	snapAfter, err := snapTx.All()
	if err != nil {
		t.Fatalf("snap All after writes: %v", err)
	}
	if !reflect.DeepEqual(snapAfter, snapExpected) {
		t.Fatalf("MVCC snapshot isolation violated")
	}
	_ = snapTx.Rollback()

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
	opts := &engine.Options{PageSize: 256, MaxLeafKeys: 3, MaxBranchKeys: 3}

	db, err := verified.OpenStorage(fs, opts)
	if err != nil {
		t.Fatalf("OpenStorage: %v", err)
	}
	if err := db.Put([]byte("alpha"), []byte("committed-v1")); err != nil {
		t.Fatalf("seed Put: %v", err)
	}

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

// TestCrossEngineDifferentialAndOnDiskInterop verifies that GoCore (`engine.Open`)
// and DafnyCore (`verified.Open`) produce bit-compatible on-disk databases and
// identical KV/Meta/Freelist state across randomized splits, deletes, and
// bidirectional on-disk reopens.
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
			_ = dafnyDB.View(func(tx *engine.Tx) error { dafnyAll, err = tx.All(); return err })
			if !reflect.DeepEqual(goAll, ref) || !reflect.DeepEqual(dafnyAll, ref) {
				t.Fatalf("step %d KV map mismatch", step)
			}
		}
	}

	// Bidirectional on-disk interoperability: verified -> engine -> verified
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
	_ = vdb2.View(func(tx *engine.Tx) error { vdb2All, err = tx.All(); return err })
	if !reflect.DeepEqual(vdb2All, ref) {
		t.Fatalf("verified.Open read mismatch on file mutated by engine.DB")
	}
}

func TestVerifiedConcurrentReadersAndWriterRace(t *testing.T) {
	st := engine.NewMemStorage()
	db, err := verified.OpenStorage(st, &engine.Options{PageSize: 256, MaxLeafKeys: 3, MaxBranchKeys: 3})
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
					_ = db.View(func(tx *engine.Tx) error {
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

// TestCompositeCoreMixAndMatch verifies that callers can compose domain-specific
// sub-interfaces from GoCore and verified.NewCore into a unified engine.Core.
func TestCompositeCoreMixAndMatch(t *testing.T) {
	goCore := engine.GoCore()
	dafnyCore := verified.NewCore()

	hybrid := engine.CompositeCore{
		PageCodec:    goCore,
		MetaCore:     dafnyCore,
		BTreeCore:    dafnyCore,
		FreelistCore: dafnyCore,
		CommitCore:   goCore,
	}

	st := engine.NewMemStorage()
	opts := &engine.Options{
		PageSize:       256,
		MaxLeafKeys:    3,
		MaxBranchKeys:  3,
		MaxFreelistIDs: 3,
		Core:           hybrid,
	}
	db, err := engine.OpenStorage(st, opts)
	if err != nil {
		t.Fatalf("OpenStorage hybrid: %v", err)
	}
	defer db.Close()

	want := make(map[string]string)
	for i := 0; i < 40; i++ {
		k := fmt.Sprintf("hk%02d", i%20)
		v := fmt.Sprintf("hv-%02d", i)
		if err := db.Put([]byte(k), []byte(v)); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
		want[k] = v
	}
	if err := db.Delete([]byte("hk05")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	delete(want, "hk05")

	if err := db.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	var got map[string]string
	if err := db.View(func(tx *engine.Tx) error {
		var allErr error
		got, allErr = tx.All()
		return allErr
	}); err != nil {
		t.Fatalf("All: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hybrid core map mismatch: got %d keys, want %d keys", len(got), len(want))
	}
}
