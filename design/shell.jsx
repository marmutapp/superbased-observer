/* ============================================================
   Sidebar, top tabs, top header, filter bar, help drawer, tweaks
   ============================================================ */

const NAV_GROUPS = [
  { id: "monitor",  label: "Monitor",  items: [
    { id: "overview", label: "Overview", icon: "overview" },
    { id: "sessions", label: "Sessions", icon: "sessions", badge: "491" },
    { id: "actions",  label: "Actions",  icon: "actions",  badge: "78.2k" },
  ]},
  { id: "analyze",  label: "Analyze",  items: [
    { id: "cost",     label: "Cost",     icon: "cost" },
    { id: "analysis", label: "Analysis", icon: "analysis" },
    { id: "tools",    label: "Tools",    icon: "tools" },
  ]},
  { id: "optimize", label: "Optimize", items: [
    { id: "compression", label: "Compression", icon: "compression" },
    { id: "discovery",   label: "Discovery",   icon: "discovery" },
    { id: "patterns",    label: "Patterns",    icon: "patterns" },
  ]},
  { id: "configure",label: "Configure",items: [
    { id: "settings", label: "Settings", icon: "settings" },
  ]},
];
window.NAV_GROUPS = NAV_GROUPS;

// === Sidebar ===
function Sidebar({ active, onNav, collapsed }) {
  return (
    <aside className={"sidebar" + (collapsed ? " collapsed" : "")}>
      <div className="sidebar-brand">
        <BrandMark />
        <div className="wordmark">
          <b>SuperBased</b>
          <span>Observer</span>
        </div>
      </div>
      <div className="sidebar-nav">
        {NAV_GROUPS.map(g => (
          <div className="nav-group" key={g.id}>
            <div className="nav-group-label">{g.label}</div>
            {g.items.map(it => (
              <div key={it.id}
                className={"nav-item" + (active === it.id ? " active" : "")}
                onClick={() => onNav(it.id)}
                title={it.label}
              >
                <Icon name={it.icon} size={15} stroke={1.6} />
                <span className="nav-item-label">{it.label}</span>
                {it.badge && <span className="nav-badge">{it.badge}</span>}
              </div>
            ))}
          </div>
        ))}
      </div>
      <div className="sidebar-foot">
        <div className="row" style={{ marginBottom: 4 }}>
          <span className="dot ok" />
          <span style={{ color: "var(--fg-2)" }}>watcher active · proxy :8820</span>
        </div>
        <div style={{ color: "var(--fg-4)", fontFamily: "var(--font-mono)" }}>schema v18 · 338.3 MB</div>
        <div style={{ color: "var(--fg-4)", fontFamily: "var(--font-mono)", marginTop: 2 }}>refreshed 11:26:17 PM</div>
      </div>
    </aside>
  );
}

// === TopTabs (alternative nav pattern) ===
function TopTabs({ active, onNav }) {
  const flat = NAV_GROUPS.flatMap(g => g.items);
  return (
    <div className="toptabs">
      {flat.map(it => (
        <div key={it.id}
          className={"toptab" + (active === it.id ? " active" : "")}
          onClick={() => onNav(it.id)}>
          <Icon name={it.icon} size={14} className="ico" />
          {it.label}
        </div>
      ))}
    </div>
  );
}

// === Top header (above filter bar) ===
function TopBar({ active, navMode, onHelp, theme, setTheme, dbInfo }) {
  const groups = NAV_GROUPS;
  const group = groups.find(g => g.items.some(i => i.id === active));
  const item = group ? group.items.find(i => i.id === active) : null;
  return (
    <header className="topbar">
      {navMode === "top" && <BrandMark />}
      {navMode === "top" && (
        <div style={{ display: "flex", flexDirection: "column", lineHeight: 1.1 }}>
          <b style={{ color: "var(--fg-0)", fontSize: 13, fontWeight: 700 }}>SuperBased</b>
          <span style={{ color: "var(--fg-3)", fontSize: 10, letterSpacing: "0.08em", textTransform: "uppercase", fontWeight: 500 }}>Observer</span>
        </div>
      )}
      <div className="crumbs">
        {navMode === "sidebar" && group && (
          <>
            <span>{group.label}</span>
            <span className="sep">/</span>
            <b>{item ? item.label : ""}</b>
          </>
        )}
      </div>
      <div className="meta">
        <span className="pill-status">
          <span className="dot ok" />
          <span style={{ color: "var(--fg-2)" }}>last activity <b style={{ color: "var(--fg-1)", fontFamily: "var(--font-mono)" }}>2m ago</b></span>
        </span>
        <button className="icon-btn" title="Refresh">
          <Icon name="refresh" size={15} />
        </button>
        <button className="icon-btn" title="Export">
          <Icon name="export" size={15} />
        </button>
        <button className="icon-btn" title={`Theme: ${theme} (click to cycle)`}
          onClick={() => {
            const order = ["dark", "light", "system"];
            const next = order[(order.indexOf(theme) + 1) % order.length];
            setTheme && setTheme(next);
          }}>
          <Icon name={theme === "light" ? "sun" : theme === "system" ? "monitor" : "moon"} size={15} />
        </button>
        <button className="icon-btn" title="Help (?)" onClick={onHelp}>
          <Icon name="help" size={15} />
        </button>
      </div>
    </header>
  );
}

// === Global filter bar ===
const WINDOWS = [
  { value: "7d",  label: "7d" },
  { value: "14d", label: "14d" },
  { value: "30d", label: "30d" },
  { value: "90d", label: "90d" },
  { value: "all", label: "All" },
];

function FilterBar({ win, setWin, tool, setTool, project, setProject }) {
  return (
    <div className="filterbar">
      <div className="row-flex" style={{ gap: 6, marginRight: 6 }}>
        <Icon name="calendar" size={14} style={{ color: "var(--fg-3)" }} />
        <span style={{ fontSize: 11, textTransform: "uppercase", letterSpacing: "0.08em", color: "var(--fg-3)", fontWeight: 600 }}>Window</span>
      </div>
      <Seg options={WINDOWS} value={win} onChange={setWin} />

      <span style={{ width: 1, height: 22, background: "var(--line-2)", margin: "0 4px" }} />

      <button className="filter-chip">
        <Icon name="cube" size={13} className="ico" />
        Tool <b>{tool === "all" ? "all" : tool}</b>
        {tool !== "all" && <span className="swatch" style={{ background: window.OBS.TOOLS[tool] ? window.OBS.TOOLS[tool].color : "var(--fg-3)" }}/>}
        <Icon name="chevron_d" size={11} style={{ color: "var(--fg-3)" }} />
      </button>

      <button className="filter-chip">
        <Icon name="folder" size={13} className="ico" />
        Project <b>{project === "all" ? "all" : project}</b>
        <Icon name="chevron_d" size={11} style={{ color: "var(--fg-3)" }} />
      </button>

      <button className="filter-chip">
        <Icon name="search" size={13} className="ico" />
        <span style={{ color: "var(--fg-3)" }}>Search anything…</span>
        <span className="kbd" style={{ marginLeft: 8 }}>⌘K</span>
      </button>

      <div className="spacer" />

      <button className="btn ghost">
        <Icon name="export" size={13} className="ico" />
        Export
        <Icon name="chevron_d" size={11} className="ico" />
      </button>
      <button className="btn primary">
        <Icon name="refresh" size={13} className="ico" />
        Refresh
      </button>
    </div>
  );
}

// === Help drawer ===
function HelpDrawer({ open, onClose }) {
  const [q, setQ] = _useState("");
  const list = window.OBS.HELP.filter(h => !q || h.title.toLowerCase().includes(q.toLowerCase()) || h.oneline.toLowerCase().includes(q.toLowerCase()));
  const byCat = list.reduce((acc, h) => { (acc[h.cat] = acc[h.cat] || []).push(h); return acc; }, {});
  return (
    <>
      {open && <div className="slideover-backdrop open" onClick={onClose} />}
      <aside className={"help-drawer" + (open ? " open" : "")}>
        <div style={{ padding: "12px 16px", borderBottom: "1px solid var(--line-1)", display: "flex", alignItems: "center", gap: 10 }}>
          <Icon name="help" size={16} style={{ color: "var(--accent)" }} />
          <b style={{ color: "var(--fg-0)" }}>Help registry</b>
          <span className="pill">{window.OBS.HELP.length} entries</span>
          <div className="spacer" />
          <button className="icon-btn" onClick={onClose}><Icon name="close" size={14} /></button>
        </div>
        <div style={{ padding: "10px 16px", borderBottom: "1px solid var(--line-1)" }}>
          <div style={{ position: "relative" }}>
            <Icon name="search" size={14} style={{ position: "absolute", left: 10, top: 9, color: "var(--fg-3)" }} />
            <input className="input" style={{ paddingLeft: 32 }} placeholder="Search help…" value={q} onChange={e => setQ(e.target.value)} autoFocus={open} />
          </div>
        </div>
        <div style={{ flex: 1, overflowY: "auto", padding: "12px 16px 24px" }}>
          {Object.keys(byCat).map(cat => (
            <div key={cat} style={{ marginBottom: 18 }}>
              <div style={{ fontSize: 10, textTransform: "uppercase", letterSpacing: "0.1em", color: "var(--fg-3)", fontWeight: 600, marginBottom: 8 }}>{cat}</div>
              <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
                {byCat[cat].map(h => (
                  <div key={h.id} style={{ padding: 10, background: "var(--bg-2)", border: "1px solid var(--line-1)", borderRadius: 6 }}>
                    <div style={{ fontSize: 12, fontWeight: 600, color: "var(--fg-0)", marginBottom: 2 }}>{h.title}</div>
                    <div style={{ fontSize: 11, color: "var(--fg-2)", lineHeight: 1.5 }}>{h.oneline}</div>
                  </div>
                ))}
              </div>
            </div>
          ))}
        </div>
        <div style={{ padding: "10px 16px", borderTop: "1px solid var(--line-1)", fontSize: 11, color: "var(--fg-3)", display: "flex", alignItems: "center", gap: 8 }}>
          Press <span className="kbd">?</span> anywhere to open · <span className="kbd">Esc</span> to close
        </div>
      </aside>
    </>
  );
}

Object.assign(window, { Sidebar, TopTabs, TopBar, FilterBar, HelpDrawer });
