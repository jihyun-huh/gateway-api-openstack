GO ?= go
GOFMT ?= gofmt
GOLANGCI_LINT ?= golangci-lint
GORELEASER ?= goreleaser
CONTAINER_TOOL ?= docker
BINARY_DIR ?= bin
PROBE_BINARY ?= $(BINARY_DIR)/octavia-capability-probe
CONTROLLER_BINARY ?= $(BINARY_DIR)/openstack-gateway-controller
VERSION ?= dev
IMAGE ?= openstack-gateway-controller:$(VERSION)

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show available targets.
	@awk 'BEGIN {FS = ":.*## "; printf "Usage: make <target>\n\n"} /^[a-zA-Z_0-9-]+:.*## / {printf "  %-16s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: build
build: build-controller build-probe ## Build the controller and Phase 0 capability probe.

.PHONY: build-controller
build-controller: ## Build the Phase 1 controller.
	@mkdir -p $(BINARY_DIR)
	$(GO) build -buildvcs=false -trimpath -ldflags "-X main.version=$(VERSION)" -o $(CONTROLLER_BINARY) ./cmd/openstack-gateway-controller

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
verify: fmt-check vet test ## Run the checks required by CI.

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
clean: ## Remove local build and probe output.
	@rm -rf "$(BINARY_DIR)" "_artifacts" "dist"
