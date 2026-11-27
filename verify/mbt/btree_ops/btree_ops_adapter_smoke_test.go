package btree_ops_mbt

import (
	"testing"

	mbt "github.com/fizzbee-io/fizzbee/mbt/lib/go"
)

func TestAdapterSmoke(t *testing.T) {
	model := NewBtreeOpsModel()
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
	tree := roles[mbt.RoleId{RoleName: "BTree", Index: 0}].(*BTreeRoleAdapter)
	arg := func(name string, v any) mbt.Arg { return mbt.Arg{Name: name, Value: v} }

	for _, k := range []string{"d", "b", "a", "c"} {
		if _, err := tree.ActionPut([]mbt.Arg{arg("k", k)}); err != nil {
			t.Fatalf("Put(%s): %v", k, err)
		}
	}
	if got, err := tree.ActionScanRange([]mbt.Arg{arg("start", "b"), arg("end", "d")}); err != nil || got != "b=v2,c=v4" {
		t.Fatalf("ScanRange[b, d) = (%v, %v), want b=v2,c=v4", got, err)
	}
	if _, err := tree.ActionDelete([]mbt.Arg{arg("k", "b")}); err != nil {
		t.Fatalf("Delete(b): %v", err)
	}
	if got, err := tree.ActionScanRange([]mbt.Arg{arg("start", ""), arg("end", "")}); err != nil || got != "a=v3,c=v4,d=v1" {
		t.Fatalf("ScanRange all = (%v, %v), want a=v3,c=v4,d=v1", got, err)
	}
}
