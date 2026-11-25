//go:build unix

package dst

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/flaneur2020/groundkv/engine"
)

const (
	crashWorkerEnv  = "GROUNDKV_OSCRASH_WORKER"
	crashDBPathEnv  = "GROUNDKV_OSCRASH_DBPATH"
	crashSeedEnv    = "GROUNDKV_OSCRASH_SEED"
	crashACKPrefix  = "ACK "
	crashStagePrefx = "STAGE "
)

// TestOSFileSIGKILLCrashRecovery spawns real child processes hammering GroundKV
// on OSFileStorage (real POSIX file I/O + flock + fsync), kills each child with
// SIGKILL mid-workload, and verifies:
//  1. Kernel releases flock immediately so reopen succeeds.
//  2. Validate() passes on the recovered file.
//  3. Every acknowledged commit survives (zero durability loss).
//  4. Recovered KV state equals either the last acknowledged commit or the
//     single in-flight staged commit (strict crash atomicity).
func TestOSFileSIGKILLCrashRecovery(t *testing.T) {
	if os.Getenv(crashWorkerEnv) == "1" {
		runOSCrashWorker(t)
		return
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sigkill.db")

	const cycles = 8
	expected := make(map[string]string)
	var lastAckTxID uint64

	for cycle := 1; cycle <= cycles; cycle++ {
		cmd := exec.Command(exe, "-test.run=^TestOSFileSIGKILLCrashRecovery$")
		cmd.Env = append(os.Environ(),
			crashWorkerEnv+"=1",
			crashDBPathEnv+"="+dbPath,
			fmt.Sprintf("%s=%d", crashSeedEnv, 1000+cycle),
		)

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatalf("cycle %d StdoutPipe: %v", cycle, err)
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr

		if err := cmd.Start(); err != nil {
			t.Fatalf("cycle %d Start: %v", cycle, err)
		}

		// Kill after receiving targetCommits acknowledged commits from this worker.
		targetCommits := 5 + (cycle*3)%10
		ackCount := 0
		var stagedNext map[string]string
		var stagedNextTxID uint64

		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, crashStagePrefx):
				txid, k, v, isDel, ok := parseCrashEvent(strings.TrimPrefix(line, crashStagePrefx))
				if !ok {
					_ = cmd.Process.Kill()
					t.Fatalf("cycle %d malformed STAGE line: %q", cycle, line)
				}
				stagedNext = cloneMap(expected)
				if isDel {
					delete(stagedNext, k)
				} else {
					stagedNext[k] = v
				}
				stagedNextTxID = txid

			case strings.HasPrefix(line, crashACKPrefix):
				txid, k, v, isDel, ok := parseCrashEvent(strings.TrimPrefix(line, crashACKPrefix))
				if !ok {
					_ = cmd.Process.Kill()
					t.Fatalf("cycle %d malformed ACK line: %q", cycle, line)
				}
				if isDel {
					delete(expected, k)
				} else {
					expected[k] = v
				}
				lastAckTxID = txid
				stagedNext = nil
				ackCount++
				if ackCount >= targetCommits {
					// Send instant SIGKILL right while the worker begins its next commit!
					if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
						t.Fatalf("cycle %d SIGKILL: %v", cycle, err)
					}
					goto killed
				}
			}
		}
	killed:
		// Wait for child process termination (ignore expected signal: killed error).
		_ = cmd.Wait()
		if ackCount < targetCommits {
			t.Fatalf("cycle %d worker exited early after %d/%d ACKs (stderr: %s)",
				cycle, ackCount, targetCommits, stderr.String())
		}

		// Reopen real OS file immediately after SIGKILL and verify durability & atomicity.
		opts := &engine.Options{PageSize: 512, MaxLeafKeys: 2, MaxBranchKeys: 2, MaxFreelistIDs: 3}
		db, err := engine.Open(dbPath, opts)
		if err != nil {
			t.Fatalf("cycle %d reopen after SIGKILL failed: %v", cycle, err)
		}
		if err := db.Validate(); err != nil {
			_ = db.Close()
			t.Fatalf("cycle %d Validate() after SIGKILL failed: %v", cycle, err)
		}

		meta := db.Meta()
		gotMap, err := db.ReadTreeAtRoot(meta.Root)
		if err != nil {
			_ = db.Close()
			t.Fatalf("cycle %d ReadTreeAtRoot failed: %v", cycle, err)
		}
		_ = db.Close()

		if meta.TxID < lastAckTxID {
			t.Fatalf("cycle %d DURABILITY LOSS after SIGKILL: recovered txid=%d < lastAckTxID=%d",
				cycle, meta.TxID, lastAckTxID)
		}
		switch {
		case meta.TxID == lastAckTxID && mapsEqual(gotMap, expected):
			// Cleanly recovered exact last acknowledged commit.
		case stagedNext != nil && meta.TxID == stagedNextTxID && mapsEqual(gotMap, stagedNext):
			// Cleanly recovered the atomic in-flight commit that landed right before SIGKILL.
			expected = stagedNext
			lastAckTxID = stagedNextTxID
		default:
			t.Fatalf("cycle %d ATOMICITY VIOLATION after SIGKILL: recovered txid=%d (ack=%d staged=%d), gotKeys=%d expectedKeys=%d",
				cycle, meta.TxID, lastAckTxID, stagedNextTxID, len(gotMap), len(expected))
		}
	}

	t.Logf("OSFileStorage SIGKILL crash recovery PASSED across %d cycles (final txid=%d, keys=%d)",
		cycles, lastAckTxID, len(expected))
}

func runOSCrashWorker(t *testing.T) {
	t.Helper()
	dbPath := os.Getenv(crashDBPathEnv)
	seed, err := strconv.ParseInt(os.Getenv(crashSeedEnv), 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid seed: %v\n", err)
		os.Exit(2)
	}
	opts := &engine.Options{PageSize: 512, MaxLeafKeys: 2, MaxBranchKeys: 2, MaxFreelistIDs: 3}
	db, err := engine.Open(dbPath, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Open(%s): %v\n", dbPath, err)
		os.Exit(2)
	}
	defer db.Close()

	rng := rand.New(rand.NewSource(seed))
	keys := makeASCIIKeys(12)
	out := bufio.NewWriter(os.Stdout)

	for step := 0; step < 500; step++ {
		k := keys[rng.Intn(len(keys))]
		nextTxID := db.Meta().TxID + 1
		if rng.Intn(4) == 0 {
			fmt.Fprintf(out, "%s%d DEL %s -\n", crashStagePrefx, nextTxID, hex.EncodeToString(k))
			_ = out.Flush()
			if err := db.Delete(k); err != nil {
				fmt.Fprintf(os.Stderr, "Delete: %v\n", err)
				os.Exit(2)
			}
			fmt.Fprintf(out, "%s%d DEL %s -\n", crashACKPrefix, db.Meta().TxID, hex.EncodeToString(k))
			_ = out.Flush()
		} else {
			v := fmt.Sprintf("val-%d-%d", seed, step)
			fmt.Fprintf(out, "%s%d PUT %s %s\n", crashStagePrefx, nextTxID, hex.EncodeToString(k), hex.EncodeToString([]byte(v)))
			_ = out.Flush()
			if err := db.Put(k, []byte(v)); err != nil {
				fmt.Fprintf(os.Stderr, "Put: %v\n", err)
				os.Exit(2)
			}
			fmt.Fprintf(out, "%s%d PUT %s %s\n", crashACKPrefix, db.Meta().TxID, hex.EncodeToString(k), hex.EncodeToString([]byte(v)))
			_ = out.Flush()
		}
	}
}

func parseCrashEvent(s string) (txid uint64, k string, v string, isDel bool, ok bool) {
	parts := strings.Split(strings.TrimSpace(s), " ")
	if len(parts) != 4 {
		return 0, "", "", false, false
	}
	id, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return 0, "", "", false, false
	}
	kb, err := hex.DecodeString(parts[2])
	if err != nil {
		return 0, "", "", false, false
	}
	if parts[1] == "DEL" {
		return id, string(kb), "", true, true
	}
	vb, err := hex.DecodeString(parts[3])
	if err != nil {
		return 0, "", "", false, false
	}
	return id, string(kb), string(vb), false, true
}
