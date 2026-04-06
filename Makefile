# Zotigo CLI Agent Makefile

# Project information
PROJECT_NAME := zotigo
BINARY_NAME := zotigo
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_TIME := $(shell date -u '+%Y-%m-%d_%H:%M:%S')
COMMIT_HASH := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")

# Go related variables
GO_VERSION := 1.23
GOPATH := $(shell go env GOPATH)
GOOS := $(shell go env GOOS)
GOARCH := $(shell go env GOARCH)

# Build flags
LDFLAGS := -ldflags "-X main.Version=$(VERSION) -X main.BuildTime=$(BUILD_TIME) -X main.CommitHash=$(COMMIT_HASH) -s -w"
BUILD_FLAGS := -trimpath $(LDFLAGS)

# Directories
BUILD_DIR := build
DIST_DIR := dist
DOCS_DIR := docs
SCRIPTS_DIR := scripts

# Testing
TEST_TIMEOUT := 5m
COVERAGE_FILE := coverage.out
COVERAGE_HTML := coverage.html

# Linting
GOLANGCI_LINT_VERSION := v1.55.2

# Default target
.PHONY: all
all: clean test build

# Help target
.PHONY: help
help: ## Show this help message
	@echo "Zotigo CLI Agent - Available targets:"
	@echo ""
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# Development targets
.PHONY: dev
dev: ## Run in development mode with hot reload
	@echo "Starting development mode..."
	@go run ./cmd/zotigo

.PHONY: install-tools
install-tools: ## Install development tools
	@echo "Installing development tools..."
	@go install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	@go install github.com/goreleaser/goreleaser@latest
	@go install golang.org/x/tools/cmd/goimports@latest
	@go install github.com/vektra/mockery/v2@latest

# Build targets
.PHONY: build
build: ## Build the binary
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	@go build $(BUILD_FLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/zotigo

.PHONY: build-all
build-all: ## Build for all supported platforms
	@echo "Building for all platforms..."
	@mkdir -p $(DIST_DIR)
	@GOOS=linux GOARCH=amd64 go build $(BUILD_FLAGS) -o $(DIST_DIR)/$(BINARY_NAME)-linux-amd64 ./cmd/zotigo
	@GOOS=linux GOARCH=arm64 go build $(BUILD_FLAGS) -o $(DIST_DIR)/$(BINARY_NAME)-linux-arm64 ./cmd/zotigo
	@GOOS=darwin GOARCH=amd64 go build $(BUILD_FLAGS) -o $(DIST_DIR)/$(BINARY_NAME)-darwin-amd64 ./cmd/zotigo
	@GOOS=darwin GOARCH=arm64 go build $(BUILD_FLAGS) -o $(DIST_DIR)/$(BINARY_NAME)-darwin-arm64 ./cmd/zotigo
	@GOOS=windows GOARCH=amd64 go build $(BUILD_FLAGS) -o $(DIST_DIR)/$(BINARY_NAME)-windows-amd64.exe ./cmd/zotigo
	@echo "Built binaries:"
	@ls -la $(DIST_DIR)/

.PHONY: install
install: build ## Install the binary to GOPATH/bin
	@echo "Installing $(BINARY_NAME) to $(GOPATH)/bin..."
	@cp $(BUILD_DIR)/$(BINARY_NAME) $(GOPATH)/bin/
	@codesign -s - $(GOPATH)/bin/$(BINARY_NAME) 2>/dev/null || true

.PHONY: uninstall
uninstall: ## Remove the binary from GOPATH/bin
	@echo "Removing $(BINARY_NAME) from $(GOPATH)/bin..."
	@rm -f $(GOPATH)/bin/$(BINARY_NAME)

# Testing targets
.PHONY: test
test: ## Run all tests
	@echo "Running tests..."
	@go test -timeout $(TEST_TIMEOUT) -race -v ./...

.PHONY: test-unit
test-unit: ## Run unit tests only
	@echo "Running unit tests..."
	@go test -timeout $(TEST_TIMEOUT) -race -v ./test/unit/...

.PHONY: test-integration
test-integration: ## Run integration tests only
	@echo "Running integration tests..."
	@go test -timeout $(TEST_TIMEOUT) -race -v ./test/integration/...

.PHONY: test-e2e
test-e2e: ## Run end-to-end tests only
	@echo "Running e2e tests..."
	@go test -timeout $(TEST_TIMEOUT) -race -v ./tests/e2e/...

.PHONY: test-bench
test-bench: ## Run benchmark tests
	@echo "Running benchmark tests..."
	@go test -bench=. -benchmem -v ./test/benchmarks/...

.PHONY: test-coverage
test-coverage: ## Run tests with coverage
	@echo "Running tests with coverage..."
	@go test -timeout $(TEST_TIMEOUT) -race -coverprofile=$(COVERAGE_FILE) -covermode=atomic ./...
	@go tool cover -html=$(COVERAGE_FILE) -o $(COVERAGE_HTML)
	@echo "Coverage report generated: $(COVERAGE_HTML)"

.PHONY: test-coverage-report
test-coverage-report: test-coverage ## Generate and open coverage report
	@go tool cover -func=$(COVERAGE_FILE)

# Code quality targets
.PHONY: lint
lint: ## Run linters
	@echo "Running linters..."
	@golangci-lint run

.PHONY: lint-fix
lint-fix: ## Run linters with auto-fix
	@echo "Running linters with auto-fix..."
	@golangci-lint run --fix

.PHONY: format
format: ## Format code
	@echo "Formatting code..."
	@go fmt ./...
	@goimports -w .

.PHONY: vet
vet: ## Run go vet
	@echo "Running go vet..."
	@go vet ./...

.PHONY: check
check: format vet lint test ## Run all checks (format, vet, lint, test)

# Documentation targets
.PHONY: docs
docs: ## Generate documentation
	@echo "Generating documentation..."
	@mkdir -p $(DOCS_DIR)
	@go doc -all ./core > $(DOCS_DIR)/api.md
	@echo "Documentation generated in $(DOCS_DIR)/"

.PHONY: docs-serve
docs-serve: ## Serve documentation locally
	@echo "Serving documentation..."
	@go install golang.org/x/tools/cmd/godoc@latest
	@echo "Documentation available at http://localhost:6060/pkg/github.com/jayyao97/zotigo/"
	@godoc -http=:6060

# Dependency management
.PHONY: deps
deps: ## Download dependencies
	@echo "Downloading dependencies..."
	@go mod download

.PHONY: deps-update
deps-update: ## Update dependencies
	@echo "Updating dependencies..."
	@go get -u ./...
	@go mod tidy

.PHONY: deps-vendor
deps-vendor: ## Vendor dependencies
	@echo "Vendoring dependencies..."
	@go mod vendor

.PHONY: deps-check
deps-check: ## Check for unused dependencies
	@echo "Checking for unused dependencies..."
	@go mod tidy
	@git diff --exit-code go.mod go.sum

# Release targets
.PHONY: release-dry
release-dry: ## Dry run release
	@echo "Running release dry run..."
	@goreleaser release --dry-run

.PHONY: release
release: ## Create a release
	@echo "Creating release..."
	@goreleaser release

.PHONY: release-snapshot
release-snapshot: ## Create a snapshot release
	@echo "Creating snapshot release..."
	@goreleaser release --snapshot --rm-dist

# Docker targets
.PHONY: docker-build
docker-build: ## Build Docker image
	@echo "Building Docker image..."
	@docker build -t $(PROJECT_NAME):$(VERSION) .
	@docker build -t $(PROJECT_NAME):latest .

.PHONY: docker-run
docker-run: docker-build ## Run Docker container
	@echo "Running Docker container..."
	@docker run --rm -it $(PROJECT_NAME):latest

# Security targets
.PHONY: security-scan
security-scan: ## Run security scan
	@echo "Running security scan..."
	@go install github.com/securecodewarrior/gosec/v2/cmd/gosec@latest
	@gosec ./...

.PHONY: vulnerability-check
vulnerability-check: ## Check for known vulnerabilities
	@echo "Checking for vulnerabilities..."
	@go install golang.org/x/vuln/cmd/govulncheck@latest
	@govulncheck ./...

# Performance targets
.PHONY: profile-cpu
profile-cpu: build ## Profile CPU usage
	@echo "Running CPU profile..."
	@mkdir -p profiles
	@./$(BUILD_DIR)/$(BINARY_NAME) --cpuprofile=profiles/cpu.prof --config configs/development.yaml chat -m "benchmark test"

.PHONY: profile-mem
profile-mem: build ## Profile memory usage
	@echo "Running memory profile..."
	@mkdir -p profiles
	@./$(BUILD_DIR)/$(BINARY_NAME) --memprofile=profiles/mem.prof --config configs/development.yaml chat -m "benchmark test"

# Clean targets
.PHONY: clean
clean: ## Clean build artifacts
	@echo "Cleaning build artifacts..."
	@rm -rf $(BUILD_DIR)
	@rm -rf $(DIST_DIR)
	@rm -f $(COVERAGE_FILE)
	@rm -f $(COVERAGE_HTML)
	@rm -rf profiles/
	@go clean -cache -testcache -modcache

.PHONY: clean-deps
clean-deps: ## Clean dependencies cache
	@echo "Cleaning dependencies cache..."
	@go clean -modcache

# Git hooks
.PHONY: install-hooks
install-hooks: ## Install git hooks
	@echo "Installing git hooks..."
	@cp $(SCRIPTS_DIR)/pre-commit .git/hooks/
	@chmod +x .git/hooks/pre-commit

# CI targets
.PHONY: ci
ci: deps check test-coverage ## Run CI pipeline

.PHONY: ci-build
ci-build: ci build-all ## Run CI pipeline with build

# Version information
.PHONY: version
version: ## Show version information
	@echo "Project: $(PROJECT_NAME)"
	@echo "Version: $(VERSION)"
	@echo "Build Time: $(BUILD_TIME)"
	@echo "Commit Hash: $(COMMIT_HASH)"
	@echo "Go Version: $(GO_VERSION)"
	@echo "OS/Arch: $(GOOS)/$(GOARCH)"

# Environment information
.PHONY: env
env: ## Show environment information
	@echo "Environment Information:"
	@echo "GO_VERSION: $(shell go version)"
	@echo "GOPATH: $(GOPATH)"
	@echo "GOOS: $(GOOS)"
	@echo "GOARCH: $(GOARCH)"
	@echo "PWD: $(PWD)"
	@echo "PROJECT_NAME: $(PROJECT_NAME)"
	@echo "BINARY_NAME: $(BINARY_NAME)"
	@echo "VERSION: $(VERSION)"

# Config validation
.PHONY: validate-config
validate-config: build ## Validate configuration files
	@echo "Validating configuration files..."
	@./$(BUILD_DIR)/$(BINARY_NAME) config validate --config configs/default.yaml
	@./$(BUILD_DIR)/$(BINARY_NAME) config validate --config configs/development.yaml
	@./$(BUILD_DIR)/$(BINARY_NAME) config validate --config configs/production.yaml

# Database migration (for future use)
.PHONY: migrate-up
migrate-up: ## Run database migrations up
	@echo "Database migrations not implemented yet"

.PHONY: migrate-down
migrate-down: ## Run database migrations down
	@echo "Database migrations not implemented yet"

# Monitoring and observability
.PHONY: metrics
metrics: ## Show project metrics
	@echo "Project Metrics:"
	@echo "Lines of code:"
	@find . -name "*.go" -not -path "./vendor/*" -not -path "./.git/*" | xargs wc -l | tail -1
	@echo "Number of Go files:"
	@find . -name "*.go" -not -path "./vendor/*" -not -path "./.git/*" | wc -l
	@echo "Test coverage:"
	@go test -coverprofile=temp_coverage.out ./... >/dev/null 2>&1 && go tool cover -func=temp_coverage.out | tail -1 | awk '{print $$3}' && rm temp_coverage.out || echo "No coverage data"

# Development utilities
.PHONY: todo
todo: ## Show TODO comments in code
	@echo "TODO items in code:"
	@grep -r "TODO\|FIXME\|HACK" --include="*.go" . || echo "No TODO items found"

.PHONY: watch
watch: ## Watch for changes and rebuild
	@echo "Watching for changes..."
	@go install github.com/air-verse/air@latest
	@air

# Prerequisites check
.PHONY: check-prereqs
check-prereqs: ## Check if all prerequisites are installed
	@echo "Checking prerequisites..."
	@command -v go >/dev/null 2>&1 || { echo >&2 "Go is required but not installed. Aborting."; exit 1; }
	@command -v git >/dev/null 2>&1 || { echo >&2 "Git is required but not installed. Aborting."; exit 1; }
	@echo "All prerequisites are installed."
