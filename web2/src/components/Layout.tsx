import type { ReactNode } from "react";
import { NavLink } from "react-router-dom";
import clsx from "clsx";
import { Activity, FileLock2, FolderGit2, LayoutDashboard, ScrollText, Settings, Shield, Users, UserPlus } from "lucide-react";

const NAV = [
  { to: "/", label: "Overview", icon: LayoutDashboard, end: true },
  { to: "/teams", label: "Teams", icon: Users },
  { to: "/projects", label: "Projects", icon: FolderGit2 },
  { to: "/security", label: "Security", icon: Shield },
  { to: "/policy", label: "Policy", icon: FileLock2 },
  { to: "/invite", label: "Invite", icon: UserPlus },
  { to: "/audit", label: "Audit", icon: ScrollText },
  { to: "/settings", label: "Settings", icon: Settings },
];

export function Layout({ children }: { children: ReactNode }) {
  return (
    <div className="flex min-h-screen">
      <aside className="flex w-56 shrink-0 flex-col border-r border-line bg-surface px-3 py-4">
        <div className="mb-6 flex items-center gap-2 px-2">
          <Activity className="h-5 w-5 text-accent" />
          <div>
            <div className="text-sm font-semibold text-fg">SuperBased</div>
            <div className="text-[11px] text-faint">Org dashboard</div>
          </div>
        </div>
        <nav className="flex flex-col gap-0.5">
          {NAV.map((n) => (
            <NavLink
              key={n.to}
              to={n.to}
              end={n.end}
              className={({ isActive }) =>
                clsx(
                  "flex items-center gap-2.5 rounded px-2.5 py-2 text-sm transition",
                  isActive ? "bg-accent/15 text-accent" : "text-muted hover:bg-surface2 hover:text-fg",
                )
              }
            >
              <n.icon className="h-4 w-4" />
              {n.label}
            </NavLink>
          ))}
        </nav>
        <div className="mt-auto px-2 pt-4 text-[11px] text-faint">
          <a href="/saml/slo" className="hover:text-muted">
            Sign out
          </a>
        </div>
      </aside>
      <main className="flex-1 overflow-x-hidden px-6 py-5">
        <div className="mx-auto max-w-6xl">{children}</div>
      </main>
    </div>
  );
}
