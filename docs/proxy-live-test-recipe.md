# Proxy live-test recipe: codex + ChatGPT-Plus auth

A reusable recipe for validating the proxy against real
codex+ChatGPT-Plus traffic without touching user state. Distilled
from the v1.7.3 live verification of V4-1 / V4-4 / V4-6
(2026-05-28).

Use this when:
- A proxy change might affect chatgpt-auth (streaming gate, header
  extraction, insert path).
- You suspect codex turns are dropping (`<unattributed>` rows, missing
  `session_id`, etc.).
- You want to confirm a release before tagging by exercising the
  ChatGPT-Plus auth path end-to-end.

## Why this is its own recipe

The chatgpt-auth path takes a fundamentally different code branch
than API-key OpenAI (see `internal/proxy/provider.go::isChatGPTAuthRequest`).
Unit tests with synthetic headers can't catch wire-format gotchas
like the underscore-vs-hyphen header bug that bit V4-4. The only way
to be sure is to drive real codex traffic through your build.

The recipe is **isolated** — runs against `/tmp/v34test/observer.db`
with `auto_register = false`, on a non-default port, with a stash of
the user's `~/.observer/` and `~/.codex/` untouched.

## Prerequisites

- WSL2 (or any Linux) with codex installed and a ChatGPT-Plus
  `~/.codex/auth.json` (`auth_mode = "chatgpt"`).
- A built observer binary at `./bin/observer` with the changes you
  want to test.
- Port `8830` free locally (or pick another; this recipe assumes
  `8830`).
- Nothing else listening on the observer port — `gh release list` /
  daemon checks recommended.

```bash
# Confirm codex auth_mode
grep auth_mode ~/.codex/auth.json
# expected: "auth_mode": "chatgpt"

# Confirm port free
ss -lnt 2>/dev/null | grep 8830 && echo "PORT IN USE — pick another" || echo "free"

# Confirm binary
ls -la bin/observer && bin/observer --version
```

## Set up

```bash
mkdir -p /tmp/v34test/cwd
cat > /tmp/v34test/config.toml <<'EOF'
# Local test rig for codex+ChatGPT-Plus verification.
# Isolated from ~/.observer/.

[observer]
db_path = "/tmp/v34test/observer.db"
log_level = "debug"

[observer.hooks]
auto_register = false  # critical: do NOT touch user state

[proxy]
enabled = true
port = 8830
anthropic_upstream = "https://api.anthropic.com"
openai_upstream    = "https://api.openai.com"
chatgpt_upstream   = "https://chatgpt.com"

[compression]
EOF
```

`auto_register = false` is **non-negotiable**. Without it, `observer
start` will rewrite `~/.codex/hooks.json` + `~/.claude/settings.json`
to point at the test binary — disrupting the user's working setup
until manually restored.

## Start the daemon

```bash
rm -f /tmp/v34test/observer.db /tmp/v34test/observer.db-shm /tmp/v34test/observer.db-wal
nohup ./bin/observer start --config /tmp/v34test/config.toml \
  --no-dashboard --port 8830 \
  > /tmp/v34test/observer.out 2> /tmp/v34test/observer.err < /dev/null &
disown
sleep 3
pgrep -af 'observer.*8830' | head -1
ss -lnt 2>/dev/null | grep 8830 | head -1
```

Expected: PID printed, port listening. If not, check
`/tmp/v34test/observer.err`.

## Fire a trivial turn

```bash
cd /tmp/v34test/cwd && codex exec --json \
  -c 'model_providers.openai-observer.base_url="http://127.0.0.1:8830/v1"' \
  --skip-git-repo-check -C /tmp/v34test/cwd "reply OK"
```

> The `-c model_providers.openai-observer.base_url=…` override is
> needed because `~/.codex/config.toml` typically hardcodes the
> production proxy port (8820); a plain `-c openai_base_url=…` does
> NOT override a `[model_providers.<name>]` entry's `base_url`. This
> caught me on first attempt.

## What to expect (post-v1.7.3)

| Check | Expected | Failure mode |
|---|---|---|
| Wall time | `~9-12 s` for a trivial turn | `~120 s` if V4-1 regressed (buffered path) |
| Stdout retries | `0` `Reconnecting...` lines | `5×` if V4-1 regressed |
| `codex exec` exit code | `0` | `1` if connection failed entirely |
| Final stdout event | `turn.completed` with usage | `turn.failed` if upstream rejected |

```bash
# Inspect the api_turn row that landed
sleep 2 && sqlite3 /tmp/v34test/observer.db \
  "SELECT id, provider, model, session_id, input_tokens, output_tokens, \
   cache_read_tokens, cost_usd FROM api_turns ORDER BY id DESC LIMIT 5;"

# Check for insert errors
grep -E "insert.*api_turn|context canceled" /tmp/v34test/observer.err | head -3
```

Expected (v1.7.3+):
- Exactly one row per turn
- `provider = openai`
- `session_id` = a UUIDv7 (matches the `thread_id` codex printed)
- `cost_usd` populated
- Zero `context canceled` warnings in stderr

Failure modes:
- `session_id = ''` → V4-4 regressed or codex changed its header
  shape (re-capture headers; see "Header capture" below)
- Zero rows in `api_turns` + `context canceled` in stderr → V4-6
  regressed (`insertTurnDetached` not in place at one or more sites)
- Zero rows with no warnings → request never reached the proxy
  (port mismatch, codex still hitting old proxy)

## Header capture (diagnostic)

If codex's wire format changes in a future release, you'll need to
re-capture the headers. Temporarily add a debug log at
`proxy.go::serve` right after `chatgptAuth := …`:

```go
chatgptAuth := provider == models.ProviderOpenAI && isChatGPTAuthRequest(r)
// TEMP-DEBUG: dump headers to figure out what codex sends.
{
    var keys []string
    for k := range r.Header { keys = append(keys, k) }
    p.logger.Info("TEMP-DEBUG req",
        "path", r.URL.Path,
        "provider", provider,
        "chatgptAuth", chatgptAuth,
        "header_keys", keys,
    )
}
```

Rebuild, restart, run codex, then:

```bash
grep "TEMP-DEBUG" /tmp/v34test/observer.err | head -3
```

Remove the debug block before committing. **Don't ship this** — it's
diagnostic-only.

## Tear down

```bash
kill $(pgrep -f 'observer.*8830') 2>/dev/null
rm -rf /tmp/v34test
```

The user's `~/.observer/` and `~/.codex/` are untouched throughout
because we set `auto_register = false` and used `-c` overrides for
codex.

## v1.7.3 verification baseline

Recorded 2026-05-28 against the merged tree of PRs #15+#16+#17+#18+#20:

| Turn | Wall | Retries | api_turn row |
|---|---:|---:|---|
| `reply OK` | 9.27 s | 0 | `session_id=019e6db0-27b0-7272-9c1e-161aca5fccd8`, full cost |
| `say hello` (multi-step + tool call) | 10.60 s | 0 | session_id matched, cost populated |

Pre-fix baselines (for comparison):
- Buffered path: `~120 s` per turn + 5× `Reconnecting...`
- Pre-V4-6 insert race: row dropped silently every time

The codex `thread_id` field in stdout MUST match the captured
`session_id` byte-for-byte. If they don't, V4-4's header fallback
isn't picking up the right header (re-capture per the diagnostic
section above).

## Related

- `docs/observer-platform-issues-v4.md` — V4-1, V4-4, V4-6 source-of-truth
- `internal/proxy/proxy.go::resolveAPITurnSessionID` — header fallback list
- `internal/proxy/proxy.go::insertTurnDetached` — the post-response insert helper
- Memory: `feedback_codex_canonical_hyphen_headers`,
  `feedback_proxy_detached_insert_context`
