GO ?= go
GOFMT ?= gofmt
GOLANGCI_LINT ?= golangci-lint
GORELEASER ?= goreleaser
CONTAINER_TOOL ?= docker
BINARY_DIR ?= bin
PROBE_BINARY ?= $(BINARY_DIR)/octavia-capability-probe
CONTROLLER_BINARY ?= $(BINARY_DIR)/openstack-gateway-controller
AUDIT_BINARY ?= $(BINARY_DIR)/openstack-gateway-audit
SETUP_ENVTEST ?= $(abspath $(BINARY_DIR)/setup-envtest)
SETUP_ENVTEST_VERSION ?= v0.24.1
ENVTEST_ASSETS_DIR ?= $(abspath $(BINARY_DIR)/envtest)
ENVTEST_K8S_VERSION ?= 1.36.2
ENVTEST_RELEASE_INDEX ?= https://raw.githubusercontent.com/kubernetes-sigs/controller-tools/3311c8d50e5c8a976266e08f1f92f827439bd34a/envtest-releases.yaml
E2E_ARTIFACT_DIR ?= $(if $(GATEWAY_OPENSTACK_E2E_ARTIFACT_DIR),$(GATEWAY_OPENSTACK_E2E_ARTIFACT_DIR),$(abspath _artifacts/e2e/$(GATEWAY_OPENSTACK_E2E_RUN_ID)))
E2E_CONFIG ?=
VERSION ?= dev
IMAGE ?= openstack-gateway-controller:$(VERSION)

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show available targets.
	@awk 'BEGIN {FS = ":.*## "; printf "Usage: make <target>\n\n"} /^[a-zA-Z_0-9-]+:.*## / {printf "  %-16s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: build
build: build-controller build-audit build-probe ## Build all release binaries.

.PHONY: build-controller
build-controller: ## Build the controller.
	@mkdir -p $(BINARY_DIR)
	$(GO) build -buildvcs=false -trimpath -ldflags "-X main.version=$(VERSION)" -o $(CONTROLLER_BINARY) ./cmd/openstack-gateway-controller

.PHONY: build-audit
build-audit: ## Build the OpenStack ownership audit tool.
	@mkdir -p $(BINARY_DIR)
	$(GO) build -buildvcs=false -trimpath -ldflags "-X main.version=$(VERSION)" -o $(AUDIT_BINARY) ./cmd/openstack-gateway-audit

.PHONY: build-probe
build-probe: ## Build the Phase 0 capability probe.
	@mkdir -p $(BINARY_DIR)
	$(GO) build -buildvcs=false -trimpath -o $(PROBE_BINARY) ./cmd/octavia-capability-probe

.PHONY: container-build
container-build: ## Build the controller container image.
	$(CONTAINER_TOOL) build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

.PHONY: test
test: ## Run unit tests.
	$(GO) test ./...

.PHONY: test-race
test-race: ## Run unit tests with the race detector.
	$(GO) test -race ./...

.PHONY: test-envtest
test-envtest: ## Run controller tests against a real API server and etcd.
	@test -x "$(SETUP_ENVTEST)" || { printf 'Run make envtest-assets first.\n'; exit 1; }
	@test "$$($(SETUP_ENVTEST) version)" = "setup-envtest version: $(SETUP_ENVTEST_VERSION)" || { printf 'Run make envtest-assets to install setup-envtest %s.\n' "$(SETUP_ENVTEST_VERSION)"; exit 1; }
	@set -eu; \
	assets="$$($(SETUP_ENVTEST) --installed-only --bin-dir "$(ENVTEST_ASSETS_DIR)" --print path use "$(ENVTEST_K8S_VERSION)")"; \
	gateway_api_module="$$($(GO) list -mod=readonly -m -f '{{.Dir}}' sigs.k8s.io/gateway-api)"; \
	KUBEBUILDER_ASSETS="$$assets" \
	GATEWAY_API_CRD_PATH="$$gateway_api_module/config/crd/standard" \
	$(GO) test -tags=envtest -count=1 -run '^TestControllerEnvtest$$' -timeout=5m ./internal/controller

.PHONY: test-e2e-compile
test-e2e-compile: ## Compile the opt-in Phase 2 E2E suite without running it.
	$(GO) test -tags=e2e -run '^$$' ./test/e2e

.PHONY: test-e2e-unit
test-e2e-unit: ## Run tagged E2E helper tests without contacting a cluster or cloud.
	GATEWAY_OPENSTACK_E2E=false $(GO) test -tags=e2e -count=1 ./test/e2e

.PHONY: test-e2e
test-e2e: build-audit ## Run the opt-in Phase 2 E2E suite against an explicitly selected cloud.
	@test "$(GATEWAY_OPENSTACK_E2E)" = "true" || { printf 'Set GATEWAY_OPENSTACK_E2E=true to run the Phase 2 E2E suite.\n'; exit 1; }
	GATEWAY_OPENSTACK_E2E_AUDIT_BINARY="$(abspath $(AUDIT_BINARY))" \
	GATEWAY_OPENSTACK_E2E_ARTIFACT_DIR="$(E2E_ARTIFACT_DIR)" \
	$(GO) test -tags=e2e -count=1 -run '^TestPhase2E2E$$' -timeout=90m ./test/e2e

.PHONY: test-e2e-shared
test-e2e-shared: build-audit ## Install a run-scoped controller and run live E2E in a shared OpenStack project.
	@test -n "$(E2E_CONFIG)" || { printf 'Set E2E_CONFIG to the shared-project E2E YAML path.\n'; exit 1; }
	$(GO) run ./test/e2e/runner \
		--config "$(abspath $(E2E_CONFIG))" \
		--repository-root "$(CURDIR)" \
		--audit-binary "$(abspath $(AUDIT_BINARY))"

.PHONY: envtest-assets
envtest-assets: ## Download the pinned envtest control-plane binaries.
	@mkdir -p "$(BINARY_DIR)" "$(ENVTEST_ASSETS_DIR)"
	GOBIN="$(abspath $(BINARY_DIR))" $(GO) install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION)
	$(SETUP_ENVTEST) --index "$(ENVTEST_RELEASE_INDEX)" --bin-dir "$(ENVTEST_ASSETS_DIR)" --print path use "$(ENVTEST_K8S_VERSION)"

.PHONY: fmt
fmt: ## Format Go source files.
	$(GOFMT) -w $$(find . -name '*.go' -type f)

.PHONY: fmt-check
fmt-check: ## Fail when Go source files need formatting.
	@files="$$($(GOFMT) -l $$(find . -name '*.go' -type f))"; \
	status=$$?; \
	if [ $$status -ne 0 ]; then exit $$status; fi; \
	if [ -n "$$files" ]; then \
		printf 'Run make fmt for:\n%s\n' "$$files"; \
		exit 1; \
	fi

.PHONY: vet
vet: ## Run go vet.
	$(GO) vet ./...

.PHONY: lint
lint: ## Run golangci-lint when installed.
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: ## Apply safe golangci-lint fixes when installed.
	$(GOLANGCI_LINT) run --fix

.PHONY: verify
verify: fmt-check vet test test-e2e-unit ## Run the checks required by CI.

.PHONY: tidy
tidy: ## Update module metadata.
	$(GO) mod tidy

.PHONY: release-check
release-check: ## Validate GoReleaser configuration and prerequisites.
	$(GORELEASER) check
	$(GORELEASER) healthcheck

.PHONY: release-snapshot
release-snapshot: ## Build release artifacts locally without publishing.
	$(GORELEASER) release --snapshot --clean

.PHONY: clean
clean: ## Remove local build and release output.
	@rm -rf "$(BINARY_DIR)" "_artifacts" "dist"
