package storage_protocol_mbt

import (
	"testing"

	mbt "github.com/fizzbee-io/fizzbee/mbt/lib/go"
)

func TestAdapterSmoke(t *testing.T) {
	model := NewStorageProtocolModel()
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
	disk := roles[mbt.RoleId{RoleName: "Disk", Index: 0}].(*DiskRoleAdapter)
	eng := roles[mbt.RoleId{RoleName: "Engine", Index: 0}].(*EngineRoleAdapter)
	arg := func(name string, v any) mbt.Arg { return mbt.Arg{Name: name, Value: v} }

	// Commit txid=1 (k1=v1)
	if _, err := eng.ActionBeginPut(nil); err != nil {
		t.Fatalf("BeginPut 1: %v", err)
	}
	for i := 0; i < 4; i++ {
		if _, err := eng.ActionCommitStep(nil); err != nil {
			t.Fatalf("CommitStep 1.%d: %v", i, err)
		}
	}

	// Pin MVCC reader at txid=1, start txid=2, advance to META_WRITTEN, then TORN crash!
	if pinned, err := eng.ActionOpenReader(nil); err != nil || pinned != int64(1) {
		t.Fatalf("OpenReader = (%v, %v), want 1", pinned, err)
	}
	if _, err := eng.ActionBeginPut(nil); err != nil {
		t.Fatalf("BeginPut 2: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := eng.ActionCommitStep(nil); err != nil {
			t.Fatalf("CommitStep 2.%d: %v", i, err)
		}
	}
	if txid, err := disk.ActionCrash([]mbt.Arg{arg("mode", "TORN")}); err != nil || txid != int64(1) {
		t.Fatalf("Crash(TORN) = (%v, %v), want 1", txid, err)
	}

	// InspectMeta directly checks both durable on-disk meta slots without shadow state.
	metaSummary, err := disk.ActionInspectMeta(nil)
	if err != nil {
		t.Fatalf("Disk.ActionInspectMeta: %v", err)
	}
	if metaSummary != "m0=0:0:NOT_FOUND|m1=1:1:v1" {
		t.Fatalf("ActionInspectMeta = %q, want m0=0:0:NOT_FOUND|m1=1:1:v1", metaSummary)
	}
	if v, err := eng.ActionRead(nil); err != nil || v != "v1" {
		t.Fatalf("Engine.ActionRead = (%v, %v), want v1", v, err)
	}
}
