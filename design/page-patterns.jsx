/* ============================================================
   Patterns — learned behaviours as insight cards by type
   ============================================================ */
function PagePatterns() {
  const O = window.OBS;
  const [filterType, setFilterType] = React.useState("all");

  const patternTypes = [
    { type: "hot_file",          label: "Hot file",          color: "var(--pat-hot)",       icon: "flame" },
    { type: "co_change",         label: "Co-change",          color: "var(--pat-cochange)",  icon: "link" },
    { type: "common_command",    label: "Common command",    color: "var(--pat-command)",   icon: "terminal" },
    { type: "edit_test_pair",    label: "Edit→test pair",    color: "var(--pat-edittest)",  icon: "beaker" },
    { type: "knowledge_snippet", label: "Knowledge snippet", color: "var(--pat-knowledge)", icon: "lightbulb" },
  ];
  const byType = patternTypes.reduce((acc, t) => {
    acc[t.type] = O.PATTERNS.filter(p => p.type === t.type);
    return acc;
  }, {});

  const filtered = filterType === "all" ? O.PATTERNS : O.PATTERNS.filter(p => p.type === filterType);

  // Formatter for rule text — highlight key=value pairs with mono colors
  function formatRule(text) {
    return text.split(/(\b[a-z_]+=[^\s]+)/g).map((part, i) => {
      const m = part.match(/^([a-z_]+)=(.+)$/);
      if (m) {
        return (
          <span key={i} style={{ marginRight: 8 }}>
            <span style={{ color: "var(--fg-3)", fontSize: 11 }}>{m[1]}=</span>
            <span style={{ color: "var(--accent)", fontFamily: "var(--font-mono)", fontSize: 11 }}>{m[2]}</span>
          </span>
        );
      }
      return <span key={i} style={{ color: "var(--fg-2)", fontSize: 12 }}>{part}</span>;
    });
  }

  return (
    <div>
      <div className="page-head">
        <div>
          <h1 className="page-title">Patterns</h1>
          <p className="page-desc">
            Repeatable behaviours the observer noticed across your sessions — for example, "after running <code>go test</code>, you almost always run <code>go vet</code>", or "the file <code>auth.ts</code> shows up in 80% of sessions touching <code>login.tsx</code>." These get fed into <code>observer suggest</code> which writes them into <code>CLAUDE.md</code> / <code>AGENTS.md</code> / <code>.cursorrules</code>, so new sessions inherit the habit without you re-typing instructions.
          </p>
        </div>
        <div className="spacer" />
        <button className="btn primary">
          <Icon name="zap" size={13}/>
          Generate suggestions
        </button>
      </div>

      {/* === Type filter row === */}
      <div className="card card-pad" style={{ marginBottom: 14 }}>
        <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
          <span style={{ fontSize: 11, textTransform: "uppercase", letterSpacing: "0.08em", color: "var(--fg-3)", fontWeight: 600, marginRight: 6 }}>Filter by type</span>
          <button
            onClick={() => setFilterType("all")}
            className="pill"
            style={{ cursor: "pointer", background: filterType === "all" ? "var(--accent-soft)" : "var(--bg-3)", color: filterType === "all" ? "var(--accent)" : "var(--fg-2)", borderColor: filterType === "all" ? "var(--accent)" : "var(--line-2)" }}>
            All <b style={{ marginLeft: 4 }}>{O.PATTERNS.length}</b>
          </button>
          {patternTypes.map(t => (
            <button key={t.type}
              onClick={() => setFilterType(t.type)}
              className="pill"
              style={{
                cursor: "pointer",
                background: filterType === t.type ? "rgba(255,255,255,0.04)" : "var(--bg-3)",
                color: filterType === t.type ? t.color : "var(--fg-2)",
                borderColor: filterType === t.type ? t.color : "var(--line-2)",
              }}>
              <span style={{ width: 6, height: 6, borderRadius: 50, background: t.color, marginRight: 2 }} />
              {t.label} <b style={{ marginLeft: 4 }}>{byType[t.type].length}</b>
            </button>
          ))}
          <div className="spacer" />
          <span className="dim" style={{ fontSize: 11 }}>{filtered.length} of 108 patterns shown</span>
        </div>
      </div>

      {/* === Insight cards === */}
      <div className="grid g-2" style={{ gap: 12 }}>
        {filtered.map((p, i) => {
          const type = patternTypes.find(t => t.type === p.type) || patternTypes[0];
          return (
            <div key={i} className="card" style={{ position: "relative", overflow: "hidden", borderLeft: `3px solid ${type.color}` }}>
              <div style={{ padding: 14 }}>
                <div style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 8 }}>
                  <span className="pill" style={{ color: type.color, background: "rgba(255,255,255,0.04)", borderColor: type.color, fontFamily: "var(--font-mono)" }}>
                    <Icon name={type.icon} size={10} />
                    {p.type}
                  </span>
                  <span className="pill" style={{ fontFamily: "var(--font-mono)" }}>
                    <Icon name="folder" size={10} />
                    {p.project}
                  </span>
                  <div className="spacer" />
                  <div style={{ display: "flex", alignItems: "center", gap: 6 }}>
                    <div style={{ width: 60, height: 4, background: "var(--bg-4)", borderRadius: 2, overflow: "hidden" }}>
                      <div style={{ width: `${p.confidence*100}%`, height: "100%", background: p.confidence >= 0.95 ? "var(--success)" : p.confidence >= 0.8 ? "var(--warn)" : "var(--info)" }}/>
                    </div>
                    <span className="num" style={{ fontFamily: "var(--font-mono)", fontSize: 11, color: "var(--fg-1)", fontWeight: 600 }}>
                      {p.confidence.toFixed(2)}
                    </span>
                  </div>
                  <span className="dim" style={{ fontSize: 11, fontFamily: "var(--font-mono)" }}>{p.observations} obs</span>
                </div>
                <div style={{ background: "var(--bg-3)", padding: "8px 10px", borderRadius: 4, lineHeight: 1.6 }}>
                  {formatRule(p.rule)}
                </div>
                {p.type === "edit_test_pair" && (
                  <div style={{ marginTop: 10, display: "flex", alignItems: "center", gap: 6, fontSize: 11 }}>
                    <span className="path-mono" style={{ color: "var(--act-file)" }}>file.go</span>
                    <Icon name="arrow_r" size={11} style={{ color: "var(--fg-3)" }}/>
                    <span className="path-mono" style={{ color: "var(--success)" }}>file_test.go</span>
                    <Icon name="arrow_r" size={11} style={{ color: "var(--fg-3)" }}/>
                    <code style={{ background: "var(--bg-1)", padding: "1px 6px", borderRadius: 3, color: "var(--accent)", fontSize: 10 }}>go test -race ./…</code>
                  </div>
                )}
              </div>
            </div>
          );
        })}
      </div>

      <div style={{ height: 14 }} />

      <div className="grid g-2" style={{ gap: 12 }}>
        <ChartShell title="Pattern discovery over time" sub="when new patterns were first detected">
          <BarChart
            data={[
              { date: "2026-04-15", value: 3 },
              { date: "2026-04-18", value: 8 },
              { date: "2026-04-22", value: 17 },
              { date: "2026-04-26", value: 22 },
              { date: "2026-04-30", value: 12 },
              { date: "2026-05-04", value: 15 },
              { date: "2026-05-08", value: 19 },
              { date: "2026-05-12", value: 12 },
            ]}
            valueKey="value" color="var(--accent)" height={160}
          />
        </ChartShell>
        <ChartShell title="Generate instruction files" sub="observer suggest writes high-confidence patterns into" pad={true}>
          <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
            {[
              { name: "CLAUDE.md", target: "Claude Code", color: "var(--tool-claude-code)", count: 47 },
              { name: "AGENTS.md", target: "Codex, Cursor, multi-tool", color: "var(--tool-codex)", count: 47 },
              { name: ".cursorrules", target: "Cursor (legacy)", color: "var(--tool-cursor)", count: 42 },
            ].map((f, i) => (
              <div key={i} style={{ display: "flex", alignItems: "center", gap: 12, padding: "10px 12px", background: "var(--bg-3)", borderRadius: 6, borderLeft: `2px solid ${f.color}` }}>
                <div style={{ flex: 1 }}>
                  <div className="path-mono" style={{ color: "var(--fg-0)", fontWeight: 500, fontSize: 13 }}>{f.name}</div>
                  <div style={{ fontSize: 11, color: "var(--fg-3)" }}>{f.target} · {f.count} rules pending</div>
                </div>
                <button className="btn ghost"><Icon name="external" size={12}/>Preview</button>
                <button className="btn primary"><Icon name="download" size={12}/>Write</button>
              </div>
            ))}
          </div>
        </ChartShell>
      </div>
    </div>
  );
}
window.PagePatterns = PagePatterns;
