# SuperBased Observer

## What This Is

SuperBased Observer is a Go tool that captures, normalizes, and analyzes tool call activity from AI coding assistants (Codex, Codex, Cursor, Cline, Copilot). It provides an API proxy for accurate token tracking, an MCP server for on-demand project knowledge queries, and a multi-layer compression pipeline.

The full specification is in `superbased-final-spec-v2.md`. Read it before making architectural decisions.

## Critical Files

- `PROGRESS.md` — Implementation progress tracker. READ THIS FIRST on every session start. UPDATE after every completed task.
- `superbased-final-spec-v2.md` — The spec. Sections are referenced as `§N` throughout.
- `Makefile` — Build, test, lint targets.
- `docs/release-runbook.md` — Two-repo release model (private `origin`, public `public`). Read before tagging or refreshing the public repo. The release script is `scripts/release.sh`.

## Session Protocol

1. **Start every session** by reading `PROGRESS.md` to understand current state
2. **Before coding**, check which phase/task is next
3. **After completing each task**, update PROGRESS.md checkboxes and commit
4. **Before ending a session**, update the "Current Status" section in PROGRESS.md with exactly where you stopped, what's in progress, and any context needed to resume

## Compact Instructions

When summarizing this conversation, preserve:
- All modified file paths with the specific changes made
- Current task from PROGRESS.md and its completion state
- Any architectural decisions made and their reasoning
- Active debugging hypotheses if mid-fix
- The current git branch and last commit hash

## Code Standards

- Go 1.22+. Use `modernc.org/sqlite` (pure Go, no CGO).
- `go fmt`, `go vet`, `golangci-lint run` must pass before commits.
- Conventional commits: `feat:`, `fix:`, `chore:`, `docs:`, `test:`, `refactor:`
- Every exported function has a doc comment. Every package has a `doc.go`.
- Table-driven tests. >80% coverage on core packages.
- Error wrapping: `fmt.Errorf("store.InsertAction: %w", err)`
- No `panic()` in library code.
- Context parameters on all DB and I/O operations.

## Architecture Quick Reference
cmd/observer/main.go            → cobra CLI entry point
internal/config/                → TOML config loading
internal/db/                    → SQLite connection, WAL, migrations
internal/models/                → Shared types (Project, Session, Action, etc.)
internal/scrub/                 → Secrets scrubbing
internal/freshness/             → Content hashing, freshness classification
internal/adapter/               → Platform adapters (Codex, codex, cline, cursor [hook-only], copilot, openclaw, opencode, pi)
internal/store/                 → Storage layer (batch inserts, queries)
internal/watcher/               → fsnotify file watcher daemon
internal/hook/                  → Hook handler, registration, integrity
internal/proxy/                 → API reverse proxy (Anthropic + OpenAI)
internal/mcp/                   → MCP server (stdio, 13 tools always-on + retrieve_stashed conditional on stash)
internal/compression/shell/     → RTK-style shell output filters
internal/compression/conversation/ → Conversation-level compression
internal/compression/indexing/  → FTS5 tool output indexing
internal/intelligence/          → discover, learn, patterns, scoring, cost, suggest, dashboard
internal/codegraph/             → codebase-memory-mcp integration
internal/git/                   → Git root detection, branch, path normalization
testdata/                       → Test fixtures (real anonymized session logs)
docs/                           → Architecture, adapter guide, MCP reference

## Build & Test

```bash
make build          # go build -o bin/observer ./cmd/observer
make test           # go test -race ./...
make lint           # golangci-lint run
make fmt            # gofmt -w .
make all            # fmt + lint + test + build
```

## Don'ts

- Don't use `log.Fatal` or `os.Exit` outside `main.go`
- Don't store file contents or command outputs in the DB (only paths, commands, excerpts)
- Don't make network calls in the observer/watcher (all local)
- Don't use CGO (use `modernc.org/sqlite`)
- Don't hardcode paths — use `os.UserHomeDir()`, `os.UserConfigDir()`, `filepath.Join()`
- Don't skip updating PROGRESS.md

