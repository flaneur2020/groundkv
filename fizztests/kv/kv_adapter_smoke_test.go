package kv_mbt

import (
	"testing"

	mbt "github.com/fizzbee-io/fizzbee/mbt/lib/go"
)

// TestAdapterSmoke exercises every generated DBRole action and GetState()
// directly in Go so adapter regressions can be caught even without running the
// external fizzbee-mbt-runner binary.
func TestAdapterSmoke(t *testing.T) {
	model := NewKvModel()
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
	db := roles[mbt.RoleId{RoleName: "DB", Index: 0}].(*DBRoleAdapter)

	arg := func(name string, v any) mbt.Arg { return mbt.Arg{Name: name, Value: v} }

	// Put k1=v1
	if res, err := db.ActionPut([]mbt.Arg{arg("k", "k1"), arg("v", "v1")}); err != nil || res != "OK" {
		t.Fatalf("ActionPut = (%v, %v), want (OK, nil)", res, err)
	}
	// Pin MVCC snapshot
	if res, err := db.ActionBeginRead(nil); err != nil || res != "OK" {
		t.Fatalf("ActionBeginRead = (%v, %v), want (OK, nil)", res, err)
	}
	// Mutate k1=v2 while snapshot is open
	if res, err := db.ActionPut([]mbt.Arg{arg("k", "k1"), arg("v", "v2")}); err != nil || res != "OK" {
		t.Fatalf("ActionPut 2 = (%v, %v), want (OK, nil)", res, err)
	}
	// Snapshot still sees v1; latest Get sees v2
	if res, err := db.ActionSnapshotGet([]mbt.Arg{arg("k", "k1")}); err != nil || res != "v1" {
		t.Fatalf("ActionSnapshotGet = (%v, %v), want (v1, nil)", res, err)
	}
	if res, err := db.ActionGet([]mbt.Arg{arg("k", "k1")}); err != nil || res != "v2" {
		t.Fatalf("ActionGet = (%v, %v), want (v2, nil)", res, err)
	}
	if res, err := db.ActionEndRead(nil); err != nil || res != "OK" {
		t.Fatalf("ActionEndRead = (%v, %v), want (OK, nil)", res, err)
	}

	// Stage uncommitted write, then crash & reopen: must not leak.
	if res, err := db.ActionBeginWrite(nil); err != nil || res != "OK" {
		t.Fatalf("ActionBeginWrite = (%v, %v)", res, err)
	}
	if res, err := db.ActionStagePut([]mbt.Arg{arg("k", "k2"), arg("v", "v1")}); err != nil || res != "OK" {
		t.Fatalf("ActionStagePut = (%v, %v)", res, err)
	}
	if res, err := db.ActionCrashAndReopen(nil); err != nil || res != int64(2) {
		t.Fatalf("ActionCrashAndReopen = (%v, %v), want (2, nil)", res, err)
	}
	if res, err := db.ActionGet([]mbt.Arg{arg("k", "k2")}); err != nil || res != notFoundSentinel {
		t.Fatalf("ActionGet(k2) after crash = (%v, %v), want NOT_FOUND", res, err)
	}

	// Verify GetState exposes every field required by specs/kv/kv.fizz.
	state, err := db.GetState()
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	for _, field := range []string{
		"txid", "meta0_txid", "meta1_txid", "meta0", "meta1",
		"committed", "has_snapshot", "snapshot", "in_write_tx", "staged",
	} {
		if _, ok := state[field]; !ok {
			t.Fatalf("GetState missing field %q", field)
		}
	}
	if state["txid"] != int64(2) {
		t.Fatalf("GetState txid = %v, want 2", state["txid"])
	}
}
