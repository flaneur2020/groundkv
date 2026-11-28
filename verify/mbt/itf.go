// Package mbt implements zero-dependency Go Model-Based Testing driven by
// Quint Informal Trace Format (ITF) JSON traces generated from formal/quint/*.qnt.
//
// Every generated trace is replayed against BOTH engine.GoCore() (pure Go) and
// verified.NewCore() (Z3-verified Dafny-compiled Go) with zero adapter shadow
// state, asserting exact agreement on public results, durable dual-meta slots,
// MVCC snapshots, B+Tree structure, and freelist invariants at every step.
package mbt

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// ITFInt decodes a Quint/Apalache ITF integer encoded as {"#bigint":"123"} or a JSON number.
type ITFInt int64

func (v *ITFInt) UnmarshalJSON(data []byte) error {
	var raw struct {
		BigInt string `json:"#bigint"`
	}
	if err := json.Unmarshal(data, &raw); err == nil && raw.BigInt != "" {
		n, err := strconv.ParseInt(raw.BigInt, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid ITF #bigint %q: %w", raw.BigInt, err)
		}
		*v = ITFInt(n)
		return nil
	}
	var n int64
	if err := json.Unmarshal(data, &n); err != nil {
		return err
	}
	*v = ITFInt(n)
	return nil
}

// ITFStringMap decodes a Quint ITF map<str, str> encoded as {"#map":[["k","v"],...]}.
type ITFStringMap map[string]string

func (m *ITFStringMap) UnmarshalJSON(data []byte) error {
	var raw struct {
		Pairs [][2]string `json:"#map"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	out := make(map[string]string, len(raw.Pairs))
	for _, p := range raw.Pairs {
		out[p[0]] = p[1]
	}
	*m = out
	return nil
}

// ITFTrace is a generic Informal Trace Format document produced by `quint run --out-itf`.
type ITFTrace[S any] struct {
	States []S `json:"states"`
}

func findQuintBin(t *testing.T) string {
	t.Helper()
	if env := os.Getenv("QUINT"); env != "" {
		return env
	}
	if p, err := exec.LookPath("quint"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	cand := filepath.Join(home, ".local", "bin", "quint")
	if _, err := os.Stat(cand); err == nil {
		return cand
	}
	t.Fatalf("quint binary not found in $QUINT, PATH, or ~/.local/bin/quint")
	return ""
}

// generateITFTraces invokes `quint run` to generate nTraces ITF JSON files
// from specRelPath (relative to repository root) into a temporary directory.
func generateITFTraces[S any](t *testing.T, specRelPath string, nTraces, maxSteps int, seed string) []ITFTrace[S] {
	t.Helper()
	quintBin := findQuintBin(t)
	backend := os.Getenv("QUINT_BACKEND")
	if backend == "" {
		backend = "typescript"
	}

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	specPath := filepath.Join(repoRoot, specRelPath)

	outDir := t.TempDir()
	outPattern := filepath.Join(outDir, "trace_{seq}.itf.json")

	args := []string{
		"run",
		"--backend=" + backend,
		"--invariant=inv_all",
		fmt.Sprintf("--max-samples=%d", nTraces),
		fmt.Sprintf("--n-traces=%d", nTraces),
		fmt.Sprintf("--max-steps=%d", maxSteps),
		"--seed=" + seed,
		"--verbosity=0",
		"--out-itf=" + outPattern,
		specPath,
	}
	cmd := exec.Command(quintBin, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("quint run (%s) failed: %v\n%s", specRelPath, err, string(out))
	}

	files, err := filepath.Glob(filepath.Join(outDir, "trace_*.itf.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("expected generated ITF traces in %s, found %d (err=%v)", outDir, len(files), err)
	}

	traces := make([]ITFTrace[S], 0, len(files))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read ITF trace %s: %v", f, err)
		}
		var tr ITFTrace[S]
		if err := json.Unmarshal(raw, &tr); err != nil {
			t.Fatalf("unmarshal ITF trace %s: %v", f, err)
		}
		if len(tr.States) < 2 {
			t.Fatalf("ITF trace %s has fewer than 2 states", f)
		}
		traces = append(traces, tr)
	}
	return traces
}
