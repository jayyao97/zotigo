# Zotigo Makefile
#
# Every target here works against the current tree. CI runs the read-only
# equivalents of `check` directly (see .github/workflows/ci.yml).

BINARY_NAME := zotigo
VERSION := $(shell sed -n 's/^const Version = "\(.*\)"/\1/p' internal/buildinfo/buildinfo.go)
COMMIT_HASH := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
GOPATH := $(shell go env GOPATH)

BUILD_FLAGS := -trimpath -ldflags "-s -w"
BUILD_DIR := build
DIST_DIR := dist

TEST_TIMEOUT := 5m
COVERAGE_FILE := coverage.out
COVERAGE_HTML := coverage.html

# Keep in step with the golangci-lint-action version in .github/workflows/ci.yml.
GOLANGCI_LINT_VERSION := v2.5.0

.PHONY: all
all: check build

.PHONY: help
help: ## Show this help message
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-24s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# Development
.PHONY: dev
dev: ## Run the TUI from source
	@go run ./cmd/zotigo

.PHONY: install-tools
install-tools: ## Install the linters used by `check`
	@go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	@go install golang.org/x/tools/cmd/goimports@latest

.PHONY: catalog-diff catalog-check
catalog-diff: ## Generate catalog up/down SQL from schema.sql (NAME=change_name)
	python3 scripts/catalog_migration.py $(NAME)

catalog-check: ## Verify catalog schema.sql matches migration history
	python3 scripts/catalog_migration.py --check

# Build
.PHONY: build
build: ## Build ./build/zotigo
	@mkdir -p $(BUILD_DIR)
	@go build $(BUILD_FLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/zotigo

.PHONY: build-all
build-all: ## Cross-build the CLI for all supported platforms into ./dist
	@mkdir -p $(DIST_DIR)
	@GOOS=linux GOARCH=amd64 go build $(BUILD_FLAGS) -o $(DIST_DIR)/$(BINARY_NAME)-linux-amd64 ./cmd/zotigo
	@GOOS=linux GOARCH=arm64 go build $(BUILD_FLAGS) -o $(DIST_DIR)/$(BINARY_NAME)-linux-arm64 ./cmd/zotigo
	@GOOS=darwin GOARCH=amd64 go build $(BUILD_FLAGS) -o $(DIST_DIR)/$(BINARY_NAME)-darwin-amd64 ./cmd/zotigo
	@GOOS=darwin GOARCH=arm64 go build $(BUILD_FLAGS) -o $(DIST_DIR)/$(BINARY_NAME)-darwin-arm64 ./cmd/zotigo
	@GOOS=windows GOARCH=amd64 go build $(BUILD_FLAGS) -o $(DIST_DIR)/$(BINARY_NAME)-windows-amd64.exe ./cmd/zotigo
	@ls -la $(DIST_DIR)/

.PHONY: install
install: build ## Install the CLI to GOPATH/bin
	@cp $(BUILD_DIR)/$(BINARY_NAME) $(GOPATH)/bin/
	@codesign -s - $(GOPATH)/bin/$(BINARY_NAME) 2>/dev/null || true

.PHONY: uninstall
uninstall: ## Remove the CLI from GOPATH/bin
	@rm -f $(GOPATH)/bin/$(BINARY_NAME)

# Tests
.PHONY: test
test: ## Run all tests with the race detector
	@go test -timeout $(TEST_TIMEOUT) -race ./...

.PHONY: test-e2e
test-e2e: ## Run provider end-to-end tests (needs zotigo.e2e.yaml, see E2E_TESTING.md)
	@go test -tags=e2e -timeout $(TEST_TIMEOUT) -v ./tests/e2e/...

.PHONY: test-coverage
test-coverage: ## Run tests and write coverage.out / coverage.html
	@go test -timeout $(TEST_TIMEOUT) -race -coverprofile=$(COVERAGE_FILE) -covermode=atomic ./...
	@go tool cover -html=$(COVERAGE_FILE) -o $(COVERAGE_HTML)
	@go tool cover -func=$(COVERAGE_FILE) | tail -1

# Code quality
.PHONY: format
format: ## Format code (gofmt + goimports)
	@go fmt ./...
	@goimports -w .

.PHONY: vet
vet: ## Run go vet, including the e2e build tag
	@go vet ./...
	@go vet -tags=e2e ./...

.PHONY: lint
lint: ## Run golangci-lint
	@golangci-lint run

.PHONY: check-testutil-imports
check-testutil-imports: ## Reject test helper imports in production sources
	@go run ./scripts/check_testutil_imports

.PHONY: check
check: check-testutil-imports format vet lint test ## Everything CI checks, plus formatting in place

# Housekeeping
.PHONY: deps-check
deps-check: ## Fail if go.mod / go.sum are not tidy
	@go mod tidy
	@git diff --exit-code go.mod go.sum

.PHONY: clean
clean: ## Remove build and coverage artifacts
	@rm -rf $(BUILD_DIR) $(DIST_DIR) $(COVERAGE_FILE) $(COVERAGE_HTML)

.PHONY: version
version: ## Show version information
	@echo "Version: $(VERSION)"
	@echo "Commit:  $(COMMIT_HASH)"
	@echo "Go:      $(shell go version)"
