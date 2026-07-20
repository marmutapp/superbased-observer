# Benchmark corpus — two strata, frozen before results

This is the frozen-corpus half of the compression/context-savings
showcase (plan: `docs/plans/compression-savings-showcase-plan-2026-07-11.md`,
§3.7). **Phase 0 scaffolding only — no task is measured here, and no
number lives in this tree.**

## The two strata (§3.7)

- **`representative/` — Stratum R (headline-eligible).** Tasks selected
  **independently of whether any compressor fires**, spanning multiple
  repos, languages, task sizes, clean *and* noisy command outputs,
  successful *and* failing commands, short *and* long sessions, and
  deliberate no-op cases. **Only Stratum R produces headline numbers.**
- **`mechanism/` — Stratum M (mechanism-positive stress, demo only).**
  Tasks engineered so a specific compressor fires hard. They show
  *mechanics*, are labelled "stress case", and are **never** headline
  numbers.

Each task is a directory: `task.toml` (declarative def), `workspace/`
(the frozen snapshot or a git-snapshot pin), and `assertions/` (semantic
quality-guard scripts, §3.6).

## Representative inclusion criteria — FROZEN 2026-07-11T20:18:01Z (session 2026-07-12) — operator approved

> **Status: FROZEN.** These inclusion criteria are frozen as of
> `2026-07-11T20:18:01Z` (operator session `2026-07-12`), alongside the
> tool-defs-trim pre-registration
> (`preregistration/tool-defs-trim-2026-07-11.md` §12). The five representative
> tasks in `representative/` (`example-go-build-fix`, `js-median-test-fix`,
> `shell-sum-range-fix`, `go-brackets-feature-add`, `go-pricing-refactor`) are
> the frozen Stratum-R set for the Phase-0a pilot and the full pre-registered
> run; their content is pinned by `corpus/manifest.json`
> (`corpus_hash=892e4afbbb5f5cb33e7d53af8d90801d11640d89ccb906fa7114541474b66bdb`).
> Adding, removing, or relaxing a task after this line invalidates any card
> built on the stratum (R3/R16); a new dated freeze supersedes it instead.

A task qualifies for Stratum R **only** if, decided before seeing any
result:

1. It was chosen for being a realistic coding turn, **not** because a
   compressor is known to fire on it (`firing_independent = true`).
2. It has a deterministic, machine-checkable completion gate
   (`[success].command`).
3. It has at least one semantic assertion beyond binary completion
   (a cheaper-but-worse arm must be detectable as a FAIL, §3.6).
4. Its workspace is reproducible: either a committed `local` snapshot or
   a `git-snapshot` pinned to an immutable commit + archive checksum.
5. Its prompt is strict and identical across arms; the only per-arm
   difference the harness introduces is a documented, semantically-inert,
   equal-length salt (§3.2) — never task content.

Adding a task after seeing results, or relaxing these criteria to fit an
outcome, invalidates any card built on the stratum (R3/R16).

## Freezing a task

**`local` workspace** (self-contained, no external repo): commit the
`workspace/` dir directly. See `representative/example-go-build-fix/`.

**`git-snapshot` workspace** (external repo — not committed wholesale):

1. Pin the exact commit: `git -C <repo> rev-parse HEAD`.
2. Archive it: `git -C <repo> archive --format=tar.gz <commit> -o snap.tgz`.
3. Record `commit` and `archive_sha256 = sha256sum snap.tgz` in
   `task.toml` under `[workspace]`. The archive itself is fetched by the
   freeze/restore step of a later phase — **do not** commit large repos.
4. Record licensing for any third-party repo (§3.12) in the task README.

## Manifest + hashes

`hash-corpus.sh` walks every task and writes `manifest.json` — a
machine-readable index with a content hash per task (over `task.toml` +
`workspace/` + `assertions/`) and a top-level corpus hash. The website's
claim-manifest test (§4.5) and the drift manifest (§3.4) read these
hashes. Regenerate after any corpus edit:

```
benchmarks/corpus/hash-corpus.sh
```

## Write-filter caution (§6)

This repo's write path corrupts secret-shaped tokens (a certain word +
`:` + a value). Every fixture here is phrased around that shape — noisy
outputs use bare tokens (`FAILURE_MARKER_7F3A`), never `word:value`.
Before committing any new fixture, scrub raw command outputs for secrets
and grep artifacts for corruption.
