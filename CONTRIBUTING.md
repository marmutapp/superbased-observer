# Contributing to SuperBased Observer

Thanks for considering a contribution. This document covers how the
project is developed, how to set up a local build, and what we're
looking for in a pull request.

## Before you start

For anything beyond a small fix (typo, obvious bug, small doc
improvement), please open an issue or a
[Discussion](https://github.com/marmutapp/superbased-observer/discussions)
first to align on approach before investing time in a large diff.
This avoids duplicate work and saves you a rewrite if the design
needs to go a different direction.

## How this project is developed

Day-to-day development happens on a private mainline — it interleaves
implementation work with operational and planning material that isn't
meant for a public history, so that full history isn't published. What
you see in this public repository is a sanitized snapshot taken at
each release. As of this writing, each new release snapshot is parented
on the previous public commit, so history accumulates from here
forward (earlier releases appeared as a single replaced commit — that
was a pipeline gap, since fixed).

External pull requests are reviewed publicly on GitHub like any other
project. Once accepted, a maintainer integrates the change into the
private mainline and it ships in the next sanitized snapshot,
credited to you in `CHANGELOG.md` and the release notes. Your commit
history on the PR branch is preserved in the PR itself; it doesn't
appear byte-for-byte in the mainline history, but the credit does.

We think this is worth stating plainly rather than leaving you to
infer it from a repository that otherwise looks like a normal
single-history project.

## Development setup

Requirements: **Go 1.22+**. Observer uses `modernc.org/sqlite` (pure
Go, no CGO) — no C toolchain needed.

```bash
git clone https://github.com/marmutapp/superbased-observer.git
cd superbased-observer
make build     # go build -o bin/observer ./cmd/observer
make test      # go test -race ./...
make lint      # golangci-lint run
make fmt       # gofmt -w .
make all       # fmt + vet + lint + test + build
```

The dashboard frontend lives in `web/` (a separate npm project). If
you touch anything under `web/`, rebuild the embedded assets before
building the Go binary:

```bash
make web-install   # first time only
make web-build     # regenerates internal/intelligence/dashboard/webapp/dist
make build
```

## Code conventions

- **Conventional commits**: `feat:`, `fix:`, `docs:`, `test:`,
  `chore:`, `refactor:`, `perf:`.
- **Table-driven tests** are the expected style for new logic.
- **Every exported function gets a doc comment.**
- Errors are wrapped with context, e.g.
  `fmt.Errorf("store.InsertAction: %w", err)`.
- No `panic()` outside `main.go`; no `log.Fatal()` / `os.Exit()`
  outside `main.go` either.
- Don't hardcode paths — use `os.UserHomeDir()`, `os.UserConfigDir()`,
  `filepath.Join()`.

## What we especially welcome

- **New AI-tool adapters.** Observer supports 26 coding tools today;
  if yours isn't one of them, this is the highest-leverage
  contribution you can make. Look at an existing adapter under
  `internal/adapter/` for the shape.
- **Sanitized test fixtures** — anonymized real session logs that
  exercise an edge case we don't have coverage for.
- **Windows / WSL fixes.** Cross-platform path handling and
  cross-OS hook bridging are an ongoing source of edge cases.
- **Pricing-table updates**, with a source (a provider pricing page
  or changelog entry) linked in the PR description.
- **Documentation and dashboard improvements.**

## Pull request expectations

- `make test` and `make lint` pass locally.
- Diffs are focused — one logical change per PR. Large refactors are
  easier to review (and land) if you flag them in an issue first.
- No secrets, real file paths from your own machine, or private data
  in the diff or in any test fixtures.
- New behavior gets a test. Bug fixes ideally include a regression
  test.

We'll review as promptly as we can and will always leave a comment
even if the answer is "not right now" — silence isn't the intended
experience here.
