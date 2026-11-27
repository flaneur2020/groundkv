# GroundKV — Multi-Layer Formal Verification, MBT, DST & Fuzzing Workflow

FIZZ ?= $(HOME)/bin/fizzbee-v0.5.4-linux_x86/fizz
FIZZBEE_MBT_SERVER ?= fizzbee-mbt-server
FIZZBEE_MBT_BIN ?= fizzbee-mbt-runner
MBT_COVER_DIR ?= coverage

CONTRACT_SPEC := formal/fizzbee/contract/kv_contract.fizz
PROTOCOL_SPEC := formal/fizzbee/storage_protocol/storage_protocol.fizz
BTREE_OPS_SPEC := formal/fizzbee/btree_ops/btree_ops.fizz

.PHONY: all help verify check-specs check-mutations check-code-mutations trace-validate prove dafny dafny-build scaffold-interfaces test race dst fuzz smoke-mbt mbt mbt-contract mbt-storage-protocol mbt-btree-ops mbt-coverage combined-coverage build clean

all: verify

## help: Show available Makefile targets and descriptions
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed -e 's/^## //' | awk -F': ' '{printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

## verify: Run all verification layers (Dafny, Lean 4, FizzBee, mutations, TLA+ trace validation, race tests, DST swarm, MBT smoke)
verify: dafny prove check-specs check-mutations check-code-mutations trace-validate race dst smoke-mbt

## check-specs: Exhaustively model-check all 3 FizzBee specs (safety + liveness) and generate state graphs
check-specs:
	@echo "==> Model checking $(CONTRACT_SPEC)"
	$(FIZZ) --output-dir formal/fizzbee/contract/out/latest $(CONTRACT_SPEC)
	@echo "==> Model checking $(PROTOCOL_SPEC)"
	$(FIZZ) --output-dir formal/fizzbee/storage_protocol/out/latest $(PROTOCOL_SPEC)
	@echo "==> Model checking $(BTREE_OPS_SPEC)"
	$(FIZZ) --output-dir formal/fizzbee/btree_ops/out/latest $(BTREE_OPS_SPEC)

## check-mutations: Negative-control mutation proofs verifying formal safety & liveness invariants catch real bugs
check-mutations:
	FIZZ=$(FIZZ) ./scripts/check_mutations.sh

## check-code-mutations: Negative-control mutation proofs directly on Go engine source code
check-code-mutations:
	./scripts/check_code_mutations.sh

## trace-validate: Model-check GroundKVProtocol.tla and run TLA+ Trace Validation on real Go DST traces
trace-validate:
	./scripts/run_trace_validation.sh

## prove: Build and axiom-audit all unbounded Lean 4 formal proofs under formal/lean/
prove:
	./scripts/check_lean_proofs.sh

## dafny: Verify all Phase 1 & Phase 2 Dafny proofs, audit zero bypasses, and check verified/internal/dafnygen/ reproducibility
dafny:
	./scripts/check_dafny.sh

## dafny-build: Verify Dafny proofs and regenerate verified/internal/dafnygen/ Go packages
dafny-build:
	./scripts/build_dafny.sh

## scaffold-interfaces: Regenerate FizzBee MBT interfaces & test runners (preserves hand-written adapters)
scaffold-interfaces:
	$(FIZZ) mbt-scaffold --lang go --go-package contract_mbt --out-dir verify/mbt/contract $(CONTRACT_SPEC)
	$(FIZZ) mbt-scaffold --lang go --go-package storage_protocol_mbt --out-dir verify/mbt/storage_protocol $(PROTOCOL_SPEC)
	$(FIZZ) mbt-scaffold --lang go --go-package btree_ops_mbt --out-dir verify/mbt/btree_ops $(BTREE_OPS_SPEC)
	gofmt -w verify/mbt/contract verify/mbt/storage_protocol verify/mbt/btree_ops

## test: Run pure-Go engine and Dafny-verified Core tests
test:
	go test -v ./engine/... ./verified

## race: Run pure-Go engine, Dafny-verified Core, and concurrent linearizability tests under -race
race:
	go test -race -v ./engine/... ./verified
	go test -race -v -run TestConcurrentLinearizability ./verify/dst

## dst: Run Deterministic Simulation Testing (7 Swarm profiles, 19 scenario checks, SIGKILL crash recovery & linearizability)
dst:
	mkdir -p $(MBT_COVER_DIR)
	go test -v -coverpkg=./engine -coverprofile=$(MBT_COVER_DIR)/dst.out ./verify/dst/...

## fuzz: Run deterministic Go mutation fuzzing on binary page decoders and corrupt disk images
fuzz:
	go test -fuzz=FuzzDecodePages -fuzztime=200000x ./engine
	go test -fuzz=FuzzOpenCorruptStorage -fuzztime=15000x ./engine

## smoke-mbt: Run Go adapter smoke tests (verifies all MBT action bindings without external runner)
smoke-mbt:
	go test -race -v -run TestAdapterSmoke ./verify/mbt/contract
	go test -race -v -run TestAdapterSmoke ./verify/mbt/storage_protocol
	go test -race -v -run TestAdapterSmoke ./verify/mbt/btree_ops

## mbt: Run full FizzBee Model-Based Testing (-race + engine coverage) across all 3 suites
mbt: check-specs mbt-contract mbt-storage-protocol mbt-btree-ops mbt-coverage

mbt-contract:
	GO_TEST_FLAGS="-race" MBT_COVER_DIR=$(MBT_COVER_DIR) FIZZBEE_MBT_SERVER=$(FIZZBEE_MBT_SERVER) FIZZBEE_MBT_BIN=$(FIZZBEE_MBT_BIN) \
		./scripts/run_mbt.sh contract ./verify/mbt/contract TestKvContract formal/fizzbee/contract/out/latest/

mbt-storage-protocol:
	GO_TEST_FLAGS="-race" MBT_COVER_DIR=$(MBT_COVER_DIR) FIZZBEE_MBT_SERVER=$(FIZZBEE_MBT_SERVER) FIZZBEE_MBT_BIN=$(FIZZBEE_MBT_BIN) \
		./scripts/run_mbt.sh storage_protocol ./verify/mbt/storage_protocol TestStorageProtocol formal/fizzbee/storage_protocol/out/latest/

mbt-btree-ops:
	GO_TEST_FLAGS="-race" MBT_COVER_DIR=$(MBT_COVER_DIR) FIZZBEE_MBT_SERVER=$(FIZZBEE_MBT_SERVER) FIZZBEE_MBT_BIN=$(FIZZBEE_MBT_BIN) \
		./scripts/run_mbt.sh btree_ops ./verify/mbt/btree_ops TestBtreeOps formal/fizzbee/btree_ops/out/latest/

## mbt-coverage: Merge MBT-only coverage profiles and report ./engine statement coverage
mbt-coverage:
	python3 ./scripts/merge_coverage.py $(MBT_COVER_DIR)/mbt_merged.out \
		$(MBT_COVER_DIR)/mbt_contract.out \
		$(MBT_COVER_DIR)/mbt_storage_protocol.out \
		$(MBT_COVER_DIR)/mbt_btree_ops.out
	go tool cover -func=$(MBT_COVER_DIR)/mbt_merged.out

## combined-coverage: Merge MBT + DST + fuzz-seed profiles and report combined ./engine statement coverage
combined-coverage:
	mkdir -p $(MBT_COVER_DIR)
	go test -coverpkg=./engine -coverprofile=$(MBT_COVER_DIR)/fuzz_seeds.out -run '^Fuzz' ./engine
	python3 ./scripts/merge_coverage.py $(MBT_COVER_DIR)/combined.out \
		$(MBT_COVER_DIR)/mbt_contract.out \
		$(MBT_COVER_DIR)/mbt_storage_protocol.out \
		$(MBT_COVER_DIR)/mbt_btree_ops.out \
		$(MBT_COVER_DIR)/dst.out \
		$(MBT_COVER_DIR)/fuzz_seeds.out
	go tool cover -func=$(MBT_COVER_DIR)/combined.out

## build: Build the groundkv and groundkv-dst CLI binaries
build:
	go build -o bin/groundkv ./cmd/groundkv
	go build -o bin/groundkv-dst ./cmd/groundkv-dst

## clean: Remove generated model-checker artifacts, coverage files, and build outputs
clean:
	rm -rf bin/ coverage/ formal/fizzbee/contract/out formal/fizzbee/contract/*.json formal/fizzbee/storage_protocol/out formal/fizzbee/storage_protocol/*.json formal/fizzbee/btree_ops/out formal/fizzbee/btree_ops/*.json formal/tla/*_TTrace_* formal/tla/states formal/lean/.lake formal/lean/lake-manifest.json
