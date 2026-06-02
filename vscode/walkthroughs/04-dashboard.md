# Open the dashboard

`Observer: Open Dashboard` opens the full React analytics dashboard
inside a VS Code editor tab. It's the same surface as the standalone
`http://127.0.0.1:8081/` — Overview / Sessions / Actions / Cost /
Analysis / Tools / Compression / Discovery / Settings.

### What you'll find on each tab

- **Overview** — today's spend, top model, burn rate, month
  projection, budget %.
- **Sessions** — every captured session with cost, duration, tool,
  and per-session drill-down (system prompt + actions timeline +
  tokens by turn).
- **Cost** — per-day / per-tool / per-model rollup with cache
  efficacy and compression savings.
- **Analysis** — high-context turns, prior-month-same-day
  comparisons, cache-savings trend.
- **Discovery** — cross-tool overlap, stale re-read waste,
  redundant command patterns.
- **Settings** — schema-driven editor for everything in
  `~/.observer/config.toml`.

### Native VS Code surfaces

Alongside the webview, the extension also adds:

- A **status bar** item showing today's spend (right side of the
  bottom bar).
- An **activity bar** view container (`observer` in the left rail)
  with four trees — Today, Sessions, Discovery, Costs.
- A **file decoration** dot next to files your AI tools touched in
  the last 24 hours, with a hover summary.

### Codespaces / Remote-SSH

The webview uses `portMapping` so the iframe's `127.0.0.1` is
rewritten to the workspace host's loopback. Codespaces, Remote-SSH,
WSL2, and Dev Containers all work without operator intervention.
