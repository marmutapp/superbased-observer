# lumen — mechanism-positive task (PLACEHOLDER, not yet frozen)

Phase 0 places this task in **Stratum M (mechanism-positive)**, per the
plan's Phase 0 step 1: *"Port the lumen task into mechanism (it is a
compressor-positive stress case, not a representative headline)."*

It is a placeholder because the real lumen task is a **git-snapshot**
workspace (an external repo) and is not committed wholesale — it gets
frozen by the procedure in [`../../README.md`](../../README.md)
("Freezing a task") once the operator supplies the pinned commit.

## What lands here when frozen

- `task.toml` with `[workspace] kind = "git-snapshot"`, a pinned
  `commit`, and an `archive_sha256`.
- `assertions/` with the semantic quality-guard scripts.
- **No** headline eligibility: mechanism tasks show mechanics only and
  are labelled "stress case" on any card (§3.7).

## Why mechanism, not representative

lumen is engineered/known to make a compressor fire hard. Putting it in
the representative stratum would be activation-biased selection (R12) and
would inflate a headline. It stays in Stratum M as a demonstration.

Until frozen, this directory is intentionally inert — `hash-corpus.sh`
records it as a placeholder (no `task.toml`).
