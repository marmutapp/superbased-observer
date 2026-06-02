/* ============================================================
   App shell — wires sidebar/topnav, filter bar, pages, help, tweaks
   ============================================================ */
const { useState, useEffect } = React;

function App() {
  const [active, setActive] = useState("overview");
  const [navMode, setNavMode] = useState("sidebar"); // "sidebar" | "top"
  const [sidebarCollapsed, setSidebarCollapsed] = useState(false);
  const [theme, setTheme] = useState(() => localStorage.getItem("obs-theme") || "dark"); // dark | light | system
  const [win, setWin] = useState("30d");
  const [tool, setTool] = useState("all");
  const [project, setProject] = useState("all");
  const [helpOpen, setHelpOpen] = useState(false);
  const [tweaksOpen, setTweaksOpen] = useState(true);
  const [session, setSession] = useState(null);

  // apply theme to root
  useEffect(() => {
    document.documentElement.setAttribute("data-theme", theme);
    localStorage.setItem("obs-theme", theme);
  }, [theme]);

  // session detail open via DOM event (data-session attribute)
  useEffect(() => {
    function onClick(e) {
      const el = e.target.closest("[data-session]");
      if (el) {
        const id = el.getAttribute("data-session");
        const s = window.OBS.SESSIONS.find(s => s.id === id || s.id.startsWith(id));
        if (s) setSession(s);
      }
    }
    document.addEventListener("click", onClick);
    return () => document.removeEventListener("click", onClick);
  }, []);

  // ? key opens help drawer
  useEffect(() => {
    function onKey(e) {
      const tag = (e.target.tagName || "").toLowerCase();
      if (tag === "input" || tag === "textarea") return;
      if (e.key === "?") { e.preventDefault(); setHelpOpen(o => !o); }
      if (e.key === "Escape") { setHelpOpen(false); setSession(null); }
    }
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, []);

  // page router
  function renderPage() {
    switch (active) {
      case "overview":    return <PageOverview />;
      case "cost":        return <PageCost />;
      case "analysis":    return <PageAnalysis />;
      case "sessions":    return <PageSessions onOpenSession={setSession} />;
      case "actions":     return <PageActions />;
      case "tools":       return <PageTools />;
      case "compression": return <PageCompression />;
      case "discovery":   return <PageDiscovery />;
      case "patterns":    return <PagePatterns />;
      case "settings":    return <PageSettings />;
      default:            return <PageOverview />;
    }
  }

  return (
    <div className="app" style={navMode === "top" ? { gridTemplateColumns: "1fr" } : {}}>
      {navMode === "sidebar" && <Sidebar active={active} onNav={setActive} collapsed={sidebarCollapsed} />}
      <div className="main">
        <TopBar active={active} navMode={navMode} onHelp={() => setHelpOpen(true)} theme={theme} setTheme={setTheme} />
        {navMode === "top" && <TopTabs active={active} onNav={setActive} />}
        <FilterBar win={win} setWin={setWin} tool={tool} setTool={setTool} project={project} setProject={setProject} />
        <div className="page-body">
          {renderPage()}
        </div>
      </div>

      <SessionDetail session={session} open={session != null} onClose={() => setSession(null)} />
      <HelpDrawer open={helpOpen} onClose={() => setHelpOpen(false)} />

      <TweaksPanel
        open={tweaksOpen} setOpen={setTweaksOpen}
        navMode={navMode} setNavMode={setNavMode}
        sidebarCollapsed={sidebarCollapsed} setSidebarCollapsed={setSidebarCollapsed}
        theme={theme} setTheme={setTheme}
        active={active} setActive={setActive}
      />
    </div>
  );
}

function TweaksPanel({ open, setOpen, navMode, setNavMode, sidebarCollapsed, setSidebarCollapsed, theme, setTheme, active, setActive }) {
  if (!open) {
    return (
      <button
        onClick={() => setOpen(true)}
        style={{
          position: "fixed", right: 18, bottom: 18, zIndex: 40,
          padding: "10px 16px", background: "var(--bg-3)", border: "1px solid var(--line-3)",
          borderRadius: 999, color: "var(--fg-1)", fontSize: 12, fontWeight: 600,
          display: "flex", alignItems: "center", gap: 8, cursor: "pointer",
          boxShadow: "var(--shadow-2)",
        }}>
        <Icon name="settings" size={13} /> Tweaks
      </button>
    );
  }
  return (
    <div className="tweaks">
      <div className="tweaks-head">
        <Icon name="dots_v" size={14} className="grip" />
        Tweaks
        <div className="spacer" style={{ flex: 1 }}/>
        <button className="icon-btn" style={{ width: 22, height: 22 }} onClick={() => setOpen(false)}><Icon name="close" size={12}/></button>
      </div>
      <div className="tweaks-body">
        <div style={{ fontSize: 10, textTransform: "uppercase", letterSpacing: "0.08em", color: "var(--fg-3)", fontWeight: 600, marginBottom: 8 }}>Appearance</div>
        <div className="tweak-row" style={{ paddingBottom: 8 }}>
          <span className="lab">Theme</span>
          <Seg
            options={[
              {value:"dark",label:<span style={{ display: "inline-flex", alignItems: "center", gap: 4 }}><Icon name="moon" size={11}/>Dark</span>},
              {value:"light",label:<span style={{ display: "inline-flex", alignItems: "center", gap: 4 }}><Icon name="sun" size={11}/>Light</span>},
              {value:"system",label:<span style={{ display: "inline-flex", alignItems: "center", gap: 4 }}><Icon name="monitor" size={11}/>Sys</span>},
            ]}
            value={theme} onChange={setTheme}
          />
        </div>

        <div style={{ fontSize: 10, textTransform: "uppercase", letterSpacing: "0.08em", color: "var(--fg-3)", fontWeight: 600, marginBottom: 8, marginTop: 16 }}>Navigation</div>
        <div className="tweak-row">
          <span className="lab">Nav pattern</span>
          <Seg
            options={[{value:"sidebar",label:"Sidebar"},{value:"top",label:"Top tabs"}]}
            value={navMode} onChange={setNavMode}
          />
        </div>
        {navMode === "sidebar" && (
          <div className="tweak-row">
            <span className="lab">Collapse sidebar</span>
            <div className={"toggle" + (sidebarCollapsed ? " on" : "")} onClick={() => setSidebarCollapsed(!sidebarCollapsed)} />
          </div>
        )}

        <div style={{ fontSize: 10, textTransform: "uppercase", letterSpacing: "0.08em", color: "var(--fg-3)", fontWeight: 600, marginBottom: 8, marginTop: 16 }}>Jump to page</div>
        <select className="input" value={active} onChange={e => setActive(e.target.value)}>
          {NAV_GROUPS.flatMap(g => g.items).map(i => <option key={i.id} value={i.id}>{i.label}</option>)}
        </select>

        <div style={{ fontSize: 10, textTransform: "uppercase", letterSpacing: "0.08em", color: "var(--fg-3)", fontWeight: 600, marginBottom: 8, marginTop: 16 }}>Keyboard</div>
        <div style={{ display: "flex", flexDirection: "column", gap: 4, fontSize: 11, color: "var(--fg-2)" }}>
          <div>Press <span className="kbd">?</span> for help drawer</div>
          <div>Press <span className="kbd">Esc</span> to close overlays</div>
          <div>Click any session row to open detail</div>
        </div>

        <div style={{ marginTop: 16, paddingTop: 12, borderTop: "1px solid var(--line-1)", fontSize: 10, color: "var(--fg-3)" }}>
          v0.1 · SuperBased Observer redesign
        </div>
      </div>
    </div>
  );
}

ReactDOM.createRoot(document.getElementById("root")).render(<App />);
