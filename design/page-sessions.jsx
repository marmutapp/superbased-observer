/* ============================================================
   Sessions — table + slide-over with messages timeline
   ============================================================ */

function PageSessions({ onOpenSession }) {
  const O = window.OBS;
  const [sortKey, setSortKey] = React.useState("started");
  const [view, setView] = React.useState("table");
  const sessions = O.SESSIONS;

  return (
    <div>
      <div className="page-head">
        <div>
          <h1 className="page-title">Sessions</h1>
          <p className="page-desc">
            One row per AI-coding session. Click a row to see action breakdown, token buckets, cost summary, and a full messages timeline with expandable tool calls.
          </p>
        </div>
        <div className="spacer" />
        <Seg options={[
          { value: "table", label: "Table" },
          { value: "calendar", label: "Calendar" }
        ]} value={view} onChange={setView} />
      </div>

      <div className="banner info" style={{ marginBottom: 14 }}>
        <Icon name="info" size={16} className="ico" />
        <div className="banner-body">
          Quality / Errors / Redundancy scoring is hidden — run <code>observer score</code> to populate the columns. Once populated, sessions get a 0–100 quality score and a redundancy index that flags repeat-work patterns.
        </div>
      </div>

      <div className="card">
        <div className="card-head">
          <h3>
            All sessions
            <span className="pill" style={{ marginLeft: 8 }}>{O.STATUS.sessions} total</span>
          </h3>
          <div className="right">
            <div style={{ position: "relative" }}>
              <Icon name="search" size={13} style={{ position: "absolute", left: 9, top: 8, color: "var(--fg-3)" }} />
              <input className="input mono" style={{ paddingLeft: 28, width: 260, fontSize: 11 }} placeholder="filter by id, project…" />
            </div>
            <button className="btn ghost"><Icon name="filter" size={13}/>Filters</button>
            <button className="btn ghost"><Icon name="export" size={13}/>Export</button>
          </div>
        </div>
        <div className="scroll-x" style={{ maxHeight: 580, overflow: "auto" }}>
          <table className="dtable">
            <thead>
              <tr>
                <th>Session</th>
                <th>Tool</th>
                <th>Project</th>
                <th>Model(s)</th>
                <th>Started</th>
                <th className="num sortable" onClick={() => setSortKey("elapsed")}>Elapsed</th>
                <th className="num sortable" onClick={() => setSortKey("actions")}>Actions</th>
                <th className="num">Sub</th>
                <th className="num">Input</th>
                <th className="num">Cache R</th>
                <th className="num">Cache W</th>
                <th className="num">Output</th>
                <th className="num sortable" onClick={() => setSortKey("api")}>API $</th>
                <th className="num">Tool $</th>
                <th className="num sortable" onClick={() => setSortKey("total")}>Total $ {sortKey==="total"&&<span className="arrow">↓</span>}</th>
              </tr>
            </thead>
            <tbody>
              {sessions.map(s => (
                <tr key={s.id} className="clickable" onClick={() => onOpenSession(s)}>
                  <td><span className="id-link">{s.id.slice(0, 12)}…</span></td>
                  <td><ToolBadge tool={s.tool} /></td>
                  <td className="path-mono dim">{s.project}</td>
                  <td>
                    <span className="pill brand" style={{ fontFamily: "var(--font-mono)" }}>
                      {s.tool === "codex" ? "gpt-5.5" : s.tool === "cursor" ? "claude-sonnet-4-6" : "claude-opus-4-7"}
                    </span>
                  </td>
                  <td className="dim mono" style={{ fontSize: 11 }}>{s.started}</td>
                  <td className="num">{s.elapsed}</td>
                  <td className="num">{s.actions}</td>
                  <td className="num dim">{s.subagent || "—"}</td>
                  <td className="num">{s.input ? O.fmt.n(s.input) : "—"}</td>
                  <td className="num">{s.cache_r ? O.fmt.n(s.cache_r) : "—"}</td>
                  <td className="num">{s.cache_w ? O.fmt.n(s.cache_w) : "—"}</td>
                  <td className="num">{s.output ? O.fmt.n(s.output) : "—"}</td>
                  <td className="num">{s.api ? O.fmt.$(s.api, 4) : "—"}</td>
                  <td className="num dim">{s.tool_cost ? O.fmt.$(s.tool_cost, 4) : "—"}</td>
                  <td className="num" style={{ color: s.total >= 50 ? "var(--danger)" : s.total >= 10 ? "var(--warn)" : "var(--fg-0)", fontWeight: 600 }}>
                    {s.total ? O.fmt.$(s.total, 4) : "—"}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <Pager page={1} pages={10} total={491} perPage={50} />
      </div>
    </div>
  );
}

// === Session detail slide-over ===
function SessionDetail({ session, onClose, open }) {
  const O = window.OBS;
  if (!session) return null;

  const messagesData = [
    { i: 1,  role: "user",      model: null,                 in: 1620, out: 0,    elapsed: "—",     tool_time: "—", cost: 0,      preview: "Refactor the compression pipeline so json-erasure runs after diff-stripper, not before" },
    { i: 2,  role: "assistant", model: "claude-opus-4-7",   in: 1620, out: 380,  elapsed: "8.4s",  tool_time: "—", cost: 0.0526, tools: 0, preview: "I'll start by reading the current compression order and the dispatcher…" },
    { i: 3,  role: "tool",      model: "claude-opus-4-7",   in: 12400, out: 0,   elapsed: "0.4s",  tool_time: "—", cost: 0.0186, tools: 3, preview: "read_file", calls: [
      { name: "read_file", target: "internal/compression/conversation/budget.go" },
      { name: "read_file", target: "internal/compression/conversation/dispatcher.go" },
      { name: "read_file", target: "internal/compression/diff/stripper.go" },
    ]},
    { i: 4,  role: "assistant", model: "claude-opus-4-7",   in: 12400, out: 720, elapsed: "11.2s", tool_time: "—", cost: 0.0726, tools: 0, preview: "The dispatcher iterates mechanisms in a fixed order. Reordering means…" },
    { i: 5,  role: "tool",      model: "claude-opus-4-7",   in: 13200, out: 0,   elapsed: "1.1s",  tool_time: "0.8s", cost: 0.0198, tools: 2, preview: "edit_file", calls: [
      { name: "edit_file", target: "internal/compression/conversation/dispatcher.go", diff_lines: 14 },
      { name: "edit_file", target: "internal/compression/conversation/dispatcher_test.go", diff_lines: 22 },
    ]},
    { i: 6,  role: "tool",      model: "claude-opus-4-7",   in: 13800, out: 0,   elapsed: "3.4s",  tool_time: "3.2s", cost: 0.0207, tools: 1, preview: "run_command — failing", error: "FAIL: TestDispatchOrder (1 of 14)", calls: [
      { name: "run_command", target: "go test -race ./internal/compression/...", error: "FAIL" },
    ]},
    { i: 7,  role: "assistant", model: "claude-opus-4-7",   in: 13800, out: 480, elapsed: "9.1s",  tool_time: "—", cost: 0.0567, tools: 0, preview: "The failing test expects the old order. Let me patch the fixture…" },
    { i: 8,  role: "tool",      model: "claude-opus-4-7",   in: 14600, out: 0,   elapsed: "0.6s",  tool_time: "0.4s", cost: 0.0219, tools: 1, preview: "edit_file", calls: [
      { name: "edit_file", target: "internal/compression/conversation/dispatcher_test.go", diff_lines: 8 },
    ]},
    { i: 9,  role: "tool",      model: "claude-opus-4-7",   in: 14900, out: 0,   elapsed: "2.9s",  tool_time: "2.7s", cost: 0.0224, tools: 1, preview: "run_command — passing", calls: [
      { name: "run_command", target: "go test -race ./internal/compression/..." },
    ]},
    { i: 10, role: "assistant", model: "claude-opus-4-7",   in: 14900, out: 220, elapsed: "5.7s",  tool_time: "—", cost: 0.0388, tools: 0, preview: "All green. Want me to commit or keep iterating?" },
  ];
  const [expanded, setExpanded] = React.useState({ 3: true, 5: true, 6: true });

  return (
    <>
      <div className={"slideover-backdrop" + (open ? " open" : "")} onClick={onClose} />
      <aside className={"slideover" + (open ? " open" : "")}>
        <div style={{ padding: "12px 18px", borderBottom: "1px solid var(--line-1)", display: "flex", alignItems: "center", gap: 12, background: "var(--bg-2)" }}>
          <ToolBadge tool={session.tool} />
          <div style={{ display: "flex", flexDirection: "column", lineHeight: 1.2 }}>
            <b style={{ color: "var(--fg-0)", fontFamily: "var(--font-mono)", fontSize: 13 }}>{session.id}</b>
            <span style={{ fontSize: 11, color: "var(--fg-3)" }}>
              <span className="path-mono" style={{ color: "var(--fg-2)" }}>{session.project}</span>
              {session.cowork_proc && <> · cowork-proc <b style={{ color: "var(--tool-cowork)", fontFamily: "var(--font-mono)" }}>{session.cowork_proc}</b></>}
            </span>
          </div>
          <div className="spacer" />
          <button className="icon-btn" title="Copy ID"><Icon name="copy" size={14} /></button>
          <button className="icon-btn" title="Open in new tab"><Icon name="external" size={14} /></button>
          <button className="icon-btn" onClick={onClose}><Icon name="close" size={14} /></button>
        </div>

        <div style={{ flex: 1, overflowY: "auto", padding: "16px 18px 32px", background: "var(--bg-0)" }}>
          {/* === Meta strip === */}
          <div className="grid g-4" style={{ marginBottom: 14 }}>
            <StatCard label="Total Cost" icon="cost" accent
              value={O.fmt.$(session.total, 4)} sub={`api ${O.fmt.$(session.api, 4)} + tool ${O.fmt.$(session.tool_cost, 4)}`} />
            <StatCard label="Actions" icon="actions"
              value={session.actions} sub={`${session.subagent || 0} sub-agent`} />
            <StatCard label="Elapsed" icon="clock"
              value={session.elapsed} sub={`started ${session.started.slice(11)}`} />
            <StatCard label="Tokens" icon="layers"
              value={O.fmt.n((session.input||0) + (session.cache_r||0) + (session.cache_w||0) + (session.output||0), 2)} sub="net+cache+output" />
          </div>

          {/* === Two side-by-side charts === */}
          <div className="grid g-2" style={{ marginBottom: 14 }}>
            <ChartShell title="Action breakdown" sub={`${session.actions} actions across this session`}>
              <Donut size={140} thickness={20}
                centerValue={session.actions} centerLabel="ACTIONS"
                data={[
                  { label: "read_file",  value: session.actions * 0.32, color: "var(--act-file)" },
                  { label: "edit_file",  value: session.actions * 0.18, color: "var(--tok-net)" },
                  { label: "run_command",value: session.actions * 0.21, color: "var(--act-cmd)" },
                  { label: "search",     value: session.actions * 0.12, color: "var(--act-search)" },
                  { label: "subagent",   value: session.actions * 0.09, color: "var(--act-agent)" },
                  { label: "other",      value: session.actions * 0.08, color: "var(--fg-3)" },
                ]}
              />
            </ChartShell>
            <ChartShell title="Token buckets" sub="net input · cache read · cache write · output">
              <HBarChart
                data={[
                  { label: "Net Input",   value: session.input || 0,   color: "var(--tok-net)" },
                  { label: "Cache Read",  value: session.cache_r || 0, color: "var(--tok-read)" },
                  { label: "Cache Write", value: session.cache_w || 0, color: "var(--tok-write)" },
                  { label: "Output",      value: session.output || 0,  color: "var(--tok-out)" },
                ]}
                labelKey="label" valueKey="value"
                color={r => r.color}
                fmt={v => O.fmt.n(v)}
              />
            </ChartShell>
          </div>

          {/* === Messages timeline === */}
          <ChartShell title="Messages" sub={`${messagesData.length} messages · expandable tool calls inline`} pad={false}>
            <table className="dtable">
              <thead>
                <tr>
                  <th style={{ width: 36 }}>#</th>
                  <th>Role</th>
                  <th>Model</th>
                  <th className="num">In</th>
                  <th className="num">Out</th>
                  <th className="num">Elapsed</th>
                  <th className="num">Tool</th>
                  <th className="num">Cost</th>
                  <th>Content</th>
                </tr>
              </thead>
              <tbody>
                {messagesData.map(m => (
                  <React.Fragment key={m.i}>
                    <tr className={m.role === "tool" ? "" : "clickable"} onClick={() => m.tools > 0 && setExpanded({ ...expanded, [m.i]: !expanded[m.i] })}>
                      <td style={{ color: "var(--fg-3)", fontFamily: "var(--font-mono)" }}>{m.i}</td>
                      <td>
                        <span className="pill" style={{
                          color: m.role === "user" ? "var(--accent)" : m.role === "tool" ? "var(--warn)" : "var(--success)",
                          background: m.role === "user" ? "var(--accent-soft)" : m.role === "tool" ? "var(--warn-soft)" : "var(--success-soft)",
                          borderColor: "transparent"
                        }}>{m.role}</span>
                      </td>
                      <td className="mono dim" style={{ fontSize: 11 }}>{m.model || "—"}</td>
                      <td className="num">{m.in ? O.fmt.n(m.in) : "—"}</td>
                      <td className="num">{m.out ? O.fmt.n(m.out) : "—"}</td>
                      <td className="num">{m.elapsed}</td>
                      <td className="num">{m.tool_time}</td>
                      <td className="num">{m.cost ? O.fmt.$(m.cost, 4) : "—"}</td>
                      <td style={{ maxWidth: 460, overflow: "hidden" }}>
                        <span style={{ color: m.error ? "var(--danger)" : "var(--fg-1)" }}>
                          {m.error && <Icon name="warn" size={12} style={{ color: "var(--danger)", marginRight: 4, verticalAlign: -1 }} />}
                          {m.preview}
                        </span>
                        {m.tools > 0 && (
                          <span className="pill" style={{ marginLeft: 8, cursor: "pointer", background: "var(--bg-3)" }}>
                            × {m.tools} <Icon name={expanded[m.i] ? "chevron_u" : "chevron_d"} size={10} />
                          </span>
                        )}
                      </td>
                    </tr>
                    {m.tools > 0 && expanded[m.i] && m.calls && (
                      <tr style={{ background: "var(--bg-1)" }}>
                        <td colSpan="9" style={{ padding: "8px 10px 12px 50px" }}>
                          <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
                            {m.calls.map((c, j) => (
                              <div key={j} style={{ display: "flex", alignItems: "center", gap: 10, padding: "6px 10px", background: "var(--bg-2)", borderRadius: 4, borderLeft: `2px solid ${c.error ? "var(--danger)" : "var(--accent)"}` }}>
                                <span className="pill" style={{ fontFamily: "var(--font-mono)", color: "var(--act-cmd)" }}>{c.name}</span>
                                <span className="path-mono" style={{ color: "var(--fg-2)" }}>{c.target}</span>
                                {c.diff_lines && <span className="pill" style={{ fontSize: 10 }}>+{c.diff_lines} lines</span>}
                                {c.error && <span className="pill danger">{c.error}</span>}
                              </div>
                            ))}
                          </div>
                        </td>
                      </tr>
                    )}
                  </React.Fragment>
                ))}
              </tbody>
            </table>
          </ChartShell>
        </div>
      </aside>
    </>
  );
}

Object.assign(window, { PageSessions, SessionDetail });
