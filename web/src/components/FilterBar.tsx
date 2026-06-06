import { useMemo } from "react";
import clsx from "clsx";
import { useFilters, windowDays, type Window } from "@/lib/filters";
import { useApi } from "@/lib/useApi";
import type { ToolsResponse, ProjectsResponse } from "@/lib/types";
import { toolMeta } from "@/lib/tools";
import { ComboChip, type ComboOption, ToolDot, Tooltip } from "./primitives";
import { HelpInd } from "./HelpInd";

const WINDOWS: { value: Window; label: string }[] = [
  { value: "7d", label: "7d" },
  { value: "14d", label: "14d" },
  { value: "30d", label: "30d" },
  { value: "90d", label: "90d" },
  { value: "1y", label: "1y" },
  { value: "all", label: "All" },
];

export function FilterBar({
  onOpenPalette,
}: {
  onOpenPalette: () => void;
}) {
  const { win, setWin, tool, setTool, project, setProject, query } =
    useFilters();
  const days = windowDays(win);
  const daysParam = days === "all" ? 36500 : days;

  // /api/tools is the source of truth for which tools have data in
  // the active window (mirrors the legacy dashboard's behaviour).
  const tools = useApi<ToolsResponse>("/api/tools", { days: daysParam }, [win]);
  const projects = useApi<ProjectsResponse>("/api/projects");

  return (
    <div className="flex h-[var(--filterbar-h)] items-center gap-3 border-b border-line-1 bg-bg-1 px-5">
      <span className="flex items-center gap-1 text-[10px] font-semibold uppercase tracking-[0.08em] text-fg-3">
        Window
        <HelpInd id="filter.window" />
      </span>
      <div className="flex items-center gap-1 rounded-2 border border-line-2 bg-bg-2 p-0.5">
        {WINDOWS.map((w) => (
          <button
            key={w.value}
            type="button"
            onClick={() => setWin(w.value)}
            className={clsx(
              "rounded-1 px-2.5 py-1 text-[11px] font-medium transition-colors",
              win === w.value
                ? "bg-bg-4 text-fg-0"
                : "text-fg-2 hover:text-fg-1",
            )}
          >
            {w.label}
          </button>
        ))}
      </div>

      <ToolSelect
        value={tool}
        onChange={setTool}
        tools={tools.data?.tools ?? []}
      />

      <ProjectSelect
        value={project}
        onChange={setProject}
        projects={projects.data?.rows ?? []}
      />

      <SearchTrigger query={query} onOpen={onOpenPalette} />

      <div className="flex-1" />
      <span className="text-[11px] text-fg-3">
        {tools.loading || projects.loading ? "loading filters…" : null}
      </span>
    </div>
  );
}

// SearchTrigger — button-chip that opens the ⌘K palette. Mirrors
// design/shell.jsx:168-172. When a global query is set, the chip
// shows it (truncated) so the user has visible feedback that a
// search is in effect. The ⌘K shortcut is wired in App.tsx so it
// works from anywhere, not just FilterBar mount.
function SearchTrigger({
  query,
  onOpen,
}: {
  query: string;
  onOpen: () => void;
}) {
  const trimmed = query.trim();
  return (
    <Tooltip
      content={
        <>
          Open command palette <kbd>⌘K</kbd> — search pages, sessions, actions
        </>
      }
      maxWidth={320}
    >
    <button
      type="button"
      onClick={onOpen}
      className={clsx(
        "flex h-7 items-center gap-1.5 rounded-2 border bg-bg-2 px-2 text-[11px] transition-colors",
        trimmed
          ? "border-accent/50 text-fg-1"
          : "border-line-2 text-fg-3 hover:bg-bg-3 hover:text-fg-1",
      )}
    >
      <SearchIcon />
      {trimmed ? (
        <span className="max-w-[180px] truncate font-mono text-[11px] text-fg-0">
          {trimmed}
        </span>
      ) : (
        <span>Search anything…</span>
      )}
      <kbd className="ml-0.5 rounded-1 border border-line-3 bg-bg-3 px-1 py-px font-mono text-[9.5px] text-fg-3">
        ⌘K
      </kbd>
    </button>
    </Tooltip>
  );
}

function SearchIcon() {
  return (
    <svg width="11" height="11" viewBox="0 0 16 16" fill="none">
      <circle
        cx="7"
        cy="7"
        r="4.5"
        stroke="currentColor"
        strokeWidth="1.4"
      />
      <path
        d="m10.5 10.5 3 3"
        stroke="currentColor"
        strokeWidth="1.4"
        strokeLinecap="round"
      />
    </svg>
  );
}

function ToolSelect({
  value,
  onChange,
  tools,
}: {
  value: string;
  onChange: (s: string) => void;
  tools: { tool: string; action_count: number }[];
}) {
  const options = useMemo<ComboOption[]>(() => {
    const opts: ComboOption[] = [
      {
        value: "all",
        label: "All tools",
        searchable: "all tools",
      },
    ];
    for (const t of tools) {
      const meta = toolMeta(t.tool);
      opts.push({
        value: t.tool,
        label: meta.label,
        searchable: `${meta.label} ${t.tool}`.toLowerCase(),
        leading: <ToolDot tool={t.tool} />,
        rightMeta: t.action_count.toLocaleString(),
      });
    }
    return opts;
  }, [tools]);

  return (
    <ComboChip
      label="Tool"
      value={value}
      onChange={onChange}
      options={options}
      icon={<ToolIcon />}
      popoverWidth={300}
      placeholder="Filter tools…"
      buttonValueRender={(sel) => {
        if (value === "all" || !sel) {
          return <b className="font-semibold text-fg-0">all</b>;
        }
        return (
          <span className="flex items-center gap-1.5">
            <ToolDot tool={value} />
            <b className="font-semibold text-fg-0">{toolMeta(value).label}</b>
          </span>
        );
      }}
    />
  );
}

function ProjectSelect({
  value,
  onChange,
  projects,
}: {
  value: string;
  onChange: (s: string) => void;
  projects: { root_path: string; action_count: number }[];
}) {
  const options = useMemo<ComboOption[]>(() => {
    const opts: ComboOption[] = [
      {
        value: "all",
        label: "All projects",
        searchable: "all projects",
      },
    ];
    for (const p of projects) {
      opts.push({
        value: p.root_path,
        label: (
          <span className="font-mono text-[11px]">{shortenPath(p.root_path)}</span>
        ),
        searchable: p.root_path.toLowerCase(),
        title: p.root_path,
        rightMeta: p.action_count.toLocaleString(),
      });
    }
    return opts;
  }, [projects]);

  return (
    <ComboChip
      label="Project"
      value={value}
      onChange={onChange}
      options={options}
      icon={<FolderIcon />}
      popoverWidth={420}
      placeholder="Filter projects…"
      buttonValueRender={(sel) => {
        if (value === "all" || !sel) {
          return <b className="font-semibold text-fg-0">all</b>;
        }
        return (
          <Tooltip
            content={<span className="break-all font-mono">{value}</span>}
            maxWidth={420}
          >
            <b
              tabIndex={0}
              className="max-w-[200px] cursor-help truncate font-mono text-[11px] font-semibold text-fg-0 focus:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent-ring)]"
            >
              {shortenPath(value)}
            </b>
          </Tooltip>
        );
      }}
    />
  );
}

function ToolIcon() {
  return (
    <svg
      width="13"
      height="13"
      viewBox="0 0 16 16"
      fill="none"
      aria-hidden
      className="text-fg-3"
    >
      <path
        d="M8 1.5 2 5v6l6 3.5L14 11V5L8 1.5Z"
        stroke="currentColor"
        strokeWidth="1.4"
        strokeLinejoin="round"
      />
      <path
        d="M2 5l6 3.5m0 0L14 5M8 8.5V14.5"
        stroke="currentColor"
        strokeWidth="1.4"
        strokeLinejoin="round"
      />
    </svg>
  );
}

function FolderIcon() {
  return (
    <svg
      width="13"
      height="13"
      viewBox="0 0 16 16"
      fill="none"
      aria-hidden
      className="text-fg-3"
    >
      <path
        d="M2 4.5A1 1 0 0 1 3 3.5h3.6l1.4 1.5H13a1 1 0 0 1 1 1V12a1 1 0 0 1-1 1H3a1 1 0 0 1-1-1V4.5Z"
        stroke="currentColor"
        strokeWidth="1.4"
        strokeLinejoin="round"
      />
    </svg>
  );
}

function shortenPath(p: string): string {
  if (!p) return "—";
  const parts = p.split("/").filter(Boolean);
  if (parts.length <= 2) return p;
  return ".../" + parts.slice(-2).join("/");
}
