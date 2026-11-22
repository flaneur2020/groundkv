package btree_mbt

import (
	"testing"

	mbt "github.com/fizzbee-io/fizzbee/mbt/lib/go"
)

// TestAdapterSmoke exercises every generated CowBTreeRole action and verifies
// byte-level append-only page immutability via GetState().
func TestAdapterSmoke(t *testing.T) {
	model := NewBtreeModel()
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
	tree := roles[mbt.RoleId{RoleName: "CowBTree", Index: 0}].(*CowBTreeRoleAdapter)
	arg := func(name string, v any) mbt.Arg { return mbt.Arg{Name: name, Value: v} }

	// Insert 3 keys to trigger a real leaf split & branch root promotion (MaxLeafKeys=2).
	for i, k := range []string{"c", "a", "b"} {
		gen, err := tree.ActionPut([]mbt.Arg{arg("k", k)})
		if err != nil || gen != int64(i+1) {
			t.Fatalf("ActionPut(%s) = (%v, %v), want (%d, nil)", k, gen, err, i+1)
		}
	}

	// Pin snapshot before deleting "b".
	pinned, err := tree.ActionPinSnapshot(nil)
	if err != nil || pinned != int64(3) {
		t.Fatalf("ActionPinSnapshot = (%v, %v), want (3, nil)", pinned, err)
	}
	if gen, err := tree.ActionDelete([]mbt.Arg{arg("k", "b")}); err != nil || gen != int64(4) {
		t.Fatalf("ActionDelete(b) = (%v, %v), want (4, nil)", gen, err)
	}

	// ScanKeys on latest root must return "a,c" in sorted order.
	keys, err := tree.ActionScanKeys(nil)
	if err != nil || keys != "a,c" {
		t.Fatalf("ActionScanKeys = (%v, %v), want a,c", keys, err)
	}

	// Pinned snapshot must still see "b"="v3".
	if v, err := tree.ActionReadPinned([]mbt.Arg{arg("k", "b")}); err != nil || v != "v3" {
		t.Fatalf("ActionReadPinned(b) = (%v, %v), want (v3, nil)", v, err)
	}

	// Verify GetState matches specs/btree/btree.fizz fields and confirms COW immutability.
	state, err := tree.GetState()
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if state["pages_immutable"] != true {
		t.Fatalf("expected pages_immutable=true, got %v", state["pages_immutable"])
	}
	if state["root_gen"] != int64(4) {
		t.Fatalf("expected root_gen=4, got %v", state["root_gen"])
	}
}
