/* ============================================================
   Cost — by-model table (the densest table in the product),
   token volume charts, Cowork reconciliation
   ============================================================ */
function PageCost() {
  const O = window.OBS;
  const [sortKey, setSortKey] = React.useState("cost");
  const sorted = [...O.TOP_MODELS].sort((a, b) => b[sortKey] - a[sortKey]);
  const maxNet = Math.max(...O.TOP_MODELS.map(m => m.net));
  const maxRead = Math.max(...O.TOP_MODELS.map(m => m.read));
  const maxWrite = Math.max(...O.TOP_MODELS.map(m => m.write));
  const maxOut = Math.max(...O.TOP_MODELS.map(m => m.output));
  const maxCost = Math.max(...O.TOP_MODELS.map(m => m.cost));

  const tokenSeries = [
    { key: "net_input",   label: "Net Input",   color: "var(--tok-net)" },
    { key: "cache_read",  label: "Cache Read",  color: "var(--tok-read)" },
    { key: "cache_write", label: "Cache Write", color: "var(--tok-write)" },
    { key: "output",      label: "Output",      color: "var(--tok-out)" },
  ];

  const byModelSeries = [
    { key: "opus-7",   label: "claude-opus-4-7",   color: "#7c9eff" },
    { key: "opus-6",   label: "claude-opus-4-6",   color: "#a78bfa" },
    { key: "haiku",    label: "claude-haiku-4-5",  color: "#fbbf24" },
    { key: "gemini",   label: "gemini-3.1-pro",    color: "#ec4899" },
    { key: "gpt",      label: "gpt-5.5",           color: "#10b981" },
    { key: "sonnet",   label: "claude-sonnet-4-6", color: "#60a5fa" },
    { key: "other",    label: "other",             color: "var(--fg-3)" },
  ];
  const byModelData = O.COST_TS.map(d => ({
    date: d.date,
    "opus-7":  d.total_tokens_m * 0.92,
    "opus-6":  d.total_tokens_m * 0.045,
    "haiku":   d.total_tokens_m * 0.020,
    "gemini":  d.total_tokens_m * 0.008,
    "gpt":     d.total_tokens_m * 0.004,
    "sonnet":  d.total_tokens_m * 0.002,
    "other":   d.total_tokens_m * 0.001,
  }));

  return (
    <div>
      <div className="page-head">
        <div>
          <h1 className="page-title">Cost</h1>
          <p className="page-desc">
            Per-model token consumption split into the four billable buckets — net input, cache read, cache write, output — with computed dollar cost. Hover any column header for the formula.
          </p>
        </div>
      </div>

      {/* === Summary row === */}
      <div className="grid g-6" style={{ marginBottom: 14 }}>
        <StatCard label="Total Cost"  helpId="tile.spend" icon="cost" accent
          value={O.fmt.$(O.ANALYSIS.spend_window, 2)} delta={O.ANALYSIS.spend_delta_pct/100} deltaLabel="vs prior 30d" />
        <StatCard label="API Turns"   icon="bolt"
          value={O.fmt.n(33113)} sub={`$${(O.ANALYSIS.spend_window/33113).toFixed(4)} mean`} />
        <StatCard label="Net Input"   icon="layers" helpId="col.net_input"
          value={O.fmt.n(5.28e6, 2)} unit="tok" sub="5.28M total" />
        <StatCard label="Cache Read"  icon="layers" helpId="col.cache_r"
          value={O.fmt.n(8.14e9, 2)} unit="tok" sub="8.14B · billed @10×↓" />
        <StatCard label="Cache Write" icon="layers"
          value={O.fmt.n(133.35e6, 2)} unit="tok" sub="133.35M · pays off >2 reads" />
        <StatCard label="Output"      icon="layers"
          value={O.fmt.n(28.47e6, 2)} unit="tok" sub="28.47M total" />
      </div>

      {/* === Cost projection banner === */}
      <div className="banner warn" style={{ marginBottom: 14 }}>
        <Icon name="trend_up" size={18} className="ico" />
        <div className="banner-body">
          <b>3 models have unreliable pricing.</b> Without ground-truth $ from the proxy, cost on those rows is interpolated from the pricing table — actual invoice may drift. Run the proxy to upgrade reliability.
          {" "}<span style={{ color: "var(--fg-3)" }}>3 model(s) without pricing · projected month spend <b style={{ color: "var(--fg-0)" }}>{O.fmt.$(O.ANALYSIS.projection_usd, 2)}</b></span>
        </div>
      </div>

      {/* === The big table === */}
      <div className="card" style={{ marginBottom: 14 }}>
        <div className="card-head">
          <h3>Cost — by model<HelpInd id="chart.cost_table" /></h3>
          <div className="sub">
            <span>13 models · <b style={{ color: "var(--fg-1)" }}>{O.fmt.$(O.TOTAL_COST, 2)}</b> · 33,113 turns</span>
          </div>
          <div className="right">
            <button className="btn ghost"><Icon name="filter" size={13}/>Group</button>
            <button className="btn ghost"><Icon name="export" size={13}/>Export</button>
          </div>
        </div>
        <div className="scroll-x" style={{ maxHeight: 560, overflow: "auto" }}>
          <table className="dtable zebra">
            <thead>
              <tr>
                <th>Model<HelpInd id="col.model" /></th>
                <th className="num sortable">Net In %</th>
                <th className="num sortable">Cache Rd %</th>
                <th className="num sortable">Cache Wr %</th>
                <th className="num sortable">Output %</th>
                <th className="num sortable" onClick={() => setSortKey("net")}>Net Input {sortKey==="net"&&<span className="arrow">↓</span>}</th>
                <th className="num sortable" onClick={() => setSortKey("read")}>Cache Read {sortKey==="read"&&<span className="arrow">↓</span>}</th>
                <th className="num sortable" onClick={() => setSortKey("write")}>Cache Write {sortKey==="write"&&<span className="arrow">↓</span>}</th>
                <th className="num sortable" onClick={() => setSortKey("output")}>Output {sortKey==="output"&&<span className="arrow">↓</span>}</th>
                <th className="num">Reasoning</th>
                <th className="num sortable" onClick={() => setSortKey("cost")}>Cost {sortKey==="cost"&&<span className="arrow">↓</span>}</th>
                <th className="num">Turns</th>
                <th>Source</th>
                <th>Reliab.<HelpInd id="col.reliability"/></th>
              </tr>
            </thead>
            <tbody>
              {sorted.map((m, i) => {
                const total = m.net + m.read + m.write + m.output || 1;
                return (
                  <tr key={i} className="clickable">
                    <td className="mono" style={{ color: "var(--fg-0)", fontWeight: 500 }}>{m.model}</td>
                    <td className="num">
                      <div className="cellbar">
                        <div className="bar"><div style={{ width: `${(m.net/total)*100}%`, background: "var(--tok-net)" }}/></div>
                        {((m.net/total)*100).toFixed(1)}%
                      </div>
                    </td>
                    <td className="num">
                      <div className="cellbar">
                        <div className="bar"><div style={{ width: `${(m.read/total)*100}%`, background: "var(--tok-read)" }}/></div>
                        {((m.read/total)*100).toFixed(1)}%
                      </div>
                    </td>
                    <td className="num">
                      <div className="cellbar">
                        <div className="bar"><div style={{ width: `${(m.write/total)*100}%`, background: "var(--tok-write)" }}/></div>
                        {((m.write/total)*100).toFixed(1)}%
                      </div>
                    </td>
                    <td className="num">
                      <div className="cellbar">
                        <div className="bar"><div style={{ width: `${(m.output/total)*100}%`, background: "var(--tok-out)" }}/></div>
                        {((m.output/total)*100).toFixed(1)}%
                      </div>
                    </td>
                    <td className="num">{O.fmt.n(m.net, 1)}</td>
                    <td className="num">{O.fmt.n(m.read, 2)}</td>
                    <td className="num">{O.fmt.n(m.write, 2)}</td>
                    <td className="num">{O.fmt.n(m.output, 1)}</td>
                    <td className="num dim">{m.reasoning ? O.fmt.n(m.reasoning) : "—"}</td>
                    <td className="num" style={{ color: "var(--fg-0)", fontWeight: 600 }}>{O.fmt.$(m.cost, 4)}</td>
                    <td className="num">{m.turns.toLocaleString()}</td>
                    <td><span className="pill" style={{ textTransform: "none", fontFamily: "var(--font-mono)" }}>{m.source}</span></td>
                    <td><ReliabilityPill v={m.reliability} /></td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      </div>

      {/* === Token volume charts === */}
      <div className="grid g-2" style={{ marginBottom: 14 }}>
        <ChartShell
          title="Token volume per day"
          helpId="chart.tokens_daily"
          sub="four billing buckets · 31 days"
        >
          <StackedBar
            data={O.COST_TS.map(d => ({
              date: d.date,
              net_input: d.net_input,
              cache_read: d.cache_read,
              cache_write: d.cache_write,
              output: d.output,
            }))}
            series={tokenSeries}
            height={220}
            yFmt={v => O.fmt.n(v*1e6, 2) === "0" ? "0" : O.fmt.n(v*1e6, 2)}
          />
        </ChartShell>
        <ChartShell
          title="Token volume per day — by model"
          helpId="chart.tokens_by_model"
          sub="top 6 models + other · 31 days"
        >
          <StackedBar
            data={byModelData}
            series={byModelSeries}
            height={220}
            yFmt={v => O.fmt.n(v*1e6, 2) === "0" ? "0" : O.fmt.n(v*1e6, 2)}
          />
        </ChartShell>
      </div>

      {/* === Cowork reconciliation === */}
      <div className="card">
        <div className="card-head">
          <h3>
            <Icon name="link" size={14} style={{ color: "var(--tool-cowork)" }}/>
            Cowork — cost reconciliation
            <HelpInd id="chart.cowork_recon" />
          </h3>
          <div className="sub">
            Observer's derived cost vs Cowork's authoritative <code style={{ background: "var(--bg-3)", padding: "1px 5px", borderRadius: 3, fontSize: 11 }}>result.total_cost_usd</code> · threshold 5%
          </div>
        </div>
        <div className="card-pad">
          <div className="grid g-4" style={{ marginBottom: 12 }}>
            <StatCard label="Sessions" value={O.COWORK.sessions} sub="cowork-tagged in window" />
            <StatCard label="Over Threshold" value={O.COWORK.over_threshold} sub="drift ≥ 5%" warn />
            <StatCard label="Cowork Total"  value={O.fmt.$(O.COWORK.cowork_total, 4)} sub="authoritative" />
            <StatCard label="Derived Total" value={O.fmt.$(O.COWORK.derived_total, 4)} sub={<span>drift <b style={{ color: O.COWORK.overall_drift >= 0 ? "var(--success)" : "var(--danger)" }}>{O.COWORK.overall_drift > 0 ? "+" : ""}{O.fmt.$(O.COWORK.overall_drift, 4)} ({(O.COWORK.overall_drift_pct*100).toFixed(1)}%)</b></span>} />
          </div>
          <table className="dtable">
            <thead>
              <tr>
                <th>Session</th>
                <th className="num">Cowork $</th>
                <th className="num">Derived $</th>
                <th className="num">Drift $</th>
                <th className="num">Drift %</th>
              </tr>
            </thead>
            <tbody>
              {O.COWORK.rows.map((r, i) => (
                <tr key={i}>
                  <td>{r.title} <Icon name="warn" size={12} style={{ color: "var(--warn)", marginLeft: 4 }} /></td>
                  <td className="num">{O.fmt.$(r.cowork, 4)}</td>
                  <td className="num">{O.fmt.$(r.derived, 4)}</td>
                  <td className="num" style={{ color: r.drift >= 0 ? "var(--success)" : "var(--danger)" }}>{r.drift > 0 ? "+" : ""}{O.fmt.$(r.drift, 4)}</td>
                  <td className="num" style={{ color: r.pct >= 0.20 ? "var(--danger)" : r.pct >= 0.05 ? "var(--warn)" : "var(--success)", fontWeight: 600 }}>
                    {(r.pct*100).toFixed(1)}%
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          <div style={{ marginTop: 10, fontSize: 11, color: "var(--fg-3)" }}>
            Showing top 5 rows by absolute drift. Fast targeted backfill: <code style={{ background: "var(--bg-3)", padding: "1px 5px", borderRadius: 3 }}>observer scan --force --adapter cowork</code> (only the cowork audit.jsonl files — seconds, not minutes).
          </div>
        </div>
      </div>
    </div>
  );
}
window.PageCost = PageCost;
