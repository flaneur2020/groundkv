package contract_mbt

import (
	"testing"

	mbt "github.com/fizzbee-io/fizzbee/mbt/lib/go"
)

func TestAdapterSmoke(t *testing.T) {
	model := NewKvContractModel()
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
	writer := roles[mbt.RoleId{RoleName: "Writer", Index: 0}].(WriterRole)
	reader0 := roles[mbt.RoleId{RoleName: "Reader", Index: 0}].(ReaderRole)
	snap := roles[mbt.RoleId{RoleName: "SnapshotReader", Index: 0}].(SnapshotReaderRole)
	arg := func(name string, v any) mbt.Arg { return mbt.Arg{Name: name, Value: v} }

	if _, err := writer.ActionBatchSwap([]mbt.Arg{arg("v", "v1")}); err != nil {
		t.Fatalf("BatchSwap: %v", err)
	}
	if _, err := snap.ActionBeginSnapshot(nil); err != nil {
		t.Fatalf("BeginSnapshot: %v", err)
	}
	if _, err := writer.ActionAbortedPut([]mbt.Arg{arg("k", "k1"), arg("v", "v2")}); err != nil {
		t.Fatalf("AbortedPut: %v", err)
	}
	if scan, err := reader0.ActionScanAll(nil); err != nil || scan != "k1=v1,k2=v1" {
		t.Fatalf("ScanAll = (%v, %v), want k1=v1,k2=v1", scan, err)
	}
	if _, err := writer.ActionPut([]mbt.Arg{arg("k", "k1"), arg("v", "v2")}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if v, err := snap.ActionSnapshotGet([]mbt.Arg{arg("k", "k1")}); err != nil || v != "v1" {
		t.Fatalf("SnapshotGet = (%v, %v), want v1", v, err)
	}
	if _, err := snap.ActionEndSnapshot(nil); err != nil {
		t.Fatalf("EndSnapshot: %v", err)
	}
}
