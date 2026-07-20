# Conversation compression — modes, providers, and profiles

> **Canonical reference for `[compression.conversation]` and the compression
> profiles.** The README and the npm/PyPI package READMEs carry a condensed
> version of the matrix + profiles below; this doc is the full source of truth
> for which knobs are real and which compression `mode` does what on Anthropic
> vs OpenAI/Codex traffic.

Conversation compression is **proxy-only**. It runs inside `observer`'s API
proxy on every upstream request, rewriting large `tool_result` blocks before
they reach the provider. It does not run on the hook / watcher / `observer run`
ingestion paths — those get shell-output filtering and FTS5 indexing instead.
If your AI client does not route through the proxy, none of this engages.

Opt in with `[compression.conversation].enabled = true` (Settings →
Compression, or `observer config set compression.conversation.enabled true`)
and route your client through the proxy — the dashboard's Compression tab →
**Proxy** banner button is the quickest way; wrappers and env vars also work
(see the README's "Per-AI-client setup").

## Profiles — who supplies the parameters

The `enabled` switch is the only thing the master config decides by itself.
The **parameters** (mode, ratio, types, …) are supplied by **profiles** —
named parameter sets resolved per traffic class at the proxy boundary:

| Profile | Auto-assigned to | Parameters |
|---|---|---|
| `claude-code` | Anthropic-path traffic | `cache_aware`, ratio 0.85, keep last 5, types json+logs+code+tools (tools-defs trim A2-adopted 2026-06-11), stash off |
| `codex-safe` | OpenAI-path traffic | `token`, ratio 0.95, keep last 15, logs + tools-defs trim (A6-adopted 2026-06-11) |
| `codex-variant` | manual (`observer profile assign openai codex-variant`) | `token`, ratio 0.99, keep last 50, no per-type compression — for `*-codex` reasoning models |
| `default` | unassigned traffic / escape hatch | exactly the master `[compression.conversation]` keys |

- Resolution precedence: per-tool assignment (`[profiles.by_tool]`) beats
  per-provider (`[profiles.by_provider]`) beats `[profiles].default`; a repo's
  own `<root>/.observer/config.toml` overrides all of it for that repo's
  traffic (and can turn compression OFF for the repo, never on).
- Tool identity for the per-tool tier resolves in two steps at the proxy
  boundary: the hook-fed pid bridge first (exact — Claude Code's SessionStart
  hook), then a request-header signature table for hookless clients
  (`codex` via its `codex_cli_rs` UA/`Originator`, `kilo-code-cli` via
  `X-Title: Kilo Code` / `Kilo-Code/` UA, `opencode` via `X-Title: opencode` /
  `opencode/` UA, plus `claude-cli/` UA as a no-hooks safety net). Clients
  with no distinctive headers (e.g. Cline CLI — stock SDK UA only) stay on
  the per-provider tier.
- Profiles are **hot for new sessions** — edits, reassignments, custom-profile
  files, and repo overlays all apply without a daemon restart. In-flight
  sessions keep the parameters they started with (deliberate: mid-session
  flips would corrupt rolling-summary state and cache alignment).
- Custom profiles: `observer profile create mine --from claude-code` +
  `observer profile set mine compression.conversation.target_ratio 0.9`, or
  the Settings → Profiles editor. Inspect anything with
  `observer profile show <name>`.
- Profiles never flip the master switch; `observer start --recipe <name>` is a
  deprecated alias that pins one profile for all traffic.
- Profile content changes are A/B-gated, not vibes — the evidence trail lives
  in `docs/plans/profile-content-refresh-ab-plan-2026-06-10.md` and
  `docs/v1.7.23-compression-savings-empirical-2026-06-01.md`.

## The `mode` knob, by provider

`mode` selects the budget-enforcement strategy — it's a profile parameter, so
each shipped profile above already pins the right one; the matrix is the why.
The per-type compression of `tool_result` bodies (json / logs / text / …) runs
in **every** mode; `mode` only changes how — and whether — messages are
dropped and whether an Anthropic `cache_control` marker is injected.

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

## Setup, per provider

### Claude Code (Anthropic Pro / Max)

Enable the switch and route — the `claude-code` profile applies itself to
Anthropic traffic. On rate-limited plans (Claude's 5h / 7d windows) the
cross-turn cache stability of its `cache_aware` mode is usually the difference
between finishing a long task and hitting the limit.

```toml
[compression.conversation]
enabled = true        # the profile supplies everything else
```

Route Claude Code through the proxy: the Compression tab's **Route through
the observer proxy…** button (durable, preview→confirm), or for a one-off
shell:

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8820
# restart Claude Code so it routes through the proxy
```

> With MCP servers registered, also `export ENABLE_TOOL_SEARCH=true` — see
> the README's quickstart note.

### Codex / OpenAI

Same single switch — OpenAI-path traffic auto-resolves to `codex-safe`
(`token` mode; the `cache`/`cache_aware` strategies are Anthropic-only and
would no-op; OpenAI prompt caching happens automatically server-side).
Running a `*-codex` reasoning model? Reassign:

```bash
observer profile assign openai codex-variant
```

Route codex through the proxy (the dashboard button, or note the `/v1`):

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
In the profile world they play two roles: in the **master config** they are the
fallback parameter set used by the `default` profile (plus `enabled`, the one
switch profiles can never flip); in a **profile file**
(`~/.observer/profiles/<name>.toml`) the same keys override the master per
traffic class. (`prefix_bytes` and a `[compression.conversation.weights]`
table are *not* config knobs — the prefix budget is a fixed 8 KB internal
default and the score weights are not user-tunable from TOML.)

```toml
[compression.conversation]
enabled = false            # opt-in; rewrites request bodies in flight (master-only key)
mode = "cache_aware"       # "token" | "cache" | "cache_aware"; default cache_aware (see matrix)
target_ratio = 0.85        # cap output bytes at this fraction of input
preserve_last_n = 5        # never drop the most recent N messages
compress_types = ["json", "logs", "code"]   # master default; the claude-code
                           # profile adds "tools" (A2); add "text"/"diff"/"html" to opt in

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

## See also

- **Per-mechanism methodology** — the dashboard help drawer (`?`) has a
  "Full methodology" expandable for every mechanism (json / code / logs /
  text / diff / html / drop); deep-link via `#help/…` fragments.
- `docs/dashboard-walkthrough.md` — the Compression tab tour (savings charts,
  events table, why negative savings can happen).
- `docs/plans/profile-content-refresh-ab-plan-2026-06-10.md` — the A/B
  evidence trail behind every profile content change (A1: ratio 0.85
  re-validated; A2: tools-defs trim adopted).
- `docs/v1.7.23-compression-savings-empirical-2026-06-01.md` — the measured
  savings the shipped defaults are built on.
- `docs/cache-tracking.md` — how cache effects of compression decisions are
  observed and attributed (the `tools_changed` cause the A2 gate watched).
