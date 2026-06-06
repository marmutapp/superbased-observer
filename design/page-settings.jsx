/* ============================================================
   Settings — left rail + pricing editor + backfill + read-only sections
   ============================================================ */
function PageSettings() {
  const O = window.OBS;
  const [section, setSection] = React.useState("pricing");
  const [helpOpen, setHelpOpen] = React.useState(true);

  const sections = [
    { id: "pricing",      label: "Pricing",      icon: "cost",        hot: true,  helpFor: "Per-model pricing overrides. Hot-reloads — no restart needed." },
    { id: "backfill",     label: "Backfill",     icon: "refresh",                  helpFor: "Recovery + maintenance for historical data." },
    { id: "observer",     label: "Observer",     icon: "eye",                      helpFor: "Top-level observer config — paths and feature flags." },
    { id: "watcher",      label: "Watcher",      icon: "pulse",                    helpFor: "JSONL/log file watcher. Read-only this PR; restart-on-save in next iteration." },
    { id: "freshness",    label: "Freshness",    icon: "shield",                   helpFor: "File-content hash engine for stale-read detection." },
    { id: "retention",    label: "Retention",    icon: "database",                 helpFor: "How long action/token rows stay in the DB before vacuum." },
    { id: "hooks",        label: "Hooks",        icon: "command",                  helpFor: "Claude Code / Codex / etc hook config — what fires on which events." },
    { id: "proxy",        label: "Proxy",        icon: "network",                  helpFor: "API proxy listener port, upstream URLs, OAuth handling." },
    { id: "compression",  label: "Compression",  icon: "compression",              helpFor: "Compression mechanism thresholds and budgets." },
    { id: "intelligence", label: "Intelligence", icon: "ai",          restart: true, helpFor: "Derived patterns, summaries, cost engine, code-graph integration." },
    { id: "antigravity",  label: "Antigravity",  icon: "zap",                      helpFor: "Antigravity-specific adapter config." },
  ];

  const current = sections.find(s => s.id === section);

  return (
    <div>
      <div className="page-head">
        <div>
          <h1 className="page-title">Settings</h1>
          <p className="page-desc">
            View and edit the live <code>config.toml</code>. Pricing is fully editable and hot-reloads — saves write the file (with a <code>.bak</code> of the prior version) and refresh the cost engine in place. Other sections render read-only this PR; restart-on-save plumbing arrives next iteration.
          </p>
        </div>
        <div className="spacer" />
        <button className="btn ghost" onClick={() => setHelpOpen(!helpOpen)}>
          <Icon name="help" size={13}/>
          {helpOpen ? "Hide help" : "Show help"}
        </button>
      </div>

      <div className="grid g-12" style={{ gap: 12 }}>
        {/* === Left rail === */}
        <div className="col-2">
          <div className="card" style={{ padding: 6 }}>
            {sections.map(s => (
              <div key={s.id}
                className={"nav-item" + (section === s.id ? " active" : "")}
                style={{ borderLeft: section === s.id ? "2px solid var(--accent)" : "2px solid transparent", borderRadius: 4, marginLeft: 0, paddingLeft: 10 }}
                onClick={() => setSection(s.id)}>
                <Icon name={s.icon} size={14} />
                <span>{s.label}</span>
                {s.hot && <span className="pill success" style={{ marginLeft: "auto", fontSize: 9 }}>hot</span>}
                {s.restart && <span className="pill warn" style={{ marginLeft: "auto", fontSize: 9 }}>restart</span>}
              </div>
            ))}
          </div>
        </div>

        {/* === Center === */}
        <div className={helpOpen ? "col-7" : "col-10"}>
          <div className="card">
            {section === "pricing" && <PricingSection O={O}/>}
            {section === "backfill" && <BackfillSection O={O}/>}
            {section === "intelligence" && <IntelligenceSection/>}
            {section === "observer" && <ReadonlyConfigSection title="Observer" config={observerToml}/>}
            {section === "watcher" && <ReadonlyConfigSection title="Watcher" config={watcherToml}/>}
            {section === "freshness" && <ReadonlyConfigSection title="Freshness" config={freshnessToml}/>}
            {section === "retention" && <ReadonlyConfigSection title="Retention" config={retentionToml}/>}
            {section === "hooks" && <ReadonlyConfigSection title="Hooks" config={hooksToml}/>}
            {section === "proxy" && <ReadonlyConfigSection title="Proxy" config={proxyToml}/>}
            {section === "compression" && <ReadonlyConfigSection title="Compression" config={compressionToml}/>}
            {section === "antigravity" && <ReadonlyConfigSection title="Antigravity" config={antigravityToml}/>}
          </div>
        </div>

        {/* === Help panel === */}
        {helpOpen && (
          <div className="col-3">
            <div className="card">
              <div className="card-head">
                <h3><Icon name="help" size={13} style={{ color: "var(--accent)"}}/>About this section</h3>
                <div className="right">
                  <button className="icon-btn" onClick={() => setHelpOpen(false)}><Icon name="close" size={12}/></button>
                </div>
              </div>
              <div style={{ padding: 14, fontSize: 12, color: "var(--fg-2)", lineHeight: 1.6 }}>
                <div style={{ fontSize: 10, textTransform: "uppercase", letterSpacing: "0.08em", color: "var(--fg-3)", marginBottom: 4 }}>{current.label}</div>
                <p>{current.helpFor}</p>
                {section === "pricing" && (
                  <>
                    <p><b style={{ color: "var(--fg-0)" }}>Why override?</b> Custom contract rate, or a new SKU lands before our baked-in defaults catch up.</p>
                    <p><b style={{ color: "var(--fg-0)" }}>Hot-reload behavior:</b> save writes <code style={{ fontSize: 11 }}>~/.observer/config.toml</code> with a <code style={{ fontSize: 11 }}>.bak</code>, and the cost engine swaps the active table atomically. No restart.</p>
                  </>
                )}
                {section === "backfill" && (
                  <>
                    <p><b style={{ color: "var(--fg-0)" }}>When to run:</b> Actions tab looks gappy, or after enabling a new adapter. Click "Run all" or pick a single mode for surgical column-update backfills.</p>
                    <p><b style={{ color: "var(--fg-0)" }}>Candidates:</b> non-zero means SQL says rows exist with NULL/empty target columns and a backfill could populate them.</p>
                  </>
                )}
                {section === "intelligence" && (
                  <>
                    <p><b style={{ color: "var(--fg-0)" }}>Restart required:</b> daemon needs a restart for changes to pick up — banner appears after save.</p>
                    <p><b style={{ color: "var(--fg-0)" }}>code_graph.enabled:</b> gates the codebase-memory MCP integration that enriches MCP queries with structural code context.</p>
                  </>
                )}
              </div>
            </div>
          </div>
        )}
      </div>
    </div>
  );
}

// === Pricing section ===
function PricingSection({ O }) {
  const [filter, setFilter] = React.useState("");
  const [expanded, setExpanded] = React.useState(false);

  return (
    <div>
      <div className="card-head">
        <h3>Pricing overrides</h3>
        <div className="sub">live config.toml · {O.PRICING_DEFAULTS.length} baked-in defaults</div>
        <div className="right">
          <button className="btn ghost"><Icon name="plus" size={13}/>Add override</button>
          <button className="btn primary"><Icon name="check" size={13}/>Save pricing</button>
        </div>
      </div>
      <div style={{ padding: 14, fontSize: 12, color: "var(--fg-2)", lineHeight: 1.6, borderBottom: "1px solid var(--line-1)" }}>
        Per-model pricing overrides. The cost engine ships with baked-in defaults for every common Anthropic / OpenAI / Gemini / xAI model (60+ entries). Override a row when you have a custom contract rate or a new SKU lands before observer's defaults catch up. <b style={{ color: "var(--success)" }}>This section hot-reloads</b> — the cost engine swaps the active pricing table atomically on save, no restart.
      </div>
      <div style={{ padding: 14, display: "flex", gap: 10 }}>
        <div style={{ position: "relative", flex: 1 }}>
          <Icon name="search" size={13} style={{ position: "absolute", left: 9, top: 8, color: "var(--fg-3)" }} />
          <input className="input mono" style={{ paddingLeft: 28 }} placeholder="filter by model id…" value={filter} onChange={e => setFilter(e.target.value)} />
        </div>
      </div>
      <div style={{ padding: "0 14px 14px", color: "var(--fg-3)", fontSize: 12, textAlign: "center", fontStyle: "italic" }}>
        No active overrides — using baked-in defaults for every model. Click "Add override" to start.
      </div>

      <div style={{ borderTop: "1px solid var(--line-1)" }}>
        <div style={{ padding: "10px 14px", display: "flex", alignItems: "center", cursor: "pointer", gap: 6 }} onClick={() => setExpanded(!expanded)}>
          <Icon name={expanded ? "chevron_d" : "chevron_r"} size={13} style={{ color: "var(--fg-3)" }}/>
          <b style={{ fontSize: 12, color: "var(--fg-0)" }}>Baked-in defaults</b>
          <span className="dim" style={{ fontSize: 11 }}>(105 models — click to {expanded ? "collapse" : "expand"})</span>
        </div>
        {expanded && (
          <div style={{ padding: "0 14px 14px" }}>
            <table className="dtable">
              <thead>
                <tr>
                  <th>Model</th>
                  <th className="num">Input $/M</th>
                  <th className="num">Cache Read $/M</th>
                  <th className="num">Cache Write $/M</th>
                  <th className="num">Output $/M</th>
                  <th></th>
                </tr>
              </thead>
              <tbody>
                {O.PRICING_DEFAULTS.filter(p => !filter || p.model.includes(filter)).map((p, i) => (
                  <tr key={i}>
                    <td className="mono">{p.model}</td>
                    <td className="num">${p.input.toFixed(3)}</td>
                    <td className="num">${p.cache_read.toFixed(3)}</td>
                    <td className="num">${p.cache_write.toFixed(3)}</td>
                    <td className="num">${p.output.toFixed(3)}</td>
                    <td><button className="btn subtle" style={{ padding: "2px 8px", fontSize: 11 }}>Override</button></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </div>
  );
}

// === Backfill section ===
function BackfillSection({ O }) {
  return (
    <div>
      <div className="card-head">
        <h3>Backfill</h3>
        <div className="sub">recovery + maintenance for historical data</div>
        <div className="right">
          <button className="btn primary"><Icon name="play" size={13}/>Run all</button>
        </div>
      </div>
      <div style={{ padding: 14, fontSize: 12, color: "var(--fg-2)", lineHeight: 1.6, borderBottom: "1px solid var(--line-1)" }}>
        Click "Run all" when the Actions tab looks gappy — the runner re-walks every JSONL from offset 0 (recovers any rows the live watcher dropped silently) and then runs the surgical column-update backfills on top. Per-mode buttons re-run a single backfill when you only care about one specific column. SQL-checkable modes show how many rows have a NULL/empty target column — a non-zero count means a backfill could populate them.
      </div>
      <table className="dtable">
        <thead>
          <tr>
            <th>Mode</th>
            <th>Flag</th>
            <th className="num">Candidates</th>
            <th>Description</th>
            <th>Status</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          {O.BACKFILL.map((b, i) => (
            <tr key={i}>
              <td className="mono" style={{ color: "var(--fg-0)" }}>{b.mode}</td>
              <td><code className="cmd-mono" style={{ color: "var(--accent)" }}>{b.flag}</code></td>
              <td className="num" style={{ color: b.candidates > 0 ? "var(--warn)" : "var(--fg-3)", fontWeight: b.candidates > 0 ? 600 : 400 }}>
                {b.candidates == null ? "—" : O.fmt.n(b.candidates)}
              </td>
              <td className="dim" style={{ fontSize: 11 }}>{b.desc}</td>
              <td><span className="pill">{b.status}</span></td>
              <td><button className="btn primary" style={{ padding: "3px 12px", fontSize: 11 }}><Icon name="play" size={10}/>Run</button></td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// === Intelligence section ===
function IntelligenceSection() {
  const [codeGraph, setCodeGraph] = React.useState(true);
  return (
    <div>
      <div className="card-head">
        <h3>Intelligence <span className="pill warn" style={{ marginLeft: 6 }}>restart required on save</span></h3>
        <div className="right">
          <button className="btn primary"><Icon name="check" size={13}/>Save Intelligence</button>
        </div>
      </div>
      <div style={{ padding: 14, fontSize: 12, color: "var(--fg-2)", lineHeight: 1.6, borderBottom: "1px solid var(--line-1)" }}>
        Intelligence layer — derived patterns (<code>observer suggest</code>), session summaries, the cost engine, and the Analysis tab dashboard. <code>CodeGraph.Enabled</code> gates the codebase-memory MCP integration that enriches MCP queries with structural code context. <code>APIKeyEnv</code> is the env-var name observer reads when intelligence features need to call an upstream LLM (summaries, etc). <code>MonthlyBudgetUSD</code> surfaces on the Analysis tab's headline tile — set it to your monthly spend cap to see "you're at 73% of $100" instead of just dollar totals.
      </div>
      <div style={{ padding: 18, display: "flex", flexDirection: "column", gap: 18 }}>
        <div style={{ display: "flex", alignItems: "center", gap: 14 }}>
          <div className={"toggle" + (codeGraph ? " on" : "")} onClick={() => setCodeGraph(!codeGraph)} />
          <div>
            <div style={{ fontWeight: 600, color: "var(--fg-0)", fontSize: 13 }}>Code graph enabled</div>
            <div style={{ fontSize: 11, color: "var(--fg-3)" }}>Enriches MCP queries with structural code context · adds ~50ms per query</div>
          </div>
        </div>

        <div>
          <label style={{ fontSize: 11, textTransform: "uppercase", letterSpacing: "0.08em", color: "var(--fg-3)", fontWeight: 600 }}>API key env var</label>
          <div style={{ fontSize: 11, color: "var(--fg-3)", marginBottom: 6 }}>name of the env variable holding the upstream provider API key (Anthropic / OpenAI)</div>
          <input className="input mono" defaultValue="ANTHROPIC_API_KEY"/>
        </div>

        <div>
          <label style={{ fontSize: 11, textTransform: "uppercase", letterSpacing: "0.08em", color: "var(--fg-3)", fontWeight: 600 }}>Summary model</label>
          <div style={{ fontSize: 11, color: "var(--fg-3)", marginBottom: 6 }}>model id used by intelligence summaries (e.g. claude-haiku-4-5)</div>
          <select className="input mono" defaultValue="claude-haiku-4-5">
            <option>claude-haiku-4-5</option>
            <option>claude-haiku-4-5-20251001</option>
            <option>claude-sonnet-4-6</option>
            <option>gpt-5.5</option>
            <option>gemini-3.1-pro-low</option>
          </select>
        </div>

        <div>
          <label style={{ fontSize: 11, textTransform: "uppercase", letterSpacing: "0.08em", color: "var(--fg-3)", fontWeight: 600 }}>Monthly budget (USD)</label>
          <div style={{ fontSize: 11, color: "var(--fg-3)", marginBottom: 6 }}>shown on the Analysis tab Month-to-date tile</div>
          <input className="input mono" defaultValue="5000" style={{ width: 180 }}/>
          <div style={{ marginTop: 10, padding: 10, background: "var(--bg-3)", borderRadius: 6 }}>
            <div style={{ fontSize: 11, color: "var(--fg-3)", marginBottom: 6 }}>preview</div>
            <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
              <div style={{ flex: 1, height: 6, background: "var(--bg-4)", borderRadius: 3, overflow: "hidden" }}>
                <div style={{ width: "36%", height: "100%", background: "var(--accent)" }}/>
              </div>
              <span style={{ fontFamily: "var(--font-mono)", fontSize: 12, color: "var(--fg-1)", fontWeight: 600 }}>$1,803 / $5,000 · 36%</span>
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}

// === Read-only config viewer ===
function ReadonlyConfigSection({ title, config }) {
  return (
    <div>
      <div className="card-head">
        <h3>{title} <span className="pill" style={{ marginLeft: 6 }}>read-only</span></h3>
        <div className="right">
          <button className="btn ghost"><Icon name="copy" size={13}/>Copy</button>
          <button className="btn ghost"><Icon name="external" size={13}/>Open file</button>
        </div>
      </div>
      <div style={{ padding: 14 }}>
        <pre style={{
          margin: 0, padding: 14,
          background: "var(--bg-1)", border: "1px solid var(--line-1)", borderRadius: 6,
          fontFamily: "var(--font-mono)", fontSize: 12, color: "var(--fg-1)",
          lineHeight: 1.6, overflowX: "auto"
        }}>
          {config.map((line, i) => {
            // simple syntax highlighting
            if (line.startsWith("#")) return <div key={i} style={{ color: "var(--fg-3)" }}>{line}</div>;
            if (line.startsWith("[")) return <div key={i} style={{ color: "var(--brand-to)", fontWeight: 600 }}>{line}</div>;
            const eq = line.indexOf("=");
            if (eq > 0) {
              return (
                <div key={i}>
                  <span style={{ color: "var(--accent)" }}>{line.slice(0, eq).trimEnd()}</span>
                  <span style={{ color: "var(--fg-3)" }}> = </span>
                  <span style={{ color: line.slice(eq+1).trim().match(/^["']/) ? "var(--success)" : line.slice(eq+1).trim().match(/^(true|false)$/) ? "var(--warn)" : "var(--fg-1)" }}>
                    {line.slice(eq+1).trim()}
                  </span>
                </div>
              );
            }
            return <div key={i}>{line}</div>;
          })}
        </pre>
      </div>
    </div>
  );
}

// === Config snippets ===
const observerToml = [
  "# Top-level observer config",
  "# This file is hot-reloaded for [pricing], restart-required for [intelligence].",
  "",
  "[observer]",
  '# database path; created on first run',
  'db_path = "/home/marmutapp/.observer/observer.db"',
  'schema_version = 18',
  '',
  '# enable telemetry (anonymous; counts only)',
  'telemetry = false',
  '',
  '# log level: trace, debug, info, warn, error',
  'log_level = "info"',
];
const watcherToml = [
  "# Filesystem watcher — tails JSONL/log files from each adapter",
  "[watcher]",
  'enabled = true',
  'poll_interval_ms = 250',
  'batch_size = 200',
  '',
  "[watcher.adapters]",
  'claude_code = true',
  'codex = true',
  'cursor = true',
  'cowork = true',
  'antigravity = true',
];
const freshnessToml = [
  "# Freshness engine — file content hash + change detection",
  "[freshness]",
  'enabled = true',
  '# hash algorithm; xxhash is ~5x faster than sha1',
  'hash_algo = "xxhash"',
  'max_file_bytes = 10000000',
];
const retentionToml = [
  "[retention]",
  'actions_days = 90',
  'token_usage_days = 180',
  'sessions_days = 365',
  '',
  "# how often to run VACUUM",
  'vacuum_interval_hours = 168',
];
const hooksToml = [
  "[hooks.claude_code]",
  'enabled = true',
  '# hook events to subscribe to',
  'events = ["UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop"]',
  '',
  "[hooks.codex]",
  'enabled = true',
  'events = ["AgentTurnEnd"]',
];
const proxyToml = [
  "[proxy]",
  'listen = "127.0.0.1:8820"',
  '',
  "# upstream routes",
  '',
  "[proxy.routes.anthropic]",
  'upstream = "https://api.anthropic.com"',
  'oauth_passthrough = true',
  '',
  "[proxy.routes.openai]",
  'upstream = "https://api.openai.com"',
];
const compressionToml = [
  "[compression]",
  'enabled = true',
  'min_bytes_to_compress = 4096',
  '',
  "# per-mechanism thresholds",
  "[compression.json]",
  'enabled = true',
  'value_max_bytes = 8192',
  '',
  "[compression.drop]",
  'enabled = true',
  'importance_threshold = 0.3',
  '',
  "[compression.stash]",
  'enabled = true',
  'min_bytes_to_stash = 8192',
];
const antigravityToml = [
  "[antigravity]",
  'enabled = true',
  'reasoning_tokens = true',
  'extra_logging = false',
];

window.PageSettings = PageSettings;
