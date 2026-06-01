/* ============================================================
   Shared primitives: StatCard, ChartShell, Badge, helpers
   ============================================================ */
const { useState: _useState, useMemo: _useMemo } = React;

// === Brand mark ===
function BrandMark({ size = 28, radius = 7 }) {
  return (
    <div style={{
      width: size, height: size, borderRadius: radius,
      background: "linear-gradient(135deg, #6199f0, #8b5cf6)",
      display: "grid", placeItems: "center",
      color: "#fff", fontWeight: 800, fontSize: size * 0.55,
      letterSpacing: "-0.03em",
      boxShadow: "0 4px 12px rgba(124, 92, 246, 0.35)",
      flexShrink: 0,
    }}>S</div>
  );
}

// === Help indicator ===
function HelpInd({ id }) {
  return <span className="help-ind" data-help={id} title="Click ? for help">?</span>;
}

// === Tool badge ===
function ToolBadge({ tool, showLabel = true, size = "sm" }) {
  const t = window.OBS.TOOLS[tool];
  if (!t) return <span className="pill">{tool}</span>;
  const iconSize = size === "sm" ? 14 : 18;
  return (
    <span className="pill tool" style={{
      fontSize: size === "sm" ? 11 : 12,
      padding: size === "sm" ? "1px 8px 1px 3px" : "3px 10px 3px 4px",
      gap: 5,
    }}>
      <ProviderIcon tool={tool} size={iconSize} />
      {showLabel && t.label}
    </span>
  );
}
function ToolDot({ tool }) {
  const t = window.OBS.TOOLS[tool];
  return <span className="tool-dot" style={{ background: t ? t.color : "var(--fg-3)" }} />;
}

// === Reliability badge ===
function ReliabilityPill({ v }) {
  if (!v) return <span className="pill">—</span>;
  const cls = v === "accurate" ? "success" : v === "approximate" ? "warn" : v === "unreliable" ? "danger" : "";
  return <span className={`pill ${cls}`}>{v}</span>;
}

// === Stat card ===
function StatCard({ label, value, unit, sub, delta, deltaLabel, sparkData, sparkColor, accent, warn, icon, helpId, children }) {
  const cls = ["stat", accent && "accent", warn && "warn"].filter(Boolean).join(" ");
  return (
    <div className={cls}>
      <div className="label">
        {icon && <Icon name={icon} size={12} />}
        {label}
        {helpId && <HelpInd id={helpId} />}
      </div>
      <div className="value">
        {value}
        {unit && <span className="unit">{unit}</span>}
      </div>
      {(sub || delta != null) && (
        <div className="sub">
          {delta != null && (
            <span className={`delta ${delta > 0 ? "up" : "down"}`}>
              <Icon name={delta > 0 ? "trend_up" : "trend_dn"} size={11} />
              {delta > 0 ? "+" : ""}{(delta*100).toFixed(1)}%
            </span>
          )}
          {deltaLabel && <span>{deltaLabel}</span>}
          {sub && <span>{sub}</span>}
        </div>
      )}
      {sparkData && (
        <div className="spark">
          <Sparkline data={sparkData} color={sparkColor || "var(--accent)"} w={80} h={28} />
        </div>
      )}
      {children}
    </div>
  );
}

// === Chart shell ===
function ChartShell({ title, sub, right, children, helpId, pad = true }) {
  return (
    <div className="chart-shell">
      <div className="card-head">
        <div>
          <h3>{title}{helpId && <HelpInd id={helpId} />}</h3>
          {sub && <div className="sub">{sub}</div>}
        </div>
        {right && <div className="right">{right}</div>}
      </div>
      <div style={{ padding: pad ? "12px 14px 14px" : 0 }}>{children}</div>
    </div>
  );
}

// === Segmented control ===
function Seg({ options, value, onChange, mini = false }) {
  return (
    <div className={mini ? "seg-mini" : "seg"}>
      {options.map(o => {
        const v = typeof o === "string" ? o : o.value;
        const l = typeof o === "string" ? o : o.label;
        return (
          <button key={v} className={v === value ? "active" : ""} onClick={() => onChange(v)}>{l}</button>
        );
      })}
    </div>
  );
}

// === Pager ===
function Pager({ page, pages, total, perPage = 50, onPage }) {
  const from = (page-1) * perPage + 1;
  const to = Math.min(page * perPage, total);
  return (
    <div className="pager">
      <button className="pager-btn" disabled={page <= 1} onClick={() => onPage && onPage(page-1)}>
        <Icon name="chevron_l" size={12} /> Prev
      </button>
      <span>page <b style={{ color: "var(--fg-1)" }}>{page}</b> / {pages}</span>
      <button className="pager-btn" disabled={page >= pages} onClick={() => onPage && onPage(page+1)}>
        Next <Icon name="chevron_r" size={12} />
      </button>
      <div className="grow" />
      <span>showing {from.toLocaleString()}–{to.toLocaleString()} of {total.toLocaleString()}</span>
    </div>
  );
}

// === Empty state ===
function EmptyState({ icon = "info", title, body, action }) {
  return (
    <div className="empty">
      <div style={{ width: 44, height: 44, margin: "0 auto 12px", borderRadius: 10, background: "var(--bg-3)", display: "grid", placeItems: "center", color: "var(--fg-3)" }}>
        <Icon name={icon} size={20} />
      </div>
      {title && <div style={{ color: "var(--fg-1)", fontWeight: 600, fontSize: 13, marginBottom: 4 }}>{title}</div>}
      {body && <div style={{ color: "var(--fg-3)", maxWidth: 380, margin: "0 auto", lineHeight: 1.5 }}>{body}</div>}
      {action && <div style={{ marginTop: 14 }}>{action}</div>}
    </div>
  );
}

// === Format helpers (alias to OBS.fmt for convenience) ===
const fmt = () => window.OBS.fmt;

Object.assign(window, { BrandMark, HelpInd, ToolBadge, ToolDot, ReliabilityPill, StatCard, ChartShell, Seg, Pager, EmptyState, fmt });
