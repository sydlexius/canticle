.PHONY: build run test test-cover patch-cover gate smoke lint fmt hooks clean help

# Binary name
BINARY=mxlrcgo-svc

## build: Build the binary
build:
	CGO_ENABLED=0 go build -o $(BINARY) ./cmd/mxlrcgo-svc

## run: Build and run
run: build
	./$(BINARY)

## test: Run all tests
test:
	go test -v -race -count=1 ./...

## test-cover: Run tests with coverage
test-cover:
	go test -count=1 -v -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

## patch-cover: Estimate Codecov patch coverage for the current diff (optional; needs claude-kit)
patch-cover:
	@helper="$$HOME/.claude/scripts/patch-coverage.sh"; \
	if [ -x "$$helper" ]; then \
		go test -count=1 -coverprofile=coverage.out ./...; \
		COVER_OUT=coverage.out bash "$$helper"; \
	else \
		echo "patch-coverage estimator not found at $$helper"; \
		echo "skipping; Codecov enforces patch coverage in CI. Install claude-kit for the local check."; \
	fi

## gate: Run the full deterministic pre-push gate (build, test, patch coverage, lint, vuln)
gate:
	bash scripts/pre-push-gate.sh

## smoke: Run CLI smoke tests
smoke:
	./scripts/smoke.sh

## lint: Run golangci-lint
lint:
	golangci-lint run ./...

## fmt: Format all Go files
fmt:
	gofmt -w .
	@command -v goimports >/dev/null 2>&1 && goimports -w . || true

## hooks: Install git pre-commit hook
hooks:
	cp .githooks/pre-commit .git/hooks/pre-commit
	chmod +x .git/hooks/pre-commit
	@echo "Pre-commit hook installed."

## clean: Remove build artifacts
clean:
	rm -f $(BINARY)
	rm -f coverage.out coverage.html

## help: Show this help message
help:
	@echo "Available targets:"
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
