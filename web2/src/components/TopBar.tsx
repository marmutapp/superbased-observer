import { useLocation } from "react-router-dom";
import clsx from "clsx";
import { HelpCircle, Monitor, Moon, RefreshCw, Sun } from "lucide-react";
import { useFilters, WINDOW_OPTIONS, type WindowDays } from "@/lib/filters";
import { useTheme, type ThemeMode } from "@/lib/theme";
import { SegmentedControl, Tooltip } from "@/components/primitives";

// `org-dashboard-refresh` is a window-level CustomEvent that web2's useApi
// listens for to re-fire its loader. The TopBar Refresh button is the emitter
// — mirrors web/'s REFRESH_EVENT pattern.
export const REFRESH_EVENT = "org-dashboard-refresh";

// Maps a pathname to its breadcrumb label. Longest-prefix match so detail
// routes (/teams/:id) resolve to their section.
const TITLES: { prefix: string; label: string }[] = [
  { prefix: "/teams", label: "Teams" },
  { prefix: "/people", label: "People" },
  { prefix: "/tools", label: "Tools" },
  { prefix: "/models", label: "Models" },
  { prefix: "/activity", label: "Activity" },
  { prefix: "/movers", label: "Movers" },
  { prefix: "/trajectories/analytics", label: "Trajectory analytics" },
  { prefix: "/trajectories/cost", label: "Trajectory cost" },
  { prefix: "/trajectories/alerts", label: "Trajectory alerts" },
  { prefix: "/trajectories/evals", label: "Eval health" },
  { prefix: "/telemetry", label: "Telemetry" },
  { prefix: "/trajectories", label: "Trajectories" },
  { prefix: "/routing", label: "Routing" },
  { prefix: "/suggestions", label: "Suggestions" },
  { prefix: "/report", label: "Cost report" },
  { prefix: "/sessions", label: "Sessions" },
  { prefix: "/live", label: "Live" },
  { prefix: "/projects", label: "Projects" },
  { prefix: "/security", label: "Security" },
  { prefix: "/policy", label: "Policy" },
  { prefix: "/invite", label: "Invite" },
  { prefix: "/audit", label: "Audit" },
  { prefix: "/settings", label: "Settings" },
];

function titleFor(pathname: string): string {
  for (const t of TITLES) {
    if (pathname === t.prefix || pathname.startsWith(t.prefix + "/")) {
      return t.label;
    }
  }
  return "Overview";
}

export function TopBar({ onHelp }: { onHelp?: () => void }) {
  const { pathname } = useLocation();
  const { days, setDays } = useFilters();
  const label = titleFor(pathname);

  function refresh() {
    window.dispatchEvent(new CustomEvent(REFRESH_EVENT));
  }

  return (
    <header className="flex h-[var(--header-h)] items-center justify-between gap-3 border-b border-line-1 bg-bg-1 px-5">
      <div className="flex items-center gap-2 text-[12px] text-fg-3">
        <span>Organization</span>
        <span>/</span>
        <b className="text-fg-1">{label}</b>
      </div>
      <div className="flex items-center gap-2 text-[11px] text-fg-3">
        <SegmentedControl<string>
          options={WINDOW_OPTIONS.map((d) => ({ value: String(d), label: `${d}d` }))}
          value={String(days)}
          onChange={(v) => setDays(Number(v) as WindowDays)}
        />
        <div className="mx-1 h-4 w-px bg-line-2" />
        <Tooltip content={<>Help <kbd>?</kbd></>}>
          <button
            type="button"
            onClick={onHelp}
            aria-label="Open help"
            className="grid h-7 w-7 place-items-center rounded-2 border border-line-2 bg-bg-2 text-fg-3 transition-colors hover:bg-bg-3 hover:text-fg-1"
          >
            <HelpCircle size={13} />
          </button>
        </Tooltip>
        <Tooltip content="Reload data on every visible card">
          <button
            type="button"
            onClick={refresh}
            className="flex h-7 items-center gap-1.5 rounded-2 bg-accent px-2.5 text-[11px] font-semibold text-accent-on hover:bg-accent-strong"
          >
            <RefreshCw size={12} />
            Refresh
          </button>
        </Tooltip>
        <ThemeToggle />
      </div>
    </header>
  );
}

// Tri-state Light / Dark / System toggle — mirrors web/'s TopBar ThemeToggle.
function ThemeToggle() {
  const { mode, setMode } = useTheme();
  const opts: { value: ThemeMode; icon: typeof Sun; title: string }[] = [
    { value: "light", icon: Sun, title: "Light theme" },
    { value: "dark", icon: Moon, title: "Dark theme" },
    { value: "system", icon: Monitor, title: "Follow system" },
  ];
  return (
    <div
      role="radiogroup"
      aria-label="Theme"
      className="flex items-center gap-0.5 rounded-2 border border-line-2 bg-bg-2 p-0.5"
    >
      {opts.map((o) => (
        <Tooltip key={o.value} content={o.title}>
          <button
            type="button"
            role="radio"
            aria-checked={mode === o.value}
            onClick={() => setMode(o.value)}
            className={clsx(
              "grid h-6 w-6 place-items-center rounded-1 transition-colors",
              mode === o.value
                ? "bg-bg-4 text-fg-0"
                : "text-fg-3 hover:bg-bg-3 hover:text-fg-1",
            )}
          >
            <o.icon size={11} />
          </button>
        </Tooltip>
      ))}
    </div>
  );
}
