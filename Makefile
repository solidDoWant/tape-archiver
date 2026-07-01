SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -euc

PROJECT_DIR := $(shell dirname $(realpath $(firstword $(MAKEFILE_LIST))))
MODULE_NAME := $(shell go list -m)

BIN_DIR := $(PROJECT_DIR)/bin

GO_SOURCE_FILES := $(shell find cmd pkg \( -name '*.go' ! -name '*_test.go' \) 2>/dev/null)

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint.
	golangci-lint run ./...

.PHONY: lint-fix
lint-fix: ## Run golangci-lint and perform fixes.
	golangci-lint run --fix ./...

.PHONY: test
test: fmt vet ## Run unit tests with race detector.
	go test -race -count=1 ./...

.PHONY: test-integration
test-integration: fmt vet temporal-up mhvtl-up zpool-up ## Run integration tests against mhvtl + ephemeral ZFS pool + local Temporal dev stack (brings all up, tears all down after).
	@{ \
	  gocache=$$(mktemp -d); \
	  cleanup() { rc=$$?; sudo rm -rf "$$gocache"; $(MAKE) zpool-down; $(MAKE) mhvtl-down; $(MAKE) temporal-down; exit $$rc; }; \
	  trap cleanup EXIT; \
	  : "-p 1 serializes package test binaries: they share one physical mhvtl"; \
	  : "library (2 drives, 47 tapes), so running them concurrently races on"; \
	  : "the drive and contaminates shared tapes. Do not remove without"; \
	  : "giving each package its own drive/tape."; \
	  sudo env \
	    MHVTL_CHANGER_DEV=/dev/sch0 MHVTL_DRIVE0_DEV=/dev/nst0 MHVTL_DRIVE1_DEV=/dev/nst1 \
	    TAPE_POOL_MOUNT=/mnt/tape-test-pool/archive TAPE_POOL_DATASET=tape_test/archive \
	    TAPE_TEST_SNAPSHOT=test-snap TAPE_TEST_MIN_BYTES=7969177 \
	    TEMPORAL_ADDRESS=localhost:7233 TEMPORAL_NAMESPACE=default \
	    PATH="$$PATH" GOCACHE="$$gocache" GOMODCACHE="$$(go env GOMODCACHE)" \
	    go test -race -count=1 -p 1 -tags integration ./...; \
	}

.PHONY: test-e2e
test-e2e: ## TODO: Run end-to-end tests (owned by later issue).
	@echo "test-e2e: not yet implemented" >&2; exit 1

.PHONY: benchmark
benchmark: ## TODO: Run write-rate / shoe-shining benchmarks (real hardware; owned by later issue).
	@echo "benchmark: not yet implemented" >&2; exit 1

##@ Code Generation

.PHONY: generate-schema
generate-schema: ## Regenerate committed config JSON schema.
	go run ./cmd/gen-config-schema schemas/run-config.schema.json

.PHONY: check-schema
check-schema: ## Verify committed schema matches generated output (CI check fails on diff).
	go run ./cmd/gen-config-schema | diff schemas/run-config.schema.json -

##@ Dependencies

.PHONY: update-dependencies
update-dependencies: ## TODO: Update Go deps, go mod tidy, refresh Nix vendor hashes (owned by later issue).
	@echo "update-dependencies: not yet implemented" >&2; exit 1

##@ Container Images

.PHONY: build-images
build-images: ## TODO: Build worker OCI image(s) via Nix (owned by later issue).
	@echo "build-images: not yet implemented" >&2; exit 1

##@ Build

$(BIN_DIR)/worker: $(GO_SOURCE_FILES)
	@mkdir -p "$(BIN_DIR)"
	go build -ldflags="-s -w" -o "$@" ./cmd/worker

$(BIN_DIR)/gen-config-schema: $(GO_SOURCE_FILES)
	@mkdir -p "$(BIN_DIR)"
	go build -ldflags="-s -w" -o "$@" ./cmd/gen-config-schema

$(BIN_DIR)/tapectl: $(GO_SOURCE_FILES)
	@mkdir -p "$(BIN_DIR)"
	go build -ldflags="-s -w" -o "$@" ./cmd/tapectl

.PHONY: build
build: $(BIN_DIR)/worker $(BIN_DIR)/gen-config-schema $(BIN_DIR)/tapectl ## Build all binaries into bin/.

.PHONY: clean
clean: ## Remove build artifacts.
	@rm -rf $(BIN_DIR)

##@ Local Dev

.PHONY: temporal-up
temporal-up: ## Start the local Temporal dev stack (server + Web UI).
	docker compose up -d --wait
	@echo "Temporal is ready.  gRPC: localhost:7233  Web UI: http://localhost:8233  Namespace: default"

.PHONY: temporal-down
temporal-down: ## Stop the local Temporal dev stack and remove volumes/networks.
	docker compose down -v --remove-orphans

.PHONY: mhvtl-up
mhvtl-up: ## Start the mhvtl virtual tape library (2 drives, 47 storage + 3 I/O slots).
	@$(PROJECT_DIR)/scripts/mhvtl-up.sh

.PHONY: mhvtl-down
mhvtl-down: ## Stop the mhvtl virtual tape library and unload the kernel module.
	@$(PROJECT_DIR)/scripts/mhvtl-down.sh

.PHONY: zpool-up
zpool-up: ## Create an ephemeral file-backed ZFS pool for integration tests.
	@$(PROJECT_DIR)/scripts/zpool-up.sh

.PHONY: zpool-down
zpool-down: ## Destroy the ephemeral ZFS test pool.
	@$(PROJECT_DIR)/scripts/zpool-down.sh
