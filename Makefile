GO ?= go
GOFMT ?= gofmt
GOLANGCI_LINT ?= golangci-lint
BINARY_DIR ?= bin
PROBE_BINARY ?= $(BINARY_DIR)/octavia-capability-probe

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show available targets.
	@awk 'BEGIN {FS = ":.*## "; printf "Usage: make <target>\n\n"} /^[a-zA-Z_0-9-]+:.*## / {printf "  %-16s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: build
build: ## Build the Phase 0 capability probe.
	@mkdir -p $(BINARY_DIR)
	$(GO) build -buildvcs=false -trimpath -o $(PROBE_BINARY) ./cmd/octavia-capability-probe

.PHONY: test
test: ## Run unit tests.
	$(GO) test ./...

.PHONY: fmt
fmt: ## Format Go source files.
	$(GOFMT) -w $$(find . -name '*.go' -type f)

.PHONY: fmt-check
fmt-check: ## Fail when Go source files need formatting.
	@files="$$($(GOFMT) -l $$(find . -name '*.go' -type f))"; \
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

.PHONY: verify
verify: fmt-check vet test ## Run the checks required by CI.

.PHONY: tidy
tidy: ## Update module metadata.
	$(GO) mod tidy

.PHONY: clean
clean: ## Remove local build and probe output.
	@rm -rf "$(BINARY_DIR)" "_artifacts"
