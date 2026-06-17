.PHONY: build run test test-shuffle test-cover patch-cover gate scan vulncheck \
        doctor sync-tool-versions coverage-floor smoke lint fmt hooks clean help \
        docs docs-serve docs-deps templ tailwind ui ui-check

# Binary name
BINARY=mxlrcgo-svc

# Tailwind standalone CLI (v4). Override with `make ui TAILWIND=/path/to/tailwindcss`.
# Install the single, node-free binary from
# https://github.com/tailwindlabs/tailwindcss/releases (or `brew install tailwindcss`).
TAILWIND ?= tailwindcss

# Pinned govulncheck version for reproducible vulnerability scans. Manual pin:
# scripts/check-tool-versions.sh asserts the golangci-lint and grype version pins.
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

## docs-deps: Install the Python documentation tooling (ProperDocs + Material)
docs-deps:
	pip install --require-hashes -r dev-requirements.lock

## docs-serve: Live-reload preview of the documentation site
docs-serve:
	properdocs serve

## docs: Build the documentation site strictly into ./site
docs:
	properdocs build --strict

## templ: Generate Go from web/templates/*.templ (pinned via go.mod tool directive)
templ:
	go tool templ generate

## tailwind: Compile the web UI CSS (Tailwind v4 standalone CLI, no node runtime)
tailwind:
	$(TAILWIND) -i web/static/css/input.css -o web/static/css/output.css --minify

## ui: Regenerate all web UI assets (templ + Tailwind)
ui: templ tailwind

## ui-check: Fail if committed web UI assets are stale or untracked vs their sources (CI gate)
ui-check: ui
	@{ git diff --exit-code -- web/ && test -z "$$(git ls-files --others --exclude-standard -- web/)"; } || { \
		echo "Generated web UI assets are stale or untracked. Run 'make ui' and commit the result."; \
		exit 1; \
	}
	@# Guard the load-bearing shell-layout CSS rules (.mx-shell/.mx-sidebar/.mx-content,
	@# authored directly in input.css as custom CSS) against an accidental committed
	@# deletion. These classes are always emitted independent of @source globs; this
	@# check is NOT an @source-glob coverage gate.
	@for cls in mx-shell mx-sidebar mx-content; do \
		if ! grep -q "\.$$cls{" web/static/css/output.css; then \
			echo "ui-check: sentinel class .$$cls missing from output.css"; \
			echo "  A @source glob may be missing from web/static/css/input.css for a new template location."; \
			exit 1; \
		fi; \
	done
	@# Size band: output.css should stay within 7000-15000 bytes.
	@# Too small = a @source glob is too narrow (classes dropped silently).
	@# Too large = Go vocabulary may be leaking back into output.css.
	@# If an intentional change moves the size outside this band, update both bounds here.
	@size=$$(wc -c < web/static/css/output.css | awk '{print $$1}'); \
	if [ "$$size" -lt 7000 ] || [ "$$size" -gt 15000 ]; then \
		echo "ui-check: output.css is $$size B, outside the expected 7000-15000 B band."; \
		echo "  Update the band in the Makefile ui-check target if the change is intentional."; \
		exit 1; \
	fi

## clean: Remove build artifacts
clean:
	rm -f $(BINARY)
	rm -f coverage.out coverage.html

## help: Show this help message
help:
	@echo "Available targets:"
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
