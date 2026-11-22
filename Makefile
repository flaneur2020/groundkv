# GroundKV — FizzBee Model-Based Testing (MBT) First-Class Workflow

FIZZ ?= $(HOME)/bin/fizzbee-v0.5.4-linux_x86/fizz
FIZZBEE_MBT_SERVER ?= fizzbee-mbt-server
FIZZBEE_MBT_BIN ?= fizzbee-mbt-runner

CONTRACT_SPEC := models/contract/kv_contract.fizz
PROTOCOL_SPEC := models/storage_protocol/storage_protocol.fizz
BTREE_OPS_SPEC := models/btree_ops/btree_ops.fizz

.PHONY: all verify check-specs scaffold-interfaces test race smoke-mbt mbt mbt-contract mbt-storage-protocol mbt-btree-ops build clean

all: verify

## verify: Run formal model checking, Go unit/race tests, and MBT adapter smoke tests
verify: check-specs race smoke-mbt

## check-specs: Exhaustively model-check all 3 FizzBee specs and generate state graphs
check-specs:
	@echo "==> Model checking $(CONTRACT_SPEC)"
	$(FIZZ) --output-dir models/contract/out/latest $(CONTRACT_SPEC)
	@echo "==> Model checking $(PROTOCOL_SPEC)"
	$(FIZZ) --output-dir models/storage_protocol/out/latest $(PROTOCOL_SPEC)
	@echo "==> Model checking $(BTREE_OPS_SPEC)"
	$(FIZZ) --output-dir models/btree_ops/out/latest $(BTREE_OPS_SPEC)

## scaffold-interfaces: Regenerate FizzBee MBT interfaces & test runners (preserves hand-written adapters)
scaffold-interfaces:
	$(FIZZ) mbt-scaffold --lang go --go-package contract_mbt --out-dir fizztests/contract $(CONTRACT_SPEC)
	$(FIZZ) mbt-scaffold --lang go --go-package storage_protocol_mbt --out-dir fizztests/storage_protocol $(PROTOCOL_SPEC)
	$(FIZZ) mbt-scaffold --lang go --go-package btree_ops_mbt --out-dir fizztests/btree_ops $(BTREE_OPS_SPEC)
	gofmt -w fizztests/contract fizztests/storage_protocol fizztests/btree_ops

## test: Run Go unit, crash-recovery, freelist, and COW immutability tests
test:
	go test -v ./engine/...

## race: Run Go engine tests under the race detector
race:
	go test -race -v ./engine/...

## smoke-mbt: Run Go adapter smoke tests (verifies all MBT action bindings without external runner)
smoke-mbt:
	go test -race -v -run TestAdapterSmoke ./fizztests/contract
	go test -race -v -run TestAdapterSmoke ./fizztests/storage_protocol
	go test -race -v -run TestAdapterSmoke ./fizztests/btree_ops

## mbt: Run full FizzBee Model-Based Testing against GroundKV (requires fizzbee-mbt-server & fizzbee-mbt-runner)
mbt: check-specs mbt-contract mbt-storage-protocol mbt-btree-ops

mbt-contract:
	GO_TEST_FLAGS="-race" FIZZBEE_MBT_SERVER=$(FIZZBEE_MBT_SERVER) FIZZBEE_MBT_BIN=$(FIZZBEE_MBT_BIN) \
		./scripts/run_mbt.sh contract ./fizztests/contract TestKvContract models/contract/out/latest/

mbt-storage-protocol:
	FIZZBEE_MBT_SERVER=$(FIZZBEE_MBT_SERVER) FIZZBEE_MBT_BIN=$(FIZZBEE_MBT_BIN) \
		./scripts/run_mbt.sh storage_protocol ./fizztests/storage_protocol TestStorageProtocol models/storage_protocol/out/latest/

mbt-btree-ops:
	FIZZBEE_MBT_SERVER=$(FIZZBEE_MBT_SERVER) FIZZBEE_MBT_BIN=$(FIZZBEE_MBT_BIN) \
		./scripts/run_mbt.sh btree_ops ./fizztests/btree_ops TestBtreeOps models/btree_ops/out/latest/

## build: Build the groundkv CLI binary
build:
	go build -o bin/groundkv ./cmd/groundkv

## clean: Remove generated model-checker artifacts and build outputs
clean:
	rm -rf bin/ models/contract/out models/contract/*.json models/storage_protocol/out models/storage_protocol/*.json models/btree_ops/out models/btree_ops/*.json
