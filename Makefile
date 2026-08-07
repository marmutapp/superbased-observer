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
        web-install web-dev web-build web-clean docs-build \
        website-build verify-website-build \
        track-build verify-track-build \
        plugins-build verify-plugins-build \
        taxonomy-build verify-taxonomy-build verify-taxonomy-ts \
        taxonomy-migration-build verify-taxonomy-migration \
        assistant-migration-build verify-assistant-migration \
        reasoning-migration-build verify-reasoning-migration \
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

# -timeout 40m matches ci.yml's test step. Without it the local run
# inherits go test's 10m per-binary default, and cmd/observer already
# measures 593s under -race on a developer box (2026-07-26) — ~7s of
# headroom, so a busy machine fails RED with `panic: test timed out`
# and nothing actually broken. The release checklist calls for a full
# race suite, which is exactly when that false failure would land.
test-race:
	$(GO) test $(GOFLAGS) -race -timeout 40m ./...

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

# golangci-lint is deliberately NOT a `go tool` dependency: it pins its
# own Go version and must be built with the tree's Go (CI uses
# install-mode: goinstall for exactly that reason — see ci.yml). Resolve
# it from PATH first, then $(GOPATH)/bin, which is where `go install`
# puts it and which is not on PATH in every shell.
#
# A missing linter FAILS. It used to `exit 0`, which made every local
# `make lint` a green that meant "I didn't look" — the binary lives in
# $(GOPATH)/bin here and was never on PATH, so the guard fired every
# time and no local lint has ever actually run. CI was unaffected (it
# calls golangci-lint-action, not this target).
GOLANGCI_LINT ?= $(shell command -v golangci-lint 2>/dev/null || echo $(shell $(GO) env GOPATH)/bin/golangci-lint)

lint:
	@test -x "$(GOLANGCI_LINT)" || { echo "golangci-lint not found at '$(GOLANGCI_LINT)' — install it (https://golangci-lint.run/, install-mode goinstall) or pass GOLANGCI_LINT=/path/to/golangci-lint"; exit 1; }
	$(GOLANGCI_LINT) run ./...

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

# ---------------------------------------------------------------
# Marketing-site documentation (/docs). Renders curated Markdown
# from website/docs-src into the live site shell under
# website/docs/, plus the search index + syntax-highlight CSS.
# The generator is a self-contained module (its deps never touch
# the observer binary). Output is committed; the Cloudflare Pages
# deploy ships it as static files. Run after editing docs-src.
#
# changeloggen runs first: it derives website/docs-src/changelog.md
# + website/feed.xml (Atom) from the repo's own CHANGELOG.md, and
# keeps nav.toml + sitemap.xml's marker-delimited blocks in sync —
# so `gen` then renders the changelog page like any other docs page.
# ---------------------------------------------------------------
docs-build:
	cd website/docs-tools && go run ./changeloggen -changelog ../../CHANGELOG.md -src ../docs-src -sitemap ../sitemap-pages.xml -feed ../feed.xml
	cd website/docs-tools && go run ./gen -src ../docs-src -out ../docs -assets ../assets

# ---------------------------------------------------------------
# Marketing site (/, /about, /enterprise, /newsletter, /privacy,
# /security, /terms). Renders website/pages-src/*.html — TOML front
# matter + an HTML body — through ONE shared shell that owns the
# <head>, the nav and the footer, the same way docs-build renders
# /docs. Output is committed; Cloudflare Pages ships the rendered
# files and the deploy workflow strips website/pages-src/.
#
# Run after editing anything under website/pages-src/. NEVER edit
# website/*.html directly — verify-website-build will fail CI.
# ---------------------------------------------------------------
website-build:
	cd website/docs-tools && go run ./pagegen -src ../pages-src -out ..

# Drift gate for the marketing pages: renders into a temp dir and
# diffs against the committed HTML. Fails if any page drifted — i.e.
# somebody hand-edited a rendered page instead of its source, which
# is exactly how the site accumulated four #topnav variants and three
# footer variants before the shell existed. Never mutates the working
# tree, so a stale local render is still surfaced. Mirrors the
# verify-distribution-readmes pattern.
verify-website-build:
	@scripts/verify-website-build.sh

# ---------------------------------------------------------------
# Per-adapter "Track <tool> costs" SEO pages. website/track-gen reads
# internal/integration.Capabilities() (the one-owner adapter capability
# registry) and emits website/docs-src/track/*.md, then rewrites the
# marker-delimited "generated:track" blocks in nav.toml and sitemap.xml
# so the next docs-build picks up any roster change. Run after a new
# adapter ships (or any Capability row changes) so the track pages and
# nav/sitemap stay in sync with the registry — see
# docs/plans/seo-adapter-comparison-pages-proposal-2026-07-30.md §3.
# ---------------------------------------------------------------
track-build:
	$(GO) run ./website/track-gen
	$(MAKE) docs-build

# Drift gate for the track pages: regenerates into a scratch copy of
# website/docs-src + sitemap.xml and diffs against the committed files.
# Fails if the registry moved (a new/changed adapter) without a matching
# `make track-build` + commit. Never mutates the working tree. Mirrors
# verify-website-build's build-into-temp/never-mutate pattern.
verify-track-build:
	@scripts/verify-track-build.sh

# ---------------------------------------------------------------
# In-tool plugin manifests (plugins/). plugins/plugingen RUNS the real
# `observer init` registrars — internal/mcp's Registrar and
# internal/hook's Registry — against a throwaway sandbox HOME and
# transposes exactly what they wrote into each surface's own packaging
# format: a Claude Code plugin + marketplace catalog, and the static
# Cursor one-click MCP install deeplink. Run after any change to those
# registrars (a new hook event, a changed MCP argument) or after a
# version stamp, so plugin wiring can never fork from init wiring —
# see docs/plans/adapter-plugins-distribution-plan-2026-07-31.md §3.
# ---------------------------------------------------------------
plugins-build:
	$(GO) run ./plugins/plugingen

# Drift gate for the plugin manifests: copies plugins/ into a scratch
# dir, regenerates THERE and diffs against the committed files. Fails if
# a registrar moved without a matching `make plugins-build` + commit.
# Never mutates the working tree. Mirrors verify-track-build's
# build-into-temp pattern.
verify-plugins-build:
	@scripts/verify-plugins-build.sh

# ---------------------------------------------------------------
# Dashboard action taxonomy (web/src/lib/actiontax.gen.*). web/taxgen
# mirrors internal/tooltax — the one owner of the cross-adapter tool/MCP
# taxonomy — into the shape web/src/lib/actions.ts reads: the canonical
# category list, the action-type → {category, label} registry, and the
# MCP name-parse rules (actiontax.gen.json), the ActionCategory literal
# union (actiontax.gen.ts), and the parity vectors the TypeScript gate
# runs (actiontax.vectors.gen.json). Run after any change to the tooltax
# action-type registry, categories or MCP constants, so the dashboard can
# never fork its own taxonomy again — see
# docs/plans/tool-taxonomy-standardization-plan-2026-07-31.md §1/§7.
# ---------------------------------------------------------------
taxonomy-build:
	$(GO) run ./web/taxgen

# Drift gate for the generated taxonomy: regenerates into a scratch dir
# and byte-diffs each artifact against the committed one. Fails if
# tooltax moved without a matching `make taxonomy-build` + commit. Never
# mutates the working tree. Mirrors verify-plugins-build's
# build-into-temp pattern.
verify-taxonomy-build:
	@scripts/verify-taxonomy-build.sh

# Cross-language parity gate: compiles the REAL web/src/lib/actions.ts
# (esbuild, from web/node_modules) and runs it against the generated
# vectors, whose expectations come from tooltax.MCPIdentity. Separate
# target from verify-taxonomy-build because it needs node + web deps,
# while the drift gate needs only Go. Both run in ci.yml's
# taxonomy-build-drift job.
verify-taxonomy-ts:
	@scripts/verify-taxonomy-ts.sh

# ---------------------------------------------------------------
# Historical-data repair for the same taxonomy (internal/db/migrations/
# 077_tooltax_action_type_backfill.sql). internal/db/taxbackfillgen
# transposes the internal/tooltax table into one
# `UPDATE actions SET action_type = ... WHERE action_type = 'unknown'`
# per (tool, action type), so the rows already on disk get the categories
# their adapters only started emitting in WP-T3 — the plan's §3
# requirement that the repair SQL be SOURCED FROM the table rather than
# transcribed by hand.
# ---------------------------------------------------------------
taxonomy-migration-build:
	$(GO) run ./internal/db/taxbackfillgen

# Drift gate for the generated migration: regenerates into a scratch dir
# and byte-diffs against the committed file. Fails on a hand-edit, or on
# a tooltax row that moved without a matching
# `make taxonomy-migration-build` + commit. Never mutates the working
# tree. Mirrors verify-taxonomy-build's build-into-temp pattern.
verify-taxonomy-migration:
	@scripts/verify-taxonomy-migration.sh

# ---------------------------------------------------------------
# Historical-data repair for the assistant-text action-type relabel
# (internal/db/migrations/078_assistant_text_action_type_relabel.sql).
# internal/db/asstbackfillgen transposes the `<tool>.assistant_text`
# emit-site inventory into one
# `UPDATE actions SET action_type = 'assistant_message'
#   WHERE tool = ... AND action_type = 'task_complete' AND raw_tool_name
#   IN (...)` per tool, so the rows already on disk get the type the
# adapters emit after the WP-T6/B2a sweep.
#
# SIBLING of taxonomy-migration-build, deliberately not folded into it:
# 077 is sourced from internal/tooltax and is unknown-only (it can only
# move rows OUT of the unknown bucket), while 078 rewrites rows an
# adapter already classified. Regenerating 077 must never absorb 078's
# rewrite, so the generators, artifacts and gates stay separate.
# ---------------------------------------------------------------
assistant-migration-build:
	$(GO) run ./internal/db/asstbackfillgen

# Drift gate for the generated relabel migration: regenerates into a
# scratch dir and byte-diffs against the committed file. Fails on a
# hand-edit, or on an emit site that moved without a matching
# `make assistant-migration-build` + commit. Never mutates the working
# tree. Mirrors verify-taxonomy-migration's build-into-temp pattern.
verify-assistant-migration:
	@scripts/verify-assistant-migration.sh

# ---------------------------------------------------------------
# Historical-data repair for the B3 reasoning convergence
# (internal/db/migrations/079_reasoning_row_convergence.sql).
# internal/db/reasoninggen transposes the retired codex reasoning emit
# site's PLACEHOLDER shapes — `(reasoning)` and
# `(encrypted reasoning, N bytes)` — into a tool-scoped DELETE preceded
# by the retention.go dependency protocol, plus the cursor
# `cursor.assistant_response` pair rewrite that 078's `.assistant_text`
# scoping could not reach.
#
# SIBLING of assistant-migration-build, deliberately not folded into it:
# 078 REWRITES rows, 079 DELETES them. A delete is a strictly more
# dangerous mode and gets its own generator, its own artifact and its own
# gate, so one regeneration mistake can never turn a relabel into a
# deletion.
reasoning-migration-build:
	$(GO) run ./internal/db/reasoninggen

# Drift gate for the generated convergence migration: regenerates into a
# scratch dir and byte-diffs against the committed file. Fails on a
# hand-edit, or on a producer shape / dependency table that moved without
# a matching `make reasoning-migration-build` + commit. Never mutates the
# working tree.
verify-reasoning-migration:
	@scripts/verify-reasoning-migration.sh

# Generates the on-brand, dark-theme 1200x630 OG/social cards used by the
# generated docs pages (per-section, not per-page — see ogImageFor in
# website/docs-tools/gen/main.go). Uses Playwright/Chromium, already a
# devDependency of website/tools (no new dependency added). Run after
# docs-build if the Compare/Track/Changelog page roster changed.
docs-og:
	cd website/tools && node og-gen.mjs
