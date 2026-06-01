/* ============================================================
   Compression — hero savings, mechanism breakdown with 3-way
   unit toggle, CCR retrieve rate, D23 compactions, D20 rolling-summ
   ============================================================ */
function PageCompression() {
  const O = window.OBS;
  const C = O.COMPRESSION;
  const [unit, setUnit] = React.useState("$");
  const [setupOpen, setSetupOpen] = React.useState(false);

  const mechSeries = [
    { key: "drop",  label: "drop · low-importance",  color: "#f87171" },
    { key: "text",  label: "text · head/tail trunc", color: "#a78bfa" },
    { key: "json",  label: "json · value-erasure",   color: "#60a5fa" },
    { key: "code",  label: "code · whitespace+sym",  color: "#fbbf24" },
    { key: "logs",  label: "logs · dedup+anomaly",   color: "#34d399" },
    { key: "diff",  label: "diff · context strip",   color: "#06b6d4" },
    { key: "html",  label: "html · cruft removal",   color: "#ec4899" },
    { key: "stash", label: "stash · CCR offload",    color: "#c084fc" },
  ];

  return (
    <div>
      <div className="page-head">
        <div>
          <h1 className="page-title">Compression</h1>
          <p className="page-desc">
            The proxy compresses each upstream request before forwarding — truncating large <code style={{ background: "var(--bg-3)", padding: "1px 5px", borderRadius: 3, fontSize: 11 }}>tool_result</code> blocks and replacing dropped content with markers. <b>Bytes</b> is the source of truth; tokens and dollars are derived (bytes ÷ 4 × the row's input rate).
          </p>
        </div>
      </div>

      {/* === Setup status — single-line when configured === */}
      <div className="card" style={{ marginBottom: 14 }}>
        <div style={{ display: "flex", alignItems: "center", padding: "10px 14px", gap: 10, cursor: "pointer" }} onClick={() => setSetupOpen(!setupOpen)}>
          <span className="dot ok" />
          <b style={{ color: "var(--fg-0)", fontSize: 13 }}>Proxy active on :8820</b>
          <span style={{ color: "var(--fg-2)", fontSize: 12 }}>capturing Claude Code · Codex auth detected (jsonl-only mode)</span>
          <div className="spacer" />
          <span className="pill"><span className="dot ok" /> claude-code</span>
          <span className="pill warn"><span className="dot warn" /> codex — JSONL only</span>
          <Icon name={setupOpen ? "chevron_u" : "chevron_d"} size={14} style={{ color: "var(--fg-3)" }}/>
        </div>
        {setupOpen && (
          <div style={{ padding: "0 14px 14px" }}>
            <div className="grid g-2">
              <div style={{ padding: 12, background: "var(--bg-3)", borderRadius: 6, borderLeft: "2px solid var(--success)" }}>
                <div style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 8 }}>
                  <b style={{ color: "var(--fg-0)", fontSize: 13 }}>Pro/Max OAuth Claude Code</b>
                  <span className="pill success">oauth_ready</span>
                </div>
                <div style={{ fontSize: 12, color: "var(--fg-2)", lineHeight: 1.5 }}>
                  Pro/Max-OAuth Claude Code (2.1+) bypasses <code style={{ fontSize: 11, background: "var(--bg-1)", padding: "1px 4px", borderRadius: 3 }}>ANTHROPIC_BASE_URL</code> for the <code style={{ fontSize: 11 }}>/v1/messages</code> chat call — observer re-exports the OAuth access token as <code style={{ fontSize: 11 }}>ANTHROPIC_AUTH_TOKEN</code>, forcing Claude Code into its API-key code path. Same Bearer header on the wire, same Pro/Max billing.
                </div>
                <div className="mono" style={{ marginTop: 8, fontSize: 11, background: "var(--bg-1)", padding: 6, borderRadius: 4, color: "var(--accent)" }}>
                  observer claude --proxy http://127.0.0.1:8820
                </div>
              </div>
              <div style={{ padding: 12, background: "var(--bg-3)", borderRadius: 6, borderLeft: "2px solid var(--warn)" }}>
                <div style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 8 }}>
                  <b style={{ color: "var(--fg-0)", fontSize: 13 }}>Codex — not routed</b>
                  <span className="pill warn">not_configured</span>
                </div>
                <div style={{ fontSize: 12, color: "var(--fg-2)", lineHeight: 1.5 }}>
                  Observer will add <code style={{ fontSize: 11 }}>[model_providers.openai-observer]</code> to <code style={{ fontSize: 11 }}>~/.codex/config.toml</code> and switch <code style={{ fontSize: 11 }}>model_provider</code>. Existing settings preserved. ChatGPT-plan auth: live proxy compression captured but proxy → JSONL session link not yet reliable.
                </div>
                <button className="btn primary" style={{ marginTop: 10 }}>Configure now</button>
              </div>
            </div>
          </div>
        )}
      </div>

      {/* === Hero savings === */}
      <div className="card" style={{ marginBottom: 14, position: "relative", overflow: "hidden" }}>
        <div style={{ position: "absolute", inset: 0, background: "radial-gradient(circle at 15% 30%, rgba(52, 211, 153, 0.12), transparent 60%)", pointerEvents: "none" }} />
        <div style={{ padding: 24, display: "flex", alignItems: "flex-end", gap: 32, position: "relative" }}>
          <div>
            <div style={{ fontSize: 11, textTransform: "uppercase", letterSpacing: "0.08em", color: "var(--fg-3)", fontWeight: 600 }}>
              Total compression savings
            </div>
            <div style={{ fontSize: 48, fontWeight: 700, color: "var(--success)", letterSpacing: "-0.03em", lineHeight: 1, fontFamily: "var(--font-display)", margin: "6px 0" }}>
              {O.fmt.$(C.dollars_saved, 4)}
            </div>
            <div style={{ fontSize: 12, color: "var(--fg-2)" }}>
              across <b style={{ color: "var(--fg-0)" }}>{O.fmt.n(C.actions_compressed)}</b> compression events ·{" "}
              <b style={{ color: "var(--fg-0)" }}>{O.fmt.bytes(C.bytes_saved)}</b> trimmed from upstream payloads ·{" "}
              <b style={{ color: "var(--fg-0)" }}>~{O.fmt.n(C.tokens_saved)}</b> tokens
            </div>
          </div>
          <div style={{ flex: 1, display: "grid", gridTemplateColumns: "repeat(4, 1fr)", gap: 12 }}>
            <StatCard label="Tokens saved" value={<span style={{ color: "var(--success)" }}>{O.fmt.n(C.tokens_saved)}</span>} sub="~bytes ÷ 4" helpId="metric.savings"/>
            <StatCard label="Dollars saved" value={<span style={{ color: "var(--success)" }}>{O.fmt.$(C.dollars_saved, 4)}</span>} sub="tokens × input rate"/>
            <StatCard label="Bytes saved" value={<span style={{ color: "var(--success)" }}>{O.fmt.bytes(C.bytes_saved)}</span>} sub={`${O.fmt.bytes(C.bytes_before)} → ${O.fmt.bytes(C.bytes_after)}`} helpId="metric.bytes"/>
            <StatCard label="Turns compressed" value={C.turns_compressed} sub={`${C.dropped} dropped · ${C.markers} markers`}/>
          </div>
        </div>
      </div>

      {/* === Savings per day + by mechanism === */}
      <div className="grid g-2" style={{ marginBottom: 14 }}>
        <ChartShell title="Savings per day" sub={`4 day(s) with compression activity · totals shown above span the full 30d window`}>
          <div style={{ display: "flex", gap: 12, marginBottom: 8 }}>
            <div style={{ flex: 1 }}>
              <BarChart
                data={O.COMP_DAILY.map(d => ({ date: d.date, value: d.tokens_saved }))}
                valueKey="value" color="var(--tok-read)" height={200} yFmt={v => O.fmt.n(v) + " tok"}
              />
            </div>
          </div>
          <div style={{ display: "flex", gap: 16, fontSize: 11, color: "var(--fg-3)" }}>
            <span><span style={{ display: "inline-block", width: 10, height: 10, background: "var(--tok-read)", borderRadius: 2, verticalAlign: "middle", marginRight: 4 }}/> Tokens saved (est.)</span>
            <span><span style={{ display: "inline-block", width: 10, height: 10, background: "var(--success)", borderRadius: 2, verticalAlign: "middle", marginRight: 4 }}/> $ saved (est.)</span>
          </div>
        </ChartShell>

        <ChartShell
          title="Savings by mechanism"
          sub="8 mechanisms · per-day breakdown · hover for description"
          right={<Seg options={[{value:"tokens",label:"tokens"},{value:"$",label:"$"},{value:"bytes",label:"bytes"}]} value={unit} onChange={setUnit} />}
        >
          <Donut size={130} thickness={20}
            centerValue={O.fmt.$(C.dollars_saved, 2)} centerLabel="SAVED"
            data={[
              { label: "drop · low-importance",   value: 21100, color: "#f87171" },
              { label: "text · head/tail trunc",  value: 7900,  color: "#a78bfa" },
              { label: "json · value-erasure",    value: 6800,  color: "#60a5fa" },
              { label: "logs · dedup",            value: 4500,  color: "#34d399" },
              { label: "code · whitespace",       value: 2900,  color: "#fbbf24" },
              { label: "stash · CCR offload",     value: 1700,  color: "#c084fc" },
            ]}
          />
        </ChartShell>
      </div>

      {/* === Per-model breakdown === */}
      <ChartShell title="Per-model breakdown" sub={`${O.COMP_PER_MODEL.length} model(s) with compression data`} pad={false}>
        <table className="dtable">
          <thead>
            <tr>
              <th>Model</th>
              <th className="num">Tokens saved</th>
              <th className="num">$ saved</th>
              <th className="num">Bytes saved</th>
              <th>Save %</th>
              <th className="num">Turns</th>
              <th className="num">Tool results</th>
              <th className="num">Dropped</th>
              <th className="num">Markers</th>
            </tr>
          </thead>
          <tbody>
            {O.COMP_PER_MODEL.map((m, i) => (
              <tr key={i}>
                <td className="mono" style={{ color: "var(--fg-0)" }}>{m.model}</td>
                <td className="num" style={{ color: "var(--success)" }}>{O.fmt.n(m.tokens)}</td>
                <td className="num" style={{ color: "var(--success)" }}>{O.fmt.$(m.dollars, 4)}</td>
                <td className="num">{O.fmt.bytes(m.bytes)}</td>
                <td>
                  <div style={{ display: "flex", alignItems: "center", gap: 8, width: 140 }}>
                    <div style={{ flex: 1, height: 6, background: "var(--bg-4)", borderRadius: 3, overflow: "hidden" }}>
                      <div style={{ width: `${m.save_pct*100}%`, height: "100%", background: "var(--success)" }}/>
                    </div>
                    <span className="num" style={{ fontFamily: "var(--font-mono)", fontSize: 12, color: "var(--success)", fontWeight: 600 }}>
                      {(m.save_pct*100).toFixed(1)}%
                    </span>
                  </div>
                </td>
                <td className="num">{m.turns}</td>
                <td className="num">{m.tool_results}</td>
                <td className="num">{m.dropped}</td>
                <td className="num">{m.markers}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </ChartShell>

      <div style={{ height: 14 }} />

      {/* === Recent events === */}
      <ChartShell title="Recent compression events" sub="77 total · what mechanism fired, how big the input was, how big it ended up" pad={false}>
        <div className="scroll-x" style={{ maxHeight: 420, overflow: "auto" }}>
          <table className="dtable zebra">
            <thead>
              <tr>
                <th>When</th>
                <th>Mech</th>
                <th>Source</th>
                <th>Model</th>
                <th className="num">Saved (B)</th>
                <th className="num">Saved (tok)</th>
                <th className="num">$ saved</th>
                <th>Save %</th>
                <th className="num">Msg #</th>
                <th>Session</th>
                <th>Msg ID</th>
                <th className="num">Importance</th>
              </tr>
            </thead>
            <tbody>
              {O.COMP_EVENTS.map((e, i) => (
                <tr key={i}>
                  <td className="mono dim" style={{ fontSize: 11 }}>{e.when}</td>
                  <td>
                    <span className="pill" style={{
                      color: e.mech === "drop" ? "#f87171" : e.mech === "text" ? "#a78bfa" : "#60a5fa",
                      background: e.mech === "drop" ? "rgba(248,113,113,0.1)" : e.mech === "text" ? "rgba(167,139,250,0.1)" : "rgba(96,165,250,0.1)",
                      borderColor: "transparent",
                      fontFamily: "var(--font-mono)",
                    }}>{e.mech}</span>
                  </td>
                  <td className="mono dim" style={{ fontSize: 11 }}>{e.source}</td>
                  <td className="mono" style={{ fontSize: 11 }}>{e.model}</td>
                  <td className="num" style={{ color: "var(--success)" }}>{O.fmt.bytes(e.saved_b)}</td>
                  <td className="num" style={{ color: "var(--success)" }}>~{O.fmt.n(e.saved_tok)}</td>
                  <td className="num" style={{ color: "var(--success)" }}>{O.fmt.$(e.dollars, 4)}</td>
                  <td>
                    <div style={{ display: "flex", alignItems: "center", gap: 6, width: 80 }}>
                      <div style={{ flex: 1, height: 4, background: "var(--bg-4)", borderRadius: 2, overflow: "hidden" }}>
                        <div style={{ width: `${e.save_pct*100}%`, height: "100%", background: e.save_pct >= 0.95 ? "var(--success)" : e.save_pct >= 0.5 ? "var(--warn)" : "var(--info)" }}/>
                      </div>
                      <span className="num" style={{ fontFamily: "var(--font-mono)", fontSize: 11, color: "var(--success)" }}>{(e.save_pct*100).toFixed(1)}%</span>
                    </div>
                  </td>
                  <td className="num">{e.msg_idx || "—"}</td>
                  <td><span className="id-link">{e.session}</span></td>
                  <td><span className="id-link">{e.msg_id}</span></td>
                  <td className="num" style={{ color: e.importance == null ? "var(--fg-3)" : "var(--fg-1)" }}>{e.importance == null ? "—" : e.importance.toFixed(3)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <Pager page={1} pages={2} total={77} perPage={50} />
      </ChartShell>

      <div style={{ height: 14 }} />

      {/* === CCR retrieve rate === */}
      <ChartShell
        title={<span><Icon name="zap" size={14} style={{ color: "var(--brand-to)" }}/> Reversibility — CCR retrieve rate <span className="pill brand">G31</span></span>}
        helpId="g.ccr"
        sub="When the proxy stashes a tool_result body to disk and replaces it with a marker, does the model later actually retrieve it? Near-zero = stashed bodies could have been inlined."
      >
        <div className="grid g-4" style={{ marginBottom: 14 }}>
          <StatCard label="Stashes (window)" icon="layers" value={O.CCR.stashes} sub="tool_results > 8KB written to disk" />
          <StatCard label="Retrieves" icon="bolt" value={O.CCR.retrieves} sub="retrieve_stashed MCP calls" />
          <StatCard label="Retrieve rate" icon="trend_up" value={O.CCR.retrieve_rate == null ? "—" : (O.CCR.retrieve_rate*100).toFixed(1)+"%"} sub="no stashes in window" />
          <StatCard label="Search hits" icon="search" value={O.CCR.search_hits} sub="FTS5 query landings" accent />
        </div>
        <div className="grid g-2">
          <div>
            <div style={{ fontSize: 12, color: "var(--fg-2)", marginBottom: 6 }}>
              <b style={{ color: "var(--fg-0)" }}>Top retrieved SHAs</b> — bodies the model returned to most often
            </div>
            <table className="dtable">
              <thead><tr><th>SHA (first 12)</th><th className="num">Retrieves</th></tr></thead>
              <tbody>
                {O.CCR.top_shas.map((s, i) => (
                  <tr key={i}><td className="mono"><span className="id-link">{s.sha}</span></td><td className="num">{s.retrieves}</td></tr>
                ))}
                {O.CCR.top_shas.length < 5 && Array.from({ length: 4 }).map((_, i) => (
                  <tr key={"e"+i}><td className="dim">—</td><td className="num dim">—</td></tr>
                ))}
              </tbody>
            </table>
          </div>
          <div>
            <div style={{ fontSize: 12, color: "var(--fg-2)", marginBottom: 6 }}>
              <b style={{ color: "var(--fg-0)" }}>Top searched actions</b> — FTS5 hits that landed on the same action_id repeatedly
            </div>
            <table className="dtable">
              <thead><tr><th>Action ID</th><th className="num">Search hits</th></tr></thead>
              <tbody>
                {O.CCR.top_actions.map((a, i) => (
                  <tr key={i}><td className="mono">{a.id}</td><td className="num">{a.hits}</td></tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      </ChartShell>

      <div style={{ height: 14 }} />

      {/* === D23 compactions === */}
      <ChartShell
        title={<span><Icon name="layers" size={14} style={{ color: "var(--info)" }}/> Compaction events <span className="pill info">D23</span></span>}
        helpId="g.compaction"
        sub={<span>Each <code style={{ background: "var(--bg-3)", padding: "1px 5px", borderRadius: 3, fontSize: 11 }}>/compact</code> in Claude Code lands one row in <code style={{ background: "var(--bg-3)", padding: "1px 5px", borderRadius: 3, fontSize: 11 }}>compaction_events</code>. When D23 post-compact injection is enabled, <code style={{ background: "var(--bg-3)", padding: "1px 5px", borderRadius: 3, fontSize: 11 }}>injected_at</code> records the first synthetic recovery context fired for that compaction.</span>}
      >
        <div className="grid g-4" style={{ marginBottom: 14 }}>
          <StatCard label="Compactions" icon="shrink" value={O.COMPACTIONS.count} sub="/compact events in window" />
          <StatCard label="Sessions affected" icon="sessions" value={O.COMPACTIONS.sessions_affected} sub="distinct session_ids" />
          <StatCard label="Injections fired" icon="zap" value={O.COMPACTIONS.injections_fired} sub="D23 prepended recovery context" />
          <StatCard label="Inject rate" icon="trend_up" value={<span style={{ color: "var(--danger)" }}>{(O.COMPACTIONS.inject_rate*100).toFixed(1)}%</span>} sub="injections ÷ compactions" warn />
        </div>
        <table className="dtable">
          <thead>
            <tr>
              <th>When</th>
              <th>Session</th>
              <th>Tool</th>
              <th className="num">Pre-compact actions</th>
              <th className="num">File snapshot</th>
              <th className="num">Ghost files</th>
              <th>Injected</th>
            </tr>
          </thead>
          <tbody>
            {O.COMPACTIONS.events.map((e, i) => (
              <tr key={i}>
                <td className="mono dim" style={{ fontSize: 11 }}>{e.when}</td>
                <td><span className="id-link">{e.session}</span></td>
                <td><ToolBadge tool={e.tool}/></td>
                <td className="num dim">{e.pre}</td>
                <td className="num">{e.snapshot}</td>
                <td className="num" style={{ color: e.ghost > 0 ? "var(--warn)" : "var(--fg-2)" }}>{e.ghost}</td>
                <td className="dim">—</td>
              </tr>
            ))}
          </tbody>
        </table>
      </ChartShell>

      <div style={{ height: 14 }} />

      {/* === D20 rolling-summarisation === */}
      <ChartShell
        title={<span><Icon name="pulse" size={14} style={{ color: "var(--success)" }}/> Rolling-summarisation net cost <span className="pill success">D20</span></span>}
        helpId="g.rolling"
        sub="D20 calls Anthropic Haiku to produce summaries of older messages — those calls cost money too. This card joins the summary_calls ledger against the rolling_summary mechanism savings to compute net delta."
      >
        <div className="grid g-4">
          <StatCard label="Summary calls" icon="ai" value={O.ROLLING.summary_calls} sub="Haiku invocations in window" />
          <StatCard label="Haiku spend" icon="cost" value={O.fmt.$(O.ROLLING.haiku_spend, 4)} sub="0 in / 0 out" />
          <StatCard label="Cache-creation savings" icon="zap" value={O.fmt.$(O.ROLLING.cache_savings, 4)} sub="0 tokens × cache_creation rate" />
          <StatCard label="Net delta" icon="scale">
            <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
              <div className="value" style={{ color: O.ROLLING.net_delta > 0 ? "var(--success)" : O.ROLLING.net_delta < 0 ? "var(--danger)" : "var(--fg-2)" }}>
                {O.fmt.$(O.ROLLING.net_delta, 4)}
              </div>
              <span className="pill" style={{
                background: "var(--bg-3)", color: "var(--fg-2)"
              }}>● no activity</span>
            </div>
            <div className="sub">positive = paying off, negative = losing</div>
          </StatCard>
        </div>
        <div style={{ marginTop: 12, fontSize: 11, color: "var(--fg-3)", lineHeight: 1.5 }}>
          Cache-creation rate is used because the bytes rolling-summ replaces would otherwise be re-cached on the next turn (when the conversation prefix grew past Anthropic's prompt-cache window).
        </div>
      </ChartShell>
    </div>
  );
}
window.PageCompression = PageCompression;
