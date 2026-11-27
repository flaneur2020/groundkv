package verified

import (
	"bytes"
	"fmt"
	"math/rand"
	"reflect"
	"testing"

	"github.com/flaneur2020/groundkv/verified/internal/dafnygen/GroundKVBTree"
	"github.com/flaneur2020/groundkv/verified/internal/dafnygen/GroundKVEngine"
	"github.com/flaneur2020/groundkv/verified/internal/dafnygen/GroundKVTypes"
	_dafny "github.com/flaneur2020/groundkv/verified/internal/dafnygen/dafny"
)

// TestPhase2VerifiedCOWBTreeRefinement directly exercises the Phase 2 Z3-verified
// recursive COW B+Tree (`GroundKVBTree.TreePut`, `TreeGet`, `TreeScan`, `TreeDelete`,
// `TreeValidate`) compiled to Go, checking KV-map refinement and COW snapshot immutability.
func TestPhase2VerifiedCOWBTreeRefinement(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	store := _dafny.EmptyMap
	nextID := uint64(2)
	root := uint64(0)
	pageSize := uint32(256)
	maxLeaf := _dafny.IntOfInt64(3)
	maxBranch := _dafny.IntOfInt64(3)

	type snapshot struct {
		root uint64
		kv   map[string]string
	}
	var snapshots []snapshot
	ref := make(map[string]string)

	for step := 0; step < 300; step++ {
		k := fmt.Sprintf("k%02d", rng.Intn(40))
		kSeq := _dafny.SeqOfBytes([]byte(k))

		if rng.Intn(3) != 0 {
			v := fmt.Sprintf("v-%03d-%s", step, bytes.Repeat([]byte("x"), rng.Intn(30)))
			vSeq := _dafny.SeqOfBytes([]byte(v))
			ok, newStore, newNextID, newRoot := GroundKVBTree.Companion_Default___.TreePut(
				store, nextID, root, kSeq, vSeq, pageSize, maxLeaf, maxBranch,
			)
			if !ok {
				t.Fatalf("step %d: TreePut returned ok=false", step)
			}
			store, nextID, root = newStore, newNextID, newRoot
			ref[k] = v
		} else {
			ok, newStore, newNextID, newRoot, deleted := GroundKVBTree.Companion_Default___.TreeDelete(
				store, nextID, root, kSeq, pageSize,
			)
			if !ok {
				t.Fatalf("step %d: TreeDelete returned ok=false", step)
			}
			_, existed := ref[k]
			if deleted != existed {
				t.Fatalf("step %d: TreeDelete deleted=%v, want %v", step, deleted, existed)
			}
			store, nextID, root = newStore, newNextID, newRoot
			delete(ref, k)
		}

		if !GroundKVBTree.Companion_Default___.TreeValidate(store, root, nextID) {
			t.Fatalf("step %d: TreeValidate failed on root=%d nextID=%d", step, root, nextID)
		}

		if step%50 == 49 {
			cp := make(map[string]string, len(ref))
			for rk, rv := range ref {
				cp[rk] = rv
			}
			snapshots = append(snapshots, snapshot{root: root, kv: cp})
		}

		if step%20 == 0 || step == 299 {
			for i := 0; i < 40; i++ {
				qk := fmt.Sprintf("k%02d", i)
				want, exists := ref[qk]
				if root == 0 {
					if exists {
						t.Fatalf("step %d: root==0 but key %s exists", step, qk)
					}
					continue
				}
				opt := GroundKVBTree.Companion_Default___.TreeGet(store, root, _dafny.SeqOfBytes([]byte(qk)))
				if exists {
					if !opt.Is_Some() || string(_dafny.ToByteArray(opt.Dtor_value().(_dafny.Sequence))) != want {
						t.Fatalf("step %d: TreeGet(%s) mismatch", step, qk)
					}
				} else if opt.Is_Some() {
					t.Fatalf("step %d: TreeGet(%s) unexpectedly found value", step, qk)
				}
			}

			if root != 0 {
				scanned := GroundKVBTree.Companion_Default___.TreeScan(
					store, root, _dafny.SeqOfBytes([]byte("k10")), _dafny.SeqOfBytes([]byte("k30")),
				)
				gotScan := make(map[string]string)
				for i := uint32(0); i < scanned.Cardinality(); i++ {
					e := scanned.Select(i).(GroundKVTypes.LeafEntry)
					gotScan[string(_dafny.ToByteArray(e.Dtor_key()))] = string(_dafny.ToByteArray(e.Dtor_value()))
				}
				wantScan := make(map[string]string)
				for rk, rv := range ref {
					if rk >= "k10" && rk < "k30" {
						wantScan[rk] = rv
					}
				}
				if !reflect.DeepEqual(gotScan, wantScan) {
					t.Fatalf("step %d: TreeScan [k10,k30) mismatch: got %v, want %v", step, gotScan, wantScan)
				}
			}
		}
	}

	// Verify EVERY historical snapshot is still 100% intact in the final COW PageStore!
	for idx, snap := range snapshots {
		if snap.root == 0 {
			if len(snap.kv) != 0 {
				t.Fatalf("snapshot %d had root=0 with non-empty map", idx)
			}
			continue
		}
		scanned := GroundKVBTree.Companion_Default___.TreeScan(store, snap.root, _dafny.EmptySeq, _dafny.EmptySeq)
		got := make(map[string]string)
		for i := uint32(0); i < scanned.Cardinality(); i++ {
			e := scanned.Select(i).(GroundKVTypes.LeafEntry)
			got[string(_dafny.ToByteArray(e.Dtor_key()))] = string(_dafny.ToByteArray(e.Dtor_value()))
		}
		if !reflect.DeepEqual(got, snap.kv) {
			t.Fatalf("COW immutability violated on historical snapshot %d", idx)
		}
	}
}

// TestPhase2VerifiedStorageEngineProtocol directly exercises the Phase 2
// Z3-verified 4-phase dual-meta commit & crash-recovery state machine
// (`GroundKVEngine`) compiled to Go.
func TestPhase2VerifiedStorageEngineProtocol(t *testing.T) {
	eng := GroundKVEngine.Companion_Default___

	s := eng.InitEmpty(uint32(256))
	if !eng.EngineInv(s) {
		t.Fatalf("InitEmpty violated EngineInv")
	}

	kv1 := _dafny.NewMapBuilder().Add(_dafny.SeqOfBytes([]byte("a")), _dafny.SeqOfBytes([]byte("1"))).ToMap()
	pages1 := _dafny.SetOf(uint64(2))

	s = eng.BeginWrite(s)
	s = eng.WriteDataPages(s, uint64(2), kv1, pages1, uint64(3), _dafny.EmptySet, _dafny.EmptySet)
	s = eng.SyncDataPages(s)
	s = eng.WriteMetaPage(s, uint64(0))
	s = eng.SyncMetaPage(s)
	if !eng.EngineInv(s) || s.Dtor_meta().Dtor_txid() != 1 {
		t.Fatalf("Commit 1 failed EngineInv or txid != 1")
	}

	kv2 := _dafny.NewMapBuilder().Add(_dafny.SeqOfBytes([]byte("a")), _dafny.SeqOfBytes([]byte("aborted"))).ToMap()
	pages2 := _dafny.SetOf(uint64(3))
	s = eng.BeginWrite(s)
	s = eng.WriteDataPages(s, uint64(3), kv2, pages2, uint64(4), _dafny.SetOf(uint64(2)), _dafny.EmptySet)
	s = eng.SyncDataPages(s)
	s = eng.WriteMetaPage(s, uint64(0))
	s = eng.RollbackWrite(s, false)
	if !eng.EngineInv(s) || !s.Dtor_dirtyAltMeta() {
		t.Fatalf("double-fault RollbackWrite should set dirtyAltMeta=true")
	}

	kv3 := _dafny.NewMapBuilder().Add(_dafny.SeqOfBytes([]byte("a")), _dafny.SeqOfBytes([]byte("3"))).ToMap()
	pages3 := _dafny.SetOf(uint64(4))
	s = eng.BeginWrite(s)
	s = eng.WriteDataPages(s, uint64(4), kv3, pages3, uint64(5), _dafny.SetOf(uint64(2)), _dafny.EmptySet)
	s = eng.SyncDataPages(s)
	if s.Dtor_dirtyAltMeta() {
		t.Fatalf("SyncDataPages failed to clear dirtyAltMeta")
	}

	modes := []GroundKVEngine.CrashMode{
		GroundKVEngine.Companion_CrashMode_.Create_CrashDropVolatile_(),
		GroundKVEngine.Companion_CrashMode_.Create_CrashFlushDataSubset_(_dafny.SetOf(uint64(4))),
		GroundKVEngine.Companion_CrashMode_.Create_CrashFlushAltMeta_(true),
		GroundKVEngine.Companion_CrashMode_.Create_CrashTornAltMeta_(),
	}
	for i, mode := range modes {
		tup := eng.ApplyCrash(s, mode)
		d0 := (*(tup.IndexInt(0))).(GroundKVEngine.MetaSlotState)
		d1 := (*(tup.IndexInt(1))).(GroundKVEngine.MetaSlotState)
		rec := eng.SelectRecoveredMeta(eng.SlotToOption(d0), eng.SlotToOption(d1))
		if !rec.Is_Ok() {
			t.Fatalf("mode %d: recovery failed", i)
		}
		m := rec.Dtor_value().(GroundKVTypes.Meta)
		if m.Dtor_txid() != 1 || m.Dtor_root() != 2 {
			t.Fatalf("mode %d: recovered txid=%d root=%d, want txid=1 root=2", i, m.Dtor_txid(), m.Dtor_root())
		}
	}
}
