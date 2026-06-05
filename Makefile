.PHONY: build run test test-shuffle test-cover patch-cover gate scan vulncheck \
        doctor sync-tool-versions coverage-floor smoke lint fmt hooks clean help

# Binary name
BINARY=mxlrcgo-svc

# Pinned govulncheck version for reproducible vulnerability scans. Manual pin:
# scripts/check-tool-versions.sh currently asserts only the golangci-lint pin.
GOVULNCHECK_VERSION=v1.1.4

## build: Build the binary
build:
	CGO_ENABLED=0 go build -o $(BINARY) ./cmd/mxlrcgo-svc

## run: Build and run
run: build
	./$(BINARY)

## test: Run all tests
test:
	go test -v -race -count=1 ./...

## test-shuffle: Run tests with race + randomized order to surface order-dependent tests
test-shuffle:
	go test -race -shuffle=on -count=1 ./...

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

## scan: Build the Docker image and scan it for HIGH+ CVEs with grype
scan:
	docker build -t $(BINARY):scan -f Dockerfile .
	grype $(BINARY):scan --fail-on high

## vulncheck: Run govulncheck pinned to a fixed version for reproducible scans
vulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

## coverage-floor: Enforce the one-way per-package coverage floor (scripts/coverage-floor.sh)
coverage-floor:
	bash scripts/coverage-floor.sh

## doctor: Verify local dev tooling and git-hook wiring
doctor:
	bash scripts/check-hooks.sh
	bash scripts/check-tool-versions.sh

## sync-tool-versions: Assert pinned tool versions agree across CI and pre-commit
sync-tool-versions:
	bash scripts/check-tool-versions.sh

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

## hooks: Wire git to the tracked .githooks dir (pre-commit + pre-push, every worktree)
hooks:
	git config core.hooksPath .githooks
	@echo "core.hooksPath set to .githooks (pre-commit + pre-push enforced)."
	@bash scripts/check-hooks.sh

## clean: Remove build artifacts
clean:
	rm -f $(BINARY)
	rm -f coverage.out coverage.html

## help: Show this help message
help:
	@echo "Available targets:"
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
