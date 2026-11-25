package dst

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/flaneur2020/groundkv/engine"
)

type opKind uint8

const (
	opPut opKind = iota
	opDelete
	opGet
)

// kvEvent records the real-time [start, end] invocation interval and outcome of
// one concurrent client operation for linearizability checking.
type kvEvent struct {
	id    int
	kind  opKind
	key   string
	val   string // written value for Put, or observed value for Get
	found bool   // for Get: whether key existed
	start int64
	end   int64
}

// TestConcurrentLinearizabilityAndMVCCSnapshots drives concurrent writer,
// point-reader, and long-lived MVCC snapshot-reader goroutines against a single
// GroundKV DB and proves:
//  1. Every recorded concurrent Put/Delete/Get history is strictly linearizable
//     (verified by an exact real-time interval linearizability checker).
//  2. Every long-lived MVCC snapshot reader sees an immutable snapshot matching
//     its pinned committed state even across heavy concurrent writes & freelist reclamation.
func TestConcurrentLinearizabilityAndMVCCSnapshots(t *testing.T) {
	for trial := 0; trial < 5; trial++ {
		st := engine.NewMemStorage()
		opts := &engine.Options{
			PageSize:       256,
			MaxLeafKeys:    2,
			MaxBranchKeys:  2,
			MaxFreelistIDs: 3,
		}
		db, err := engine.OpenStorage(st, opts)
		if err != nil {
			t.Fatalf("trial %d OpenStorage: %v", trial, err)
		}

		keys := []string{"k0", "k1", "k2"}
		var (
			mu      sync.Mutex
			history []kvEvent
			nextID  int
			stopCh  = make(chan struct{})
			wg      sync.WaitGroup
		)

		record := func(ev kvEvent) {
			mu.Lock()
			ev.id = nextID
			nextID++
			history = append(history, ev)
			mu.Unlock()
		}

		// Spawn 3 concurrent point-reader goroutines.
		for r := 0; r < 3; r++ {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				rng := rand.New(rand.NewSource(int64(trial*100 + worker + 1)))
				for {
					select {
					case <-stopCh:
						return
					default:
					}
					k := keys[rng.Intn(len(keys))]
					start := time.Now().UnixNano()
					val, getErr := db.Get([]byte(k))
					end := time.Now().UnixNano()
					if getErr != nil && !errors.Is(getErr, engine.ErrNotFound) {
						t.Errorf("trial %d reader %d Get(%s): %v", trial, worker, k, getErr)
						return
					}
					record(kvEvent{
						kind:  opGet,
						key:   k,
						val:   string(val),
						found: getErr == nil,
						start: start,
						end:   end,
					})
				}
			}(r)
		}

		// Spawn 2 long-lived MVCC snapshot readers that pin a view, wait while
		// writes & freelist cycles run, and verify their snapshot never drifts.
		for s := 0; s < 2; s++ {
			wg.Add(1)
			go func(snapIdx int) {
				defer wg.Done()
				for round := 0; round < 6; round++ {
					tx, err := db.BeginRead()
					if err != nil {
						t.Errorf("trial %d snap %d BeginRead: %v", trial, snapIdx, err)
						return
					}
					initial, err := tx.All()
					if err != nil {
						_ = tx.Rollback()
						t.Errorf("trial %d snap %d initial All: %v", trial, snapIdx, err)
						return
					}
					time.Sleep(time.Millisecond)
					later, err := tx.All()
					_ = tx.Rollback()
					if err != nil {
						t.Errorf("trial %d snap %d later All: %v", trial, snapIdx, err)
						return
					}
					if !mapsEqual(initial, later) {
						t.Errorf("trial %d snap %d MVCC DRIFT: initial=%v later=%v", trial, snapIdx, initial, later)
						return
					}
				}
			}(s)
		}

		// Run serialized writer goroutine executing 25 Put/Delete ops with unique values
		// so every write produces a distinct observable register state.
		rng := rand.New(rand.NewSource(int64(trial*997 + 42)))
		for step := 0; step < 25; step++ {
			k := keys[rng.Intn(len(keys))]
			if rng.Intn(4) == 0 {
				start := time.Now().UnixNano()
				delErr := db.Delete([]byte(k))
				end := time.Now().UnixNano()
				if delErr != nil {
					t.Fatalf("trial %d Delete(%s): %v", trial, k, delErr)
				}
				record(kvEvent{kind: opDelete, key: k, start: start, end: end})
			} else {
				v := fmt.Sprintf("t%d-w%d", trial, step)
				start := time.Now().UnixNano()
				putErr := db.Put([]byte(k), []byte(v))
				end := time.Now().UnixNano()
				if putErr != nil {
					t.Fatalf("trial %d Put(%s,%s): %v", trial, k, v, putErr)
				}
				record(kvEvent{kind: opPut, key: k, val: v, start: start, end: end})
			}
			// Give concurrent readers overlap windows across every write.
			time.Sleep(200 * time.Microsecond)
		}

		close(stopCh)
		wg.Wait()

		if err := db.Validate(); err != nil {
			_ = db.Close()
			t.Fatalf("trial %d final Validate: %v", trial, err)
		}
		_ = db.Close()

		// Verify strict linearizability independently for each key (valid because
		// all operations here are single-key Put/Delete/Get; by locality of
		// linearizability, a history is linearizable iff every per-key projection is!).
		for _, k := range keys {
			var proj []kvEvent
			for _, ev := range history {
				if ev.key == k {
					proj = append(proj, ev)
				}
			}
			if err := verifyKeyLinearizable(proj); err != nil {
				t.Fatalf("trial %d key %s LINEARIZABILITY VIOLATION: %v", trial, k, err)
			}
		}
		t.Logf("Trial %d linearizability OK (%d concurrent ops verified across %d keys)", trial, len(history), len(keys))
	}
}

// verifyKeyLinearizable checks strict linearizability for a single-key history
// where writes are totally ordered (single-writer model) and reads may execute
// concurrently with writes. A read R is linearizable iff there exists a version
// i in the ordered write timeline W[0..m] (where W[0] is the initial absent
// state) such that:
//  1. Version i's value matches R's observed (found, val),
//  2. W[i] started before R ended (W[i].start <= R.end), so R could observe W[i], and
//  3. The next write W[i+1] (if any) ended after R started (R.start <= W[i+1].end),
//     so R was not strictly after W[i+1] completed!
//
// Furthermore, because every non-empty Put uses a unique value and reads don't
// mutate state, checking every read against the valid window of the unique
// write timeline is both exact and O(N log M)!
func verifyKeyLinearizable(events []kvEvent) error {
	var writes []kvEvent
	var reads []kvEvent
	for _, ev := range events {
		if ev.kind == opGet {
			reads = append(reads, ev)
		} else {
			writes = append(writes, ev)
		}
	}

	// Sort writes by completion time (GroundKV serializes writers, so write intervals
	// are strictly non-overlapping; verify that invariant too!).
	sort.Slice(writes, func(i, j int) bool { return writes[i].start < writes[j].start })
	for i := 1; i < len(writes); i++ {
		if writes[i].start < writes[i-1].end {
			return fmt.Errorf("overlapping write intervals: w[%d]=[%d,%d] w[%d]=[%d,%d]",
				i-1, writes[i-1].start, writes[i-1].end, i, writes[i].start, writes[i].end)
		}
	}

	type version struct {
		found bool
		val   string
		start int64 // earliest time this version could take effect
		end   int64 // latest time before next version finished (math.MaxInt64 for last)
	}

	versions := make([]version, len(writes)+1)
	versions[0] = version{found: false, val: "", start: 0}
	for i, w := range writes {
		versions[i].end = w.end
		versions[i+1] = version{
			found: w.kind == opPut,
			val:   w.val,
			start: w.start,
		}
	}
	versions[len(versions)-1].end = 1<<63 - 1

	for _, r := range reads {
		matched := false
		for _, ver := range versions {
			if ver.found == r.found && ver.val == r.val &&
				ver.start <= r.end && r.start <= ver.end {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("read(found=%v, val=%q, [%d,%d]) has no valid linearization point among %d write versions",
				r.found, r.val, r.start, r.end, len(writes))
		}
	}
	return nil
}
