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
test-e2e: fmt vet temporal-up mhvtl-up zpool-up ## Run the end-to-end suite: control worker in a kind cluster (Helm chart + image), data worker as its OCI container on the host, against mhvtl + ZFS pool + Temporal (brings host resources up/down; the suite manages the cluster + containers).
	@{ \
	  gocache=$$(mktemp -d); \
	  cleanup() { rc=$$?; sudo rm -rf "$$gocache"; \
	    kind delete cluster --name tape-archiver-e2e >/dev/null 2>&1 || true; \
	    docker rm -f tape-archiver-e2e-data >/dev/null 2>&1 || true; \
	    $(MAKE) zpool-down; $(MAKE) mhvtl-down; $(MAKE) temporal-down; exit $$rc; }; \
	  trap cleanup EXIT; \
	  : "-p 1 serializes the suite: it drives one physical mhvtl library and one"; \
	  : "shared kind cluster + Temporal, so package binaries must not run"; \
	  : "concurrently. Runs under sudo so the test process can drive the tape"; \
	  : "devices and corrupt the root-owned staged slices (AC4)."; \
	  sudo env \
	    MHVTL_CHANGER_DEV=/dev/sch0 MHVTL_DRIVE0_DEV=/dev/nst0 MHVTL_DRIVE1_DEV=/dev/nst1 \
	    TAPE_POOL_MOUNT=/mnt/tape-test-pool/archive TAPE_POOL_DATASET=tape_test/archive \
	    TAPE_TEST_SNAPSHOT=test-snap TAPE_TEST_MIN_BYTES=7969177 \
	    TEMPORAL_ADDRESS=localhost:7233 TEMPORAL_NAMESPACE=default \
	    PATH="$$PATH" HOME="$$HOME" GOCACHE="$$gocache" GOMODCACHE="$$(go env GOMODCACHE)" \
	    go test -count=1 -p 1 -timeout 30m -tags e2e ./e2e/...; \
	}

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

.PHONY: generate-versions
generate-versions: ## Regenerate committed external-tool versions for the report (needs the pinned tools on PATH).
	go generate ./internal/buildinfo

##@ Dependencies

.PHONY: update-dependencies
update-dependencies: ## TODO: Update Go deps, go mod tidy, refresh Nix vendor hashes (owned by later issue).
	@echo "update-dependencies: not yet implemented" >&2; exit 1

##@ Recovery Disc

.PHONY: recovery-binaries
recovery-binaries: ## Build the static recovery-binary set (age, par2, zstd, tar) + source into result/.
	nix build .#recoveryBinaries

##@ Container Images

# Registry, version, and push toggle for the OCI worker images, following the
# media-processor pattern. VERSION defaults to a git-derived tag; override for a
# release (`make build-images VERSION=v1.2.3`). PUSH_ALL=true additionally tags
# `:latest` and pushes every tag to the registry; the default (false) only builds
# the images and loads them into the local Docker daemon — no publish. Registry
# auth is assumed to be present in the Docker daemon (no `docker login` step
# here), matching media-processor.
CONTAINER_REGISTRY ?= ghcr.io/soliddowant
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PUSH_ALL ?= false

DATA_WORKER_IMAGE := $(CONTAINER_REGISTRY)/tape-archiver/data-worker
DATA_WORKER_IMAGE_TAGS := $(DATA_WORKER_IMAGE):$(VERSION) $(if $(filter true,$(PUSH_ALL)),$(DATA_WORKER_IMAGE):latest)

CONTROL_WORKER_IMAGE := $(CONTAINER_REGISTRY)/tape-archiver/control-worker
CONTROL_WORKER_IMAGE_TAGS := $(CONTROL_WORKER_IMAGE):$(VERSION) $(if $(filter true,$(PUSH_ALL)),$(CONTROL_WORKER_IMAGE):latest)

# Build a streamed OCI image, load it into the local Docker daemon, tag it, and
# (when PUSH_ALL=true) push every tag. $(1)=flake attr, $(2)=tag list.
define build-load-image
	image_ref="$$(nix eval --raw .#$(1).imageName):$$(nix eval --raw .#$(1).imageTag)"; \
	"$$(nix build --print-out-paths --no-link .#$(1))" | docker load; \
	for tag in $(2); do \
	  echo "tagging $$image_ref -> $$tag"; \
	  docker tag "$$image_ref" "$$tag"; \
	done; \
	if [ "$(PUSH_ALL)" = "true" ]; then \
	  for tag in $(2); do \
	    echo "pushing $$tag"; docker push "$$tag"; \
	  done; \
	else \
	  echo "Skipping registry push (set PUSH_ALL=true to push: $(2))"; \
	fi
endef

.PHONY: build-images
build-images: ## Build the data- and control-worker OCI images, load them into the local Docker daemon, and tag them (PUSH_ALL=true also tags :latest and pushes).
	@$(call build-load-image,dataWorkerImage,$(DATA_WORKER_IMAGE_TAGS))
	@$(call build-load-image,controlWorkerImage,$(CONTROL_WORKER_IMAGE_TAGS))

##@ Deploy

CONTROL_WORKER_CHART := deploy/charts/tape-archiver-control-worker
# A Temporal address is required to render the chart; this placeholder only
# satisfies the lint/template checks and is not baked into any release artifact.
CHART_LINT_ADDRESS ?= temporal-frontend.temporal.svc.cluster.local:7233

.PHONY: chart-lint
chart-lint: ## Fetch chart deps, lint, and render the control-worker Helm chart (no cluster needed).
	helm dependency update $(CONTROL_WORKER_CHART)
	helm lint $(CONTROL_WORKER_CHART) --set config.temporal.address=$(CHART_LINT_ADDRESS)
	helm template $(CONTROL_WORKER_CHART) --set config.temporal.address=$(CHART_LINT_ADDRESS) >/dev/null

DATA_WORKER_UNIT := deploy/data-worker/tape-archiver-data-worker.service

.PHONY: unit-lint
unit-lint: ## Verify the reference data-worker systemd unit (no running service needed).
	systemd-analyze verify $(DATA_WORKER_UNIT)

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
