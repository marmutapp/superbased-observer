/* ============================================================
   Analysis — 10 KPI tiles + dimension toggle trend +
   heatmap + cache savings trend + movers/new + top sessions + routing
   ============================================================ */
function PageAnalysis() {
  const O = window.OBS;
  const A = O.ANALYSIS;
  const [dim, setDim] = React.useState("Model");

  const sparkSpend = O.COST_TS.slice(-14).map(d => d.cost_usd);
  const sparkOutput = O.COST_TS.slice(-14).map(d => d.output * 100);

  const trendSeries = dim === "Model" ? [
    { key: "opus-7", label: "claude-opus-4-7",  color: "#7c9eff" },
    { key: "opus-6", label: "claude-opus-4-6",  color: "#a78bfa" },
    { key: "haiku",  label: "claude-haiku-4-5", color: "#fbbf24" },
    { key: "gpt",    label: "gpt-5.5",          color: "#10b981" },
    { key: "gemini", label: "gemini-3.1-pro",   color: "#ec4899" },
    { key: "other",  label: "other",            color: "var(--fg-3)" },
  ] : dim === "Project" ? [
    { key: "obs",    label: "marmutapp/superbased-observer", color: "#7c9eff" },
    { key: "sb",     label: "marmutapp/superbased",          color: "#a78bfa" },
    { key: "npos",   label: "marmutapp/npos",                color: "#fbbf24" },
    { key: "other",  label: "other",                         color: "var(--fg-3)" },
  ] : [
    { key: "claude-code",  label: "claude-code", color: "var(--tool-claude-code)" },
    { key: "codex",        label: "codex",       color: "var(--tool-codex)" },
    { key: "antigravity",  label: "antigravity", color: "var(--tool-antigravity)" },
    { key: "cowork",       label: "cowork",      color: "var(--tool-cowork)" },
    { key: "other",        label: "other",       color: "var(--fg-3)" },
  ];

  const trendData = O.COST_TS.map(d => {
    const c = d.cost_usd;
    if (dim === "Model") return { date: d.date, "opus-7": c*0.92, "opus-6": c*0.045, "haiku": c*0.020, "gpt": c*0.008, "gemini": c*0.005, "other": c*0.002 };
    if (dim === "Project") return { date: d.date, "obs": c*0.88, "sb": c*0.08, "npos": c*0.03, "other": c*0.01 };
    return { date: d.date, "claude-code": c*0.94, "codex": c*0.025, "antigravity": c*0.018, "cowork": c*0.012, "other": c*0.005 };
  });

  const cacheSavingsData = [50, 1900, 450, 400, 1600, 2850, 2780, 1980, 3200, 1980, 2050, 800, 750, 1080, 1850, 2010, 740, 660, 530, 470, 50, 510, 1540, 720, 700, 850, 670, 700, 660, 70, 1130]
    .map((v, i) => ({ date: O.COST_TS[i].date, value: v }));

  return (
    <div>
      <div className="page-head">
        <div>
          <h1 className="page-title">Analysis</h1>
          <p className="page-desc">
            Spending insights for the selected window. Headline KPIs comparing this period to the prior period, daily trend with a dimension toggle, top movers, and cost-sensitive signals — $/M output, cache savings, high-context turns, per-turn variance, burn rate, top-model concentration, and routing efficiency suggestions.
          </p>
        </div>
      </div>

      {/* === 10 KPI tiles in a 2×4+2 grid (use 4-col, last row 2) === */}
      <div className="grid g-4" style={{ marginBottom: 14 }}>
        <StatCard label="Spend (30d)" icon="cost" accent helpId="tile.spend"
          value={O.fmt.$(A.spend_window, 2)}
          delta={A.spend_delta_pct/100} deltaLabel={`vs prior 30d ($${A.spend_prior.toFixed(2)})`}
          sparkData={sparkSpend} sparkColor="var(--accent)" />
        <StatCard label="Month-to-Date" icon="calendar" helpId="tile.mtd"
          value={O.fmt.$(A.mtd, 2)}
          delta={A.mtd_delta_pct/100} deltaLabel={`vs prior month`}
          sub={<span>proj <b style={{ color: "var(--fg-0)" }}>{O.fmt.$(A.projection_usd, 0)}</b> · budget {Math.round((A.mtd/A.budget_usd)*100)}%</span>}
        >
          <div style={{ marginTop: 10, height: 4, background: "var(--bg-4)", borderRadius: 2, overflow: "hidden" }}>
            <div style={{ width: `${Math.min(100, (A.mtd/A.budget_usd)*100)}%`, height: "100%", background: "var(--accent)" }}/>
          </div>
        </StatCard>
        <StatCard label="$ / M Output" icon="layers" helpId="tile.output"
          value={O.fmt.$(A.output_rate, 2)} unit="/M"
          sub={`${O.fmt.n(A.output_tokens_m*1e6, 2)} output tokens in window`}
          sparkData={sparkOutput} sparkColor="var(--tok-out)" />
        <StatCard label="Cache Savings" icon="zap" helpId="tile.cache" accent
          value={O.fmt.$(A.cache_savings, 2)}
          sub={`${O.fmt.n(A.cache_read_b, 2)} cache_read vs uncached rate`}
          sparkData={[40,1900,450,400,1600,2850,2780,1980,3200,1980,2050,800,750,1080]} sparkColor="var(--success)" />

        <StatCard label="Cache Efficacy" icon="shield" helpId="tile.cache_eff"
          value={<span style={{ color: "var(--success)" }}>{(A.cache_efficacy*100).toFixed(1)}%</span>} unit=""
          sub={`${O.fmt.n(A.cache_read_total_b, 2)} read · ${O.fmt.n(A.cache_write_total_m, 2)} written`}>
        </StatCard>
        <StatCard label="High-Context Turns" icon="alert" helpId="tile.high_ctx"
          value={O.fmt.n(A.high_ctx_100k)} unit="turns"
          sub={<span>&gt;100K: {O.fmt.n(A.high_ctx_100k)} ({O.fmt.$(A.high_ctx_100k_cost, 0)}) · &gt;200K: {O.fmt.n(A.high_ctx_200k)} ({O.fmt.$(A.high_ctx_200k_cost, 0)})</span>}
        />
        <StatCard label="$ per Turn (p95)" icon="trend_up" helpId="tile.per_turn"
          value={O.fmt.$(A.per_turn_p95, 4)}
          sub={`p95 across ${O.fmt.n(A.total_turns)} turns · mean ${O.fmt.$(A.per_turn_mean, 4)}`}
        />
        <StatCard label="Burn Rate" icon="flame" helpId="tile.burn"
          value={O.fmt.$(A.burn_rate, 2)} unit="/hr"
          sub={`${A.active_hours} active hours in window`}
        />

        <StatCard label="Top Model" icon="cube" helpId="tile.top_model"
          value={O.fmt.$(A.top_model_cost, 2)}
          sub={<span><span className="path-mono" style={{ color: "var(--fg-1)" }}>{A.top_model}</span> · {(A.top_model_concentration*100).toFixed(1)}% of spend</span>}
        />
        <StatCard label="Waste $" icon="warn" warn helpId="tile.waste"
          value={<span style={{ color: "var(--danger)" }}>{O.fmt.$(A.waste_usd, 4)}</span>}
          sub={`${O.fmt.n(A.waste_tokens, 2)} stale-read tokens × $${A.waste_rate_perm.toFixed(2)}/M`}
        />
        <div className="stat" style={{ background: "transparent", border: "1px dashed var(--line-2)" }}>
          <div className="label">Budget headroom</div>
          <div className="value" style={{ fontSize: 28 }}>{O.fmt.$(A.budget_usd - A.mtd, 0)}</div>
          <div className="sub">remaining this month at current pace</div>
        </div>
        <div className="stat" style={{ background: "transparent", border: "1px dashed var(--line-2)" }}>
          <div className="label">Total Sessions</div>
          <div className="value" style={{ fontSize: 28 }}>{O.STATUS.sessions}</div>
          <div className="sub">across 7 projects · 7 tools</div>
        </div>
      </div>

      {/* === Daily spend by dimension === */}
      <ChartShell
        title={`Daily spend — by ${dim.toLowerCase()}`}
        helpId="chart.daily_dim"
        sub={`31 day(s) · ${trendSeries.length} ${dim.toLowerCase()}(s) shown`}
        right={<Seg options={["Model", "Project", "Tool"]} value={dim} onChange={setDim} />}
      >
        <StackedBar
          data={trendData}
          series={trendSeries}
          height={220}
          yFmt={v => "$" + v.toFixed(0)}
        />
      </ChartShell>

      <div style={{ height: 14 }} />

      {/* === Heatmap + cache savings === */}
      <div className="grid g-12" style={{ marginBottom: 14 }}>
        <div className="col-7">
          <ChartShell
            title="When you spend"
            helpId="chart.hourly"
            sub="cost by hour of day × day of week · UTC"
          >
            <Heatmap data={O.HOURLY} h={172} color="var(--accent)" />
            <div style={{ display: "flex", alignItems: "center", gap: 8, marginTop: 10, fontSize: 11, color: "var(--fg-3)" }}>
              <span>less</span>
              {[0.1, 0.25, 0.45, 0.7, 1].map((o, i) => (
                <span key={i} style={{ width: 14, height: 10, background: `rgba(124,158,255,${o})`, borderRadius: 2 }}/>
              ))}
              <span>more</span>
              <div className="spacer" />
              <span>Peak: <b style={{ color: "var(--fg-1)" }}>Wed 09:00–10:00 UTC ($442)</b></span>
            </div>
          </ChartShell>
        </div>
        <div className="col-5">
          <ChartShell
            title="Cache savings trend"
            helpId="chart.cache_trend"
            sub={<span>Total <b style={{ color: "var(--success)" }}>{O.fmt.$(A.cache_savings, 2)}</b> saved · {O.fmt.n(A.cache_read_b, 2)} cache_read tokens × 31d</span>}
          >
            <LineChart data={cacheSavingsData} valueKey="value" color="var(--success)" yFmt={v => "$" + v.toFixed(0)} height={172} />
          </ChartShell>
        </div>
      </div>

      {/* === Movers + new + top sessions + routing === */}
      <div className="grid g-2" style={{ marginBottom: 14 }}>
        <ChartShell
          title="What changed — top movers"
          helpId="chart.movers"
          sub="top movers by absolute Δ$ · 30d vs prior 30d · split by model"
          pad={false}
        >
          <table className="dtable">
            <thead>
              <tr>
                <th>Model</th>
                <th className="num">Prior $</th>
                <th className="num">Current $</th>
                <th className="num">Δ$</th>
                <th className="num">Δ%</th>
              </tr>
            </thead>
            <tbody>
              {O.MOVERS.map((m, i) => (
                <tr key={i}>
                  <td className="mono">{m.model}</td>
                  <td className="num dim">{O.fmt.$(m.prior, 4)}</td>
                  <td className="num">{O.fmt.$(m.current, 4)}</td>
                  <td className="num" style={{ color: m.delta < 0 ? "var(--success)" : "var(--danger)", fontWeight: 600 }}>
                    {m.delta > 0 ? "+" : ""}{O.fmt.$(m.delta, 4)}
                  </td>
                  <td className="num" style={{ color: m.pct < 0 ? "var(--success)" : "var(--danger)", fontWeight: 600 }}>
                    {m.pct > 0 ? "+" : ""}{(m.pct*100).toFixed(1)}%
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </ChartShell>

        <ChartShell
          title="New this period"
          helpId="chart.new_models"
          sub={`${O.NEW_THIS_PERIOD.length} model(s) appeared in current period that weren't in prior`}
          pad={false}
        >
          <table className="dtable">
            <thead><tr><th>Model</th><th className="num">Current $</th></tr></thead>
            <tbody>
              {O.NEW_THIS_PERIOD.map((m, i) => (
                <tr key={i}>
                  <td className="mono">
                    <span className="pill" style={{ background: "var(--success-soft)", color: "var(--success)", border: "1px solid rgba(52,211,153,0.2)", marginRight: 6 }}>NEW</span>
                    {m.model}
                  </td>
                  <td className="num">{O.fmt.$(m.current, 4)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </ChartShell>
      </div>

      <ChartShell
        title="Top expensive sessions"
        helpId="chart.top_sessions"
        sub="top 10 by cost over 30d · click a row to drill in"
        right={<button className="btn subtle">View all 14 <Icon name="arrow_r" size={11} /></button>}
        pad={false}
      >
        <table className="dtable">
          <thead>
            <tr>
              <th style={{ width: 36 }}>#</th>
              <th>Session</th>
              <th>Tool</th>
              <th>Model(s)</th>
              <th className="num">Turns</th>
              <th className="num">Cost</th>
              <th>Why flagged</th>
            </tr>
          </thead>
          <tbody>
            {O.TOP_EXPENSIVE.map((s, i) => (
              <tr key={i} className="clickable" data-session={s.id}>
                <td><span style={{ color: i < 3 ? "var(--warn)" : "var(--fg-3)", fontWeight: 700, fontFamily: "var(--font-mono)" }}>#{i+1}</span></td>
                <td><span className="id-link">{s.id}</span></td>
                <td><ToolBadge tool={s.tool} /></td>
                <td className="mono">
                  {s.model}{s.extra > 0 && <span className="pill" style={{ marginLeft: 4 }}>+{s.extra}</span>}
                </td>
                <td className="num">{s.turns.toLocaleString()}</td>
                <td className="num" style={{ color: i < 3 ? "var(--warn)" : "var(--fg-0)", fontWeight: 600 }}>{O.fmt.$(s.cost, 4)}</td>
                <td>
                  {s.why.map((w, j) => (
                    <span key={j} className="pill" style={{ marginRight: 4, color: "var(--fg-2)" }}>{w}</span>
                  ))}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </ChartShell>

      <div style={{ height: 14 }} />

      {/* === Routing efficiency === */}
      <ChartShell
        title={<span><Icon name="lightbulb" size={14} style={{ color: "var(--warn)" }} /> Routing efficiency — opportunities</span>}
        helpId="chart.routing"
        sub={<span><b style={{ color: "var(--fg-1)" }}>{O.ROUTING.length} flagged</b> · projected savings <b style={{ color: "var(--success)" }}>$0.0689</b> · informational only — model choice may be deliberate</span>}
        pad={false}
      >
        <table className="dtable">
          <thead>
            <tr>
              <th>Session</th>
              <th>Suggested migration</th>
              <th className="num">Current $</th>
              <th className="num">Sonnet $</th>
              <th className="num">Savings</th>
              <th>Reasoning</th>
            </tr>
          </thead>
          <tbody>
            {O.ROUTING.map((r, i) => (
              <tr key={i}>
                <td><span className="id-link">{r.id}</span></td>
                <td className="mono">
                  <span style={{ color: "var(--warn)" }}>{r.from}</span>
                  <Icon name="arrow_r" size={11} style={{ margin: "0 6px", color: "var(--fg-3)" }} />
                  <span style={{ color: "var(--success)" }}>{r.to}</span>
                </td>
                <td className="num">{O.fmt.$(r.current, 4)}</td>
                <td className="num">{O.fmt.$(r.suggested, 4)}</td>
                <td className="num" style={{ color: "var(--success)", fontWeight: 600 }}>−{O.fmt.$(r.savings, 4)}</td>
                <td>{r.reasons.map((re, j) => <span key={j} className="pill" style={{ marginRight: 4 }}>{re}</span>)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </ChartShell>
    </div>
  );
}
window.PageAnalysis = PageAnalysis;
