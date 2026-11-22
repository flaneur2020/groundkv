package durability_mbt

import (
	"testing"

	mbt "github.com/fizzbee-io/fizzbee/mbt/lib/go"
)

// TestAdapterSmoke exercises all 4 commit phases and all 3 power-loss modes
// (drop unsynced, reordered flush, and back-to-back torn meta writes) against
// the real GroundKV engine.
func TestAdapterSmoke(t *testing.T) {
	model := NewDurabilityModel()
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
	db := roles[mbt.RoleId{RoleName: "DurabilityDB", Index: 0}].(*DurabilityDBRoleAdapter)
	arg := func(name string, v any) mbt.Arg { return mbt.Arg{Name: name, Value: v} }

	// 1. Complete a full 4-phase commit for k1=v1 (txid=1 -> meta slot 1).
	if res, err := db.ActionBeginStage([]mbt.Arg{arg("k", "k1"), arg("v", "v1")}); err != nil || res != "OK" {
		t.Fatalf("BeginStage 1 = (%v, %v)", res, err)
	}
	if _, err := db.ActionWriteDataPages(nil); err != nil {
		t.Fatalf("WriteDataPages 1: %v", err)
	}
	if _, err := db.ActionSyncDataPages(nil); err != nil {
		t.Fatalf("SyncDataPages 1: %v", err)
	}
	if _, err := db.ActionWriteMetaPage(nil); err != nil {
		t.Fatalf("WriteMetaPage 1: %v", err)
	}
	if txid, err := db.ActionSyncMetaPage(nil); err != nil || txid != int64(1) {
		t.Fatalf("SyncMetaPage 1 = (%v, %v), want 1", txid, err)
	}

	// 2. Start txid=2 (targets meta slot 0), reach WriteMetaPage, then inject
	// TWO back-to-back torn meta crashes: slot 0 becomes torn, recovery falls
	// back to slot 1 (txid=1), and the second attempt targets slot 0 again!
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := db.ActionBeginStage([]mbt.Arg{arg("k", "k2"), arg("v", "v2")}); err != nil {
			t.Fatalf("torn attempt %d BeginStage: %v", attempt, err)
		}
		if _, err := db.ActionWriteDataPages(nil); err != nil {
			t.Fatalf("torn attempt %d WriteDataPages: %v", attempt, err)
		}
		if _, err := db.ActionSyncDataPages(nil); err != nil {
			t.Fatalf("torn attempt %d SyncDataPages: %v", attempt, err)
		}
		if _, err := db.ActionWriteMetaPage(nil); err != nil {
			t.Fatalf("torn attempt %d WriteMetaPage: %v", attempt, err)
		}
		txid, err := db.ActionPowerLossTornMeta(nil)
		if err != nil || txid != int64(1) {
			t.Fatalf("torn attempt %d PowerLossTornMeta = (%v, %v), want txid=1", attempt, txid, err)
		}
		st, err := db.GetState()
		if err != nil {
			t.Fatalf("GetState after torn attempt %d: %v", attempt, err)
		}
		if st["meta0_valid"] != false || st["meta1_valid"] != true {
			t.Fatalf("expected meta0 torn & meta1 valid, got meta0=%v meta1=%v", st["meta0_valid"], st["meta1_valid"])
		}
	}

	// 3. Start txid=2 again, reach WriteMetaPage, then trigger PowerLossReorderedFlush:
	// because SyncDataPages already ran, the reordered meta flush lands cleanly on txid=2
	// and heals meta slot 0!
	if _, err := db.ActionBeginStage([]mbt.Arg{arg("k", "k2"), arg("v", "v2")}); err != nil {
		t.Fatalf("reordered BeginStage: %v", err)
	}
	if _, err := db.ActionWriteDataPages(nil); err != nil {
		t.Fatalf("reordered WriteDataPages: %v", err)
	}
	if _, err := db.ActionSyncDataPages(nil); err != nil {
		t.Fatalf("reordered SyncDataPages: %v", err)
	}
	if _, err := db.ActionWriteMetaPage(nil); err != nil {
		t.Fatalf("reordered WriteMetaPage: %v", err)
	}
	txid, err := db.ActionPowerLossReorderedFlush(nil)
	if err != nil || txid != int64(2) {
		t.Fatalf("PowerLossReorderedFlush = (%v, %v), want txid=2", txid, err)
	}
	st, err := db.GetState()
	if err != nil {
		t.Fatalf("GetState after reordered flush: %v", err)
	}
	if st["meta0_valid"] != true || st["meta1_valid"] != true {
		t.Fatalf("expected both meta slots valid after healing commit, got meta0=%v meta1=%v", st["meta0_valid"], st["meta1_valid"])
	}
	if st["meta0_data_durable"] != true || st["meta1_data_durable"] != true {
		t.Fatalf("expected durable B+Tree roots on both slots, got meta0=%v meta1=%v", st["meta0_data_durable"], st["meta1_data_durable"])
	}

	// 4. Crash after WriteDataPages (before SyncDataPages) drops unsynced writes cleanly.
	if _, err := db.ActionBeginStage([]mbt.Arg{arg("k", "k1"), arg("v", "v2")}); err != nil {
		t.Fatalf("drop BeginStage: %v", err)
	}
	if _, err := db.ActionWriteDataPages(nil); err != nil {
		t.Fatalf("drop WriteDataPages: %v", err)
	}
	txid, err = db.ActionPowerLossDropUnsynced(nil)
	if err != nil || txid != int64(2) {
		t.Fatalf("PowerLossDropUnsynced = (%v, %v), want txid=2", txid, err)
	}
}
