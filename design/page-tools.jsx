/* ============================================================
   Tools — KPI strip, activity over time, 100% stacked action mix,
   per-tool table
   ============================================================ */
function PageTools() {
  const O = window.OBS;
  const busiest = O.TOOLS_AGG[0];

  // synthetic action-mix per tool — proportions
  const ACTION_TYPES_SHORT = ["read_file","edit_file","run_command","user_prompt","task_complete","todo_update","post_tool_batch","write_file","search_text","subagent_stop","mcp_call","web_search","other"];
  function mix(seed) {
    // deterministic pseudo proportions
    let acc = []; let s = seed;
    ACTION_TYPES_SHORT.forEach((_, i) => {
      s = (s * 9301 + 49297) % 233280;
      acc.push(s / 233280);
    });
    const sum = acc.reduce((a,b) => a+b, 0);
    return acc.map(v => v/sum);
  }

  const toolActivityData = O.COST_TS.map(d => {
    const total = d.actions;
    return {
      date: d.date,
      "claude-code":  total * 0.94,
      "codex":        total * 0.022,
      "antigravity":  total * 0.018,
      "cowork":       total * 0.012,
      "cursor":       total * 0.003,
      "copilot":      total * 0.001,
      "other":        total * 0.004,
    };
  });

  return (
    <div>
      <div className="page-head">
        <div>
          <h1 className="page-title">Tools</h1>
          <p className="page-desc">
            <b>Tool</b> means the AI client (claude-code / cursor / codex / cline / copilot), not the per-message tool name. KPI tiles summarise activity; the charts below show <b>when</b> each tool was active and <b>what kind</b> of work it did.
          </p>
        </div>
      </div>

      {/* === KPI row === */}
      <div className="grid g-4" style={{ marginBottom: 14 }}>
        <StatCard label="Total Actions" icon="actions" accent
          value={O.fmt.n(35000)} sub="across all tools" />
        <StatCard label="Distinct Tools" icon="cube"
          value="7" sub="AI clients seen" />
        <StatCard label="Overall Success" icon="check"
          value={<span style={{ color: "var(--success)" }}>97.8%</span>} sub="757 failures" />
        <StatCard label="Busiest Tool" icon="flame">
          <div style={{ display: "flex", alignItems: "center", gap: 10, marginTop: 4 }}>
            <ProviderIcon tool={busiest.tool} size={36} radius={9} />
            <div>
              <div style={{ fontSize: 22, fontWeight: 700, color: "var(--fg-0)", lineHeight: 1.1, fontFamily: "var(--font-display)" }}>{busiest.tool}</div>
              <div style={{ fontSize: 11, color: "var(--fg-3)" }}>{O.fmt.n(busiest.actions)} actions · 97.9% ok</div>
            </div>
          </div>
        </StatCard>
      </div>

      {/* === Charts === */}
      <div className="grid g-2" style={{ marginBottom: 14 }}>
        <ChartShell title="Activity over time" sub="31 day(s) · 6 tool(s) + 1 rolled into 'other'">
          <AreaChart
            data={toolActivityData}
            series={[
              { key: "claude-code", label: "claude-code", color: "var(--tool-claude-code)" },
              { key: "codex",       label: "codex",       color: "var(--tool-codex)" },
              { key: "antigravity", label: "antigravity", color: "var(--tool-antigravity)" },
              { key: "cowork",      label: "cowork",      color: "var(--tool-cowork)" },
              { key: "cursor",      label: "cursor",      color: "var(--tool-cursor)" },
              { key: "copilot",     label: "copilot",     color: "var(--tool-copilot)" },
              { key: "other",       label: "other",       color: "var(--tool-other)" },
            ]}
            height={240} yFmt={v => O.fmt.n(v)}
          />
        </ChartShell>

        <ChartShell title="Action-type mix per tool" sub="100% stacked · 7 tools · 13 action types"
          right={<Seg mini options={["%", "count"]} value="%" onChange={() => {}}/>}
        >
          <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
            {O.TOOLS_AGG.map((t, ti) => {
              const m = mix(ti * 7919);
              return (
                <div key={t.tool}>
                  <div style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 4 }}>
                    <span className="tool-dot" style={{ background: O.TOOLS[t.tool].color }} />
                    <span className="mono" style={{ fontSize: 11, color: "var(--fg-1)", fontWeight: 500, flex: 1 }}>{t.tool}</span>
                    <span className="dim" style={{ fontSize: 11, fontFamily: "var(--font-mono)" }}>{O.fmt.n(t.actions)}</span>
                  </div>
                  <div style={{ display: "flex", width: "100%", height: 12, borderRadius: 3, overflow: "hidden", background: "var(--bg-3)" }}>
                    {m.map((p, i) => {
                      const colors = ["var(--act-file)", "var(--tok-net)", "var(--act-cmd)", "var(--act-user)", "var(--act-meta)", "var(--act-meta)", "var(--act-meta)", "var(--act-file)", "var(--act-search)", "var(--act-agent)", "var(--act-mcp)", "var(--act-web)", "var(--fg-3)"];
                      return <span key={i} style={{ width: `${p*100}%`, background: colors[i] }} title={`${ACTION_TYPES_SHORT[i]}: ${(p*100).toFixed(1)}%`} />;
                    })}
                  </div>
                </div>
              );
            })}
          </div>
          <div className="chart-legend" style={{ marginTop: 14, fontSize: 10, justifyContent: "center" }}>
            {[
              { l: "read_file", c: "var(--act-file)" },
              { l: "edit_file", c: "var(--tok-net)" },
              { l: "run_command", c: "var(--act-cmd)" },
              { l: "user_prompt", c: "var(--act-user)" },
              { l: "search", c: "var(--act-search)" },
              { l: "subagent", c: "var(--act-agent)" },
              { l: "mcp_call", c: "var(--act-mcp)" },
              { l: "web", c: "var(--act-web)" },
              { l: "other", c: "var(--fg-3)" },
            ].map((x, i) => (
              <div className="item" key={i}>
                <span className="sw" style={{ background: x.c }}/>{x.l}
              </div>
            ))}
          </div>
        </ChartShell>
      </div>

      {/* === Per-tool aggregates table === */}
      <ChartShell title="Per-tool aggregates" sub={`${O.TOOLS_AGG.length} tools`} pad={false}>
        <table className="dtable">
          <thead>
            <tr>
              <th>Tool</th>
              <th className="num">Actions</th>
              <th className="num">Failures</th>
              <th>Success rate</th>
              <th className="num">Sessions</th>
              <th>First seen</th>
              <th>Last seen</th>
            </tr>
          </thead>
          <tbody>
            {O.TOOLS_AGG.map(t => {
              const maxAct = Math.max(...O.TOOLS_AGG.map(x => x.actions));
              return (
                <tr key={t.tool} className="clickable">
                  <td>
                    <ToolBadge tool={t.tool} />
                  </td>
                  <td className="num">
                    <div className="cellbar">
                      <div className="bar"><div style={{ width: `${(t.actions/maxAct)*100}%`, background: O.TOOLS[t.tool].color }}/></div>
                      {O.fmt.n(t.actions)}
                    </div>
                  </td>
                  <td className="num" style={{ color: t.failures > 100 ? "var(--danger)" : "var(--fg-2)" }}>{t.failures.toLocaleString()}</td>
                  <td style={{ width: 200 }}>
                    <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
                      <div style={{ flex: 1, height: 6, background: "var(--bg-4)", borderRadius: 3, overflow: "hidden" }}>
                        <div style={{ width: `${t.success*100}%`, height: "100%", background: t.success >= 0.98 ? "var(--success)" : t.success >= 0.9 ? "var(--warn)" : "var(--danger)" }}/>
                      </div>
                      <span className="num" style={{ fontFamily: "var(--font-mono)", fontSize: 12, color: t.success >= 0.98 ? "var(--success)" : "var(--fg-1)", fontWeight: 600 }}>
                        {(t.success*100).toFixed(1)}%
                      </span>
                    </div>
                  </td>
                  <td className="num">{t.sessions.toLocaleString()}</td>
                  <td className="dim mono" style={{ fontSize: 11 }}>{t.first}</td>
                  <td className="dim mono" style={{ fontSize: 11 }}>{t.last}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </ChartShell>
    </div>
  );
}
window.PageTools = PageTools;
