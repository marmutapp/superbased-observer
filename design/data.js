/* ============================================================
   Sample data — populated from real numbers in the brief.
   Exposed on window for cross-script access.
   ============================================================ */
(function () {
  // ---- Tool catalog ----
  const TOOLS = {
    "claude-code":  { color: "var(--tool-claude-code)",  label: "Claude Code" },
    "codex":        { color: "var(--tool-codex)",        label: "Codex" },
    "cursor":       { color: "var(--tool-cursor)",       label: "Cursor" },
    "cline":        { color: "var(--tool-cline)",        label: "Cline" },
    "copilot":      { color: "var(--tool-copilot)",      label: "Copilot" },
    "cowork":       { color: "var(--tool-cowork)",       label: "Cowork" },
    "antigravity":  { color: "var(--tool-antigravity)",  label: "Antigravity" },
    "opencode":     { color: "var(--tool-opencode)",     label: "OpenCode" },
    "openclaw":     { color: "var(--tool-openclaw)",     label: "OpenClaw" },
    "pi":           { color: "var(--tool-pi)",           label: "Pi" },
    "gemini":       { color: "var(--tool-gemini)",       label: "Gemini CLI" },
  };

  // ---- KPIs (overview) ----
  const STATUS = {
    sessions: 491,
    actions: 78158,
    api_turns_proxy: 49,
    token_rows_jsonl: 71218,
    failures_24h: 7,
    last_activity: "2026-05-15T17:55:13Z",
    refresh_time: "2026-05-15T23:26:17Z",
    db_path: "/home/marmutapp/.observer/observer.db",
    schema: "v18",
    db_size: "338.3 MB",
    watcher_active: true,
    proxy_active: true,
  };

  // ---- 30 days of cost timeseries ----
  // 4 buckets per day: net_input, cache_read, cache_write, output (in millions of tokens)
  function dailyCost() {
    const days = 31;
    const seed = [25, 430, 100, 90, 360, 630, 620, 540, 730, 460, 470, 180, 145, 220, 410, 450, 195, 145, 175, 130, 25, 125, 350, 245, 130, 150, 205, 130, 65, 15, 265];
    return seed.map((v, i) => {
      const d = new Date(2026, 3, 15 + i);
      return {
        date: d.toISOString().slice(0,10),
        net_input: v * 0.018,
        cache_read: v * 0.97,
        cache_write: v * 0.015,
        output: v * 0.003,
        total_tokens_m: v,
        cost_usd: v * 0.32 + (i === 0 ? 5 : 0),
        actions: Math.round(v * 4 + Math.random() * 200),
        failures: Math.round(Math.random() * 4),
      };
    });
  }
  const COST_TS = dailyCost();
  const TOTAL_COST = COST_TS.reduce((s, d) => s + d.cost_usd, 0);

  // ---- Top models by tokens ----
  const TOP_MODELS = [
    { model: "claude-opus-4-7",            net: 152.3e6, read: 7.55e9,  write: 116.41e6, output: 25.23e6, reasoning: 0,    cost: 5561.0974, turns: 28310, source: "mixed",  reliability: "unreliable" },
    { model: "claude-opus-4-6",            net: 11.5e6,  read: 520.10e6, write: 9.98e6,  output: 2.68e6,  reasoning: 0,    cost: 425.5221,  turns: 2932,  source: "jsonl",  reliability: "unreliable" },
    { model: "gpt-5.5",                    net: 3.12e6,  read: 2.28e6,   write: 0,       output: 41.7e3,  reasoning: 4.7e3, cost: 18.2721,  turns: 117,   source: "jsonl",  reliability: "approximate" },
    { model: "claude-haiku-4-5-20251001",  net: 953.9e3, read: 58.38e6,  write: 4.39e6,  output: 388.0e3, reasoning: 0,    cost: 14.6597,   turns: 1494,  source: "mixed",  reliability: "unreliable" },
    { model: "gemini-3.1-pro-high",        net: 148.3e3, read: 8.83e6,   write: 1.87e6,  output: 82.9e3,  reasoning: 0,    cost: 3.0575,    turns: 143,   source: "jsonl",  reliability: "approximate" },
    { model: "claude-sonnet-4-6",          net: 58.0e3,  read: 1.61e6,   write: 408.0e3, output: 27.3e3,  reasoning: 0,    cost: 2.5951,    turns: 56,    source: "jsonl",  reliability: "approximate" },
    { model: "gpt-5.4",                    net: 604.2e3, read: 453.2e3,  write: 0,       output: 5.2e3,   reasoning: 655,  cost: 1.7117,    turns: 31,    source: "jsonl",  reliability: "approximate" },
    { model: "gemini-pro-agent",           net: 6.1e3,   read: 179.6e3,  write: 87.3e3,  output: 5.8e3,   reasoning: 0,    cost: 0.1172,    turns: 6,     source: "jsonl",  reliability: "approximate" },
    { model: "gemini-3.1-pro-low",         net: 5.2e3,   read: 134.6e3,  write: 85.1e3,  output: 3.8e3,   reasoning: 0,    cost: 0.0834,    turns: 5,     source: "jsonl",  reliability: "approximate" },
    { model: "gemini-3-flash-agent",       net: 9.8e3,   read: 449.7e3,  write: 114.7e3, output: 4.0e3,   reasoning: 0,    cost: 0.0394,    turns: 9,     source: "jsonl",  reliability: "approximate" },
    { model: "grok-code-fast-1",           net: 0,       read: 0,         write: 0,       output: 137,     reasoning: 0,   cost: 0.0002,    turns: 1,     source: "jsonl",  reliability: "approximate" },
    { model: "default",                    net: 181.7e3, read: 137.0e3,  write: 0,       output: 3.8e3,   reasoning: 0,    cost: 0,         turns: 5,     source: "jsonl",  reliability: "accurate" },
    { model: "big-pickle",                 net: 30.8e3,  read: 5.5e3,    write: 0,       output: 363,     reasoning: 0,    cost: 0,         turns: 3,     source: "jsonl",  reliability: "approximate" },
  ];

  // ---- Top tools by actions ----
  const TOOLS_AGG = [
    { tool: "claude-code", actions: 33300, failures: 714, success: 0.979, sessions: 238, first: "2026-04-15 21:39:11", last: "2026-05-15 17:56:35" },
    { tool: "codex",       actions: 724,   failures: 0,   success: 1.000, sessions: 25,  first: "2026-05-08 05:08:39", last: "2026-05-15 12:52:20" },
    { tool: "antigravity", actions: 562,   failures: 27,  success: 0.952, sessions: 26,  first: "2026-04-16 06:00:37", last: "2026-05-13 16:03:58" },
    { tool: "cowork",      actions: 351,   failures: 13,  success: 0.963, sessions: 2,   first: "2026-05-14 09:47:43", last: "2026-05-15 06:02:29" },
    { tool: "cursor",      actions: 44,    failures: 0,   success: 1.000, sessions: 5,   first: "2026-05-08 10:47:24", last: "2026-05-08 20:30:07" },
    { tool: "copilot",     actions: 16,    failures: 3,   success: 0.813, sessions: 2,   first: "2026-05-02 11:08:10", last: "2026-05-02 13:11:53" },
    { tool: "opencode",    actions: 7,     failures: 0,   success: 1.000, sessions: 2,   first: "2026-04-29 04:41:57", last: "2026-04-29 04:42:55" },
  ];

  // ---- Sessions ----
  const SESSIONS = [
    { id: "90968aa4-99fa-4ce1-bd47-001", tool: "claude-code", project: "marmutapp/superbased-observer", started: "2026-05-15 17:08:25", elapsed: "48m 10s", actions: 259, subagent: 2, input: 4100, cache_r: 19.56e6, cache_w: 347.8e3, output: 78.8e3, api: 15.2493, tool_cost: 0, total: 15.2493 },
    { id: "019e2bb0-6045-7c2e-ba39-002", tool: "codex",       project: "2026-05-15/can-you-tell-me-who-the", started: "2026-05-15 12:50:47", elapsed: "1m 32s", actions: 29, subagent: 0, input: 71100, cache_r: 14.7e3, cache_w: 0, output: 2.7e3, api: 0.4901, tool_cost: 0.09, total: 0.5801 },
    { id: "26d04ba5-56e1-7c1f-bc48-003", tool: "claude-code", project: "marmutapp/superbased-observer", started: "2026-05-15 12:49:23", elapsed: "4h 17m", actions: 1290, subagent: 83, input: 628, cache_r: 126.72e6, cache_w: 863.0e3, output: 244.5e3, api: 76.8373, tool_cost: 0, total: 76.8373 },
    { id: "0a29b98c-8775-7c45-c194-004", tool: "claude-code", project: "marmutapp/superbased-observer", started: "2026-05-15 10:58:33", elapsed: "1h 49m", actions: 402, subagent: 3, input: 8700, cache_r: 30.73e6, cache_w: 693.1e3, output: 218.2e3, api: 27.7941, tool_cost: 0, total: 27.7941 },
    { id: "local-5afdac34-7c95-8121-005", tool: "cowork",     project: "sessions/gifted-kind-euler", started: "2026-05-15 05:21:55", elapsed: "19m 47s", actions: 53, subagent: 13, input: 191100, cache_r: 789.3e3, cache_w: 60.2e3, output: 19.6e3, api: 1.2904, tool_cost: 0.11, total: 1.4004, cowork_proc: "jolly-tender-wozniak", cowork_title: "Refactor compression pipeline", host_mode: "auto" },
    { id: "local-7b03e0aa-8431-7f33-006", tool: "cowork",     project: "programsx/superbased-observer", started: "2026-05-14 09:47:43", elapsed: "20h 14m", actions: 298, subagent: 35, input: 523400, cache_r: 15.77e6, cache_w: 628.1e3, output: 130.3e3, api: 17.6346, tool_cost: 0.31, total: 17.9446, cowork_proc: "bold-rocket-knuth", cowork_title: "Dashboard redesign sprint", host_mode: "auto" },
    { id: "907f4a49-33a8-7c52-bd44-007", tool: "claude-code", project: "marmutapp/superbased-observer", started: "2026-05-14 09:40:14", elapsed: "25h 17m", actions: 694, subagent: 9, input: 1200, cache_r: 82.84e6, cache_w: 698.2e3, output: 300.2e3, api: 55.9145, tool_cost: 0, total: 55.9145 },
    { id: "019e22f3-8d76-7c41-b834-008", tool: "codex",       project: "programsx/superbased-observer", started: "2026-05-13 20:07:31", elapsed: "19s", actions: 18, subagent: 0, input: 65500, cache_r: 47.7e3, cache_w: 0, output: 865, api: 0.1905, tool_cost: 0, total: 0.1905 },
    { id: "019e22b1-84a8-7c54-c812-009", tool: "codex",       project: "programsx/superbased-observer", started: "2026-05-13 18:55:25", elapsed: "33m 56s", actions: 24, subagent: 0, input: 160500, cache_r: 112.4e3, cache_w: 0, output: 1.5e3, api: 0.4541, tool_cost: 0, total: 0.4541 },
    { id: "9ac6acfc-cd87-7c33-bd12-010", tool: "claude-code", project: "marmutapp/superbased-observer", started: "2026-05-13 16:19:50", elapsed: "16h 29m", actions: 625, subagent: 10, input: 3000, cache_r: 52.99e6, cache_w: 1.01e6, output: 216.1e3, api: 42.0463, tool_cost: 0, total: 42.0463 },
    { id: "462469ee-88aa-7c41-c019-011", tool: "antigravity", project: "marmutapp/superbased-observer", started: "2026-05-13 16:03:39", elapsed: "19s", actions: 7, subagent: 0, input: 4100, cache_r: 69.0e3, cache_w: 27.8e3, output: 1.4e3, api: 0.1582, tool_cost: 0, total: 0.1582 },
    { id: "dee8ab55-712f-7c45-b832-012", tool: "claude-code", project: "marmutapp/superbased", started: "2026-05-13 07:53:56", elapsed: "0ms", actions: 1, subagent: 0, input: 0, cache_r: 0, cache_w: 0, output: 0, api: 0, tool_cost: 0, total: 0 },
    { id: "162c4ab9-6593-7c12-bc44-013", tool: "antigravity", project: "marmutapp/superbased-observer", started: "2026-05-12 22:00:29", elapsed: "50s", actions: 16, subagent: 0, input: 5200, cache_r: 134.6e3, cache_w: 85.1e3, output: 3.8e3, api: 0.0834, tool_cost: 0, total: 0.0834 },
    { id: "ae703782-85e1-7c43-bd23-014", tool: "antigravity", project: "marmutapp/superbased-observer", started: "2026-05-12 21:58:26", elapsed: "36s", actions: 16, subagent: 0, input: 9800, cache_r: 449.7e3, cache_w: 114.7e3, output: 4.0e3, api: 0.0394, tool_cost: 0, total: 0.0394 },
    { id: "0303c07b-4d31-7c33-b923-015", tool: "claude-code", project: "marmutapp/superbased-observer", started: "2026-05-12 21:50:56", elapsed: "0ms", actions: 1, subagent: 0, input: 0, cache_r: 0, cache_w: 0, output: 0, api: 0, tool_cost: 0, total: 0 },
    { id: "da973a42-8af9-7c41-bd55-016", tool: "antigravity", project: "marmutapp/superbased-observer", started: "2026-05-12 21:24:43", elapsed: "1m 5s", actions: 16, subagent: 0, input: 6100, cache_r: 179.6e3, cache_w: 87.3e3, output: 5.8e3, api: 0.1172, tool_cost: 0, total: 0.1172 },
    { id: "fc8d44e5-f635-7c34-bd66-017", tool: "antigravity", project: "marmutapp/superbased-observer", started: "2026-05-12 21:20:38", elapsed: "2m 18s", actions: 22, subagent: 0, input: 9300, cache_r: 432.5e3, cache_w: 127.3e3, output: 5.4e3, api: 0.7165, tool_cost: 0, total: 0.7165 },
    { id: "1a5c8b9d-2e7f-7c81-c911-018", tool: "cursor",      project: "marmutapp/superbased",          started: "2026-05-08 20:30:07", elapsed: "12m 4s", actions: 18, subagent: 0, input: 12300, cache_r: 0, cache_w: 0, output: 4.2e3, api: 0.2814, tool_cost: 0, total: 0.2814 },
    { id: "2b6d9caf-3f80-7d12-c022-019", tool: "copilot",     project: "off/repo",                       started: "2026-05-02 13:11:53", elapsed: "8m 22s", actions: 8, subagent: 0, input: 8200, cache_r: 0, cache_w: 0, output: 1.8e3, api: 0.0612, tool_cost: 0, total: 0.0612 },
    { id: "3c7eadb0-4091-7e23-c133-020", tool: "opencode",    project: "marmutapp/npos",                 started: "2026-04-29 04:42:55", elapsed: "1m 2s", actions: 4, subagent: 0, input: 3100, cache_r: 0, cache_w: 0, output: 1.1e3, api: 0.0123, tool_cost: 0, total: 0.0123 },
  ];

  // ---- Actions (firehose sample) ----
  const ACTIONS = [
    { when: "2026-05-15 17:56:35", tool: "claude-code", type: "subagent_stop", effort: "xhigh", target: "a47795107fea68b3d", msg: "Goal was the antigravity reasoning_tokens dig plus follow-up validation across 4 backfill modes; subagent returned summary of findings.", session: "90968aa4...", msg_id: null, ok: true },
    { when: "2026-05-15 17:55:13", tool: "claude-code", type: "config_change", effort: "xhigh", target: "/home/marmutapp/.claude/settings.json", msg: "Updated settings.json — auto-approve flag toggled for /tmp paths", session: "90968aa4...", msg_id: null, ok: true },
    { when: "2026-05-15 17:53:29", tool: "claude-code", type: "subagent_stop", effort: "xhigh", target: "a43861abd1453cecd", msg: "commit this and ship v1.4.53", session: "90968aa4...", msg_id: null, ok: true },
    { when: "2026-05-15 17:53:27", tool: "claude-code", type: "task_complete", effort: "xhigh", target: "claudecode.assistant_text", msg: "All four workstreams done. Summary of this session: **1. Antigravity reasoning_tokens** — confirmed parity with Anthropic billing.", session: "90968aa4...", msg_id: "msg_01Vr6JsZ...", ok: true },
    { when: "2026-05-15 17:53:10", tool: "claude-code", type: "run_command",   effort: "xhigh", target: "Bash", msg: 'git status --short | wc -l && echo "---untracked docs---" git status -s | grep "^?? docs/"', session: "90968aa4...", msg_id: "msg_01EEpXhu...", ok: true },
    { when: "2026-05-15 17:53:04", tool: "claude-code", type: "edit_file",     effort: "xhigh", target: "[external]//home/marmutapp/.claude/projects/-home-marmutapp/superbased-observer/internal/intelligence/dashboard.go", msg: "Replaced rendering of the Compression help banner block with structured headline; preserved data hooks.", session: "90968aa4...", msg_id: "msg_013q7qir...", ok: true },
    { when: "2026-05-15 17:52:54", tool: "claude-code", type: "read_file",     effort: "xhigh", target: "[external]//home/marmutapp/.claude/projects/-home-marmutapp/superbased-observer/internal/proxy/proxy.go", msg: "Read for cross-reference with stash-and-retrieve handlers", session: "90968aa4...", msg_id: "msg_01RpGQor...", ok: true },
    { when: "2026-05-15 17:52:48", tool: "claude-code", type: "write_file",    effort: "xhigh", target: "[external]//home/marmutapp/.claude/projects/-home-marmutapp/superbased-observer/internal/intelligence/dashboard/static/index.html", msg: "Wrote new index.html (replaced)", session: "90968aa4...", msg_id: "msg_015LXuoQ...", ok: true },
    { when: "2026-05-15 17:52:18", tool: "claude-code", type: "task_complete", effort: "xhigh", target: "claudecode.assistant_text", msg: "Tree clean. Let me capture the two key empirical findings as patterns and ship.", session: "90968aa4...", msg_id: "msg_01GK6M37...", ok: true },
    { when: "2026-05-15 17:51:42", tool: "claude-code", type: "search_text",   effort: "high",  target: "Grep", msg: 'rg "ANTHROPIC_BASE_URL" --type go -l', session: "90968aa4...", msg_id: null, ok: true },
    { when: "2026-05-15 17:48:01", tool: "claude-code", type: "tool_failure",  effort: "high",  target: "Bash", msg: 'go test -race ./internal/compression/... — exit 1 — FAIL: TestStashRetrieve (concurrent map writes)', session: "26d04ba5...", msg_id: null, ok: false, error: "exit 1" },
    { when: "2026-05-15 17:47:55", tool: "claude-code", type: "run_command",   effort: "high",  target: "Bash", msg: 'go test -race ./internal/compression/...', session: "26d04ba5...", msg_id: null, ok: true },
    { when: "2026-05-15 17:44:12", tool: "codex",       type: "user_prompt",   effort: "medium", target: "user_prompt", msg: "Tell me who the top user is in the model cost table — and how that concentration changed", session: "019e2bb0...", msg_id: null, ok: true },
    { when: "2026-05-15 17:42:08", tool: "codex",       type: "web_search",    effort: "medium", target: "web", msg: "site:docs.anthropic.com prompt caching cache_creation_input_tokens behavior", session: "019e2bb0...", msg_id: null, ok: true },
    { when: "2026-05-15 17:38:51", tool: "antigravity", type: "mcp_call",      effort: "high",  target: "get_file_history", msg: 'mcp.tool=get_file_history file="internal/intelligence/dashboard.go" since="2026-05-13"', session: "462469ee...", msg_id: null, ok: true },
    { when: "2026-05-15 17:35:14", tool: "claude-code", type: "spawn_subagent",effort: "xhigh", target: "subagent.id=s_4729", msg: 'Spawned: "audit cowork reasoning cost reconciliation — find drift > 5%"', session: "26d04ba5...", msg_id: null, ok: true },
    { when: "2026-05-15 17:30:00", tool: "cowork",      type: "user_prompt",   effort: "high",  target: "host.user_prompt", msg: 'Refactor the compression pipeline so json-erasure runs after diff-stripper, not before', session: "local-5af...", msg_id: null, ok: true },
    { when: "2026-05-15 17:28:42", tool: "claude-code", type: "todo_update",   effort: "high",  target: "todos", msg: '6 tasks: [x] retrieve rate calc, [x] D23 inject schema, [ ] D20 rolling-summ ledger, [ ] cowork drift table, [ ] docs/INTELLIGENCE.md, [ ] PROGRESS.md', session: "907f4a49...", msg_id: null, ok: true },
    { when: "2026-05-15 17:25:09", tool: "claude-code", type: "permission_request", effort: "high", target: "Bash", msg: 'sudo systemctl restart observer-daemon — requesting permission_mode=acceptEdits override', session: "26d04ba5...", msg_id: null, ok: true },
    { when: "2026-05-15 17:22:46", tool: "claude-code", type: "permission_denied",  effort: "high", target: "WebFetch", msg: 'WebFetch denied by allowlist policy (host: scrape.anthropic-internal.com)', session: "26d04ba5...", msg_id: null, ok: false },
    { when: "2026-05-15 17:18:33", tool: "cursor",      type: "edit_file",     effort: "medium", target: "src/App.tsx", msg: "Inline edit: replace useState pattern with reducer", session: "1a5c8b9d...", msg_id: null, ok: true },
  ];

  // ---- Compression ----
  const COMPRESSION = {
    tokens_saved: 44900,
    dollars_saved: 0.2193,
    bytes_saved: 180000,  // 179.6 KB
    bytes_before: 7430000,
    bytes_after: 7250000,
    turns_compressed: 27,
    results_count: 147,
    dropped: 83,
    markers: 46,
    actions_compressed: 45100,
    proxy_active: true,
    codex_jsonl_only: true,
    setup_claude: { status: "oauth_ready", proxy: "127.0.0.1:8820", binary: "/home/marmutapp/.local/bin/claude" },
    setup_codex:  { status: "not_configured", auth: "chatgpt", proxy: "127.0.0.1:8820" },
  };

  const COMP_DAILY = [
    { date: "2026-04-23", tokens_saved: 50,    dollars: 0.0001,  json: 0, code: 0, logs: 0, text: 100,  diff: 0, html: 0, drop: 0,    stash: 0 },
    { date: "2026-04-24", tokens_saved: 12000, dollars: 0.060,   json: 0, code: 0, logs: 0, text: 5500, diff: 0, html: 0, drop: 6500, stash: 0 },
    { date: "2026-04-28", tokens_saved: 6500,  dollars: 0.032,   json: 0, code: 0, logs: 0, text: 2800, diff: 0, html: 0, drop: 8200, stash: 0 },
    { date: "2026-04-29", tokens_saved: 26400, dollars: 0.1270,  json: 0, code: 0, logs: 0, text: 6800, diff: 0, html: 0, drop: 22500, stash: 0 },
  ];

  const COMP_PER_MODEL = [
    { model: "claude-opus-4-7",           tokens: 43600, dollars: 0.2180, bytes: 174400, save_pct: 0.031, turns: 13, tool_results: 18, dropped: 74, markers: 37 },
    { model: "claude-haiku-4-5-20251001", tokens: 1300,  dollars: 0.0013, bytes: 5300,   save_pct: 0.003, turns: 14, tool_results: 0,  dropped: 9,  markers: 9 },
  ];

  const COMP_EVENTS = [
    { when: "2026-04-29 06:12:23", mech: "drop", source: "main", model: "claude-opus-4-7", saved_b: 38,    saved_tok: 9,    dollars: 0.0000, save_pct: 1.000, msg_idx: 38, session: "b9bd459d...", msg_id: "msg_01UJ39XW...", importance: 0.534 },
    { when: "2026-04-29 06:12:23", mech: "drop", source: "main", model: "claude-opus-4-7", saved_b: 8100,  saved_tok: 2000, dollars: 0.0101, save_pct: 1.000, msg_idx: 37, session: "b9bd459d...", msg_id: "msg_01UJ39XW...", importance: 0.499 },
    { when: "2026-04-29 06:12:23", mech: "drop", source: "main", model: "claude-opus-4-7", saved_b: 789,   saved_tok: 197,  dollars: 0.0010, save_pct: 1.000, msg_idx: 30, session: "b9bd459d...", msg_id: "msg_01UJ39XW...", importance: 0.475 },
    { when: "2026-04-29 06:12:23", mech: "drop", source: "main", model: "claude-opus-4-7", saved_b: 536,   saved_tok: 134,  dollars: 0.0007, save_pct: 1.000, msg_idx: 28, session: "b9bd459d...", msg_id: "msg_01UJ39XW...", importance: 0.457 },
    { when: "2026-04-29 06:12:23", mech: "drop", source: "main", model: "claude-opus-4-7", saved_b: 80,    saved_tok: 20,   dollars: 0.0001, save_pct: 1.000, msg_idx: 29, session: "b9bd459d...", msg_id: "msg_01UJ39XW...", importance: 0.455 },
    { when: "2026-04-29 06:12:23", mech: "drop", source: "main", model: "claude-opus-4-7", saved_b: 86,    saved_tok: 21,   dollars: 0.0001, save_pct: 1.000, msg_idx: 26, session: "b9bd459d...", msg_id: "msg_01UJ39XW...", importance: 0.443 },
    { when: "2026-04-29 06:12:23", mech: "drop", source: "main", model: "claude-opus-4-7", saved_b: 80,    saved_tok: 20,   dollars: 0.0001, save_pct: 1.000, msg_idx: 27, session: "b9bd459d...", msg_id: "msg_01UJ39XW...", importance: 0.442 },
    { when: "2026-04-29 06:12:23", mech: "drop", source: "main", model: "claude-opus-4-7", saved_b: 1400,  saved_tok: 359,  dollars: 0.0018, save_pct: 1.000, msg_idx: 25, session: "b9bd459d...", msg_id: "msg_01UJ39XW...", importance: 0.419 },
    { when: "2026-04-29 06:12:23", mech: "drop", source: "main", model: "claude-opus-4-7", saved_b: 5500,  saved_tok: 1400, dollars: 0.0069, save_pct: 1.000, msg_idx: null, session: "b9bd459d...", msg_id: "msg_01UJ39XW...", importance: 0.262 },
    { when: "2026-04-29 06:12:23", mech: "text", source: "main", model: "claude-opus-4-7", saved_b: 3900,  saved_tok: 965,  dollars: 0.0048, save_pct: 0.413, msg_idx: null, session: "b9bd459d...", msg_id: "msg_01UJ39XW...", importance: null },
    { when: "2026-04-29 06:09:10", mech: "drop", source: "main", model: "claude-opus-4-7", saved_b: 38,    saved_tok: 9,    dollars: 0.0000, save_pct: 1.000, msg_idx: 38, session: "b9bd459d...", msg_id: "msg_01Df9Uqs...", importance: 0.544 },
    { when: "2026-04-29 06:09:10", mech: "drop", source: "main", model: "claude-opus-4-7", saved_b: 8100,  saved_tok: 2000, dollars: 0.0101, save_pct: 1.000, msg_idx: 37, session: "b9bd459d...", msg_id: "msg_01Df9Uqs...", importance: 0.508 },
    { when: "2026-04-29 06:09:10", mech: "drop", source: "main", model: "claude-opus-4-7", saved_b: 789,   saved_tok: 197,  dollars: 0.0010, save_pct: 1.000, msg_idx: 30, session: "b9bd459d...", msg_id: "msg_01Df9Uqs...", importance: 0.482 },
    { when: "2026-04-29 06:09:10", mech: "drop", source: "main", model: "claude-opus-4-7", saved_b: 536,   saved_tok: 134,  dollars: 0.0007, save_pct: 1.000, msg_idx: 28, session: "b9bd459d...", msg_id: "msg_01Df9Uqs...", importance: 0.464 },
    { when: "2026-04-29 06:09:10", mech: "drop", source: "main", model: "claude-opus-4-7", saved_b: 80,    saved_tok: 20,   dollars: 0.0001, save_pct: 1.000, msg_idx: 29, session: "b9bd459d...", msg_id: "msg_01Df9Uqs...", importance: 0.462 },
    { when: "2026-04-29 06:09:10", mech: "drop", source: "main", model: "claude-opus-4-7", saved_b: 86,    saved_tok: 21,   dollars: 0.0001, save_pct: 1.000, msg_idx: 26, session: "b9bd459d...", msg_id: "msg_01Df9Uqs...", importance: 0.449 },
  ];

  const CCR = {
    stashes: 0,
    retrieves: 1,
    retrieve_rate: null,
    search_hits: 179,
    top_shas: [{ sha: "b26f626caef7...", retrieves: 1 }],
    top_actions: [
      { id: "#1688916", hits: 10 }, { id: "#1688913", hits: 10 },
      { id: "#1687094", hits: 10 }, { id: "#1683187", hits: 10 },
      { id: "#1678557", hits: 10 }, { id: "#1705599", hits: 9 },
      { id: "#1689652", hits: 9 },  { id: "#1678092", hits: 8 },
      { id: "#1678086", hits: 7 },  { id: "#1706359", hits: 5 },
    ],
  };

  const COMPACTIONS = {
    count: 8, sessions_affected: 8, injections_fired: 0, inject_rate: 0,
    events: [
      { when: "2026-05-09T16:50:12", session: "7037e8c5...", tool: "claude-code", pre: 0, snapshot: 462, ghost: 0, injected: null },
      { when: "2026-05-07T20:55:24", session: "1d771ab6...", tool: "claude-code", pre: 0, snapshot: 11, ghost: 11, injected: null },
      { when: "2026-05-07T20:55:08", session: "0ec4d6db...", tool: "claude-code", pre: 0, snapshot: 13, ghost: 13, injected: null },
      { when: "2026-05-07T20:05:17", session: "c0206ee8...", tool: "claude-code", pre: 0, snapshot: 11, ghost: 11, injected: null },
      { when: "2026-05-07T19:36:03", session: "a09a6a3c...", tool: "claude-code", pre: 0, snapshot: 13, ghost: 13, injected: null },
      { when: "2026-05-07T19:13:34", session: "17915852...", tool: "claude-code", pre: 0, snapshot: 11, ghost: 11, injected: null },
      { when: "2026-05-07T19:13:29", session: "ba0f6b06...", tool: "claude-code", pre: 0, snapshot: 13, ghost: 13, injected: null },
      { when: "2026-04-29T04:54:01", session: "98214246...", tool: "claude-code", pre: 0, snapshot: 277, ghost: 277, injected: null },
    ],
  };

  const ROLLING = { summary_calls: 0, haiku_spend: 0, cache_savings: 0, net_delta: 0 };

  // ---- Analysis ----
  const ANALYSIS = {
    spend_window: 6027.1558, spend_prior: 3441.0968, spend_delta_pct: 75.2,
    mtd: 1803.3359, mtd_prior: 2473.1018, mtd_delta_pct: -27.1, projection_usd: 3726.8941, budget_usd: 5000,
    output_rate: 211.68, output_tokens_m: 28.47,
    cache_savings: 36408.3324, cache_read_b: 8.14e9,
    cache_efficacy: 0.984, cache_read_total_b: 8.14e9, cache_write_total_m: 133.35e6,
    high_ctx_100k: 26800, high_ctx_100k_cost: 5576.0382,
    high_ctx_200k: 19100, high_ctx_200k_cost: 4625.1946,
    per_turn_p95: 0.3520, per_turn_mean: 0.1821, total_turns: 33100,
    burn_rate: 15.8609, active_hours: 380,
    top_model: "claude-opus-4-7", top_model_cost: 5561.0974, top_model_concentration: 0.923,
    waste_usd: 4.9055, waste_tokens: 1.19e6, waste_rate_perm: 4.13,
  };

  const MOVERS = [
    { model: "claude-opus-4-6",       prior: 3164.7818, current: 425.5221, delta: -2739.2597, pct: -0.866 },
    { model: "gpt-5.4",                prior: 192.6857,  current: 1.7117,   delta: -190.9740,  pct: -0.991 },
    { model: "gpt-5.4-mini",           prior: 42.9451,   current: 0.0000,   delta: -42.9451,   pct: -1.000 },
    { model: "claude-haiku-4-5-20251001", prior: 32.5025, current: 14.6597, delta: -17.8428,   pct: -0.549 },
    { model: "claude-sonnet-4-6",      prior: 7.1983,    current: 2.5951,   delta: -4.6031,    pct: -0.639 },
    { model: "gemini-3.1-pro-high",    prior: 0.9834,    current: 3.0575,   delta: +2.0741,    pct: +2.109 },
  ];
  const NEW_THIS_PERIOD = [
    { model: "claude-opus-4-7",  current: 5561.0974 },
    { model: "gpt-5.5",           current: 18.2721 },
    { model: "gemini-pro-agent",  current: 0.1172 },
    { model: "gemini-3.1-pro-low", current: 0.0834 },
    { model: "gemini-3-flash-agent", current: 0.0394 },
  ];
  const TOP_EXPENSIVE = [
    { id: "9823d244...", tool: "claude-code", model: "claude-haiku-4-5-20251001", extra: 1, turns: 1170, cost: 378.8313, why: ["Opus","many turns","large prompt"] },
    { id: "6bbd2340...", tool: "claude-code", model: "<synthetic>",                extra: 1, turns: 1119, cost: 243.6438, why: ["Opus","many turns","large prompt"] },
    { id: "69a8e96b...", tool: "claude-code", model: "claude-opus-4-7",            extra: 0, turns: 964,  cost: 213.9464, why: ["Opus","many turns","large prompt"] },
    { id: "c178fb67...", tool: "claude-code", model: "claude-opus-4-7",            extra: 0, turns: 649,  cost: 191.0395, why: ["Opus","many turns","large prompt"] },
    { id: "e42de349...", tool: "claude-code", model: "claude-opus-4-7",            extra: 0, turns: 730,  cost: 179.1028, why: ["Opus","many turns","large prompt"] },
    { id: "56dc0497...", tool: "claude-code", model: "claude-haiku-4-5-20251001", extra: 1, turns: 616,  cost: 167.7933, why: ["Opus","many turns","large prompt"] },
    { id: "4a3900b1...", tool: "claude-code", model: "claude-haiku-4-5-20251001", extra: 2, turns: 1258, cost: 155.3773, why: ["Opus","many turns","large prompt"] },
    { id: "ea65f017...", tool: "claude-code", model: "claude-opus-4-7",            extra: 0, turns: 704,  cost: 144.0557, why: ["Opus","many turns","large prompt"] },
    { id: "2626c1f6...", tool: "claude-code", model: "<synthetic>",                extra: 2, turns: 848,  cost: 133.2332, why: ["Opus","many turns","large prompt"] },
    { id: "7d103bc5...", tool: "claude-code", model: "claude-haiku-4-5-20251001", extra: 1, turns: 613,  cost: 120.1253, why: ["Opus","many turns","large prompt"] },
  ];
  const ROUTING = [
    { id: "48565af3...", from: "claude-opus-4-7", to: "claude-sonnet-4-6", current: 0.1722, suggested: 0.1033, savings: 0.0689, reasons: ["small prompt","low output","no LC tier","single-model session"] },
  ];
  // 24-hour cost array (heatmap source)
  const HOURLY = [
    72,38,75,180,260,270,300,350,390,440,300,355,285,190,185,250,335,360,265,380,395,300,30,0
  ];

  // ---- Discovery ----
  const STALE_REREADS = {
    count: 2300,
    files: 249,
    cross_thread: 455,
    tokens_wasted: 1.19e6,
    dollars_wasted: 4.9055,
    blended_rate: 4.13,
    rows: [
      { file: "desktop/electron/modules/gui-automation.ts", reads: 423, stale: 396, cross: 12, wasted: 202800, project: "marmutapp/superbased" },
      { file: "internal/intelligence/dashboard/static/index.html", reads: 149, stale: 122, cross: 35, wasted: 62500, project: "marmutapp/superbased-observer" },
      { file: "internal/intelligence/dashboard/dashboard.go", reads: 122, stale: 87, cross: 18, wasted: 44500, project: "marmutapp/superbased-observer" },
      { file: "internal/adapter/codex/adapter.go", reads: 93, stale: 74, cross: 31, wasted: 37900, project: "marmutapp/superbased-observer" },
      { file: "desktop/electron/main.ts", reads: 73, stale: 63, cross: 44, wasted: 32300, project: "marmutapp/superbased" },
      { file: "desktop/electron/modules/api-server.ts", reads: 91, stale: 62, cross: 5, wasted: 31700, project: "marmutapp/superbased" },
      { file: "desktop/mcp-server/src/index.ts", reads: 86, stale: 61, cross: 16, wasted: 31200, project: "marmutapp/superbased" },
      { file: "cmd/observer/backfill.go", reads: 65, stale: 49, cross: 0, wasted: 25100, project: "marmutapp/superbased-observer" },
      { file: "internal/store/store.go", reads: 72, stale: 42, cross: 12, wasted: 21500, project: "marmutapp/superbased-observer" },
      { file: "desktop/SUPERBASED_SKILL.md", reads: 68, stale: 42, cross: 0, wasted: 21500, project: "marmutapp/superbased" },
      { file: "lib/application.js", reads: 58, stale: 40, cross: 2, wasted: 20500, project: "on/repo" },
      { file: "internal/intelligence/cost/summary.go", reads: 49, stale: 37, cross: 13, wasted: 18900, project: "marmutapp/superbased-observer" },
    ],
  };
  const REPEATED_CMDS = [
    { cmd: "go build ./... 2>&1 | head -10", runs: 74, no_change: 0, failed: 0, project: "marmutapp/superbased-observer" },
    { cmd: "git status --short", runs: 24, no_change: 1, failed: 1, project: "marmutapp/superbased" },
    { cmd: "git status --short", runs: 23, no_change: 1, failed: 0, project: "marmutapp/superbased-observer" },
    { cmd: "git -C /home/marmutapp/superbased status --short", runs: 21, no_change: 2, failed: 0, project: "marmutapp/superbased" },
    { cmd: "cd /home/marmutapp/superbased && git status --short", runs: 20, no_change: 0, failed: 0, project: "marmutapp/superbased" },
    { cmd: "go build ./... 2>&1 | head -20", runs: 20, no_change: 0, failed: 0, project: "marmutapp/superbased-observer" },
    { cmd: 'go test -race ./... 2>&1 | grep -cE "^FAIL"', runs: 19, no_change: 0, failed: 0, project: "marmutapp/superbased-observer" },
    { cmd: "npm run size:check 2>&1 | tail -10", runs: 18, no_change: 0, failed: 0, project: "marmutapp/superbased" },
    { cmd: "go build ./... 2>&1 | tail -10", runs: 17, no_change: 0, failed: 0, project: "marmutapp/superbased-observer" },
    { cmd: "ls -la /tmp/ab-claude/off/repo", runs: 17, no_change: 0, failed: 0, project: "off/repo" },
  ];

  // ---- Patterns ----
  const PATTERNS = [
    { type: "hot_file",          project: "marmutapp/superbased-observer", confidence: 1.00, observations: 69, rule: "file_path=PROGRESS.md reads=15 edits=52 writes=2 total_touch=69" },
    { type: "co_change",         project: "marmutapp/superbased-observer", confidence: 1.00, observations: 5,  rule: "file_a=PROGRESS.md file_b=cmd/superbased/main.go pair_count=5" },
    { type: "co_change",         project: "marmutapp/superbased-observer", confidence: 1.00, observations: 5,  rule: "file_a=PROGRESS.md file_b=cmd/superbased/hook.go pair_count=5" },
    { type: "common_command",    project: "marmutapp/superbased-observer", confidence: 1.00, observations: 11, rule: 'command=go build ./... 2>&1 command_hash=a75cc319d761cc611ea2eb07f5653e62523904ab... run_count=11 success_rate=0.818' },
    { type: "edit_test_pair",    project: "marmutapp/superbased-observer", confidence: 1.00, observations: 2,  rule: "edit_target=internal/hook/rewrite.go test_command=go test -race ./internal/hook/... 2>&1 pair_count=2" },
    { type: "edit_test_pair",    project: "marmutapp/superbased-observer", confidence: 1.00, observations: 2,  rule: "edit_target=internal/compression/conversation/compre... test_command=go test -race ./internal/compression/con... pair_count=2" },
    { type: "edit_test_pair",    project: "marmutapp/superbased-observer", confidence: 1.00, observations: 2,  rule: "edit_target=internal/compression/shell/stream_test.g... test_command=go test -race ./internal/compression/she... pair_count=2" },
    { type: "edit_test_pair",    project: "marmutapp/superbased-observer", confidence: 1.00, observations: 2,  rule: "edit_target=cmd/superbased/main.go test_command=go test -race ./... 2>&1 | tail -10 pair_count=2" },
    { type: "knowledge_snippet", project: "marmutapp/superbased-observer", confidence: 1.00, observations: 14, rule: "span=Now the failure-pattern correlation / 'oxygen-debt' heuristic landed and is being dogfooded against last week's flake corpus. source_count=14" },
    { type: "hot_file",          project: "marmutapp/superbased-observer", confidence: 0.99, observations: 68, rule: "file_path=docs/user-walkthrough.md reads=5 edits=62 writes=1 total_touch=68" },
    { type: "knowledge_snippet", project: "marmutapp/superbased-observer", confidence: 0.86, observations: 12, rule: 'span=Phase 1 is substantial — let me first verify the cache_tier backfill numbers before claiming a win. source_count=12' },
    { type: "knowledge_snippet", project: "marmutapp/superbased-observer", confidence: 0.86, observations: 12, rule: 'span=Mapping cost engine and existing HTTP search-results routes onto the proxy interceptor surface. source_count=12' },
    { type: "co_change",         project: "marmutapp/superbased-observer", confidence: 0.80, observations: 4,  rule: "file_a=PROGRESS.md file_b=internal/config/config.go pair_count=4" },
  ];

  // ---- Cowork reconciliation ----
  const COWORK = {
    sessions: 8, over_threshold: 8, cowork_total: 44.1662, derived_total: 45.7041,
    overall_drift: 1.5379, overall_drift_pct: 0.035, threshold_pct: 0.05,
    rows: [
      { title: "Create AI Accelerator VC pitch deck",        cowork: 15.5891, derived: 17.9446, drift: 2.3555, pct: 0.151 },
      { title: "Analyze Canadian Environmental Protection Act", cowork: 8.6869, derived: 6.3711, drift: -2.3158, pct: 0.267 },
      { title: "Fixed income fund performance review and outlook", cowork: 3.9164, derived: 4.6569, drift: 0.7406, pct: 0.189 },
      { title: "Indian Fixed Income Investor Newsletter",    cowork: 2.2028, derived: 1.5177, drift: -0.6851, pct: 0.311 },
      { title: "Analyze Carbon Pricing Act regulatory document", cowork: 5.7900, derived: 6.3185, drift: 0.5285, pct: 0.091 },
    ],
  };

  // ---- Projects ----
  const PROJECTS = [
    "marmutapp/superbased-observer", "marmutapp/superbased", "programsx/superbased-observer",
    "on/repo", "off/repo", "marmutapp/npos", "sessions/gifted-kind-euler"
  ];

  // ---- Action-type catalog ----
  const ACTION_TYPES = {
    read_file:          { color: "var(--act-file)",  cat: "file" },
    write_file:         { color: "var(--act-file)",  cat: "file" },
    edit_file:          { color: "var(--act-file)",  cat: "file" },
    run_command:        { color: "var(--act-cmd)",   cat: "cmd" },
    search_text:        { color: "var(--act-search)",cat: "search" },
    search_files:       { color: "var(--act-search)",cat: "search" },
    web_search:         { color: "var(--act-web)",   cat: "web" },
    web_fetch:          { color: "var(--act-web)",   cat: "web" },
    spawn_subagent:     { color: "var(--act-agent)", cat: "agent" },
    subagent_start:     { color: "var(--act-agent)", cat: "agent" },
    subagent_stop:      { color: "var(--act-agent)", cat: "agent" },
    todo_update:        { color: "var(--act-meta)",  cat: "meta" },
    ask_user:           { color: "var(--act-user)",  cat: "user" },
    mcp_call:           { color: "var(--act-mcp)",   cat: "mcp" },
    user_prompt:        { color: "var(--act-user)",  cat: "user" },
    user_prompt_expansion:{color: "var(--act-user)", cat: "user" },
    task_complete:      { color: "var(--act-meta)",  cat: "meta" },
    tool_failure:       { color: "var(--act-fail)",  cat: "fail" },
    permission_request: { color: "var(--act-meta)",  cat: "meta" },
    permission_denied:  { color: "var(--act-fail)",  cat: "fail" },
    post_tool_batch:    { color: "var(--act-meta)",  cat: "meta" },
    setup:              { color: "var(--act-meta)",  cat: "meta" },
    instructions_loaded:{ color: "var(--act-meta)",  cat: "meta" },
    config_change:      { color: "var(--act-meta)",  cat: "meta" },
    session_start:      { color: "var(--act-meta)",  cat: "meta" },
    session_end:        { color: "var(--act-meta)",  cat: "meta" },
    notification:       { color: "var(--act-meta)",  cat: "meta" },
    cwd_change:         { color: "var(--act-meta)",  cat: "meta" },
    api_error:          { color: "var(--act-fail)",  cat: "fail" },
  };

  // ---- Backfill modes ----
  const BACKFILL = [
    { mode: "is-sidechain",       flag: "--is-sidechain",        candidates: 0,    desc: "actions.is_sidechain from JSONL (Claude Code parent/sub-agent boundary)", status: "idle" },
    { mode: "cache-tier",         flag: "--cache-tier",          candidates: 13,   desc: "api_turns.cache_creation_1h_tokens from JSONL (since migration 008)", status: "idle" },
    { mode: "message-id",         flag: "--message-id",          candidates: 8300, desc: "actions + token_usage.message_id (claudecode + codex + cursor + opencode)", status: "idle" },
    { mode: "opencode-message-id",flag: "--opencode-message-id", candidates: null, desc: "opencode.db row IDs (assistant rows + parent message ids)", status: "idle" },
    { mode: "opencode-parts",     flag: "--opencode-parts",      candidates: null, desc: "opencode tool output excerpts from State.Output / Metadata.Output", status: "idle" },
    { mode: "opencode-tokens",    flag: "--opencode-tokens",     candidates: null, desc: "re-ingest opencode token_usage rows missed pre-fix", status: "idle" },
    { mode: "openclaw-action-types",flag:"--openclaw-action-types",candidates: null, desc: "spawn_subagent action_type for sessions_spawn rows", status: "idle" },
    { mode: "openclaw-model",     flag: "--openclaw-model",      candidates: null, desc: "sessions.model + workspace_dir from sessions.json aliases", status: "idle" },
    { mode: "openclaw-reasoning", flag: "--openclaw-reasoning",  candidates: null, desc: "preceding_reasoning from openclaw JSONL assistant text/thinking parts", status: "idle" },
    { mode: "codex-reasoning",    flag: "--codex-reasoning",     candidates: null, desc: "codex preceding_reasoning from agent_message events", status: "idle" },
    { mode: "cursor-model",       flag: "--cursor-model",        candidates: null, desc: "actions.model from cursor rawHookPayload.Model", status: "idle" },
    { mode: "copilot-message-id", flag: "--copilot-message-id",  candidates: null, desc: "actions.message_id from spanId / parentSpanId", status: "idle" },
  ];

  // ---- Models (defaults — 105 baked-in) ----
  const PRICING_DEFAULTS = [
    { model: "claude-opus-4-7", input: 15.00, cache_read: 1.50, cache_write: 18.75, output: 75.00 },
    { model: "claude-opus-4-6", input: 15.00, cache_read: 1.50, cache_write: 18.75, output: 75.00 },
    { model: "claude-sonnet-4-6", input: 3.00, cache_read: 0.30, cache_write: 3.75, output: 15.00 },
    { model: "claude-haiku-4-5-20251001", input: 1.00, cache_read: 0.10, cache_write: 1.25, output: 5.00 },
    { model: "gpt-5.5", input: 1.25, cache_read: 0.125, cache_write: 0, output: 10.00 },
    { model: "gemini-3.1-pro-high", input: 1.25, cache_read: 0.125, cache_write: 0, output: 5.00 },
    { model: "grok-code-fast-1", input: 0.20, cache_read: 0.02, cache_write: 0, output: 1.50 },
  ];

  // ---- Help registry (subset for the drawer) ----
  const HELP = [
    { id: "tab.overview",   cat: "Tabs",   title: "Overview tab",   oneline: "High-level snapshot — KPI tiles, daily cost & activity, top-N models and tools for the selected window." },
    { id: "tab.cost",       cat: "Tabs",   title: "Cost tab",       oneline: "Per-model token consumption split into the four billable buckets with computed dollar cost." },
    { id: "tab.analysis",   cat: "Tabs",   title: "Analysis tab",   oneline: "Spending insights — headline KPIs comparing this period to the prior period of equal length." },
    { id: "tab.sessions",   cat: "Tabs",   title: "Sessions tab",   oneline: "One row per AI-coding session. Click a row to see action breakdown, token buckets, cost, and recent actions." },
    { id: "tab.actions",    cat: "Tabs",   title: "Actions tab",    oneline: "The flat firehose — every recorded tool call, normalised across adapters. Filter by type below." },
    { id: "tab.tools",      cat: "Tabs",   title: "Tools tab",      oneline: "Per AI client — KPI tiles summarise activity in the selected window; charts show when each tool was active and what kind of work it did." },
    { id: "tab.compression",cat: "Tabs",   title: "Compression tab",oneline: "How many tokens and dollars the proxy saved by trimming conversations before forwarding upstream." },
    { id: "tab.discovery",  cat: "Tabs",   title: "Discovery tab",  oneline: "Stale re-reads, repeated commands, and cross-tool overlap — the waste detection page." },
    { id: "tab.patterns",   cat: "Tabs",   title: "Patterns tab",   oneline: "Repeatable behaviours the observer noticed across your sessions, ready to be written into CLAUDE.md / AGENTS.md / .cursorrules." },
    { id: "tab.settings",   cat: "Tabs",   title: "Settings tab",   oneline: "View and edit the live config.toml. Pricing is fully editable and hot-reloads." },
    { id: "tile.spend",     cat: "Tiles",  title: "Spend (window)", oneline: "Total cost over the selected window. Delta compares to the prior period of equal length." },
    { id: "tile.mtd",       cat: "Tiles",  title: "Month-to-date",  oneline: "MTD cost with vs-prior-month-same-day comparison, projection, and budget % (if configured)." },
    { id: "tile.output",    cat: "Tiles",  title: "$ / M output",   oneline: "Output-rate — the clean 'what are you paying per million output tokens' metric." },
    { id: "tile.cache",     cat: "Tiles",  title: "Cache savings",  oneline: "Counterfactual $ saved (cache_read priced at input rate vs cache_read rate)." },
    { id: "tile.cache_eff", cat: "Tiles",  title: "Cache efficacy", oneline: "cache_read / (cache_read + cache_write) — how well caching is working." },
    { id: "tile.high_ctx",  cat: "Tiles",  title: "High-context turns", oneline: "Turns over 100K/200K prompt tokens with attributed cost." },
    { id: "tile.per_turn",  cat: "Tiles",  title: "$ per Turn",     oneline: "Mean and p95 cost per API turn — variance signal." },
    { id: "tile.burn",      cat: "Tiles",  title: "Burn rate",      oneline: "cost_per_hour during active hours." },
    { id: "tile.top_model", cat: "Tiles",  title: "Top model",      oneline: "Highest-spend model + concentration % (concentration risk signal)." },
    { id: "tile.waste",     cat: "Tiles",  title: "Waste $",        oneline: "Estimated $ wasted on stale re-reads + repeated commands." },
    { id: "col.reliability",cat: "Columns",title: "Reliability",    oneline: "accurate / approximate / unreliable — how trustworthy a model's cost calculation is." },
    { id: "col.cache_r",    cat: "Columns",title: "Cache Read",     oneline: "Tokens served from prompt cache. Billed at ~10× discount vs net input." },
    { id: "col.cache_w",    cat: "Columns",title: "Cache Write",    oneline: "Tokens written to prompt cache. Billed at 1.25× input rate; pays off after ~2 reads." },
    { id: "calc.cache_eff", cat: "Calc",   title: "Cache efficacy", oneline: "cache_read ÷ (cache_read + cache_write). Above 80% means caching is paying for itself." },
    { id: "calc.burn",      cat: "Calc",   title: "Burn rate",      oneline: "Total cost ÷ active hours (UTC hour bins where at least one turn fired)." },
    { id: "calc.proj",      cat: "Calc",   title: "Projection",     oneline: "Linear extrapolation of MTD spend to end of month. Updates daily." },
    { id: "g.ccr",          cat: "Glossary",title: "CCR retrieve rate",oneline: "Stash-and-retrieve: when the proxy disk-offloads a large tool_result, does the model later actually retrieve it?" },
    { id: "g.compaction",   cat: "Glossary",title: "Compaction",    oneline: "Claude Code's /compact command. D23 records the synthetic recovery context the proxy injects post-compact." },
    { id: "g.rolling",      cat: "Glossary",title: "Rolling-summ",  oneline: "When the conversation grows past Anthropic's cache window, the proxy summarises older messages via Haiku and replaces them inline." },
    { id: "g.api_turn",     cat: "Glossary",title: "API turn",      oneline: "One round-trip request to the model. A session typically has hundreds of turns." },
    { id: "filter.window",  cat: "Filters",title: "Window",         oneline: "Time range for all data on the active tab. Switching the window re-fires the active tab's API calls." },
    { id: "filter.tool",    cat: "Filters",title: "Tool",           oneline: "Restrict to one AI client (claude-code, codex, cursor, …). 'All' keeps everything." },
    { id: "filter.project", cat: "Filters",title: "Project",        oneline: "Restrict to one project (file path of the working directory)." },
    { id: "metric.savings", cat: "Metrics",title: "Tokens saved",   oneline: "bytes_saved ÷ 4 (Claude tokenizer ratio). Dollars = tokens × the row's model input rate." },
    { id: "metric.bytes",   cat: "Metrics",title: "Bytes saved",    oneline: "Actual measured bytes — the source of truth. Tokens and dollars are derived from bytes." },
  ];

  window.OBS = {
    TOOLS, ACTION_TYPES, STATUS,
    COST_TS, TOTAL_COST, TOP_MODELS, TOOLS_AGG,
    SESSIONS, ACTIONS,
    COMPRESSION, COMP_DAILY, COMP_PER_MODEL, COMP_EVENTS, CCR, COMPACTIONS, ROLLING,
    ANALYSIS, MOVERS, NEW_THIS_PERIOD, TOP_EXPENSIVE, ROUTING, HOURLY,
    STALE_REREADS, REPEATED_CMDS, PATTERNS,
    COWORK, PROJECTS, BACKFILL, PRICING_DEFAULTS, HELP,
    fmt: {
      n(v, d=0) { if (v == null) return "—"; if (Math.abs(v) >= 1e9) return (v/1e9).toFixed(d||2)+"B"; if (Math.abs(v) >= 1e6) return (v/1e6).toFixed(d||2)+"M"; if (Math.abs(v) >= 1e3) return (v/1e3).toFixed(d||1)+"k"; return v.toLocaleString("en-US"); },
      n1(v) { if (v == null) return "—"; return v.toLocaleString("en-US"); },
      $(v, d=4) { if (v == null) return "—"; return "$" + Number(v).toLocaleString("en-US", { minimumFractionDigits: d, maximumFractionDigits: d }); },
      $2(v) { return window.OBS.fmt.$(v, 2); },
      pct(v, d=1) { if (v == null) return "—"; return (v*100).toFixed(d) + "%"; },
      bytes(b) { if (b == null) return "—"; if (b >= 1e6) return (b/1e6).toFixed(2) + " MB"; if (b >= 1e3) return (b/1e3).toFixed(1) + " KB"; return b + " B"; },
    },
  };
})();
