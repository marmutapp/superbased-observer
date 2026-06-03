/* ============================================================
   Actions — event log / audit trail with faceted filters
   ============================================================ */
function PageActions() {
  const O = window.OBS;
  const [type, setType] = React.useState("all");
  const [effort, setEffort] = React.useState("all");
  const [perm, setPerm] = React.useState("all");
  const [intOnly, setIntOnly] = React.useState(false);
  const [aiOnly, setAiOnly] = React.useState(false);
  const [view, setView] = React.useState("table");

  const allTypes = Object.keys(O.ACTION_TYPES);

  return (
    <div>
      <div className="page-head">
        <div>
          <h1 className="page-title">Actions</h1>
          <p className="page-desc">
            The flat firehose — every recorded tool call, normalised across adapters. Filter by type, effort, permission mode, and source.
          </p>
        </div>
        <div className="spacer" />
        <Seg options={[
          { value: "table", label: "Table" },
          { value: "timeline", label: "Timeline" }
        ]} value={view} onChange={setView} />
      </div>

      <div className="grid g-12" style={{ gap: 12 }}>
        {/* === Faceted filter sidebar === */}
        <div className="col-3" style={{ display: "flex", flexDirection: "column", gap: 10 }}>
          <div className="card card-pad">
            <div style={{ fontSize: 11, textTransform: "uppercase", letterSpacing: "0.08em", color: "var(--fg-3)", fontWeight: 600, marginBottom: 10 }}>
              Filters
              <span className="pill" style={{ marginLeft: 8 }}>78,159 total</span>
            </div>

            <div style={{ marginBottom: 14 }}>
              <div style={{ fontSize: 10, textTransform: "uppercase", letterSpacing: "0.08em", color: "var(--fg-2)", fontWeight: 600, marginBottom: 6 }}>Action type</div>
              <div style={{ maxHeight: 240, overflowY: "auto", display: "flex", flexDirection: "column", gap: 2 }}>
                <div
                  onClick={() => setType("all")}
                  style={{ display: "flex", justifyContent: "space-between", padding: "5px 8px", borderRadius: 4, cursor: "pointer", background: type === "all" ? "var(--bg-3)" : "transparent", fontSize: 12, color: type === "all" ? "var(--fg-0)" : "var(--fg-1)" }}>
                  <span>All types</span>
                  <span className="dim" style={{ fontFamily: "var(--font-mono)", fontSize: 11 }}>78.2k</span>
                </div>
                {allTypes.map(t => {
                  const a = O.ACTION_TYPES[t];
                  const count = Math.round(Math.random() * 8000) + 50;
                  return (
                    <div key={t}
                      onClick={() => setType(t)}
                      style={{ display: "flex", justifyContent: "space-between", alignItems: "center", padding: "5px 8px", borderRadius: 4, cursor: "pointer", background: type === t ? "var(--bg-3)" : "transparent", fontSize: 12, color: type === t ? "var(--fg-0)" : "var(--fg-1)" }}>
                      <span style={{ display: "flex", alignItems: "center", gap: 6 }}>
                        <span style={{ width: 6, height: 6, borderRadius: 50, background: a.color }} />
                        <span className="mono" style={{ fontSize: 11 }}>{t}</span>
                      </span>
                      <span className="dim" style={{ fontFamily: "var(--font-mono)", fontSize: 11 }}>{O.fmt.n(count)}</span>
                    </div>
                  );
                })}
              </div>
            </div>

            <div style={{ marginBottom: 14 }}>
              <div style={{ fontSize: 10, textTransform: "uppercase", letterSpacing: "0.08em", color: "var(--fg-2)", fontWeight: 600, marginBottom: 6 }}>Effort</div>
              <div style={{ display: "flex", flexWrap: "wrap", gap: 4 }}>
                {["all","minimal","low","medium","high","xhigh","max"].map(e => (
                  <button key={e}
                    onClick={() => setEffort(e)}
                    className="pill"
                    style={{
                      cursor: "pointer",
                      background: effort === e ? "var(--accent-soft)" : "var(--bg-3)",
                      color: effort === e ? "var(--accent)" : "var(--fg-2)",
                      borderColor: effort === e ? "var(--accent)" : "var(--line-2)",
                    }}>{e}</button>
                ))}
              </div>
            </div>

            <div style={{ marginBottom: 14 }}>
              <div style={{ fontSize: 10, textTransform: "uppercase", letterSpacing: "0.08em", color: "var(--fg-2)", fontWeight: 600, marginBottom: 6 }}>Permission mode</div>
              <div style={{ display: "flex", flexWrap: "wrap", gap: 4 }}>
                {["all","default","plan","acceptEdits","auto","dontAsk","bypass"].map(p => (
                  <button key={p}
                    onClick={() => setPerm(p)}
                    className="pill"
                    style={{
                      cursor: "pointer",
                      background: perm === p ? "var(--accent-soft)" : "var(--bg-3)",
                      color: perm === p ? "var(--accent)" : "var(--fg-2)",
                      borderColor: perm === p ? "var(--accent)" : "var(--line-2)",
                    }}>{p}</button>
                ))}
              </div>
            </div>

            <div style={{ borderTop: "1px solid var(--line-1)", paddingTop: 10, display: "flex", flexDirection: "column", gap: 8 }}>
              <label className="check">
                <input type="checkbox" checked={intOnly} onChange={e => setIntOnly(e.target.checked)} />
                <span className="box" />
                Interrupted only
              </label>
              <label className="check">
                <input type="checkbox" checked={aiOnly} onChange={e => setAiOnly(e.target.checked)} />
                <span className="box" />
                AI messages only
              </label>
            </div>

            {(type !== "all" || effort !== "all" || perm !== "all" || intOnly || aiOnly) && (
              <div style={{ marginTop: 10, paddingTop: 10, borderTop: "1px solid var(--line-1)" }}>
                <div style={{ fontSize: 10, textTransform: "uppercase", letterSpacing: "0.08em", color: "var(--fg-3)", fontWeight: 600, marginBottom: 6 }}>Active filters</div>
                <div style={{ display: "flex", flexWrap: "wrap", gap: 4 }}>
                  {type !== "all" && <span className="pill" style={{ background: "var(--accent-soft)", color: "var(--accent)", borderColor: "var(--accent)", cursor: "pointer" }} onClick={() => setType("all")}>type: {type} <Icon name="close" size={9} /></span>}
                  {effort !== "all" && <span className="pill" style={{ background: "var(--accent-soft)", color: "var(--accent)", borderColor: "var(--accent)", cursor: "pointer" }} onClick={() => setEffort("all")}>effort: {effort} <Icon name="close" size={9} /></span>}
                  {perm !== "all" && <span className="pill" style={{ background: "var(--accent-soft)", color: "var(--accent)", borderColor: "var(--accent)", cursor: "pointer" }} onClick={() => setPerm("all")}>perm: {perm} <Icon name="close" size={9} /></span>}
                  {intOnly && <span className="pill" style={{ background: "var(--accent-soft)", color: "var(--accent)", borderColor: "var(--accent)", cursor: "pointer" }} onClick={() => setIntOnly(false)}>interrupted <Icon name="close" size={9} /></span>}
                  {aiOnly && <span className="pill" style={{ background: "var(--accent-soft)", color: "var(--accent)", borderColor: "var(--accent)", cursor: "pointer" }} onClick={() => setAiOnly(false)}>AI only <Icon name="close" size={9} /></span>}
                </div>
              </div>
            )}
          </div>
        </div>

        {/* === Log viewer === */}
        <div className="col-9">
          <div className="card">
            <div className="card-head">
              <h3>Event log<HelpInd id="chart.actions_log" /></h3>
              <div className="sub">live tail · 78,159 events · streaming</div>
              <div className="right">
                <span className="pill"><span className="dot ok" /> Live</span>
                <button className="btn ghost"><Icon name="pause" size={12} /></button>
              </div>
            </div>

            {view === "table" ? (
              <div className="scroll-x" style={{ maxHeight: 620, overflow: "auto" }}>
                <table className="dtable">
                  <thead>
                    <tr>
                      <th>When</th>
                      <th>Tool</th>
                      <th>Type</th>
                      <th>Effort</th>
                      <th>Target</th>
                      <th>Content</th>
                      <th>Session</th>
                      <th></th>
                    </tr>
                  </thead>
                  <tbody>
                    {O.ACTIONS.map((a, i) => {
                      const meta = O.ACTION_TYPES[a.type] || {};
                      return (
                        <tr key={i} className="clickable">
                          <td className="mono dim" style={{ fontSize: 11, whiteSpace: "nowrap" }}>{a.when}</td>
                          <td><ProviderIcon tool={a.tool} size={18} /></td>
                          <td>
                            <span className="pill" style={{
                              fontFamily: "var(--font-mono)", textTransform: "none",
                              color: meta.color, background: "rgba(255,255,255,0.03)",
                              borderColor: "var(--line-2)"
                            }}>
                              <span className="sw" style={{ width: 4, height: 4, background: meta.color }}/>
                              {a.type}
                            </span>
                          </td>
                          <td><span className="pill" style={{ fontSize: 10 }}><Icon name="bolt" size={9} style={{ color: "var(--warn)" }}/>{a.effort}</span></td>
                          <td className="mono" style={{ fontSize: 11, color: "var(--fg-1)", whiteSpace: "nowrap", maxWidth: 200, overflow: "hidden", textOverflow: "ellipsis" }}>{a.target}</td>
                          <td style={{ maxWidth: 380, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap", color: a.ok ? "var(--fg-1)" : "var(--danger)" }}>
                            {!a.ok && <Icon name="warn" size={11} style={{ color: "var(--danger)", marginRight: 4 }} />}
                            {a.msg}
                          </td>
                          <td><span className="id-link">{a.session.slice(0, 9)}…</span></td>
                          <td>{a.ok ? <Icon name="check" size={13} style={{ color: "var(--success)" }}/> : <Icon name="x" size={13} style={{ color: "var(--danger)" }}/>}</td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            ) : (
              <div style={{ padding: 16, maxHeight: 620, overflow: "auto" }}>
                {O.ACTIONS.map((a, i) => {
                  const meta = O.ACTION_TYPES[a.type] || {};
                  return (
                    <div key={i} style={{ display: "grid", gridTemplateColumns: "120px 1fr", gap: 14, marginBottom: 12 }}>
                      <div style={{ fontSize: 11, color: "var(--fg-3)", fontFamily: "var(--font-mono)", textAlign: "right", paddingTop: 8 }}>{a.when.split(" ")[1]}</div>
                      <div style={{ borderLeft: `2px solid ${meta.color}`, paddingLeft: 12, paddingBottom: 6, position: "relative" }}>
                        <div style={{ position: "absolute", left: -5, top: 10, width: 8, height: 8, borderRadius: 50, background: meta.color }}/>
                        <div style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 4 }}>
                          <ToolBadge tool={a.tool} />
                          <span className="pill" style={{ fontFamily: "var(--font-mono)", color: meta.color, textTransform: "none" }}>{a.type}</span>
                          <span className="pill" style={{ fontSize: 10 }}>{a.effort}</span>
                          <span className="id-link" style={{ marginLeft: "auto" }}>{a.session.slice(0, 9)}…</span>
                        </div>
                        <div className="mono" style={{ fontSize: 11, color: "var(--fg-2)", marginBottom: 4 }}>{a.target}</div>
                        <div style={{ fontSize: 12, color: a.ok ? "var(--fg-1)" : "var(--danger)" }}>{a.msg}</div>
                      </div>
                    </div>
                  );
                })}
              </div>
            )}
            <Pager page={1} pages={1564} total={78159} perPage={50} />
          </div>
        </div>
      </div>
    </div>
  );
}
window.PageActions = PageActions;
