BINARY := observer
PKG    := github.com/marmutapp/superbased-observer
CMD    := ./cmd/observer

ORG_BINARY := observer-org
ORG_CMD    := ./cmd/observer-org

GO         ?= go
GOFLAGS    ?=
BUILD_DIR  := bin
COVER_OUT  := coverage.txt

WEB_DIR        := web
WEB_DIST       := $(WEB_DIR)/dist
WEB_EMBED_DIST := internal/intelligence/dashboard/webapp/dist

WEB2_DIR        := web2
WEB2_DIST       := $(WEB2_DIR)/dist
WEB2_EMBED_DIST := internal/orgserver/dashboard/webapp/dist

OPENAPI_SPEC := docs/openapi/orgserver.yaml
OAPI         := $(GO) tool oapi-codegen

.PHONY: all build test test-race test-invariant lint fmt vet tidy clean run cover \
        gen-openapi verify-openapi build-orgserver \
        web-install web-dev web-build web-clean \
        sync-distribution-readmes verify-distribution-readmes

all: fmt vet lint test build

build: build-observer build-antigravity-bridge build-orgserver

build-observer:
	@mkdir -p $(BUILD_DIR)
	$(GO) build $(GOFLAGS) -o $(BUILD_DIR)/$(BINARY) $(CMD)

# Build the org server binary (cmd/observer-org). Separate binary, separate
# deployment from the agent; built as part of `make build` so CI covers it.
build-orgserver:
	@mkdir -p $(BUILD_DIR)
	$(GO) build $(GOFLAGS) -o $(BUILD_DIR)/$(ORG_BINARY) $(ORG_CMD)

# Cross-compile the Antigravity Windows-side gRPC bridge. Used by
# observer-on-WSL2 to reach Antigravity's local language_server,
# which binds to Windows-side 127.0.0.1 and isn't reachable from
# inside a WSL distro under default networking. The bridge is a
# tiny Go binary (~8 MB) that runs Windows-side under powershell.exe,
# does process discovery + the gRPC call, and returns Markdown via
# stdout. Skipped silently on non-Linux build hosts where it'd
# never be invoked.
build-antigravity-bridge:
	@mkdir -p $(BUILD_DIR)
	GOOS=windows GOARCH=amd64 $(GO) build $(GOFLAGS) -o $(BUILD_DIR)/antigravity-bridge.exe ./cmd/antigravity-bridge

run: build
	$(BUILD_DIR)/$(BINARY)

test:
	$(GO) test $(GOFLAGS) ./...

test-race:
	$(GO) test $(GOFLAGS) -race ./...

# Single-user-local invariant net: seeds a fixed corpus, drives the
# dashboard's headline endpoints, and diffs the canonicalised JSON
# against goldens captured before the org-mode (Teams) code landed. A
# non-empty diff means an additive change leaked into a solo-local
# response — the one thing the Teams feature must never do. Regenerate
# the goldens intentionally with `go test ./tests/invariant -update`.
test-invariant:
	$(GO) test $(GOFLAGS) ./tests/invariant/...

# Regenerate the org server's agent-protocol stubs from the OpenAPI spec.
# The OpenAPI doc is the source of truth (spec §2.5); the client stubs
# (internal/orgclient/gen) and the server interface
# (internal/orgserver/api/gen) are committed, generated artefacts.
gen-openapi:
	$(OAPI) -config internal/orgclient/gen/cfg.yaml $(OPENAPI_SPEC)
	$(OAPI) -config internal/orgserver/api/gen/cfg.yaml $(OPENAPI_SPEC)
	$(OAPI) -config internal/orgserver/dashboard/gen/cfg.yaml $(OPENAPI_SPEC)

# Fail if the committed stubs drift from what the spec generates.
# oapi-codegen v2 has no `--validate-strict` flag (the literal flag the
# spec mentions does not exist); regenerating and diffing is the
# equivalent, stronger guarantee — a divergent handler/spec is caught at
# CI time. Checks both modified tracked files and any new untracked file.
verify-openapi: gen-openapi
	@if ! git diff --quiet -- internal/orgclient/gen internal/orgserver/api/gen internal/orgserver/dashboard/gen || \
	    [ -n "$$(git ls-files --others --exclude-standard internal/orgclient/gen internal/orgserver/api/gen internal/orgserver/dashboard/gen)" ]; then \
	  echo "openapi codegen drift: run 'make gen-openapi' and commit the result"; \
	  git --no-pager diff -- internal/orgclient/gen internal/orgserver/api/gen internal/orgserver/dashboard/gen; \
	  exit 1; \
	fi
	@echo "openapi: generated stubs match $(OPENAPI_SPEC)"

# Regenerate npm/observer/README.md + pypi/observer/README.md from the
# channel-specific templates by substituting the shared body block
# (docs/distribution/README-body.md). The body is the canonical source
# for everything from "Per-AI-client setup" through "Configuration"; the
# templates own each channel's title, badges, install, quickstart step 1,
# and channel-specific troubleshooting + footer.
sync-distribution-readmes:
	scripts/build-distribution-readmes.sh

# Drift gate for the distribution READMEs: regenerates into temp files
# and diffs against the committed READMEs. Fails if either drifts. Runs
# in CI so an edit to one channel's README directly (instead of the
# shared body or template) fails fast with the diff and a remediation
# hint. Never mutates the working tree, so a stale local README during
# `make verify-distribution-readmes` is still surfaced.
verify-distribution-readmes:
	@scripts/verify-distribution-readmes.sh

cover:
	$(GO) test $(GOFLAGS) -race -coverprofile=$(COVER_OUT) -covermode=atomic ./...
	$(GO) tool cover -func=$(COVER_OUT) | tail -1

lint:
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not installed — see https://golangci-lint.run/"; exit 0; }
	golangci-lint run ./...

fmt:
	$(GO) tool gofumpt -w .

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

clean:
	rm -rf $(BUILD_DIR) $(COVER_OUT)

# ---------------------------------------------------------------
# Web (redesigned React/Vite dashboard, mounted at /v2/).
#
# `make build` stays pure-Go and does NOT require Node. The built
# artifacts at $(WEB_EMBED_DIST) are committed; regenerate them
# via `make web-build` whenever you touch web/ sources, before
# committing.
# ---------------------------------------------------------------
web-install:
	cd $(WEB_DIR) && npm ci

web-dev:
	cd $(WEB_DIR) && npm run dev

web-build:
	cd $(WEB_DIR) && npm ci --silent && npm run build
	@rm -rf $(WEB_EMBED_DIST)
	@mkdir -p $(WEB_EMBED_DIST)
	@cp -R $(WEB_DIST)/. $(WEB_EMBED_DIST)/
	@echo "web: rebuilt $(WEB_EMBED_DIST) from $(WEB_DIST)"

web-clean:
	rm -rf $(WEB_DIST) $(WEB_DIR)/node_modules

# Build the org dashboard SPA (web2/) and refresh its embedded dist. Mirrors
# web-build; the artifacts at $(WEB2_EMBED_DIST) are committed and embedded
# into the observer-org binary.
web-build-org:
	cd $(WEB2_DIR) && npm ci --silent && npm run build
	@rm -rf $(WEB2_EMBED_DIST)
	@mkdir -p $(WEB2_EMBED_DIST)
	@cp -R $(WEB2_DIST)/. $(WEB2_EMBED_DIST)/
	@echo "web-org: rebuilt $(WEB2_EMBED_DIST) from $(WEB2_DIST)"

web-org-clean:
	rm -rf $(WEB2_DIST) $(WEB2_DIR)/node_modules
