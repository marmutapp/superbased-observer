import { useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import clsx from "clsx";
import { Moon, Search, Sun } from "lucide-react";
import { useTheme } from "@/lib/theme";

// CommandPalette — ⌘K / Ctrl-K quick navigation + a couple of actions, mirror
// of the primary dashboard's palette. Frontend-only; no new data. Owned by the
// Layout shell, which also opens it via the keyboard handler.
type Cmd = {
  id: string;
  label: string;
  hint?: string;
  run: () => void;
};

const NAV: { to: string; label: string }[] = [
  { to: "/", label: "Overview" },
  { to: "/teams", label: "Teams" },
  { to: "/people", label: "People" },
  { to: "/projects", label: "Projects" },
  { to: "/tools", label: "Tools" },
  { to: "/models", label: "Models" },
  { to: "/activity", label: "Activity" },
  { to: "/telemetry", label: "Telemetry" },
  { to: "/trajectories", label: "Trajectory explorer" },
  { to: "/trajectories/analytics", label: "Trajectory analytics" },
  { to: "/trajectories/cost", label: "Trajectory cost" },
  { to: "/trajectories/alerts", label: "Trajectory alerts" },
  { to: "/trajectories/evals", label: "Eval health" },
  { to: "/routing", label: "Routing" },
  { to: "/sessions", label: "Sessions" },
  { to: "/live", label: "Live" },
  { to: "/movers", label: "Movers" },
  { to: "/report", label: "Cost report" },
  { to: "/suggestions", label: "Suggestions" },
  { to: "/security", label: "Security" },
  { to: "/policy", label: "Policy" },
  { to: "/audit", label: "Audit" },
  { to: "/invite", label: "Invite" },
  { to: "/settings", label: "Settings" },
];

export function CommandPalette({ open, onClose }: { open: boolean; onClose: () => void }) {
  const navigate = useNavigate();
  const { mode, setMode } = useTheme();
  const [query, setQuery] = useState("");
  const [active, setActive] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);

  const commands = useMemo<Cmd[]>(() => {
    const nav = NAV.map((n) => ({
      id: `go:${n.to}`,
      label: n.label,
      hint: "Go to",
      run: () => navigate(n.to),
    }));
    const actions: Cmd[] = [
      {
        id: "theme",
        label: mode === "dark" ? "Switch to light theme" : "Switch to dark theme",
        hint: "Action",
        run: () => setMode(mode === "dark" ? "light" : "dark"),
      },
    ];
    return [...nav, ...actions];
  }, [navigate, mode, setMode]);

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return commands;
    return commands.filter((c) => c.label.toLowerCase().includes(q));
  }, [commands, query]);

  useEffect(() => {
    if (open) {
      setQuery("");
      setActive(0);
      const id = requestAnimationFrame(() => inputRef.current?.focus());
      return () => cancelAnimationFrame(id);
    }
  }, [open]);

  useEffect(() => {
    setActive(0);
  }, [query]);

  if (!open) return null;

  function choose(c: Cmd | undefined) {
    if (!c) return;
    c.run();
    onClose();
  }

  function onKey(e: React.KeyboardEvent) {
    if (e.key === "Escape") {
      onClose();
    } else if (e.key === "ArrowDown") {
      e.preventDefault();
      setActive((a) => Math.min(filtered.length - 1, a + 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setActive((a) => Math.max(0, a - 1));
    } else if (e.key === "Enter") {
      e.preventDefault();
      choose(filtered[active]);
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-start justify-center pt-[12vh]" role="dialog" aria-modal="true">
      <div className="fixed inset-0 bg-black/60" onClick={onClose} aria-hidden />
      <div className="relative z-10 w-full max-w-lg overflow-hidden rounded-3 border border-line-2 bg-bg-1 shadow-drawer">
        <div className="flex items-center gap-2 border-b border-line-1 px-3">
          <Search size={14} className="text-fg-3" />
          <input
            ref={inputRef}
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            onKeyDown={onKey}
            placeholder="Jump to…"
            className="h-11 w-full bg-transparent text-[13px] text-fg-1 placeholder:text-fg-4 focus:outline-none"
          />
          <kbd className="rounded-1 border border-line-3 bg-bg-3 px-1.5 py-0.5 font-mono text-[10px] text-fg-3">esc</kbd>
        </div>
        <ul className="max-h-[50vh] overflow-y-auto p-1.5">
          {filtered.length === 0 ? (
            <li className="px-3 py-6 text-center text-[12px] text-fg-3">No matches.</li>
          ) : (
            filtered.map((c, i) => (
              <li key={c.id}>
                <button
                  type="button"
                  onMouseEnter={() => setActive(i)}
                  onClick={() => choose(c)}
                  className={clsx(
                    "flex w-full items-center justify-between gap-2 rounded-2 px-3 py-2 text-left text-[12.5px]",
                    i === active ? "bg-bg-3 text-fg-0" : "text-fg-2",
                  )}
                >
                  <span className="flex items-center gap-2">
                    {c.id === "theme" && (mode === "dark" ? <Sun size={13} /> : <Moon size={13} />)}
                    {c.label}
                  </span>
                  <span className="text-[10px] uppercase tracking-wide text-fg-4">{c.hint}</span>
                </button>
              </li>
            ))
          )}
        </ul>
      </div>
    </div>
  );
}
