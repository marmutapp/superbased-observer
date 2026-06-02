/* ============================================================
   Overview — hero KPIs, cost over time, actions over time,
   top models, top tools, recent sessions
   ============================================================ */
function PageOverview() {
  const O = window.OBS;
  const costSeries = [
    { key: "net_input",   label: "Net Input",   color: "var(--tok-net)" },
    { key: "cache_read",  label: "Cache Read",  color: "var(--tok-read)" },
    { key: "cache_write", label: "Cache Write", color: "var(--tok-write)" },
    { key: "output",      label: "Output",      color: "var(--tok-out)" },
  ];

  // top models for hbar — sum of input + cache_read + output
  const topModelData = O.TOP_MODELS.slice(0, 8).map(m => ({
    label: m.model,
    value: m.net + m.read + m.output,
    color: modelColor(m.model),
  }));
  function modelColor(name) {
    if (name.includes("opus")) return "#a78bfa";
    if (name.includes("sonnet")) return "#7c9eff";
    if (name.includes("haiku")) return "#fbbf24";
    if (name.includes("gpt")) return "#10b981";
    if (name.includes("gemini")) return "#ec4899";
    return "var(--fg-3)";
  }

  // top tools — flat bar (actions count)
  const topToolsData = O.TOOLS_AGG.map(t => ({
    label: t.tool, value: t.actions,
    color: O.TOOLS[t.tool] ? O.TOOLS[t.tool].color : "var(--fg-3)",
  }));

  const sparkBase = O.COST_TS.slice(-14).map(d => d.cost_usd);
  const sparkActions = O.COST_TS.slice(-14).map(d => d.actions);
  const sparkFailures = [3,1,0,2,4,1,2,0,1,3,2,0,1,1];

  return (
    <div>
      <div className="page-head">
        <div>
          <h1 className="page-title">Overview</h1>
          <p className="page-desc">
            High-level snapshot — KPI tiles, daily cost and activity, plus top-N models and tools across the selected window.
          </p>
        </div>
        <div className="spacer" />
        <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
          <span className="pill brand">window 30d</span>
          <span className="pill success">all systems nominal</span>
        </div>
      </div>

      {/* === KPI row === */}
      <div className="grid g-4" style={{ marginBottom: 14 }}>
        <StatCard
          label="Sessions" icon="sessions" helpId="tile.sessions"
          value={O.STATUS.sessions.toLocaleString()}
          delta={0.122} deltaLabel="vs prior 30d"
          sub={`${O.fmt.n(O.STATUS.actions)} actions`}
          sparkData={sparkActions} sparkColor="var(--accent)"
        />
        <StatCard
          label="API Turns (proxy)" icon="bolt" helpId="tile.api_turns"
          value={O.STATUS.api_turns_proxy.toLocaleString()}
          sub="accurate token source"
          accent
          sparkData={[2,5,8,3,12,8,9,11,4,6,8,5,12,7]} sparkColor="var(--accent)"
        />
        <StatCard
          label="Token Rows (JSONL)" icon="database" helpId="tile.token_rows"
          value={O.fmt.n(O.STATUS.token_rows_jsonl)}
          delta={0.45} deltaLabel="vs prior 30d"
          sub="unreliable but plentiful"
          sparkData={sparkActions} sparkColor="var(--info)"
        />
        <StatCard
          label="Failures (24h)" icon="alert" helpId="tile.failures"
          value={O.STATUS.failures_24h}
          sub="check Discovery tab"
          sparkData={sparkFailures} sparkColor="var(--danger)"
          warn={O.STATUS.failures_24h > 5}
        />
      </div>

      {/* === Hero charts === */}
      <div className="grid g-2" style={{ marginBottom: 14 }}>
        <ChartShell
          title="Cost over time"
          helpId="chart.cost_ts"
          sub={<span>window <b style={{ color: "var(--fg-1)" }}>30d</b> · <b style={{ color: "var(--fg-1)" }}>{O.fmt.$(O.TOTAL_COST, 2)}</b> · 31 days</span>}
          right={<Seg mini options={["Tokens", "Cost $"]} value="Cost $" onChange={() => {}} />}
        >
          <AreaChart
            data={O.COST_TS}
            series={costSeries}
            height={240}
            yFmt={v => v < 1 ? v.toFixed(2) : O.fmt.n(v*1e6) + ""}
          />
        </ChartShell>

        <ChartShell
          title="Actions over time"
          helpId="chart.actions_ts"
          sub={<span><b style={{ color: "var(--fg-1)" }}>{O.fmt.n(O.STATUS.actions)}</b> total actions · 31 days</span>}
          right={<Seg mini options={["by Tool", "by Action"]} value="by Tool" onChange={() => {}} />}
        >
          <AreaChart
            data={O.COST_TS.map(d => ({
              date: d.date,
              "claude-code": d.actions * 0.95,
              "codex": d.actions * 0.022,
              "antigravity": d.actions * 0.018,
              "cowork": d.actions * 0.011,
              "other": d.actions * 0.003,
            }))}
            series={[
              { key: "claude-code", label: "claude-code", color: "var(--tool-claude-code)" },
              { key: "codex",       label: "codex",       color: "var(--tool-codex)" },
              { key: "antigravity", label: "antigravity", color: "var(--tool-antigravity)" },
              { key: "cowork",      label: "cowork",      color: "var(--tool-cowork)" },
              { key: "other",       label: "other",       color: "var(--tool-other)" },
            ]}
            height={240}
            yFmt={v => O.fmt.n(v)}
          />
        </ChartShell>
      </div>

      {/* === Distribution row === */}
      <div className="grid g-12" style={{ marginBottom: 14 }}>
        <div className="col-7">
          <ChartShell
            title="Top models by tokens"
            helpId="chart.top_models"
            sub={<span>top <b style={{ color: "var(--fg-1)" }}>8</b> · sum of net input + cache read + output</span>}
          >
            <HBarChart
              data={topModelData}
              labelKey="label"
              valueKey="value"
              color={(r) => r.color}
              fmt={v => O.fmt.n(v, 2)}
              maxBars={8}
            />
          </ChartShell>
        </div>
        <div className="col-5">
          <ChartShell
            title="Top tools (by actions)"
            helpId="chart.top_tools"
            sub={<span><b style={{ color: "var(--fg-1)" }}>7</b> tools · 30d</span>}
          >
            <Donut
              size={140}
              thickness={20}
              centerValue={O.fmt.n(O.TOOLS_AGG.reduce((s,t) => s + t.actions, 0))}
              centerLabel="ACTIONS"
              data={O.TOOLS_AGG.map(t => ({
                label: t.tool, value: t.actions,
                color: O.TOOLS[t.tool] ? O.TOOLS[t.tool].color : "var(--fg-3)",
              }))}
            />
          </ChartShell>
        </div>
      </div>

      {/* === Recent sessions === */}
      <ChartShell
        title="Recent sessions"
        helpId="chart.recent_sessions"
        sub="latest 5 · click to drill into session detail"
        right={<button className="btn subtle">View all <Icon name="arrow_r" size={11} /></button>}
        pad={false}
      >
        <table className="dtable">
          <thead>
            <tr>
              <th>Tool</th>
              <th>Session</th>
              <th>Project</th>
              <th>Model(s)</th>
              <th>Started</th>
              <th className="num">Elapsed</th>
              <th className="num">Actions</th>
              <th className="num">Cost</th>
            </tr>
          </thead>
          <tbody>
            {O.SESSIONS.slice(0, 5).map(s => (
              <tr key={s.id} className="clickable" data-session={s.id}>
                <td><ToolBadge tool={s.tool} /></td>
                <td><span className="id-link">{s.id.slice(0, 8)}…</span></td>
                <td className="mono dim">{s.project}</td>
                <td>
                  <span className="pill brand">
                    {s.tool === "codex" ? "gpt-5.5" : s.tool === "cowork" ? "claude-opus-4-7" : "claude-opus-4-7"}
                  </span>
                </td>
                <td className="dim mono" style={{ fontSize: 11 }}>{s.started}</td>
                <td className="num">{s.elapsed}</td>
                <td className="num">{s.actions}</td>
                <td className="num" style={{ color: s.total > 10 ? "var(--warn)" : "var(--fg-1)", fontWeight: 600 }}>{O.fmt.$2(s.total)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </ChartShell>
    </div>
  );
}
window.PageOverview = PageOverview;
