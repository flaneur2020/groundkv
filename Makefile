# GroundKV — FizzBee MBT, DST & Fuzzing First-Class Workflow

FIZZ ?= $(HOME)/bin/fizzbee-v0.5.4-linux_x86/fizz
FIZZBEE_MBT_SERVER ?= fizzbee-mbt-server
FIZZBEE_MBT_BIN ?= fizzbee-mbt-runner
MBT_COVER_DIR ?= coverage

CONTRACT_SPEC := models/contract/kv_contract.fizz
PROTOCOL_SPEC := models/storage_protocol/storage_protocol.fizz
BTREE_OPS_SPEC := models/btree_ops/btree_ops.fizz

.PHONY: all verify check-specs check-mutations scaffold-interfaces test race dst fuzz smoke-mbt mbt mbt-contract mbt-storage-protocol mbt-btree-ops mbt-coverage combined-coverage build clean

all: verify

## verify: Run formal model checking, mutation checks, Go unit/race tests, DST swarm, and MBT adapter smoke tests
verify: check-specs check-mutations race dst smoke-mbt

## check-specs: Exhaustively model-check all 3 FizzBee specs and generate state graphs
check-specs:
	@echo "==> Model checking $(CONTRACT_SPEC)"
	$(FIZZ) --output-dir models/contract/out/latest $(CONTRACT_SPEC)
	@echo "==> Model checking $(PROTOCOL_SPEC)"
	$(FIZZ) --output-dir models/storage_protocol/out/latest $(PROTOCOL_SPEC)
	@echo "==> Model checking $(BTREE_OPS_SPEC)"
	$(FIZZ) --output-dir models/btree_ops/out/latest $(BTREE_OPS_SPEC)

## check-mutations: Negative-control mutation proofs verifying formal invariants catch real bugs
check-mutations:
	FIZZ=$(FIZZ) ./scripts/check_mutations.sh

## scaffold-interfaces: Regenerate FizzBee MBT interfaces & test runners (preserves hand-written adapters)
scaffold-interfaces:
	$(FIZZ) mbt-scaffold --lang go --go-package contract_mbt --out-dir fizztests/contract $(CONTRACT_SPEC)
	$(FIZZ) mbt-scaffold --lang go --go-package storage_protocol_mbt --out-dir fizztests/storage_protocol $(PROTOCOL_SPEC)
	$(FIZZ) mbt-scaffold --lang go --go-package btree_ops_mbt --out-dir fizztests/btree_ops $(BTREE_OPS_SPEC)
	gofmt -w fizztests/contract fizztests/storage_protocol fizztests/btree_ops

## test: Run Go unit, crash-recovery, freelist, fuzz-seed, and COW immutability tests
test:
	go test -v ./engine/...

## race: Run Go engine tests under the race detector
race:
	go test -race -v ./engine/...

## dst: Run Deterministic Simulation Testing (Swarm Testing + 16 sometimes scenario coverage assertions)
dst:
	mkdir -p $(MBT_COVER_DIR)
	go test -v -coverpkg=./engine -coverprofile=$(MBT_COVER_DIR)/dst.out ./dst/...

## fuzz: Run active Go mutation fuzzing on binary page decoders and corrupt disk images
fuzz:
	go test -fuzz=FuzzDecodePages -fuzztime=10s ./engine
	go test -fuzz=FuzzOpenCorruptStorage -fuzztime=10s ./engine

## smoke-mbt: Run Go adapter smoke tests (verifies all MBT action bindings without external runner)
smoke-mbt:
	go test -race -v -run TestAdapterSmoke ./fizztests/contract
	go test -race -v -run TestAdapterSmoke ./fizztests/storage_protocol
	go test -race -v -run TestAdapterSmoke ./fizztests/btree_ops

## mbt: Run full FizzBee Model-Based Testing (-race + engine coverage) across all 3 suites
mbt: check-specs mbt-contract mbt-storage-protocol mbt-btree-ops mbt-coverage

mbt-contract:
	GO_TEST_FLAGS="-race" MBT_COVER_DIR=$(MBT_COVER_DIR) FIZZBEE_MBT_SERVER=$(FIZZBEE_MBT_SERVER) FIZZBEE_MBT_BIN=$(FIZZBEE_MBT_BIN) \
		./scripts/run_mbt.sh contract ./fizztests/contract TestKvContract models/contract/out/latest/

mbt-storage-protocol:
	GO_TEST_FLAGS="-race" MBT_COVER_DIR=$(MBT_COVER_DIR) FIZZBEE_MBT_SERVER=$(FIZZBEE_MBT_SERVER) FIZZBEE_MBT_BIN=$(FIZZBEE_MBT_BIN) \
		./scripts/run_mbt.sh storage_protocol ./fizztests/storage_protocol TestStorageProtocol models/storage_protocol/out/latest/

mbt-btree-ops:
	GO_TEST_FLAGS="-race" MBT_COVER_DIR=$(MBT_COVER_DIR) FIZZBEE_MBT_SERVER=$(FIZZBEE_MBT_SERVER) FIZZBEE_MBT_BIN=$(FIZZBEE_MBT_BIN) \
		./scripts/run_mbt.sh btree_ops ./fizztests/btree_ops TestBtreeOps models/btree_ops/out/latest/

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
	rm -rf bin/ coverage/ models/contract/out models/contract/*.json models/storage_protocol/out models/storage_protocol/*.json models/btree_ops/out models/btree_ops/*.json
