import { useEffect, useState } from "react";
import { NavLink } from "react-router-dom";
import clsx from "clsx";
import { NAV_GROUPS, type NavIcon } from "@/lib/nav";
import { useApi } from "@/lib/useApi";
import { fmtBytes, fmtCompact } from "@/lib/format";
import {
  BarChartIcon,
  BoltIcon,
  CoinsIcon,
  DatabaseIcon,
  DollarIcon,
  DropletIcon,
  EyeIcon,
  GearIcon,
  LightningIcon,
  ListIcon,
  PercentIcon,
  SearchIcon,
  ShieldIcon,
  SparklesIcon,
  WrenchIcon,
} from "@/components/icons";
import type { StatusSnapshot, SetupClaude, WatcherHealth } from "@/lib/types";

function NavIconSvg({ icon }: { icon: NavIcon }) {
  switch (icon) {
    case "overview":
      return <EyeIcon size={13} />;
    case "live":
      return <BoltIcon size={13} />;
    case "search":
      return <SearchIcon size={13} />;
    case "sessions":
      return <ListIcon size={13} />;
    case "actions":
      return <LightningIcon size={13} />;
    case "security":
      return <ShieldIcon size={13} />;
    case "cost":
      return <DollarIcon size={13} />;
    case "analysis":
      return <BarChartIcon size={13} />;
    case "tools":
      return <WrenchIcon size={13} />;
    case "compression":
      return <DropletIcon size={13} />;
    case "cache":
      return <DatabaseIcon size={13} />;
    case "suggestions":
      return <CoinsIcon size={13} />;
    case "discovery":
      return <SearchIcon size={13} />;
    case "patterns":
      return <SparklesIcon size={13} />;
    case "trajectories":
      return <BarChartIcon size={13} />;
    case "evals":
      return <PercentIcon size={13} />;
    case "privacy":
      return <ShieldIcon size={13} />;
    case "settings":
      return <GearIcon size={13} />;
  }
}

export function Sidebar() {
  // Live-capture refresh: 5s on the status snapshot keeps the
  // sidebar's nav counts current as fresh actions land.
  const status = useApi<StatusSnapshot>("/api/status", undefined, [], { refreshMs: 5000 });
  const setupClaude = useApi<SetupClaude>("/api/setup/claude");
  // Watcher health (P1.7): behind/orphan/misroute counts only — a
  // slow 60s cadence is plenty for a lag signal.
  const watcherHealth = useApi<WatcherHealth>("/api/health/watcher", undefined, [], {
    refreshMs: 60000,
  });
  const counts = navCounts(status.data);
  // Footer "refreshed Xs ago" — recomputes every second so the clock
  // walks even between /api/status fetches.
  const [tick, setTick] = useState(0);
  useEffect(() => {
    const id = window.setInterval(() => setTick((t) => t + 1), 5000);
    return () => window.clearInterval(id);
  }, []);
  void tick;
  return (
    <aside className="flex w-[var(--sidebar-w)] shrink-0 flex-col border-r border-line-1 bg-bg-1">
      <Brand />
      <nav className="flex-1 overflow-y-auto px-3 py-4">
        {NAV_GROUPS.map((g) => (
          <div key={g.id} className="mb-5">
            <div className="mb-2 px-2 text-[10px] font-semibold uppercase tracking-[0.08em] text-fg-3">
              {g.label}
            </div>
            {g.items.map((it) => (
              <NavLink
                key={it.id}
                to={it.path}
                end={it.path === "/"}
                className={({ isActive }) =>
                  clsx(
                    "flex items-center gap-2 rounded-2 px-2 py-1.5 text-[12.5px] transition-colors",
                    isActive
                      ? "bg-bg-3 text-fg-0"
                      : "text-fg-2 hover:bg-bg-2 hover:text-fg-1",
                  )
                }
              >
                <span className="shrink-0 text-fg-3">
                  <NavIconSvg icon={it.icon} />
                </span>
                <span className="flex-1 truncate">{it.label}</span>
                {counts[it.id] != null && (
                  <span className="shrink-0 font-mono text-[10px] tabular-nums text-fg-4">
                    {fmtCompact(counts[it.id] as number)}
                  </span>
                )}
              </NavLink>
            ))}
          </div>
        ))}
      </nav>
      <Foot
        setup={setupClaude.data}
        status={status.data}
        watcher={watcherHealth.data}
      />
    </aside>
  );
}

function Brand() {
  return (
    <div className="flex h-[var(--header-h)] items-center gap-2.5 border-b border-line-1 px-4">
      <div
        className="grid h-7 w-7 place-items-center rounded-2 text-base font-extrabold text-white"
        style={{
          background:
            "linear-gradient(135deg, var(--brand-from), var(--brand-to))",
          boxShadow: "0 4px 12px rgba(124, 92, 246, 0.35)",
        }}
      >
        S
      </div>
      <div className="flex flex-col leading-tight">
        <b className="text-[13px] font-bold text-fg-0">SuperBased</b>
        <span className="text-[10px] font-medium uppercase tracking-[0.08em] text-fg-3">
          Observer
        </span>
      </div>
    </div>
  );
}

// navCounts maps each NavItem.id to a compact stat for the right
// gutter. Every data page carries a badge; only Overview (the home)
// and the Configure group (Privacy / Settings — no event streams)
// stay deliberately un-badged. The newer fields (live_sessions /
// guard_events / router_decisions) are optional in the type because
// an older daemon may serve a /api/status without them — `?? null`
// keeps those badges hidden instead of rendering a bogus 0.
function navCounts(s?: StatusSnapshot | null): Record<string, number | null> {
  if (!s) return {};
  const c = s.counts;
  return {
    live: c.live_sessions ?? null,
    sessions: c.sessions,
    actions: c.actions,
    security: c.guard_events ?? null,
    // The search corpus IS the FTS5 excerpt index — same substrate the
    // Compression badge counts, surfaced here as "how much is searchable".
    search: c.action_excerpts,
    cost: c.api_turns,
    analysis: c.token_usage,
    tools: distinctTools(s),
    compression: c.action_excerpts,
    cache: c.cache_events,
    suggestions: c.suggestions,
    routing: c.router_decisions ?? null,
    discovery: c.file_state,
    patterns: c.projects,
    // settings and overview stay un-badged
  };
}

function distinctTools(s: StatusSnapshot): number {
  return s.per_tool_last_seen?.length ?? 0;
}

function Foot({
  setup,
  status,
  watcher,
}: {
  setup: SetupClaude | null;
  status: StatusSnapshot | null;
  watcher: WatcherHealth | null;
}) {
  const proxyOn =
    setup != null &&
    (setup.status === "oauth_ready" || setup.status === "api_key_ready");
  // Watcher-lag signal (P1.7): files the watcher is behind on risk
  // dropped capture; misroute/orphan cursors mean rows silently not
  // landing. Surface a warn line the moment either is non-zero.
  const lagging = (watcher?.behind_count ?? 0) > 0;
  const misrouted = (watcher?.suspected_misrouted_count ?? 0) > 0;
  return (
    <div className="border-t border-line-1 px-4 py-3 text-[11px] text-fg-3">
      <div className="mb-0.5 flex items-center gap-1.5 text-fg-2">
        <span
          className={clsx(
            "relative h-1.5 w-1.5 rounded-full",
            proxyOn ? "bg-success" : "bg-warn",
          )}
        >
          {proxyOn && (
            <span className="absolute inset-0 -m-0.5 animate-ping rounded-full bg-success/40" />
          )}
        </span>
        watcher {proxyOn ? "active" : "—"}
        {setup?.proxy_port ? (
          <>
            {" · "}
            <span className="font-mono text-fg-3">
              proxy {setup.proxy_port}
            </span>
          </>
        ) : null}
      </div>
      {(lagging || misrouted) && (
        <div
          className="mb-0.5 flex items-center gap-1.5 text-[10px] text-warn"
          title={
            (lagging
              ? `Watcher is behind on ${watcher!.behind_count} file(s) (${fmtBytes(watcher!.behind_total_bytes)} unread) — recent activity may not be captured yet. A rescan (Settings → Backfill) catches up if it persists. `
              : "") +
            (misrouted
              ? `${watcher!.suspected_misrouted_count} session file(s) look misrouted (cursor at EOF, zero rows emitted).`
              : "")
          }
        >
          <span className="h-1.5 w-1.5 rounded-full bg-warn" />
          {lagging && <>behind {watcher!.behind_count} file{watcher!.behind_count === 1 ? "" : "s"}</>}
          {lagging && misrouted && " · "}
          {misrouted && <>{watcher!.suspected_misrouted_count} misrouted</>}
        </div>
      )}
      {status && (
        <div className="font-mono text-[10px] text-fg-4">
          schema v{status.schema_version}
          {status.db_size_bytes != null && (
            <> · {fmtBytes(status.db_size_bytes)}</>
          )}
          {status.uptime_seconds != null && status.uptime_seconds > 0 && (
            <> · up {fmtUptime(status.uptime_seconds)}</>
          )}
        </div>
      )}
      {status?.last_action_at && (
        <div className="font-mono text-[10px] text-fg-4">
          last activity {fmtRelative(status.last_action_at)} ago
        </div>
      )}
    </div>
  );
}

function fmtUptime(sec: number): string {
  if (sec < 3600) return `${Math.max(1, Math.floor(sec / 60))}m`;
  const hr = Math.floor(sec / 3600);
  if (hr < 48) return `${hr}h`;
  return `${Math.floor(hr / 24)}d`;
}

function fmtRelative(iso: string): string {
  const t = new Date(iso).getTime();
  if (!Number.isFinite(t)) return "—";
  const sec = Math.max(0, Math.floor((Date.now() - t) / 1000));
  if (sec < 60) return `${sec}s`;
  const min = Math.floor(sec / 60);
  if (min < 60) return `${min}m`;
  const hr = Math.floor(min / 60);
  if (hr < 24) return `${hr}h`;
  return `${Math.floor(hr / 24)}d`;
}
