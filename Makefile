# GroundKV — Multi-Layer Formal Verification (Quint + Lean 4 + Dafny), MBT, DST & Fuzzing

QUINT ?= $(shell command -v quint 2>/dev/null || echo "$(HOME)/.local/bin/quint")
QUINT_BACKEND ?= typescript
COVER_DIR ?= coverage

.PHONY: all help verify check-specs check-mutations check-code-mutations trace-validate prove dafny dafny-build test race dst fuzz mbt combined-coverage build clean

all: verify

## help: Show available Makefile targets and descriptions
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed -e 's/^## //' | awk -F': ' '{printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

## verify: Run all verification layers (Dafny, Lean 4, Quint specs/mutations/trace-validation/MBT, race tests, DST swarm, Go code mutations)
verify: dafny prove check-specs check-mutations check-code-mutations trace-validate race dst mbt

## check-specs: Typecheck, unit-test, and simulate all 3 Quint formal specs
check-specs:
	@echo "==> Typechecking formal/quint/*.qnt"
	$(QUINT) typecheck formal/quint/storage_protocol.qnt
	$(QUINT) typecheck formal/quint/btree_ops.qnt
	$(QUINT) typecheck formal/quint/trace_protocol.qnt
	@echo "==> Running Quint scenario tests"
	$(QUINT) test --backend=$(QUINT_BACKEND) --max-samples=1 formal/quint/storage_protocol.qnt
	$(QUINT) test --backend=$(QUINT_BACKEND) --max-samples=1 formal/quint/btree_ops.qnt
	@echo "==> Simulating Quint invariants (storage_protocol & btree_ops)"
	$(QUINT) run --backend=$(QUINT_BACKEND) --invariant=inv_all --max-samples=1000 --max-steps=35 --seed=0x2026 --verbosity=1 formal/quint/storage_protocol.qnt
	$(QUINT) run --backend=$(QUINT_BACKEND) --invariant=inv_all --max-samples=1000 --max-steps=25 --seed=0x2026 --verbosity=1 formal/quint/btree_ops.qnt

## check-mutations: Negative-control mutation proofs verifying Quint safety invariants catch 5 real protocol bugs
check-mutations:
	QUINT=$(QUINT) QUINT_BACKEND=$(QUINT_BACKEND) ./scripts/check_mutations.sh

## check-code-mutations: Negative-control mutation proofs directly on Go engine source code
check-code-mutations:
	./scripts/check_code_mutations.sh

## trace-validate: Simulate trace_protocol.qnt and run Quint Trace Validation on real Go DST traces
trace-validate:
	QUINT=$(QUINT) QUINT_BACKEND=$(QUINT_BACKEND) ./scripts/run_trace_validation.sh

## prove: Build and axiom-audit all unbounded Lean 4 formal proofs under formal/lean/
prove:
	./scripts/check_lean_proofs.sh

## dafny: Verify all Phase 1 & Phase 2 Dafny proofs, audit zero bypasses, and check engine/verified/internal/dafnygen/ reproducibility
dafny:
	./scripts/check_dafny.sh

## dafny-build: Verify Dafny proofs and regenerate engine/verified/internal/dafnygen/ Go packages
dafny-build:
	./scripts/build_dafny.sh

## test: Run pure-Go engine, core/gocore, and Dafny-verified Core tests
test:
	go test -v ./engine ./engine/core ./engine/gocore ./engine/verified

## race: Run pure-Go engine, core/gocore, Dafny-verified Core, and linearizability tests under -race
race:
	go test -race -v ./engine ./engine/core ./engine/gocore ./engine/verified
	go test -race -v -run TestConcurrentLinearizability ./verify/dst

## dst: Run Deterministic Simulation Testing (7 Swarm profiles, 19 scenario checks, SIGKILL crash recovery & linearizability)
dst:
	mkdir -p $(COVER_DIR)
	go test -v -coverpkg=./engine,./engine/core,./engine/gocore -coverprofile=$(COVER_DIR)/dst.out ./verify/dst/...

## mbt: Run zero-dependency Quint ITF Model-Based Testing (-race + coverage) against both GoCore and verified.NewCore
mbt:
	mkdir -p $(COVER_DIR)
	QUINT=$(QUINT) QUINT_BACKEND=$(QUINT_BACKEND) go test -race -v -count=1 \
		-coverpkg=./engine,./engine/core,./engine/gocore -coverprofile=$(COVER_DIR)/mbt.out ./verify/mbt/...

## fuzz: Run deterministic Go mutation fuzzing on binary page decoders and corrupt disk images
fuzz:
	go test -fuzz=FuzzDecodePages -fuzztime=200000x ./engine
	go test -fuzz=FuzzOpenCorruptStorage -fuzztime=15000x ./engine

## combined-coverage: Merge Quint MBT + DST + fuzz-seed profiles and report combined ./engine statement coverage
combined-coverage:
	mkdir -p $(COVER_DIR)
	go test -coverpkg=./engine,./engine/core,./engine/gocore -coverprofile=$(COVER_DIR)/fuzz_seeds.out -run '^Fuzz' ./engine
	python3 ./scripts/merge_coverage.py $(COVER_DIR)/combined.out \
		$(COVER_DIR)/mbt.out \
		$(COVER_DIR)/dst.out \
		$(COVER_DIR)/fuzz_seeds.out
	go tool cover -func=$(COVER_DIR)/combined.out

## build: Build the groundkv and groundkv-dst CLI binaries
build:
	go build -o bin/groundkv ./cmd/groundkv
	go build -o bin/groundkv-dst ./cmd/groundkv-dst

## clean: Remove generated build, coverage, and prover cache artifacts
clean:
	rm -rf bin/ coverage/ formal/lean/.lake formal/lean/lake-manifest.json
