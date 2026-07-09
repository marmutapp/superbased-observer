import { NavLink } from "react-router-dom";
import clsx from "clsx";
import {
  Activity,
  BarChart3,
  BellRing,
  Boxes,
  FileLock2,
  FlaskConical,
  FolderGit2,
  History,
  LayoutDashboard,
  Lightbulb,
  type LucideIcon,
  Radio,
  Receipt,
  ScrollText,
  Settings,
  Shield,
  Shuffle,
  Signal,
  Spline,
  TrendingUp,
  User,
  Users,
  UserPlus,
  Wrench,
} from "lucide-react";

// Org dashboard sidebar — mirrors web/src/components/Sidebar.tsx's visual
// treatment (gradient brand mark, grouped uppercase sections, active =
// elevated surface) but without the per-tab nav counts (the org server has no
// /api/status counters endpoint). Grouping makes the governance vs admin split
// legible as the nav grows (People / Tools / Models / Activity land in later
// phases).

type NavItem = { to: string; label: string; icon: LucideIcon; end?: boolean };
type NavGroup = { id: string; label: string; items: NavItem[] };

const NAV_GROUPS: NavGroup[] = [
  {
    id: "org",
    label: "Organization",
    items: [
      { to: "/", label: "Overview", icon: LayoutDashboard, end: true },
      { to: "/teams", label: "Teams", icon: Users },
      { to: "/people", label: "People", icon: User },
      { to: "/projects", label: "Projects", icon: FolderGit2 },
    ],
  },
  {
    id: "analytics",
    label: "Analytics",
    items: [
      { to: "/tools", label: "Tools", icon: Wrench },
      { to: "/models", label: "Models", icon: Boxes },
      { to: "/activity", label: "Activity", icon: BarChart3 },
      { to: "/movers", label: "Movers", icon: TrendingUp },
      { to: "/telemetry", label: "Telemetry", icon: Radio },
    ],
  },
  {
    id: "trajectories",
    label: "Trajectories",
    items: [
      { to: "/trajectories", label: "Explorer", icon: Spline, end: true },
      { to: "/trajectories/analytics", label: "Analytics", icon: BarChart3 },
      { to: "/trajectories/cost", label: "Cost", icon: Receipt },
      { to: "/trajectories/evals", label: "Eval health", icon: FlaskConical },
      { to: "/trajectories/alerts", label: "Alerts", icon: BellRing },
    ],
  },
  {
    id: "optimize",
    label: "Optimize",
    items: [
      { to: "/routing", label: "Routing", icon: Shuffle },
      { to: "/suggestions", label: "Suggestions", icon: Lightbulb },
      { to: "/report", label: "Cost report", icon: Receipt },
    ],
  },
  {
    id: "monitor",
    label: "Monitor",
    items: [
      { to: "/sessions", label: "Sessions", icon: History },
      { to: "/live", label: "Live", icon: Signal },
    ],
  },
  {
    id: "governance",
    label: "Governance",
    items: [
      { to: "/security", label: "Security", icon: Shield },
      { to: "/policy", label: "Policy", icon: FileLock2 },
      { to: "/audit", label: "Audit", icon: ScrollText },
    ],
  },
  {
    id: "admin",
    label: "Admin",
    items: [
      { to: "/invite", label: "Invite", icon: UserPlus },
      { to: "/settings", label: "Settings", icon: Settings },
    ],
  },
];

export function Sidebar() {
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
                key={it.to}
                to={it.to}
                end={it.end}
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
                  <it.icon size={14} />
                </span>
                <span className="flex-1 truncate">{it.label}</span>
              </NavLink>
            ))}
          </div>
        ))}
      </nav>
      <Foot />
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
          Org dashboard
        </span>
      </div>
    </div>
  );
}

function Foot() {
  return (
    <div className="border-t border-line-1 px-4 py-3 text-[11px] text-fg-3">
      <div className="mb-0.5 flex items-center gap-1.5 text-fg-2">
        <Activity size={12} className="text-accent" />
        Org server
      </div>
      <a href="/saml/slo" className="font-mono text-[10px] text-fg-4 hover:text-fg-2">
        Sign out
      </a>
    </div>
  );
}
