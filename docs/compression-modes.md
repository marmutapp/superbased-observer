# Conversation compression — modes, providers, and recipes

> **Canonical reference for `[compression.conversation]`.** The README and the
> npm package README carry a condensed version of the matrix + recipes below;
> this doc is the full source of truth for which knobs are real and which
> compression `mode` does what on Anthropic vs OpenAI/Codex traffic.

Conversation compression is **proxy-only**. It runs inside `observer`'s API
proxy on every upstream request, rewriting large `tool_result` blocks before
they reach the provider. It does not run on the hook / watcher / `observer run`
ingestion paths — those get shell-output filtering and FTS5 indexing instead.
If your AI client does not route through the proxy, none of this engages.

Opt in with `[compression.conversation].enabled = true` and point your client at
the proxy (`ANTHROPIC_BASE_URL` / `OPENAI_BASE_URL = http://127.0.0.1:8820…`).

## The `mode` knob, by provider

`mode` selects the budget-enforcement strategy. The per-type compression of
`tool_result` bodies (json / logs / text / …) runs in **every** mode; `mode`
only changes how — and whether — messages are dropped and whether an Anthropic
`cache_control` marker is injected.

| `mode` | What it does | Claude Code (Anthropic) | Codex / OpenAI |
|---|---|---|---|
| `token` | Per-type compress `tool_result` bodies, then drop the lowest-scored non-preserved messages until `target_ratio` is met. | ✅ Works. | ✅ Works — the clearest choice for Codex/OpenAI. |
| `cache` | Restricts drops to the tail half of the conversation so the prefix stays stable, and injects a `cache_control` marker at the prefix boundary. | ✅ Anthropic-specific. | ⚠️ No effect beyond `token`. The OpenAI path is mode-agnostic and `cache_control` is not an OpenAI concept. |
| `cache_aware` *(default)* | Skips the drop pass entirely, narrows per-type compression to `tool_result` blocks only, and skips `cache_control` injection. Keeps every historical message **byte-identical across turns** so Anthropic's prefix cache keeps hitting and `cache_creation` tokens fall on later turns. No-ops gracefully (behaves like `token` without drops) when there is no SDK marker. | ✅ **Recommended for Anthropic Pro/Max** (Claude Code, where the SDK already places `cache_control` markers) — and the shipped default. | ⚠️ No effect beyond `token` (mode-agnostic OpenAI path), so the default is harmless for Codex. |

> The shipped default is `cache_aware`. (`token` is only the internal fallback
> the enforcer uses when `mode` is left empty.) Because the OpenAI path ignores
> `mode`, leaving the default in place is fine for Codex — it behaves like
> `token` there; set `mode = "token"` explicitly if you want the config to read
> honestly for an OpenAI-only setup.

### Why the cache modes are Anthropic-only

Anthropic's prompt cache is **content-hash based**: the API caches a prefix of
the request keyed by its exact bytes, and you pay a one-time `cache_creation`
premium to write it plus a cheap `cache_read` to reuse it. Keeping the
historical prefix byte-stable across turns is what makes the cache hit — and
that is precisely what `cache`/`cache_aware` are engineered to preserve. The
`cache_control` marker is part of the Anthropic Messages API.

OpenAI (and therefore Codex) prompt caching is **automatic and server-side**:
there is no client-side cache marker to place and nothing to tune. The proxy's
OpenAI path always does per-type `tool_result` compression (plus the optional
stash and rolling-summary passes) regardless of `mode`; the `cache` and
`cache_aware` strategies have no additional effect there. Use `token` for
Codex/OpenAI.

## Recipes

### Claude Code (Anthropic Pro / Max) — recommended

`cache_aware` is the recommended mode. On rate-limited plans (Claude's 5h / 7d
windows) the cross-turn cache stability it preserves is usually the difference
between finishing a long task and hitting the limit.

```toml
[proxy]
enabled = true
anthropic_upstream = "https://api.anthropic.com"

[compression.conversation]
enabled = true
mode = "cache_aware"
target_ratio = 0.85
preserve_last_n = 5
compress_types = ["json", "logs", "code"]
```

Point Claude Code at the proxy:

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8820
# restart Claude Code so it routes through the proxy
```

### Codex / OpenAI

Use `token`. The `cache`/`cache_aware` modes are Anthropic-only and would no-op;
OpenAI prompt caching happens automatically server-side.

```toml
[proxy]
enabled = true
openai_upstream = "https://api.openai.com"

[compression.conversation]
enabled = true
mode = "token"
target_ratio = 0.85
preserve_last_n = 5
compress_types = ["json", "logs", "code"]
```

Point Codex at the proxy (note the `/v1`):

```bash
export OPENAI_BASE_URL=http://127.0.0.1:8820/v1
# restart Codex so it routes through the proxy
```

> Codex routed through the proxy with API-key auth gets live proxy compression.
> Codex logged in with a ChatGPT plan currently behaves as JSONL-only on the
> local machine (sessions/actions/approximate tokens are still recovered from
> `~/.codex/sessions`, but live proxy compression is not available in that mode).

## Full `[compression.conversation]` knob reference

These are the **only** TOML keys the proxy reads for conversation compression.
(`prefix_bytes` and a `[compression.conversation.weights]` table are *not*
config knobs — the prefix budget is a fixed 8 KB internal default and the score
weights are not user-tunable from TOML.)

```toml
[compression.conversation]
enabled = false            # opt-in; rewrites request bodies in flight
mode = "cache_aware"       # "token" | "cache" | "cache_aware"; default cache_aware (see matrix)
target_ratio = 0.85        # cap output bytes at this fraction of input
preserve_last_n = 5        # never drop the most recent N messages
compress_types = ["json", "logs", "code"]   # default; add "text"/"diff"/"html" to opt in

# Compressed-Content Retrieval (CCR): stash oversized tool_result bodies on
# disk and replace them inline with a marker the model can pull back via the
# retrieve_stashed MCP tool. Provider-agnostic. Opt-in.
[compression.conversation.stash]
enabled = false
dir = "~/.observer/stash"
threshold_bytes = 8192
max_total_mb = 256

# Rolling summarisation: once a session crosses threshold_tokens, replace older
# messages with a one-paragraph summary marker. The summary model is chosen per
# provider — Anthropic and OpenAI traffic each use their own. Opt-in.
[compression.conversation.rolling]
enabled = false
threshold_tokens = 80000
summary_model = "claude-haiku-4-5"   # Anthropic-side summariser
openai_summary_model = "gpt-5-nano"  # OpenAI/Codex-side summariser
auth_cache_size = 1024

# Compaction survival: when a session has a recent compaction event, prepend a
# synthetic recovery-context system block (Anthropic only). Opt-in.
[compression.conversation.compaction]
inject_post_compact = false
```

Every key also has an environment override:
`OBSERVER_COMPRESSION_CONVERSATION_<KEY>` (uppercased; nested sections join with
extra underscores, e.g. `OBSERVER_COMPRESSION_CONVERSATION_MODE=cache_aware`).

## What you lose without the proxy

Skip the proxy and you keep full hook + JSONL ingestion, the dashboard, MCP, and
the shell + FTS5 indexing compression layers. You lose proxy-grade token
accuracy, conversation compression (all modes above), and the stash.
