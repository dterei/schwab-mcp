# Development tasks. `make check` runs what CI runs (.github/workflows/ci.yml).

# Keep in step with the lint job in ci.yml, which also pins the Go version
# this golangci-lint supports. To use a binary you installed yourself:
# make lint GOLANGCI_LINT=golangci-lint
GOLANGCI_LINT_VERSION := v2.15.0
GOLANGCI_LINT ?= go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
GOVULNCHECK ?= go run golang.org/x/vuln/cmd/govulncheck@latest

# The race detector needs a C compiler on Linux and Windows. Without one:
# make test RACE=
RACE ?= -race

.PHONY: all check build test cover lint fmt tidy vuln clean help

all: check

check: ## Everything CI checks
	go mod tidy -diff
	$(MAKE) --no-print-directory lint
	go test $(RACE) -shuffle=on ./...
	$(GOVULNCHECK) ./...

build: ## Build the commands into bin/
	go build -o bin/ ./cmd/...

test: ## Run the tests with the race detector
	go test $(RACE) -shuffle=on ./...

cover: ## Run the tests and print coverage per function
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

lint: ## Run the linters, go vet, the format check and ShellCheck
	$(GOLANGCI_LINT) run
	@if command -v shellcheck >/dev/null; then shellcheck cmd/schwabtx/schedule/*.sh; \
	else echo "shellcheck not found; skipping the shell scripts"; fi

fmt: ## Format the code (gofumpt and goimports)
	$(GOLANGCI_LINT) fmt

tidy: ## Tidy go.mod and go.sum
	go mod tidy

vuln: ## Check for known vulnerabilities in the code and the Go toolchain
	$(GOVULNCHECK) ./...

clean: ## Remove build and test output
	rm -rf bin coverage.out

help: ## List the targets
	@grep -E '^[a-z]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-8s %s\n", $$1, $$2}'
