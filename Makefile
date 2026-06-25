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
test-integration: ## TODO: Run integration tests against mhvtl and dev Temporal (owned by later issue).
	@echo "test-integration: not yet implemented" >&2; exit 1

.PHONY: test-e2e
test-e2e: ## TODO: Run end-to-end tests (owned by later issue).
	@echo "test-e2e: not yet implemented" >&2; exit 1

.PHONY: benchmark
benchmark: ## TODO: Run write-rate / shoe-shining benchmarks (real hardware; owned by later issue).
	@echo "benchmark: not yet implemented" >&2; exit 1

##@ Code Generation

.PHONY: generate-schema
generate-schema: ## TODO: Regenerate committed config JSON schema (owned by later issue).
	@echo "generate-schema: not yet implemented" >&2; exit 1

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

.PHONY: build
build: $(BIN_DIR)/worker ## Build all binaries into bin/.

.PHONY: clean
clean: ## Remove build artifacts.
	@rm -rf $(BIN_DIR)

##@ Local Dev

.PHONY: temporal-up
temporal-up: ## TODO: Start local Temporal dev stack (owned by later issue).
	@echo "temporal-up: not yet implemented" >&2; exit 1

.PHONY: temporal-down
temporal-down: ## TODO: Stop local Temporal dev stack (owned by later issue).
	@echo "temporal-down: not yet implemented" >&2; exit 1

.PHONY: mhvtl-up
mhvtl-up: ## TODO: Start virtual tape library (owned by later issue).
	@echo "mhvtl-up: not yet implemented" >&2; exit 1

.PHONY: mhvtl-down
mhvtl-down: ## TODO: Stop virtual tape library (owned by later issue).
	@echo "mhvtl-down: not yet implemented" >&2; exit 1
