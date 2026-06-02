/* ============================================================
   Discovery — waste hero, stale re-reads, repeated commands, cross-tool
   ============================================================ */
function PageDiscovery() {
  const O = window.OBS;
  const S = O.STALE_REREADS;
  const maxReads = Math.max(...S.rows.map(r => r.reads));
  const maxRuns = Math.max(...O.REPEATED_CMDS.map(r => r.runs));

  return (
    <div>
      <div className="page-head">
        <div>
          <h1 className="page-title">Discovery</h1>
          <p className="page-desc">
            Waste detection. Files re-read after they changed inside the same session, commands run repeatedly with no relevant change, and cross-tool overlap. The observer reports — it doesn't auto-prevent.
          </p>
        </div>
      </div>

      {/* === Waste hero === */}
      <div className="card" style={{ marginBottom: 14, position: "relative", overflow: "hidden" }}>
        <div style={{ position: "absolute", inset: 0, background: "radial-gradient(circle at 15% 30%, rgba(248, 113, 113, 0.10), transparent 60%)", pointerEvents: "none" }} />
        <div style={{ padding: 24, display: "flex", alignItems: "flex-end", gap: 32, position: "relative" }}>
          <div>
            <div style={{ fontSize: 11, textTransform: "uppercase", letterSpacing: "0.08em", color: "var(--fg-3)", fontWeight: 600 }}>
              Estimated waste — last 30 days
            </div>
            <div style={{ fontSize: 48, fontWeight: 700, color: "var(--danger)", letterSpacing: "-0.03em", lineHeight: 1, fontFamily: "var(--font-display)", margin: "6px 0" }}>
              {O.fmt.$(S.dollars_wasted, 4)}
            </div>
            <div style={{ fontSize: 12, color: "var(--fg-2)" }}>
              <b style={{ color: "var(--fg-0)" }}>{O.fmt.n(S.count)}</b> stale re-reads ·{" "}
              <b style={{ color: "var(--fg-0)" }}>{S.files}</b> unique files ·{" "}
              <b style={{ color: "var(--fg-0)" }}>{S.cross_thread}</b> cross-thread reads ·{" "}
              same-session scoped (cross-session reads excluded)
            </div>
          </div>
          <div style={{ flex: 1, display: "grid", gridTemplateColumns: "repeat(4, 1fr)", gap: 12 }}>
            <StatCard label="Stale re-reads" value={O.fmt.n(S.count)} sub={`${S.cross_thread} cross-thread (parent ↔ sub-agent)`} warn />
            <StatCard label="~Tokens wasted" value={<span style={{ color: "var(--danger)" }}>{O.fmt.n(S.tokens_wasted, 2)}</span>} sub="estimated, file_size ÷ 4" />
            <StatCard label="~$ wasted" value={<span style={{ color: "var(--danger)" }}>{O.fmt.$(S.dollars_wasted, 4)}</span>} sub={`@ $${S.blended_rate.toFixed(2)}/M blended input rate`} />
            <StatCard label="Affected files" value={S.files} sub="top 12 shown below" />
          </div>
        </div>
      </div>

      {/* === Stale re-reads table === */}
      <ChartShell
        title={<span>Top files re-read <span className="pill warn">same-session only</span></span>}
        helpId="chart.stale_reads"
        sub={<span>Files re-read after they changed inside the same session burned prompt tokens to re-discover what the AI should still have had in context. <b>The fix is upstream</b> — cache_control on hot files, shorter sessions, scoping context.</span>}
        pad={false}
      >
        <div className="scroll-x" style={{ maxHeight: 380, overflow: "auto" }}>
          <table className="dtable zebra">
            <thead>
              <tr>
                <th>File</th>
                <th>Project</th>
                <th>Reads (severity)</th>
                <th className="num">Reads</th>
                <th className="num">Stale</th>
                <th className="num">Cross-thread</th>
                <th className="num">Est wasted tokens</th>
              </tr>
            </thead>
            <tbody>
              {S.rows.map((r, i) => (
                <tr key={i}>
                  <td className="path-mono">{r.file}</td>
                  <td><span className="pill" style={{ fontFamily: "var(--font-mono)", color: "var(--accent)", background: "var(--accent-soft)", borderColor: "transparent" }}>{r.project}</span></td>
                  <td style={{ width: 200 }}>
                    <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
                      <div style={{ flex: 1, height: 6, background: "var(--bg-4)", borderRadius: 3, overflow: "hidden" }}>
                        <div style={{ width: `${(r.reads/maxReads)*100}%`, height: "100%", background: r.reads > 200 ? "var(--danger)" : r.reads > 100 ? "var(--warn)" : "var(--info)" }}/>
                      </div>
                    </div>
                  </td>
                  <td className="num">{r.reads}</td>
                  <td className="num" style={{ color: r.stale > 100 ? "var(--danger)" : "var(--warn)", fontWeight: 600 }}>{r.stale}</td>
                  <td className="num" style={{ color: r.cross > 20 ? "var(--danger)" : r.cross > 0 ? "var(--warn)" : "var(--fg-3)" }}>{r.cross}</td>
                  <td className="num">{O.fmt.n(r.wasted)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <Pager page={1} pages={13} total={249} perPage={20} />
      </ChartShell>

      <div style={{ height: 14 }} />

      {/* === Repeated commands === */}
      <ChartShell
        title="Repeated commands"
        helpId="chart.repeated_cmds"
        sub="Commands run multiple times with no relevant inputs changed in between — usually a sign the model didn't trust its own previous result. Each rerun pays output tokens for an answer that hasn't changed."
        pad={false}
      >
        <table className="dtable">
          <thead>
            <tr>
              <th>Command</th>
              <th>Project</th>
              <th>Frequency</th>
              <th className="num">Runs</th>
              <th className="num">No-change reruns</th>
              <th className="num">Failed</th>
            </tr>
          </thead>
          <tbody>
            {O.REPEATED_CMDS.map((r, i) => (
              <tr key={i}>
                <td><code style={{ background: "var(--bg-3)", padding: "2px 6px", borderRadius: 3, fontSize: 11, fontFamily: "var(--font-mono)", color: "var(--accent)" }}>{r.cmd}</code></td>
                <td><span className="pill" style={{ fontFamily: "var(--font-mono)", color: "var(--accent)" }}>{r.project}</span></td>
                <td style={{ width: 160 }}>
                  <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
                    <div style={{ flex: 1, height: 4, background: "var(--bg-4)", borderRadius: 2, overflow: "hidden" }}>
                      <div style={{ width: `${(r.runs/maxRuns)*100}%`, height: "100%", background: "var(--warn)" }}/>
                    </div>
                  </div>
                </td>
                <td className="num" style={{ color: "var(--fg-0)", fontWeight: 600 }}>{r.runs}</td>
                <td className="num">
                  <span style={{ color: r.no_change > 0 ? "var(--warn)" : "var(--fg-3)" }}>
                    {r.no_change}/{r.runs}
                  </span>
                </td>
                <td className="num" style={{ color: r.failed > 0 ? "var(--danger)" : "var(--fg-3)" }}>{r.failed}</td>
              </tr>
            ))}
          </tbody>
        </table>
        <Pager page={1} pages={25} total={500} perPage={20} />
      </ChartShell>

      <div style={{ height: 14 }} />

      {/* === Cross-tool overlap === */}
      <ChartShell
        title={<span>Cross-tool overlap <span className="pill brand">multi-client</span></span>}
        helpId="chart.cross_tool"
        sub={<span>Files touched by <b>more than one</b> AI client in the selected window. The visible side of cross-platform tool-call sharing — every MCP query (<code style={{ background: "var(--bg-3)", padding: "1px 4px", borderRadius: 3, fontSize: 11 }}>get_file_history</code>, <code style={{ background: "var(--bg-3)", padding: "1px 4px", borderRadius: 3, fontSize: 11 }}>check_file_freshness</code>, <code style={{ background: "var(--bg-3)", padding: "1px 4px", borderRadius: 3, fontSize: 11 }}>get_last_test_result</code>) pulls from the same database, so when one agent reads a file, the next agent's query sees it.</span>}
      >
        <EmptyState
          icon="network"
          title="No cross-tool overlap detected"
          body="This surface lights up when 2+ AI clients (e.g. Claude Code + Cursor, or Cowork + Codex) touch the same file inside the window. With your current 30d filter, only claude-code has been heavily used. Try widening the window or enabling cursor/codex routing to see the cross-tool MCP value prop in action."
          action={
            <div style={{ display: "flex", gap: 8, justifyContent: "center" }}>
              <button className="btn ghost"><ProviderIcon tool="cursor" size={14}/>Configure Cursor</button>
              <button className="btn ghost"><ProviderIcon tool="codex" size={14}/>Configure Codex</button>
              <button className="btn primary"><Icon name="lightbulb" size={13}/>Learn about MCP cross-tool</button>
            </div>
          }
        />
      </ChartShell>
    </div>
  );
}
window.PageDiscovery = PageDiscovery;
