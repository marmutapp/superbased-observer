import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useNavigate } from "react-router-dom";
import clsx from "clsx";
import { Moon, Search, Sun } from "lucide-react";
import { useTheme } from "@/lib/theme";
import { useFilters } from "@/lib/filters";
import {
  api,
  type PersonRollup,
  type ProjectRollup,
  type SessionRow,
  type TeamRollup,
} from "@/lib/api";
import { projectName, usd } from "@/lib/format";
import { ToolBadge } from "@/components/primitives";

// CommandPalette — ⌘K / Ctrl-K quick navigation + data search (G11). Mirror of
// the primary dashboard's palette, but scoped to org data: alongside page
// jumps and the theme toggle, typing (≥2 chars) searches Teams, Projects,
// People, and recent Sessions and jumps straight to the entity.
//
// Audit note: /api/org/people and /api/org/sessions are AUDITED disclosures
// (each write a view_org_* row). We therefore fetch them LAZILY — only once the
// admin has actually typed a query, never on a bare ⌘K open — and cache the
// result for the palette's lifetime, so a search is one deliberate, recorded
// disclosure rather than a page-load side effect. Teams/Projects are
// content-free aggregates and ride the same lazy fetch for simplicity.

type NavCmd = { kind: "page"; id: string; label: string; run: () => void };
type ActionCmd = { kind: "action"; id: string; label: string; run: () => void };
type TeamCmd = { kind: "team"; id: string; team: TeamRollup };
type ProjectCmd = { kind: "project"; id: string; project: ProjectRollup };
type PersonCmd = { kind: "person"; id: string; person: PersonRollup };
type SessionCmd = { kind: "session"; id: string; session: SessionRow };
type Item = NavCmd | ActionCmd | TeamCmd | ProjectCmd | PersonCmd | SessionCmd;

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
  { to: "/trajectories/end-users", label: "End-user spend" },
  { to: "/trajectories/alerts", label: "Trajectory alerts" },
  { to: "/trajectories/evals", label: "Eval health" },
  { to: "/trajectories/admission", label: "Admission" },
  { to: "/routing", label: "Routing" },
  { to: "/sessions", label: "Sessions" },
  { to: "/live", label: "Live" },
  { to: "/movers", label: "Movers" },
  { to: "/report", label: "Cost report" },
  { to: "/suggestions", label: "Suggestions" },
  { to: "/security", label: "Security" },
  { to: "/policy", label: "Policy" },
  { to: "/announcements", label: "Announcements" },
  { to: "/audit", label: "Audit" },
  { to: "/invite", label: "Invite" },
  { to: "/settings", label: "Settings" },
];

const SEARCH_LEN = 2; // min query length before firing the audited data fetch
const SESSION_LIMIT = 25;

type DataBundle = {
  teams: TeamRollup[];
  projects: ProjectRollup[];
  people: PersonRollup[];
  sessions: SessionRow[];
};

export function CommandPalette({ open, onClose }: { open: boolean; onClose: () => void }) {
  const navigate = useNavigate();
  const { mode, setMode } = useTheme();
  const { days } = useFilters();
  const [query, setQuery] = useState("");
  const [active, setActive] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);

  const [bundle, setBundle] = useState<DataBundle | null>(null);
  const [loadingData, setLoadingData] = useState(false);
  const [dataErr, setDataErr] = useState<string | null>(null);
  const startedRef = useRef(false);
  const q = query.trim().toLowerCase();

  // Lazy, audited data fetch — fires once the admin has typed a real query, and
  // only once per palette lifetime (a ref latch guards against re-entry; the
  // result is cached in `bundle`). Deliberately NOT keyed on `loadingData` — a
  // set-state dep would let the effect's own re-run cancel the in-flight fetch.
  useEffect(() => {
    if (!open || q.length < SEARCH_LEN || startedRef.current) return;
    startedRef.current = true;
    setLoadingData(true);
    setDataErr(null);
    (async () => {
      try {
        const [teams, projects, people, sessions] = await Promise.all([
          api.teams(days),
          api.projects(days),
          api.people(days),
          api.sessions({ days, limit: SESSION_LIMIT }),
        ]);
        setBundle({
          teams: teams.teams ?? [],
          projects: projects.projects ?? [],
          people: people.people ?? [],
          sessions: sessions.sessions ?? [],
        });
      } catch (e) {
        setDataErr(e instanceof Error ? e.message : String(e));
        startedRef.current = false; // allow a retry on the next keystroke
      } finally {
        setLoadingData(false);
      }
    })();
  }, [open, q, days]);

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

  const commands = useMemo<Item[]>(() => {
    const nav: Item[] = NAV.map((n) => ({
      kind: "page",
      id: `go:${n.to}`,
      label: n.label,
      run: () => navigate(n.to),
    }));
    const actions: Item[] = [
      {
        kind: "action",
        id: "theme",
        label: mode === "dark" ? "Switch to light theme" : "Switch to dark theme",
        run: () => setMode(mode === "dark" ? "light" : "dark"),
      },
    ];
    return [...nav, ...actions];
  }, [navigate, mode, setMode]);

  // Build the flat, ordered, filtered item list. Nav + actions filter locally;
  // the data sections appear only once the bundle has loaded for the query.
  const items = useMemo<Item[]>(() => {
    const navMatches = !q
      ? commands
      : commands.filter((c) => "label" in c && c.label.toLowerCase().includes(q));
    if (q.length < SEARCH_LEN || !bundle) return navMatches;

    const teams: Item[] = bundle.teams
      .filter((t) => t.display_name.toLowerCase().includes(q))
      .slice(0, 6)
      .map((t) => ({ kind: "team", id: `team:${t.team_id}`, team: t }));
    const projects: Item[] = bundle.projects
      .filter((p) => projectName(p.project_root).toLowerCase().includes(q) || p.project_root.toLowerCase().includes(q))
      .slice(0, 6)
      .map((p) => ({ kind: "project", id: `proj:${p.project_id}`, project: p }));
    const people: Item[] = bundle.people
      .filter((p) =>
        [p.display_name, p.email, p.user_id].some((s) => s?.toLowerCase().includes(q)),
      )
      .slice(0, 6)
      .map((p) => ({ kind: "person", id: `person:${p.user_id}`, person: p }));
    const sessions: Item[] = bundle.sessions
      .filter((s) =>
        [s.session_id, s.display_name, s.email, s.tool, s.model].some((v) =>
          v?.toLowerCase().includes(q),
        ),
      )
      .slice(0, 6)
      .map((s) => ({ kind: "session", id: `sess:${s.session_id}`, session: s }));
    return [...navMatches, ...teams, ...projects, ...people, ...sessions];
  }, [commands, q, bundle]);

  useEffect(() => {
    if (active >= items.length) setActive(Math.max(0, items.length - 1));
  }, [items.length, active]);

  const sections = useMemo(() => {
    const buckets: Record<string, { item: Item; idx: number }[]> = {};
    const titles: Record<string, string> = {
      page: "Jump to",
      action: "Actions",
      team: "Teams",
      project: "Projects",
      person: "People",
      session: "Recent sessions",
    };
    const order = ["page", "action", "team", "project", "person", "session"];
    items.forEach((item, idx) => {
      (buckets[item.kind] ??= []).push({ item, idx });
    });
    return order
      .filter((k) => buckets[k]?.length)
      .map((k) => ({ title: titles[k], rows: buckets[k] }));
  }, [items]);

  if (!open) return null;

  function activate(it: Item | undefined) {
    if (!it) return;
    if (it.kind === "page" || it.kind === "action") it.run();
    else if (it.kind === "team") navigate(`/teams/${encodeURIComponent(it.team.team_id)}`);
    else if (it.kind === "project") navigate(`/projects/${encodeURIComponent(it.project.project_id)}`);
    else if (it.kind === "person") navigate("/people");
    else if (it.kind === "session")
      navigate(`/sessions?focus=${encodeURIComponent(it.session.session_id)}`);
    onClose();
  }

  function onKey(e: React.KeyboardEvent) {
    if (e.key === "Escape") {
      onClose();
    } else if (e.key === "ArrowDown") {
      e.preventDefault();
      setActive((a) => Math.min(items.length - 1, a + 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setActive((a) => Math.max(0, a - 1));
    } else if (e.key === "Enter") {
      e.preventDefault();
      activate(items[active]);
    }
  }

  const searching = q.length >= SEARCH_LEN;

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
            placeholder="Jump to a page, or search teams, projects, people, sessions…"
            className="h-11 w-full bg-transparent text-[13px] text-fg-1 placeholder:text-fg-4 focus:outline-none"
          />
          <kbd className="rounded-1 border border-line-3 bg-bg-3 px-1.5 py-0.5 font-mono text-[10px] text-fg-3">esc</kbd>
        </div>
        <div className="max-h-[52vh] overflow-y-auto p-1.5">
          {dataErr && (
            <p className="px-3 py-2 text-[11px] text-bad">Search failed: {dataErr}</p>
          )}
          {items.length === 0 ? (
            <p className="px-3 py-6 text-center text-[12px] text-fg-3">
              {searching && loadingData ? "Searching…" : "No matches."}
            </p>
          ) : (
            sections.map((sec) => (
              <div key={sec.title}>
                <div className="px-3 pb-1 pt-2 text-[10px] font-semibold uppercase tracking-[0.08em] text-fg-4">
                  {sec.title}
                </div>
                {sec.rows.map(({ item, idx }) => (
                  <button
                    key={item.id}
                    type="button"
                    onMouseEnter={() => setActive(idx)}
                    onClick={() => activate(item)}
                    className={clsx(
                      "flex w-full items-center gap-2 rounded-2 px-3 py-2 text-left text-[12.5px]",
                      idx === active ? "bg-bg-3 text-fg-0" : "text-fg-2",
                    )}
                  >
                    <RowBody item={item} mode={mode} />
                  </button>
                ))}
              </div>
            ))
          )}
          {searching && loadingData && items.length > 0 && (
            <p className="px-3 py-1.5 text-[10.5px] text-fg-4">Searching org data…</p>
          )}
        </div>
      </div>
    </div>
  );
}

function RowBody({ item, mode }: { item: Item; mode: string }): ReactNode {
  if (item.kind === "action") {
    return (
      <>
        <span className="flex items-center gap-2">
          {mode === "dark" ? <Sun size={13} /> : <Moon size={13} />}
          {item.label}
        </span>
        <span className="ml-auto text-[10px] uppercase tracking-wide text-fg-4">Action</span>
      </>
    );
  }
  if (item.kind === "page") {
    return (
      <>
        <span>{item.label}</span>
        <span className="ml-auto text-[10px] uppercase tracking-wide text-fg-4">Go to</span>
      </>
    );
  }
  if (item.kind === "team") {
    return (
      <>
        <span className="truncate">{item.team.display_name}</span>
        <span className="ml-auto shrink-0 font-mono text-[11px] text-fg-3">{usd(item.team.cost_usd)}</span>
      </>
    );
  }
  if (item.kind === "project") {
    return (
      <>
        <span className="truncate" title={item.project.project_root}>
          {projectName(item.project.project_root)}
        </span>
        <span className="ml-auto shrink-0 font-mono text-[11px] text-fg-3">{usd(item.project.cost_usd)}</span>
      </>
    );
  }
  if (item.kind === "person") {
    const p = item.person;
    return (
      <>
        <span className="truncate">{p.display_name || p.email || p.user_id}</span>
        <span className="ml-auto shrink-0 font-mono text-[11px] text-fg-3">{usd(p.cost_usd)}</span>
      </>
    );
  }
  const s = item.session;
  return (
    <>
      {s.tool ? <ToolBadge tool={s.tool} /> : null}
      <span className="min-w-0 truncate font-mono text-[11px] text-accent">
        {s.session_id.slice(0, 12)}…
      </span>
      <span className="min-w-0 truncate text-fg-3">{s.display_name || s.email || s.user_id}</span>
      <span className="ml-auto shrink-0 font-mono text-[11px] text-fg-3">{usd(s.cost_usd)}</span>
    </>
  );
}
