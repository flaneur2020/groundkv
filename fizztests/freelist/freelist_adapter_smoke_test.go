package freelist_mbt

import (
	"testing"

	mbt "github.com/fizzbee-io/fizzbee/mbt/lib/go"
)

// TestAdapterSmoke verifies MVCC reader-aware freelist blocking, release upon
// reader close, page recycling, and crash recovery through FreelistDBRoleAdapter.
func TestAdapterSmoke(t *testing.T) {
	model := NewFreelistModel()
	if err := model.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer func() {
		if err := model.Cleanup(); err != nil {
			t.Fatalf("Cleanup: %v", err)
		}
	}()

	roles, err := model.GetRoles()
	if err != nil {
		t.Fatalf("GetRoles: %v", err)
	}
	db := roles[mbt.RoleId{RoleName: "FreelistDB", Index: 0}].(*FreelistDBRoleAdapter)
	arg := func(name string, v any) mbt.Arg { return mbt.Arg{Name: name, Value: v} }

	// txid=1: k1=v1
	if txid, err := db.ActionPut([]mbt.Arg{arg("k", "k1")}); err != nil || txid != int64(1) {
		t.Fatalf("Put 1 = (%v, %v), want 1", txid, err)
	}
	// Pin reader r1 at txid=1
	if pinned, err := db.ActionOpenReader([]mbt.Arg{arg("rid", "r1")}); err != nil || pinned != int64(1) {
		t.Fatalf("OpenReader r1 = (%v, %v), want 1", pinned, err)
	}
	// txid=2 & txid=3 overwrite k1: pending[2] and pending[3] MUST remain blocked by r1!
	if _, err := db.ActionPut([]mbt.Arg{arg("k", "k1")}); err != nil {
		t.Fatalf("Put 2: %v", err)
	}
	if _, err := db.ActionPut([]mbt.Arg{arg("k", "k1")}); err != nil {
		t.Fatalf("Put 3: %v", err)
	}
	st, err := db.GetState()
	if err != nil {
		t.Fatalf("GetState blocked: %v", err)
	}
	if st["min_reader_txid"] != int64(1) || st["release_bound"] != int64(1) {
		t.Fatalf("expected min_reader_txid=1, release_bound=1, got %v, %v", st["min_reader_txid"], st["release_bound"])
	}
	if st["pending_txids"] != "2,3" {
		t.Fatalf("expected pending_txids=2,3 while r1 open, got %v", st["pending_txids"])
	}
	if st["no_live_page_in_free"] != true || st["no_pending_below_bound"] != true {
		t.Fatalf("safety flags violated: %+v", st)
	}
	// Reader r1 must still see v1!
	if v, err := db.ActionReadFromReader([]mbt.Arg{arg("rid", "r1"), arg("k", "k1")}); err != nil || v != "v1" {
		t.Fatalf("ReadFromReader(r1, k1) = (%v, %v), want v1", v, err)
	}

	// Closing r1 advances release_bound from 1 -> 2, releasing pending[2]!
	if res, err := db.ActionCloseReader([]mbt.Arg{arg("rid", "r1")}); err != nil || res != "OK" {
		t.Fatalf("CloseReader r1 = (%v, %v)", res, err)
	}
	st, err = db.GetState()
	if err != nil {
		t.Fatalf("GetState after close: %v", err)
	}
	if st["min_reader_txid"] != int64(-1) || st["release_bound"] != int64(2) {
		t.Fatalf("expected min_reader_txid=-1, release_bound=2, got %v, %v", st["min_reader_txid"], st["release_bound"])
	}
	if st["pending_txids"] != "3" {
		t.Fatalf("expected pending_txids=3 after releasing tx2, got %v", st["pending_txids"])
	}

	// txid=4 recycles released pages; CrashAndReopen recovers freelist cleanly.
	if txid, err := db.ActionPut([]mbt.Arg{arg("k", "k2")}); err != nil || txid != int64(4) {
		t.Fatalf("Put 4 = (%v, %v), want 4", txid, err)
	}
	if txid, err := db.ActionCrashAndReopen(nil); err != nil || txid != int64(4) {
		t.Fatalf("CrashAndReopen = (%v, %v), want 4", txid, err)
	}
	st, err = db.GetState()
	if err != nil {
		t.Fatalf("GetState after crash: %v", err)
	}
	if st["pending_txids"] != "4" || st["no_live_page_in_free"] != true {
		t.Fatalf("unexpected post-crash state: %+v", st)
	}
}
