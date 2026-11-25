package engine_test

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/flaneur2020/groundkv/engine"
)

func TestBasicCRUDAndPersistenceOnDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := engine.Open(path, &engine.Options{
		PageSize:      512,
		MaxLeafKeys:   3,
		MaxBranchKeys: 3,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	for i := 0; i < 40; i++ {
		k := fmt.Sprintf("key-%03d", i)
		v := fmt.Sprintf("val-%03d", i)
		if err := db.Put([]byte(k), []byte(v)); err != nil {
			t.Fatalf("Put(%s): %v", k, err)
		}
	}
	if err := db.Validate(); err != nil {
		t.Fatalf("Validate after puts: %v", err)
	}

	// Update half, delete a quarter.
	for i := 0; i < 20; i++ {
		k := fmt.Sprintf("key-%03d", i)
		v := fmt.Sprintf("updated-%03d", i)
		if err := db.Put([]byte(k), []byte(v)); err != nil {
			t.Fatalf("Put update(%s): %v", k, err)
		}
	}
	for i := 30; i < 40; i++ {
		k := fmt.Sprintf("key-%03d", i)
		if err := db.Delete([]byte(k)); err != nil {
			t.Fatalf("Delete(%s): %v", k, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen from disk and verify everything persisted cleanly.
	db2, err := engine.Open(path, nil)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer db2.Close()

	if err := db2.Validate(); err != nil {
		t.Fatalf("Validate after reopen: %v", err)
	}

	for i := 0; i < 40; i++ {
		k := fmt.Sprintf("key-%03d", i)
		got, err := db2.Get([]byte(k))
		if i >= 30 {
			if !errors.Is(err, engine.ErrNotFound) {
				t.Fatalf("expected ErrNotFound for %s, got (%q, %v)", k, got, err)
			}
			continue
		}
		want := fmt.Sprintf("val-%03d", i)
		if i < 20 {
			want = fmt.Sprintf("updated-%03d", i)
		}
		if err != nil || string(got) != want {
			t.Fatalf("Get(%s) = (%q, %v), want %q", k, got, err, want)
		}
	}

	// Range scan check [key-010, key-015)
	var scanned []string
	err = db2.View(func(tx *engine.Tx) error {
		return tx.Scan([]byte("key-010"), []byte("key-015"), func(k, v []byte) bool {
			scanned = append(scanned, string(k))
			return true
		})
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	wantScan := []string{"key-010", "key-011", "key-012", "key-013", "key-014"}
	if fmt.Sprint(scanned) != fmt.Sprint(wantScan) {
		t.Fatalf("Scan = %v, want %v", scanned, wantScan)
	}
}

func TestMVCCSnapshotIsolationAndConcurrentReaders(t *testing.T) {
	st := engine.NewMemStorage()
	db, err := engine.OpenStorage(st, &engine.Options{
		PageSize:      512,
		MaxLeafKeys:   2,
		MaxBranchKeys: 2,
	})
	if err != nil {
		t.Fatalf("OpenStorage: %v", err)
	}
	defer db.Close()

	if err := db.Put([]byte("k1"), []byte("v1")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Pin snapshot at txid=1.
	snap1, err := db.BeginRead()
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}
	defer snap1.Rollback()

	// Mutate DB concurrently while readers read both old snapshot and latest state.
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				got, err := snap1.Get([]byte("k1"))
				if err != nil || string(got) != "v1" {
					t.Errorf("snap1.Get(k1) = (%q, %v), want v1", got, err)
					return
				}
				if _, err := snap1.Get([]byte("k2")); !errors.Is(err, engine.ErrNotFound) {
					t.Errorf("snap1.Get(k2) should be ErrNotFound, got %v", err)
					return
				}
			}
		}()
	}

	for i := 0; i < 20; i++ {
		if err := db.Put([]byte("k1"), []byte(fmt.Sprintf("v-%d", i))); err != nil {
			t.Fatalf("Put k1: %v", err)
		}
		if err := db.Put([]byte("k2"), []byte(fmt.Sprintf("w-%d", i))); err != nil {
			t.Fatalf("Put k2: %v", err)
		}
	}
	wg.Wait()

	// Latest view sees updated values while snap1 still sees original values.
	latest, err := db.Get([]byte("k1"))
	if err != nil || string(latest) != "v-19" {
		t.Fatalf("latest Get(k1) = (%q, %v), want v-19", latest, err)
	}
	old, err := snap1.Get([]byte("k1"))
	if err != nil || string(old) != "v1" {
		t.Fatalf("snap1 Get(k1) = (%q, %v), want v1", old, err)
	}
}

func TestRollbackAndCrashRecoveryDualMeta(t *testing.T) {
	st := engine.NewMemStorage()
	opts := &engine.Options{PageSize: 512, MaxLeafKeys: 2, MaxBranchKeys: 2}
	db, err := engine.OpenStorage(st, opts)
	if err != nil {
		t.Fatalf("OpenStorage: %v", err)
	}

	if err := db.Put([]byte("a"), []byte("1")); err != nil {
		t.Fatalf("Put a: %v", err)
	}
	if err := db.Put([]byte("b"), []byte("2")); err != nil {
		t.Fatalf("Put b: %v", err)
	}

	// Open a write transaction, stage changes, then rollback.
	wtx, err := db.BeginWrite()
	if err != nil {
		t.Fatalf("BeginWrite: %v", err)
	}
	if err := wtx.Put([]byte("a"), []byte("uncommitted")); err != nil {
		t.Fatalf("wtx.Put: %v", err)
	}
	if err := wtx.Rollback(); err != nil {
		t.Fatalf("wtx.Rollback: %v", err)
	}

	// Stage another write tx and simulate abrupt crash BEFORE commit.
	wtx2, err := db.BeginWrite()
	if err != nil {
		t.Fatalf("BeginWrite 2: %v", err)
	}
	if err := wtx2.Put([]byte("c"), []byte("ghost")); err != nil {
		t.Fatalf("wtx2.Put: %v", err)
	}

	// Crash drops uncommitted tx and unsynced writes.
	st2 := st.SimulateCrash()
	db2, err := engine.OpenStorage(st2, opts)
	if err != nil {
		t.Fatalf("Reopen after crash: %v", err)
	}

	if got, err := db2.Get([]byte("a")); err != nil || string(got) != "1" {
		t.Fatalf("Get(a) after crash = (%q, %v), want 1", got, err)
	}
	if _, err := db2.Get([]byte("c")); !errors.Is(err, engine.ErrNotFound) {
		t.Fatalf("uncommitted key c leaked after crash: %v", err)
	}

	// Now corrupt the newer meta page (txid=2 lives on slot 0) and verify recovery
	// falls back cleanly to the previous valid meta page (txid=1 on slot 1).
	st2.CorruptPage(0, opts.PageSize)
	st3 := st2.SimulateCrash()
	db3, err := engine.OpenStorage(st3, opts)
	if err != nil {
		t.Fatalf("Reopen with torn meta0: %v", err)
	}
	defer db3.Close()

	if db3.Meta().TxID != 1 {
		t.Fatalf("expected fallback to txid=1, got %d", db3.Meta().TxID)
	}
	if got, err := db3.Get([]byte("a")); err != nil || string(got) != "1" {
		t.Fatalf("fallback Get(a) = (%q, %v), want 1", got, err)
	}
	if _, err := db3.Get([]byte("b")); !errors.Is(err, engine.ErrNotFound) {
		t.Fatalf("expected b absent in fallback txid=1, got %v", err)
	}
}

func TestAppendOnlyCOWPageImmutabilityProperty(t *testing.T) {
	st := engine.NewMemStorage()
	db, err := engine.OpenStorage(st, &engine.Options{
		PageSize:      512,
		MaxLeafKeys:   2,
		MaxBranchKeys: 2,
		NoFreelist:    true, // Pure append-only mode: every historical page remains immutable forever
	})
	if err != nil {
		t.Fatalf("OpenStorage: %v", err)
	}
	defer db.Close()

	rng := rand.New(rand.NewSource(42))
	keys := []string{"a", "b", "c", "d", "e", "f", "g"}
	ref := make(map[string]string)
	savedPages := make(map[engine.PageID][]byte)

	for step := 0; step < 120; step++ {
		k := keys[rng.Intn(len(keys))]
		if rng.Intn(3) == 0 {
			if err := db.Delete([]byte(k)); err != nil {
				t.Fatalf("step %d Delete(%s): %v", step, k, err)
			}
			delete(ref, k)
		} else {
			v := fmt.Sprintf("v%d", step)
			if err := db.Put([]byte(k), []byte(v)); err != nil {
				t.Fatalf("step %d Put(%s): %v", step, k, err)
			}
			ref[k] = v
		}

		if err := db.Validate(); err != nil {
			t.Fatalf("step %d Validate: %v", step, err)
		}

		// Assert every previously committed B+Tree page is byte-for-byte immutable.
		meta := db.Meta()
		for pgid, orig := range savedPages {
			cur, err := db.RawPage(pgid)
			if err != nil {
				t.Fatalf("step %d RawPage(%d): %v", step, pgid, err)
			}
			if !bytes.Equal(orig, cur) {
				t.Fatalf("step %d COW violation: committed page %d was mutated in place!", step, pgid)
			}
		}
		for pgid := engine.PageID(2); pgid < meta.HighWater; pgid++ {
			if _, ok := savedPages[pgid]; ok {
				continue
			}
			raw, err := db.RawPage(pgid)
			if err != nil {
				t.Fatalf("step %d RawPage new %d: %v", step, pgid, err)
			}
			cp := make([]byte, len(raw))
			copy(cp, raw)
			savedPages[pgid] = cp
		}

		// Assert full Scan matches sorted reference oracle.
		var gotKeys []string
		err := db.View(func(tx *engine.Tx) error {
			return tx.Scan(nil, nil, func(k, v []byte) bool {
				gotKeys = append(gotKeys, string(k))
				if ref[string(k)] != string(v) {
					t.Errorf("step %d value mismatch for %s: got %q, want %q", step, k, v, ref[string(k)])
				}
				return true
			})
		})
		if err != nil {
			t.Fatalf("step %d Scan: %v", step, err)
		}
		wantKeys := make([]string, 0, len(ref))
		for k := range ref {
			wantKeys = append(wantKeys, k)
		}
		sort.Strings(wantKeys)
		if fmt.Sprint(gotKeys) != fmt.Sprint(wantKeys) {
			t.Fatalf("step %d Scan keys = %v, want %v", step, gotKeys, wantKeys)
		}
	}
}

func TestMidCommitCrashDurabilityAndWriteReordering(t *testing.T) {
	opts := &engine.Options{PageSize: 512, MaxLeafKeys: 2, MaxBranchKeys: 2}

	// Verify crashes at every phase of the 4-phase commit protocol under all 3 crash modes.
	type crashPoint struct {
		name      string
		stopPhase int // 1=after WriteDataPages, 2=after SyncDataPages, 3=after WriteMetaPage
		mode      engine.CrashMode
		wantTxID  uint64
		wantK2    bool
	}

	cases := []crashPoint{
		{"after_data_write_drop", 1, engine.CrashDropUnsynced, 1, false},
		{"after_data_write_reordered", 1, engine.CrashReorderedMetaOnly, 1, false},
		{"after_data_sync_drop", 2, engine.CrashDropUnsynced, 1, false},
		{"after_meta_write_drop", 3, engine.CrashDropUnsynced, 1, false},
		{"after_meta_write_torn", 3, engine.CrashTornMetaWrite, 1, false},
		{"after_meta_write_reordered_lands_cleanly", 3, engine.CrashReorderedMetaOnly, 2, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := engine.NewMemStorage()
			db, err := engine.OpenStorage(st, opts)
			if err != nil {
				t.Fatalf("OpenStorage: %v", err)
			}
			if err := db.Put([]byte("k1"), []byte("v1")); err != nil {
				t.Fatalf("Put k1: %v", err)
			}

			tx, err := db.BeginWrite()
			if err != nil {
				t.Fatalf("BeginWrite: %v", err)
			}
			if err := tx.Put([]byte("k2"), []byte("v2")); err != nil {
				t.Fatalf("tx.Put k2: %v", err)
			}
			if tc.stopPhase >= 1 {
				if err := tx.WriteDataPages(); err != nil {
					t.Fatalf("WriteDataPages: %v", err)
				}
			}
			if tc.stopPhase >= 2 {
				if err := tx.SyncDataPages(); err != nil {
					t.Fatalf("SyncDataPages: %v", err)
				}
			}
			if tc.stopPhase >= 3 {
				if err := tx.WriteMetaPage(); err != nil {
					t.Fatalf("WriteMetaPage: %v", err)
				}
			}

			recovered := st.SimulateCrashWithMode(tc.mode, opts.PageSize)
			db2, err := engine.OpenStorage(recovered, opts)
			if err != nil {
				t.Fatalf("Reopen after %s: %v", tc.name, err)
			}
			defer db2.Close()

			if err := db2.Validate(); err != nil {
				t.Fatalf("Validate after %s: %v", tc.name, err)
			}
			if db2.Meta().TxID != tc.wantTxID {
				t.Fatalf("recovered txid = %d, want %d", db2.Meta().TxID, tc.wantTxID)
			}
			if got, err := db2.Get([]byte("k1")); err != nil || string(got) != "v1" {
				t.Fatalf("Get(k1) = (%q, %v), want v1", got, err)
			}
			got2, err2 := db2.Get([]byte("k2"))
			if tc.wantK2 {
				if err2 != nil || string(got2) != "v2" {
					t.Fatalf("Get(k2) = (%q, %v), want v2", got2, err2)
				}
			} else if !errors.Is(err2, engine.ErrNotFound) {
				t.Fatalf("Get(k2) = (%q, %v), want ErrNotFound", got2, err2)
			}
		})
	}

	// Negative-control proof: if NoSync disables the data-page sync barrier,
	// CrashReorderedMetaOnly persists the new meta page while dropping its B+Tree
	// data pages — proving our fault injector actually catches missing Sync barriers!
	t.Run("negative_control_missing_sync_barrier_detected", func(t *testing.T) {
		noSyncOpts := &engine.Options{PageSize: 512, MaxLeafKeys: 2, MaxBranchKeys: 2, NoSync: true}
		st := engine.NewMemStorage()
		// Initialize with sync enabled first so meta 0/1 exist durably.
		db, err := engine.OpenStorage(st, opts)
		if err != nil {
			t.Fatalf("OpenStorage: %v", err)
		}
		if err := db.Put([]byte("k1"), []byte("v1")); err != nil {
			t.Fatalf("Put k1: %v", err)
		}
		// Reopen on same storage with NoSync=true to simulate a buggy commit missing SyncDataPages.
		dbNoSync, err := engine.OpenStorage(st, noSyncOpts)
		if err != nil {
			t.Fatalf("OpenStorage NoSync: %v", err)
		}
		tx, err := dbNoSync.BeginWrite()
		if err != nil {
			t.Fatalf("BeginWrite: %v", err)
		}
		if err := tx.Put([]byte("k2"), []byte("v2")); err != nil {
			t.Fatalf("tx.Put: %v", err)
		}
		if err := tx.WriteDataPages(); err != nil {
			t.Fatalf("WriteDataPages: %v", err)
		}
		if err := tx.SyncDataPages(); err != nil { // No-op due to NoSync=true!
			t.Fatalf("SyncDataPages: %v", err)
		}
		if err := tx.WriteMetaPage(); err != nil {
			t.Fatalf("WriteMetaPage: %v", err)
		}

		// Reordered flush writes meta slot 0 pointing to unsynced (dropped!) B+Tree root page.
		corrupted := st.SimulateCrashWithMode(engine.CrashReorderedMetaOnly, opts.PageSize)
		dbRecovered, err := engine.OpenStorage(corrupted, opts)
		if err != nil {
			// Caught directly at OpenStorage!
			return
		}
		defer dbRecovered.Close()
		if err := dbRecovered.Validate(); err == nil {
			t.Fatalf("expected Validate() to catch dangling root when data Sync barrier was skipped")
		}
	})
}

func TestFreelistReclamationAndMVCCReaderBarrier(t *testing.T) {
	opts := &engine.Options{PageSize: 512, MaxLeafKeys: 2, MaxBranchKeys: 2}
	st := engine.NewMemStorage()
	db, err := engine.OpenStorage(st, opts)
	if err != nil {
		t.Fatalf("OpenStorage: %v", err)
	}

	// 1. Pin MVCC reader at txid=1 and verify its pages are NOT recycled across
	// many subsequent commits while it remains open.
	if err := db.Put([]byte("k1"), []byte("orig")); err != nil {
		t.Fatalf("Put orig: %v", err)
	}
	r1, err := db.BeginRead()
	if err != nil {
		t.Fatalf("BeginRead r1: %v", err)
	}

	for i := 0; i < 15; i++ {
		if err := db.Put([]byte("k1"), []byte(fmt.Sprintf("val-%d", i))); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
		if err := db.Put([]byte("k2"), []byte(fmt.Sprintf("other-%d", i))); err != nil {
			t.Fatalf("Put k2 %d: %v", i, err)
		}
	}

	if got, err := r1.Get([]byte("k1")); err != nil || string(got) != "orig" {
		t.Fatalf("r1.Get(k1) = (%q, %v), want orig", got, err)
	}
	if fl := db.FreelistState(); fl.ReclaimedHits != 0 {
		t.Fatalf("expected 0 reclaimed pages while reader at txid=1 is open, got %d", fl.ReclaimedHits)
	}

	// 2. Closing r1 immediately releases pending pages into free.
	if err := r1.Rollback(); err != nil {
		t.Fatalf("r1.Rollback: %v", err)
	}
	flAfterClose := db.FreelistState()
	if len(flAfterClose.FreeIDs) == 0 {
		t.Fatalf("expected freed pages to be released after closing r1")
	}

	// 3. Verify write tx Rollback returns popped free pages without leaking.
	freeBeforeRollback := len(flAfterClose.FreeIDs)
	wtx, err := db.BeginWrite()
	if err != nil {
		t.Fatalf("BeginWrite: %v", err)
	}
	if err := wtx.Put([]byte("k3"), []byte("temp")); err != nil {
		t.Fatalf("wtx.Put: %v", err)
	}
	if err := wtx.Rollback(); err != nil {
		t.Fatalf("wtx.Rollback: %v", err)
	}
	if got := len(db.FreelistState().FreeIDs); got != freeBeforeRollback {
		t.Fatalf("free count after write rollback = %d, want %d", got, freeBeforeRollback)
	}

	// 4. Steady-state overwrites recycle freed pages so HighWater stops growing!
	hwBefore := db.Meta().HighWater
	for i := 0; i < 40; i++ {
		if err := db.Put([]byte("k1"), []byte(fmt.Sprintf("steady-%d", i))); err != nil {
			t.Fatalf("steady Put %d: %v", i, err)
		}
		if err := db.Validate(); err != nil {
			t.Fatalf("steady Validate %d: %v", i, err)
		}
	}
	flSteady := db.FreelistState()
	if flSteady.ReclaimedHits == 0 {
		t.Fatalf("expected pages to be reclaimed during steady-state writes")
	}
	if db.Meta().HighWater > hwBefore+4 {
		t.Fatalf("HighWater grew unboundedly despite freelist reclamation: before=%d, after=%d", hwBefore, db.Meta().HighWater)
	}

	// 5. Crash & reopen reloads on-disk Meta.Freelist and reclaims pages on subsequent writes.
	st2 := st.SimulateCrash()
	db2, err := engine.OpenStorage(st2, opts)
	if err != nil {
		t.Fatalf("Reopen after crash: %v", err)
	}
	defer db2.Close()

	if err := db2.Validate(); err != nil {
		t.Fatalf("Validate after reopen: %v", err)
	}
	hwReopen := db2.Meta().HighWater
	for i := 0; i < 10; i++ {
		if err := db2.Put([]byte("k1"), []byte(fmt.Sprintf("post-crash-%d", i))); err != nil {
			t.Fatalf("post-crash Put %d: %v", i, err)
		}
	}
	if db2.FreelistState().ReclaimedHits == 0 {
		t.Fatalf("expected recovered on-disk freelist pages to be reclaimed after reopen")
	}
	if db2.Meta().HighWater > hwReopen+6 {
		t.Fatalf("HighWater grew unexpectedly after reopen: before=%d, after=%d", hwReopen, db2.Meta().HighWater)
	}
}

func TestSyncFailureRecoveryAndMultiPageFreelistChain(t *testing.T) {
	opts := &engine.Options{
		PageSize:       512,
		MaxLeafKeys:    2,
		MaxBranchKeys:  2,
		MaxFreelistIDs: 3, // Force multi-page linked freelist chains (next != 0)
	}
	st := engine.NewMemStorage()
	db, err := engine.OpenStorage(st, opts)
	if err != nil {
		t.Fatalf("OpenStorage: %v", err)
	}

	// Build a multi-level tree and churn writes to generate multi-page freelist chains.
	for i := 0; i < 12; i++ {
		if err := db.Put([]byte(fmt.Sprintf("k%02d", i%5)), []byte(fmt.Sprintf("v%02d", i))); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}

	// Inject a Sync() failure during Update() — must return error AND release writeTx lock!
	st.SetFailSync(true)
	if err := db.Put([]byte("k00"), []byte("should-fail")); err == nil {
		t.Fatalf("expected Put to fail when Sync fails")
	}
	st.SetFailSync(false)

	// Subsequent writes must succeed (proving db.Update rolled back and released writer lock).
	if err := db.Put([]byte("k00"), []byte("recovered-write")); err != nil {
		t.Fatalf("Put after Sync failure recovery failed (writer lock leaked?): %v", err)
	}
	if got, err := db.Get([]byte("k00")); err != nil || string(got) != "recovered-write" {
		t.Fatalf("Get(k00) = (%q, %v), want recovered-write", got, err)
	}

	// Reopen from durable image and confirm multi-page freelist chain decodes cleanly.
	st2 := st.SimulateCrash()
	db2, err := engine.OpenStorage(st2, opts)
	if err != nil {
		t.Fatalf("Reopen multi-page freelist DB: %v", err)
	}
	defer db2.Close()
	if err := db2.Validate(); err != nil {
		t.Fatalf("Validate after reopen: %v", err)
	}
	if got, err := db2.Get([]byte("k00")); err != nil || string(got) != "recovered-write" {
		t.Fatalf("reopened Get(k00) = (%q, %v), want recovered-write", got, err)
	}
}
