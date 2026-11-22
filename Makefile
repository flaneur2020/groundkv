# GroundKV — FizzBee Model-Based Testing (MBT) First-Class Workflow

FIZZ ?= $(HOME)/bin/fizzbee-v0.5.4-linux_x86/fizz
FIZZBEE_MBT_SERVER ?= fizzbee-mbt-server
FIZZBEE_MBT_BIN ?= fizzbee-mbt-runner

KV_SPEC := models/kv/kv.fizz
BTREE_SPEC := models/btree/btree.fizz
DURABILITY_SPEC := models/durability/durability.fizz
FREELIST_SPEC := models/freelist/freelist.fizz

.PHONY: all verify check-specs scaffold-interfaces test race smoke-mbt mbt mbt-kv mbt-btree mbt-durability mbt-freelist build clean

all: verify

## verify: Run formal model checking, Go unit/race tests, and MBT adapter smoke tests
verify: check-specs race smoke-mbt

## check-specs: Exhaustively model-check all FizzBee specs and generate state graphs
check-specs:
	@echo "==> Model checking $(KV_SPEC)"
	$(FIZZ) --output-dir models/kv/out/latest $(KV_SPEC)
	@echo "==> Model checking $(BTREE_SPEC)"
	$(FIZZ) --output-dir models/btree/out/latest $(BTREE_SPEC)
	@echo "==> Model checking $(DURABILITY_SPEC)"
	$(FIZZ) --output-dir models/durability/out/latest $(DURABILITY_SPEC)
	@echo "==> Model checking $(FREELIST_SPEC)"
	$(FIZZ) --output-dir models/freelist/out/latest $(FREELIST_SPEC)

## scaffold-interfaces: Regenerate FizzBee MBT interfaces & test runners (preserves hand-written adapters)
scaffold-interfaces:
	$(FIZZ) mbt-scaffold --lang go --go-package kv_mbt --out-dir fizztests/kv $(KV_SPEC)
	$(FIZZ) mbt-scaffold --lang go --go-package btree_mbt --out-dir fizztests/btree $(BTREE_SPEC)
	$(FIZZ) mbt-scaffold --lang go --go-package durability_mbt --out-dir fizztests/durability $(DURABILITY_SPEC)
	$(FIZZ) mbt-scaffold --lang go --go-package freelist_mbt --out-dir fizztests/freelist $(FREELIST_SPEC)
	gofmt -w fizztests/kv fizztests/btree fizztests/durability fizztests/freelist

## test: Run Go unit, crash-recovery, freelist, and COW immutability tests
test:
	go test -v ./engine/...

## race: Run Go engine tests under the race detector
race:
	go test -race -v ./engine/...

## smoke-mbt: Run Go adapter smoke tests (verifies all MBT action bindings without external runner)
smoke-mbt:
	go test -v -run TestAdapterSmoke ./fizztests/kv
	go test -v -run TestAdapterSmoke ./fizztests/btree
	go test -v -run TestAdapterSmoke ./fizztests/durability
	go test -v -run TestAdapterSmoke ./fizztests/freelist

## mbt: Run full FizzBee Model-Based Testing against GroundKV (requires fizzbee-mbt-server & fizzbee-mbt-runner)
mbt: check-specs mbt-kv mbt-btree mbt-durability mbt-freelist

mbt-kv:
	FIZZBEE_MBT_SERVER=$(FIZZBEE_MBT_SERVER) FIZZBEE_MBT_BIN=$(FIZZBEE_MBT_BIN) ./scripts/run_mbt.sh kv ./fizztests/kv TestKv models/kv/out/latest/

mbt-btree:
	FIZZBEE_MBT_SERVER=$(FIZZBEE_MBT_SERVER) FIZZBEE_MBT_BIN=$(FIZZBEE_MBT_BIN) ./scripts/run_mbt.sh btree ./fizztests/btree TestBtree models/btree/out/latest/

mbt-durability:
	FIZZBEE_MBT_SERVER=$(FIZZBEE_MBT_SERVER) FIZZBEE_MBT_BIN=$(FIZZBEE_MBT_BIN) ./scripts/run_mbt.sh durability ./fizztests/durability TestDurability models/durability/out/latest/

mbt-freelist:
	FIZZBEE_MBT_SERVER=$(FIZZBEE_MBT_SERVER) FIZZBEE_MBT_BIN=$(FIZZBEE_MBT_BIN) ./scripts/run_mbt.sh freelist ./fizztests/freelist TestFreelist models/freelist/out/latest/

## build: Build the groundkv CLI binary
build:
	go build -o bin/groundkv ./cmd/groundkv

## clean: Remove generated model-checker artifacts and build outputs
clean:
	rm -rf bin/ models/kv/out models/kv/*.json models/btree/out models/btree/*.json models/durability/out models/durability/*.json models/freelist/out models/freelist/*.json
