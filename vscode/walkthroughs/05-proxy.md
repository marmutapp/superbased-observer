# Route AI CLIs through the proxy

JSONL adapters give Observer approximate token counts. The
**reverse proxy** gives you **accurate** counts by sitting between
your AI tool and the upstream API — every request/response is
captured byte-for-byte.

To use it, your AI CLI needs to be told to talk to
`http://127.0.0.1:8820` instead of `https://api.anthropic.com` (or
the OpenAI equivalent).

### Option 1 — Use the contributed terminal profile

Open the terminal dropdown (down arrow next to **+** in the
Terminal panel) → **AI Coding Tool (Observer-proxied)**.

The new terminal already has these env vars set:

```sh
ANTHROPIC_BASE_URL=http://127.0.0.1:8820
OPENAI_BASE_URL=http://127.0.0.1:8820/v1
ENABLE_TOOL_SEARCH=true
```

Launch `claude`, `codex`, or any AI CLI from here and it routes
through Observer automatically.

### Option 2 — Copy the env to your existing terminal

The `Observer: Copy Proxy Env Vars` command writes the same triple
to your clipboard in your shell's syntax (`export …` for bash/zsh,
`$env:…` for PowerShell, `set …` for cmd). Paste into any existing
terminal.

### Why `ENABLE_TOOL_SEARCH=true`?

Without it, Claude Code's SDK disables its `ToolSearch` feature
when `ANTHROPIC_BASE_URL` is set, and the proxy becomes a net
**cost** on the claude-code recipe. The Observer proxy is
specifically engineered to forward `tool_reference` blocks safely,
so this override re-enables the SDK's normal lazy-MCP path.
