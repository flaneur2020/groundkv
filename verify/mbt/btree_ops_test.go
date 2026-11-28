// Source: ../../formal/quint/btree_ops.qnt
// Ordered B+Tree & half-open [start, end) range-scan Quint ITF Model-Based
// Testing suite driving BOTH engine.GoCore() and verified.NewCore() with small
// node fanout caps (MaxLeafKeys=2, MaxBranchKeys=2, MaxFreelistIDs=3, freelist
// enabled) to cover 3-level branch splits, collapses, empty trees, and range scans.
package mbt

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/flaneur2020/groundkv/engine"
	"github.com/flaneur2020/groundkv/engine/verified"
)

type quintKV struct {
	K string `json:"k"`
	V string `json:"v"`
}

type btreeOpRecord struct {
	Op         string    `json:"op"`
	Key        string    `json:"key"`
	StartBound string    `json:"startBound"`
	EndBound   string    `json:"endBound"`
	Result     string    `json:"result"`
	ScanResult []quintKV `json:"scanResult"`
}

type btreeOpsState struct {
	TxID    ITFInt        `json:"txid"`
	Entries ITFStringMap  `json:"entries"`
	LastOp  btreeOpRecord `json:"lastOp"`
}

func runBTreeOp(db *engine.DB, op btreeOpRecord) error {
	switch op.Op {
	case "BulkLoad":
		return db.Update(func(tx *engine.Tx) error {
			for _, k := range []string{"a", "b", "c", "d", "e"} {
				if err := tx.Put([]byte(k), []byte(op.Result)); err != nil {
					return err
				}
			}
			return nil
		})

	case "Put":
		return db.Put([]byte(op.Key), []byte(op.Result))

	case "Delete":
		return db.Delete([]byte(op.Key))

	case "Get":
		val, err := db.Get([]byte(op.Key))
		got := notFoundSentinel
		if errors.Is(err, engine.ErrNotFound) {
			err = nil
		} else if err == nil {
			got = string(val)
		}
		if err != nil {
			return fmt.Errorf("Get(%q): %w", op.Key, err)
		}
		if got != op.Result {
			return fmt.Errorf("Get(%q) mismatch: got=%q want=%q", op.Key, got, op.Result)
		}
		return nil

	case "ScanRange":
		var start, end []byte
		if op.StartBound != "" {
			start = []byte(op.StartBound)
		}
		if op.EndBound != "" {
			end = []byte(op.EndBound)
		}
		got := []quintKV{}
		err := db.View(func(tx *engine.Tx) error {
			return tx.Scan(start, end, func(k, v []byte) bool {
				got = append(got, quintKV{K: string(k), V: string(v)})
				return true
			})
		})
		if err != nil {
			return fmt.Errorf("Scan([%q,%q)): %w", op.StartBound, op.EndBound, err)
		}
		want := op.ScanResult
		if want == nil {
			want = []quintKV{}
		}
		if !reflect.DeepEqual(got, want) {
			return fmt.Errorf("Scan([%q,%q)) mismatch:\ngot:  %v\nwant: %v", op.StartBound, op.EndBound, got, want)
		}
		return nil

	default:
		return fmt.Errorf("unhandled Quint btree op %q", op.Op)
	}
}

func assertBTreeStateAndInvariants(db *engine.DB, want btreeOpsState) error {
	if err := db.Validate(); err != nil {
		return fmt.Errorf("Validate(): %w", err)
	}
	meta := db.Meta()
	if int64(meta.TxID) != int64(want.TxID) {
		return fmt.Errorf("txid mismatch: got=%d want=%d", meta.TxID, want.TxID)
	}

	// Verify live multi-level B+Tree & freelist pages never overlap freelist.free.
	livePages := make(map[engine.PageID]bool)
	slot0, slot1, err := db.InspectMeta()
	if err != nil {
		return err
	}
	for _, slot := range []engine.MetaSlot{slot0, slot1} {
		if !slot.Valid {
			continue
		}
		pages, err := db.ReachablePages(slot.Meta.Root, slot.Meta.Freelist)
		if err != nil {
			return fmt.Errorf("ReachablePages(txid=%d): %w", slot.Meta.TxID, err)
		}
		for _, id := range pages {
			livePages[id] = true
		}
	}
	for _, freeID := range db.FreelistState().FreeIDs {
		if livePages[freeID] {
			return fmt.Errorf("freelist violation: live page %d in freelist.free", freeID)
		}
	}

	gotAll, err := db.ReadTreeAtRoot(meta.Root)
	if err != nil {
		return fmt.Errorf("ReadTreeAtRoot: %w", err)
	}
	wantAll := map[string]string(want.Entries)
	if wantAll == nil {
		wantAll = map[string]string{}
	}
	if !reflect.DeepEqual(gotAll, wantAll) {
		return fmt.Errorf("full tree entries mismatch:\ngot:  %v\nwant: %v", gotAll, wantAll)
	}
	return nil
}

func TestQuintBTreeOpsMBT(t *testing.T) {
	traces := generateITFTraces[btreeOpsState](t, "formal/quint/btree_ops.qnt", 30, 24, "0x2026")

	kernels := []struct {
		name string
		core engine.Core
	}{
		{name: "GoCore", core: engine.GoCore()},
		{name: "VerifiedDafnyCore", core: verified.NewCore()},
	}

	for _, k := range kernels {
		t.Run(k.name, func(t *testing.T) {
			opCounts := make(map[string]int)
			for traceIdx, tr := range traces {
				st := engine.NewMemStorage()
				opts := &engine.Options{
					PageSize:       512,
					MaxLeafKeys:    2,
					MaxBranchKeys:  2,
					MaxFreelistIDs: 3,
					NoFreelist:     false,
					Core:           k.core,
				}
				db, err := engine.OpenStorage(st, opts)
				if err != nil {
					t.Fatalf("trace %d OpenStorage: %v", traceIdx, err)
				}
				if err := assertBTreeStateAndInvariants(db, tr.States[0]); err != nil {
					_ = db.Close()
					t.Fatalf("trace %d state 0: %v", traceIdx, err)
				}
				for stepIdx := 1; stepIdx < len(tr.States); stepIdx++ {
					s := tr.States[stepIdx]
					opCounts[s.LastOp.Op]++
					if err := runBTreeOp(db, s.LastOp); err != nil {
						_ = db.Close()
						t.Fatalf("trace %d step %d (%s): %v", traceIdx, stepIdx, s.LastOp.Op, err)
					}
					if err := assertBTreeStateAndInvariants(db, s); err != nil {
						_ = db.Close()
						t.Fatalf("trace %d step %d (%s): %v", traceIdx, stepIdx, s.LastOp.Op, err)
					}
				}
				_ = db.Close()
			}
			t.Logf("%s replayed %d Quint ITF traces cleanly: %v", k.name, len(traces), opCounts)
		})
	}
}
