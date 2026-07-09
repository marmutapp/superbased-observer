# testdata/aider

Fixtures for the Aider adapter (`internal/adapter/aider`). All content is
ANONYMIZED — real project content, paths, UUIDs, and any secrets have been
redacted or replaced with synthetic values. None of these are live user
data.

Aider keeps ONE append-only Markdown transcript per git repo at the repo
root: `.aider.chat.history.md`. The fixtures below are named for their
scenario; tests copy them into a temp dir as `.aider.chat.history.md`
before exercising `IsSessionFile` (which requires that exact basename).

## Files

| File | Origin | Exercises |
|---|---|---|
| `readonly.md` | Anonymized from a LIVE v0.86.2 session captured in this repo (2026-07-09, read-only Q&A) | Session header + argv/version/model banner, `#### ` user prompts (incl. `N` / `/exit`), assistant prose, plain-integer + `k`-suffix token lines (`11k sent, 21 received`, `9.4k sent, 328 received`) with per-message + cumulative-session cost |
| `edit-linux.md` | SYNTHESIZED from documented aider output (`base_coder.py`/`repo.py` string grounding, 2026-07-09) — the live read-only session had no edits | `> Applied edit to <path>` (edit_file), `> Running <cmd>` (run_command), a SEARCH/REPLACE block inside assistant prose, a token line WITH cache-write + cache-hit segments (gross→net input), and a secret in a user prompt for the scrubbing test |
| `windows-multi.md` | SYNTHESIZED, Windows-flavored (per the live-capture tracker's Windows facts: v0.86.2, raw `C:\…` paths in prose) | TWO sessions in one file, Windows `C:\…` argv + prose paths, edit + shell markers, distinct per-session ids |
| `analytics.json` | Shape of the global `~/.aider/analytics.json` (uuid zeroed) | Documents the ONLY global aider state (analytics/installs metadata — no session index); NOT parsed by the adapter |

## Ground-truth notes (2026-07-09)

- **Session delimiter**: `# aider chat started at YYYY-MM-DD HH:MM:SS` (local time).
- **Token line** (`aider/coders/base_coder.py`): `Tokens: {sent} sent[, {cw} cache write][, {ch} cache hit], {recv} received.[ Cost: ${msg} message, ${session} session.]`. `format_tokens` rounds: `<1000` verbatim, `<10000` one-decimal `k`, else rounded `k` — so counts are lossy (reliability=unreliable). `sent` is GROSS (includes the cache hit) → the adapter nets `InputTokens = sent − cacheHit`. `message` cost is per-turn, `session` cost cumulative.
- **Edit / shell markers** (`base_coder.py:2334` / `2472`): `Applied edit to {path}`, `Running {command}`; commit line `Commit {hash} {message}` (`repo.py:313`) is informational and not emitted.
- **Off-limits siblings**: `.aider.input.history` (user inputs only — deliberately unclaimed) and `.aider.tags.cache.v4/` (repo-map cache) are NEVER parsed.
- **No per-turn timestamps** exist in the Markdown, so every event in a session carries the session-start time.
