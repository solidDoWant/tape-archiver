SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -euc

PROJECT_DIR := $(shell dirname $(realpath $(firstword $(MAKEFILE_LIST))))
MODULE_NAME := $(shell go list -m)

BIN_DIR := $(PROJECT_DIR)/bin

GO_SOURCE_FILES := $(shell find cmd pkg \( -name '*.go' ! -name '*_test.go' \) 2>/dev/null)

# web/ (Vite + React + TypeScript) frontend, embedded into cmd/web via
# go:embed (cmd/web/assets.go). WEB_DIR is the npm project root; vite.config.ts
# writes `npm run build`'s output directly into CMD_WEB_DIST (go:embed can only
# reach the embedding file's own directory subtree, so the build output has to
# land inside cmd/web/, not WEB_DIR itself — see cmd/web/assets.go).
WEB_DIR := web
CMD_WEB_DIST := cmd/web/dist
CMD_WEB_DIST_STAMP := $(CMD_WEB_DIST)/.build-stamp
WEB_SRC_FILES := $(shell find $(WEB_DIR)/src $(WEB_DIR)/public -type f 2>/dev/null) \
	$(WEB_DIR)/package.json $(WEB_DIR)/index.html $(WEB_DIR)/vite.config.ts \
	$(WEB_DIR)/tsconfig.json $(WEB_DIR)/tsconfig.app.json $(WEB_DIR)/tsconfig.node.json \
	$(WEB_DIR)/eslint.config.js

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
# golangci-lint (like go vet/go build — see cmd/web/assets.go) compiles fine
# against the committed dist/.gitkeep placeholder, so it has no dependency on
# a real frontend build; only frontend-lint (eslint + tsc, which lint web/'s
# sources directly) needs the frontend's node_modules installed.
lint: frontend-lint ## Run golangci-lint (Go) and eslint + tsc --noEmit (frontend).
	golangci-lint run ./...

.PHONY: lint-fix
lint-fix: ## Run golangci-lint and perform fixes.
	golangci-lint run --fix ./...

.PHONY: test
# go vet/go test compile cmd/web fine against the committed dist/.gitkeep
# placeholder (see cmd/web/assets.go), so fmt/vet have no dependency on a real
# frontend build; frontend-test (vitest, which needs node_modules but not a
# built dist/) still makes this target exercise the frontend too.
test: fmt vet frontend-test ## Run unit tests with race detector (Go + frontend vitest).
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
# media-processor pattern. VERSION is a single, manually-bumped value — the one
# place a release is versioned (it drives the image tags, the packaged Helm chart
# version, and the release git tag); bump it by hand for a release. PUSH_ALL=true
# additionally tags `:latest` and pushes every tag to the registry; the default
# (false) only builds the images and loads them into the local Docker daemon — no
# publish. Registry auth is assumed to be present in the Docker daemon (no
# `docker login` step here), matching media-processor.
CONTAINER_REGISTRY ?= ghcr.io/soliddowant
VERSION ?= 0.0.1-dev
PUSH_ALL ?= false

DATA_WORKER_IMAGE := $(CONTAINER_REGISTRY)/tape-archiver/data-worker
DATA_WORKER_IMAGE_TAGS := $(DATA_WORKER_IMAGE):$(VERSION) $(if $(filter true,$(PUSH_ALL)),$(DATA_WORKER_IMAGE):latest)

CONTROL_WORKER_IMAGE := $(CONTAINER_REGISTRY)/tape-archiver/control-worker
CONTROL_WORKER_IMAGE_TAGS := $(CONTROL_WORKER_IMAGE):$(VERSION) $(if $(filter true,$(PUSH_ALL)),$(CONTROL_WORKER_IMAGE):latest)

WEB_IMAGE := $(CONTAINER_REGISTRY)/tape-archiver/web
WEB_IMAGE_TAGS := $(WEB_IMAGE):$(VERSION) $(if $(filter true,$(PUSH_ALL)),$(WEB_IMAGE):latest)

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
build-images: ## Build the data-worker, control-worker, and web OCI images, load them into the local Docker daemon, and tag them (PUSH_ALL=true also tags :latest and pushes).
	@$(call build-load-image,dataWorkerImage,$(DATA_WORKER_IMAGE_TAGS))
	@$(call build-load-image,controlWorkerImage,$(CONTROL_WORKER_IMAGE_TAGS))
	@$(call build-load-image,webImage,$(WEB_IMAGE_TAGS))

##@ Deploy

CONTROL_WORKER_CHART := deploy/charts/tape-archiver-control-worker
# A Temporal address is required to render the chart; this placeholder only
# satisfies the lint/template checks and is not baked into any release artifact.
CHART_LINT_ADDRESS ?= temporal-frontend.temporal.svc.cluster.local:7233

# Render args for the opt-in KEDA ScaledJob path (type: scaledjob + a KEDA credential).
# The dev API key value only satisfies the render check; it is not a release artifact.
CHART_LINT_SCALEDJOB_ARGS := --set resources.controllers.main.type=scaledjob \
	--set config.temporal.keda.apiKey.value=chart-lint-placeholder

WEB_CHART := deploy/charts/tape-archiver-web
# Placeholder values only used to satisfy chart-lint's render/lint checks; none of
# these are baked into any release artifact. secretKeyRef references point at
# Secret names that need not actually exist for `helm template`/`helm lint`.
WEB_CHART_LINT_ARGS := --set config.temporal.address=$(CHART_LINT_ADDRESS) \
	--set config.web.oidc.issuerUrl=https://idp.example.com \
	--set config.web.oidc.clientId=tape-archiver-web \
	--set config.web.oidc.redirectUrl=https://tape-archiver.example.com/auth/callback \
	--set config.web.oidc.clientSecret.secretKeyRef.name=chart-lint-placeholder \
	--set config.web.oidc.clientSecret.secretKeyRef.key=clientSecret \
	--set config.web.sessionKey.secretKeyRef.name=chart-lint-placeholder \
	--set config.web.sessionKey.secretKeyRef.key=sessionKey

# Render args for the web chart's optional Ingress shape.
WEB_CHART_LINT_INGRESS_ARGS := --set resources.ingress.main.enabled=true \
	--set resources.ingress.main.className=nginx \
	--set resources.ingress.main.hosts[0].host=tape-archiver.example.com \
	--set resources.ingress.main.hosts[0].paths[0].path=/

.PHONY: chart-lint
chart-lint: ## Fetch chart deps, lint, and render the control-worker chart shapes (Deployment + ScaledJob) and the web chart shapes (Deployment + Service, and Ingress when enabled); no cluster needed.
	helm dependency update $(CONTROL_WORKER_CHART)
	helm lint $(CONTROL_WORKER_CHART) --set config.temporal.address=$(CHART_LINT_ADDRESS)
	helm lint $(CONTROL_WORKER_CHART) --set config.temporal.address=$(CHART_LINT_ADDRESS) $(CHART_LINT_SCALEDJOB_ARGS)
	# Default (Deployment) shape.
	helm template $(CONTROL_WORKER_CHART) --set config.temporal.address=$(CHART_LINT_ADDRESS) >/dev/null
	# Opt-in ScaledJob shape: must render a ScaledJob + TriggerAuthentication and no Deployment.
	helm template $(CONTROL_WORKER_CHART) --set config.temporal.address=$(CHART_LINT_ADDRESS) $(CHART_LINT_SCALEDJOB_ARGS) \
		| grep -q '^kind: ScaledJob' || { echo "chart-lint: ScaledJob not rendered on the scaledjob path"; exit 1; }
	helm template $(CONTROL_WORKER_CHART) --set config.temporal.address=$(CHART_LINT_ADDRESS) $(CHART_LINT_SCALEDJOB_ARGS) \
		| grep -q '^kind: Deployment' && { echo "chart-lint: Deployment leaked onto the scaledjob path"; exit 1; } || true
	# Enabling scaledjob without a KEDA credential renders a plaintext ScaledJob (for an
	# unauthenticated Temporal): it must succeed and emit NO TriggerAuthentication.
	helm template $(CONTROL_WORKER_CHART) --set config.temporal.address=$(CHART_LINT_ADDRESS) --set resources.controllers.main.type=scaledjob >/dev/null \
		|| { echo "chart-lint: scaledjob without a KEDA credential must render (plaintext Temporal)"; exit 1; }
	helm template $(CONTROL_WORKER_CHART) --set config.temporal.address=$(CHART_LINT_ADDRESS) --set resources.controllers.main.type=scaledjob \
		| grep -q '^kind: TriggerAuthentication' && { echo "chart-lint: plaintext scaledjob must not emit a TriggerAuthentication"; exit 1; } || true
	# --- web chart ---
	helm dependency update $(WEB_CHART)
	helm lint $(WEB_CHART) $(WEB_CHART_LINT_ARGS)
	helm lint $(WEB_CHART) $(WEB_CHART_LINT_ARGS) $(WEB_CHART_LINT_INGRESS_ARGS)
	# Default shape: Deployment + Service, no Ingress (disabled by default), no
	# ScaledJob (the web UI is a Deployment-only chart, no KEDA support).
	helm template $(WEB_CHART) $(WEB_CHART_LINT_ARGS) > /tmp/tape-archiver-web-chart-lint-default.yaml
	grep -q '^kind: Deployment' /tmp/tape-archiver-web-chart-lint-default.yaml \
		|| { echo "chart-lint: Deployment not rendered by the web chart"; exit 1; }
	grep -q '^kind: Service' /tmp/tape-archiver-web-chart-lint-default.yaml \
		|| { echo "chart-lint: Service not rendered by the web chart"; exit 1; }
	grep -q '^kind: Ingress' /tmp/tape-archiver-web-chart-lint-default.yaml \
		&& { echo "chart-lint: Ingress rendered by the web chart despite being disabled by default"; exit 1; } || true
	grep -q '^kind: ScaledJob' /tmp/tape-archiver-web-chart-lint-default.yaml \
		&& { echo "chart-lint: ScaledJob rendered by the web chart — it is a Deployment-only chart"; exit 1; } || true
	@rm -f /tmp/tape-archiver-web-chart-lint-default.yaml
	# Opt-in Ingress shape: must render an Ingress alongside the Deployment + Service.
	helm template $(WEB_CHART) $(WEB_CHART_LINT_ARGS) $(WEB_CHART_LINT_INGRESS_ARGS) \
		| grep -q '^kind: Ingress' || { echo "chart-lint: Ingress not rendered by the web chart when enabled"; exit 1; }

DATA_WORKER_UNIT := deploy/data-worker/tape-archiver-data-worker.service

.PHONY: unit-lint
unit-lint: ## Verify the reference data-worker systemd unit (no running service needed).
	systemd-analyze verify $(DATA_WORKER_UNIT)

##@ Helm

# Package (and optionally publish) the control-worker and web Helm charts, following
# the media-processor pattern. Each chart's version and app version are both stamped
# from VERSION at package time, so the packaged artifacts are versioned in lockstep
# with the images. HELM_PUSH defaults to PUSH_ALL: the default (false) only writes
# the packaged .tgz files under bin/helm/ — it never publishes; HELM_PUSH=true pushes
# them to the OCI chart registry. The data worker has no chart — it deploys via a
# systemd unit + Docker container (SPEC §4.1).
HELM_REGISTRY := $(CONTAINER_REGISTRY)/charts
HELM_PUSH ?= $(PUSH_ALL)
HELM_PACKAGE := $(BIN_DIR)/helm/tape-archiver-control-worker-$(VERSION).tgz
WEB_HELM_PACKAGE := $(BIN_DIR)/helm/tape-archiver-web-$(VERSION).tgz
# Chart inputs, excluding fetched subcharts under charts/ so a dependency update does
# not spuriously re-trigger packaging.
HELM_CHART_FILES := $(shell find $(CONTROL_WORKER_CHART) -type f ! -path "*/charts/*" 2>/dev/null)
WEB_HELM_CHART_FILES := $(shell find $(WEB_CHART) -type f ! -path "*/charts/*" 2>/dev/null)

$(HELM_PACKAGE): $(HELM_CHART_FILES)
	@mkdir -p "$(@D)"
	helm package "$(CONTROL_WORKER_CHART)" --dependency-update --version "$(VERSION)" --app-version "$(VERSION)" --destination "$(@D)"
	$(if $(filter true,$(HELM_PUSH)),helm push "$(HELM_PACKAGE)" oci://$(HELM_REGISTRY),@echo "Skipping chart push (set PUSH_ALL=true to push to oci://$(HELM_REGISTRY))")

$(WEB_HELM_PACKAGE): $(WEB_HELM_CHART_FILES)
	@mkdir -p "$(@D)"
	helm package "$(WEB_CHART)" --dependency-update --version "$(VERSION)" --app-version "$(VERSION)" --destination "$(@D)"
	$(if $(filter true,$(HELM_PUSH)),helm push "$(WEB_HELM_PACKAGE)" oci://$(HELM_REGISTRY),@echo "Skipping chart push (set PUSH_ALL=true to push to oci://$(HELM_REGISTRY))")

.PHONY: helm
helm: $(HELM_PACKAGE) $(WEB_HELM_PACKAGE) ## Package the control-worker and web Helm charts into bin/helm/ (PUSH_ALL=true also pushes them to the OCI chart registry).

##@ Frontend

$(WEB_DIR)/node_modules/.package-lock.json: $(WEB_DIR)/package.json $(WEB_DIR)/package-lock.json
	cd $(WEB_DIR) && npm ci
	@touch $@

# Rebuilds only when frontend sources (or the installed deps) actually
# changed, same file-target caching pattern as the $(BIN_DIR)/* Go targets
# below. `npm run build` (tsc -b && vite build) empties CMD_WEB_DIST before
# writing, which is why the stamp file is (re)touched after the build rather
# than tracked as a build output itself. emptyOutDir also deletes the
# committed CMD_WEB_DIST/.gitkeep placeholder (see cmd/web/assets.go), so it
# is recreated afterward — otherwise every build would dirty the working
# tree with a spurious "deleted: .gitkeep".
$(CMD_WEB_DIST_STAMP): $(WEB_DIR)/node_modules/.package-lock.json $(WEB_SRC_FILES)
	cd $(WEB_DIR) && npm run build
	@touch $(CMD_WEB_DIST)/.gitkeep
	@touch $@

.PHONY: frontend-build
frontend-build: $(CMD_WEB_DIST_STAMP) ## Build the web/ frontend; output lands in cmd/web/dist (embedded by cmd/web).

.PHONY: frontend-test
frontend-test: $(WEB_DIR)/node_modules/.package-lock.json ## Run the frontend's vitest unit tests.
	cd $(WEB_DIR) && npm test

.PHONY: frontend-lint
frontend-lint: $(WEB_DIR)/node_modules/.package-lock.json ## Run eslint and tsc --noEmit over the frontend.
	cd $(WEB_DIR) && npm run lint
	cd $(WEB_DIR) && npm run typecheck

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

# cmd/web embeds CMD_WEB_DIST at compile time (go:embed), so it must exist
# (and be current) before this compiles; see the Frontend section above.
$(BIN_DIR)/web: $(CMD_WEB_DIST_STAMP) $(GO_SOURCE_FILES)
	@mkdir -p "$(BIN_DIR)"
	go build -ldflags="-s -w" -o "$@" ./cmd/web

.PHONY: build
build: $(BIN_DIR)/worker $(BIN_DIR)/gen-config-schema $(BIN_DIR)/tapectl $(BIN_DIR)/web ## Build all binaries into bin/.

.PHONY: clean
# find, not rm -rf $(CMD_WEB_DIST)/*: dist/.build-stamp is a dotfile, and a
# shell glob doesn't match those, so a glob-based rm would silently leave the
# stamp in place and cause the next build to skip a fresh frontend build.
clean: ## Remove build artifacts (binaries, packaged Helm charts, fetched chart subcharts, frontend build output).
	@rm -rf $(BIN_DIR) $(CONTROL_WORKER_CHART)/charts $(WEB_CHART)/charts
	@find $(CMD_WEB_DIST) -mindepth 1 -not -name '.gitkeep' -delete

##@ Release

.PHONY: build-all
build-all: build-images helm ## Build all release artifacts: the data-, control-, and web images, and the packaged control-worker + web Helm charts (pushes when PUSH_ALL=true).

# Cut a release, mirroring media-processor: VERSION drives the tag `v$(VERSION)`. This
# is a dry run by default — SAFETY_PREFIX is `echo` unless PUSH_ALL=true, so the tag,
# pushes, and GitHub release are only printed. PUSH_ALL=true runs them for real. The
# unprefixed `gh auth status` runs first (under `set -e`), so an unauthenticated `gh`
# aborts the whole target before any tag is created. Requires the GitHub CLI (gh).
.PHONY: release
release: TAG = v$(VERSION)
release: SAFETY_PREFIX = $(if $(filter true,$(PUSH_ALL)),,echo)
release: build-all ## Cut a GitHub release for v$(VERSION). Dry run by default; set PUSH_ALL=true to tag, push, and publish. Requires gh.
	@gh auth status
	@$(SAFETY_PREFIX) git tag -a $(TAG) -m "Release $(TAG)"
	@$(SAFETY_PREFIX) git push origin
	@$(SAFETY_PREFIX) git push origin --tags
	@$(SAFETY_PREFIX) gh release create $(TAG) --generate-notes --latest --verify-tag

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
