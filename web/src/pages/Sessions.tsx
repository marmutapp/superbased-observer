import { useEffect, useMemo, useState } from "react";
import { useSearchParams } from "react-router-dom";
import type { ColumnDef, SortingState } from "@tanstack/react-table";
import {
  ActiveFilterChips,
  ChartShell,
  type FilterChip,
  ModelDot,
  PageHeader,
  Pill,
  SegmentedControl,
  ToolBadge,
  Tooltip,
} from "@/components/primitives";
import { shortModel } from "@/lib/models";
import { HelpInd } from "@/components/HelpInd";
import { CopyOnClick } from "@/components/CopyOnClick";
import { DataTable, Pagination } from "@/components/DataTable";
import { ChartState } from "@/components/ChartState";
import { SessionDetailPanel } from "@/components/SessionDetailPanel";
import { useFilters, windowDays } from "@/lib/filters";
import { useApi } from "@/lib/useApi";
import {
  fmtCompact,
  fmtDuration,
  fmtInt,
  fmtPct,
  fmtUSD,
} from "@/lib/format";
import type {
  SessionRow,
  SessionsCalendarResponse,
  SessionsResponse,
} from "@/lib/types";
import {
  activeFilterCount,
  applyFilters,
  EMPTY_FILTERS,
  loadFilters,
  saveFilters,
  type SessionFilters,
  SessionsFiltersDrawer,
} from "./sessions/FiltersDrawer";

const PAGE_LIMIT = 50;

type View = "table" | "calendar";

export function SessionsPage() {
  const { win, tool, project, query: globalQuery } = useFilters();
  const days = windowDays(win);
  const daysParam = days === "all" ? 36500 : days;
  const toolParam = tool === "all" ? undefined : tool;
  const projectParam = project === "all" ? undefined : project;

  const [view, setView] = useState<View>("table");
  const [page, setPage] = useState(1);
  // Server-side sort. The table is controlled (manualSorting): a header click
  // updates this state, which feeds sort_by/sort_dir into the fetch so the
  // server orders the WHOLE filtered set before paging. Without this, sorting
  // reordered only the loaded page (e.g. "cost desc" showed the priciest of
  // the visible 20, not the priciest session overall).
  const [sorting, setSorting] = useState<SortingState>([
    { id: "started_at", desc: true },
  ]);
  const sortBy = sorting[0]?.id ?? "started_at";
  const sortDir = sorting[0]?.desc === false ? "asc" : "desc";
  const [localQuery, setLocalQuery] = useState("");
  // pickedDay is set when the user clicks a CalendarView cell — drives
  // a server-side from_date/to_date filter on /api/sessions so the
  // refetch returns that day's sessions regardless of pagination
  // position. Local substring filtering against the loaded page can't
  // see sessions from days outside the current page (e.g. clicking a
  // calendar cell from a month ago when the table only has the most
  // recent page-50 loaded).
  const [pickedDay, setPickedDay] = useState<string | null>(null);
  // Deep-linkable detail panel: /sessions?session=<id> opens it directly
  // (the Suggestions page links session-scoped suggestions here). The URL
  // is the source of truth so back/forward and copy-paste both work.
  const [searchParams, setSearchParams] = useSearchParams();
  const selected = searchParams.get("session");
  const setSelected = (id: string | null) => {
    setSearchParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        if (id) {
          next.set("session", id);
        } else {
          next.delete("session");
        }
        return next;
      },
      { replace: true },
    );
  };
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [filters, setFilters] = useState<SessionFilters>(() => loadFilters());
  useEffect(() => {
    saveFilters(filters);
  }, [filters]);
  const drawerCount = activeFilterCount(filters);

  // Reset page when filters change (incl. picked calendar day).
  const filterKey = `${win}|${tool}|${project}|${pickedDay ?? ""}|${sortBy}|${sortDir}`;
  const lastKey = useMemo(() => filterKey, [filterKey]);
  // eslint-disable-next-line react-hooks/exhaustive-deps
  useMemo(() => setPage(1), [lastKey]);

  const sessions = useApi<SessionsResponse>(
    "/api/sessions",
    {
      page,
      limit: PAGE_LIMIT,
      tool: toolParam,
      project: projectParam,
      days: daysParam,
      from_date: pickedDay ?? undefined,
      to_date: pickedDay ?? undefined,
      sort_by: sortBy,
      sort_dir: sortDir,
    },
    [page, win, tool, project, pickedDay, sortBy, sortDir],
    // Live-capture refresh: 5s while the tab is visible. Lets fresh
    // Antigravity-CLI .pb files (and any other in-progress session)
    // appear without requiring the operator to manually reload.
    { refreshMs: 5000 },
  );

  // Per-day rollup over the full window — drives the Calendar view
  // so the grid carries real data across the configured Window, not
  // just whatever's in the most recent 50 rows of the table page.
  // Fetched only when Calendar view is active to avoid the cost on
  // the default Table view.
  const calendar = useApi<SessionsCalendarResponse>(
    view === "calendar" ? "/api/sessions/calendar" : null,
    {
      tool: toolParam,
      project: projectParam,
      days: daysParam,
    },
    [view, win, tool, project],
  );

  const rows = sessions.data?.rows ?? [];
  const query = localQuery || globalQuery;
  const filtered = useMemo(() => {
    let out = rows;
    if (drawerCount > 0) out = applyFilters(out, filters);
    const q = query.trim().toLowerCase();
    if (q) {
      out = out.filter(
        (r) =>
          r.id.toLowerCase().includes(q) ||
          (r.project ?? "").toLowerCase().includes(q) ||
          // Typing "2026-05-16" into the search box still substring-
          // matches against started_at. Calendar day-click uses the
          // server-side from_date/to_date path via pickedDay so it
          // works across pages, not just the loaded slice.
          (r.started_at ?? "").toLowerCase().includes(q),
      );
    }
    return out;
  }, [rows, query, filters, drawerCount]);

  // Models seen in the loaded page — feeds the drawer's Models chip
  // group so the user only sees models that would actually match a
  // visible session. Deduplicated; order matches first-seen.
  const availableModels = useMemo(() => {
    const seen = new Set<string>();
    const out: string[] = [];
    for (const r of rows) {
      for (const m of r.models ?? []) {
        if (!seen.has(m)) {
          seen.add(m);
          out.push(m);
        }
      }
    }
    return out;
  }, [rows]);

  const drawerChips = useMemo<FilterChip[]>(() => {
    const c: FilterChip[] = [];
    if (filters.models.length > 0) {
      c.push({
        label: `models: ${filters.models.length}`,
        title: filters.models.join(", "),
        onClear: () => setFilters((f) => ({ ...f, models: [] })),
      });
    }
    if (filters.minCostUsd || filters.maxCostUsd) {
      c.push({
        label: `cost: ${rangeLabel(filters.minCostUsd, filters.maxCostUsd, "$")}`,
        onClear: () =>
          setFilters((f) => ({ ...f, minCostUsd: "", maxCostUsd: "" })),
      });
    }
    if (filters.minActions || filters.maxActions) {
      c.push({
        label: `actions: ${rangeLabel(filters.minActions, filters.maxActions, "")}`,
        onClear: () =>
          setFilters((f) => ({ ...f, minActions: "", maxActions: "" })),
      });
    }
    if (filters.duration !== "any") {
      c.push({
        label: `duration: ${durationLabel(filters.duration)}`,
        onClear: () => setFilters((f) => ({ ...f, duration: "any" })),
      });
    }
    if (filters.sidechain !== "any") {
      c.push({
        label:
          filters.sidechain === "with" ? "with sidechain" : "no sidechain",
        onClear: () => setFilters((f) => ({ ...f, sidechain: "any" })),
      });
    }
    if (filters.reliability !== "any") {
      c.push({
        label: `reliability: ${filters.reliability}`,
        onClear: () => setFilters((f) => ({ ...f, reliability: "any" })),
      });
    }
    return c;
  }, [filters]);

  const showScoring = (sessions.data?.scored_count ?? 0) > 0;
  const columns = useMemo<ColumnDef<SessionRow, unknown>[]>(
    () => buildColumns(showScoring),
    [showScoring],
  );

  return (
    <div className="space-y-4 p-6">
      <PageHeader
        title="Sessions"
        sub="One row per AI-coding session. Click a row to see action breakdown, token buckets, cost summary, and a full messages timeline with expandable tool calls."
        helpId="tab.sessions"
        right={
          <SegmentedControl<View>
            options={[
              { value: "table", label: "Table" },
              { value: "calendar", label: "Calendar" },
            ]}
            value={view}
            onChange={setView}
            size="sm"
          />
        }
      />
      {!showScoring && sessions.data && <ScoringHintBanner />}

      <ChartShell
        title={
          <span className="flex items-baseline gap-2">
            All sessions
            {sessions.data && (
              <Pill>{fmtInt(sessions.data.total)} total</Pill>
            )}
          </span>
        }
        sub="click any row for the per-session breakdown"
        right={
          <div className="flex items-center gap-2">
            <input
              type="search"
              placeholder="filter by id, project…"
              value={localQuery}
              onChange={(e) => setLocalQuery(e.target.value)}
              className="h-7 w-[240px] rounded-2 border border-line-2 bg-bg-2 px-2 font-mono text-[11px] text-fg-1 placeholder:text-fg-4 focus:border-accent focus:outline-none"
            />
            <Tooltip
              content={
                drawerCount > 0
                  ? `${drawerCount} drawer filter${drawerCount === 1 ? "" : "s"} active — click to edit`
                  : "Open the filters drawer (model, cost, duration, sidechain…)"
              }
              maxWidth={320}
            >
              <button
                type="button"
                onClick={() => setDrawerOpen(true)}
                className={
                  drawerCount > 0
                    ? "inline-flex items-center gap-1.5 rounded-2 border border-accent/50 bg-accent-soft px-2.5 py-1 text-[11px] text-accent hover:bg-accent-soft/70"
                    : "inline-flex items-center gap-1.5 rounded-2 border border-line-2 bg-bg-2 px-2.5 py-1 text-[11px] text-fg-2 hover:bg-bg-3"
                }
              >
                Filters
                {drawerCount > 0 && (
                  <span className="rounded-pill bg-accent px-1.5 py-px font-mono text-[9.5px] font-semibold leading-none text-accent-on">
                    {drawerCount}
                  </span>
                )}
              </button>
            </Tooltip>
            <Tooltip content="Download visible sessions as CSV">
              <button
                type="button"
                onClick={() => exportSessionsCsv(filtered)}
                disabled={!filtered.length}
                className="rounded-2 border border-line-2 bg-bg-2 px-2.5 py-1 text-[11px] text-fg-2 hover:bg-bg-3 disabled:opacity-40"
              >
                Export
              </button>
            </Tooltip>
          </div>
        }
      >
        {(pickedDay || drawerChips.length > 0) && (
          <ActiveFilterChips
            className="mb-3"
            chips={[
              ...(pickedDay
                ? [
                    {
                      label: `day: ${pickedDay}`,
                      onClear: () => setPickedDay(null),
                    } as FilterChip,
                  ]
                : []),
              ...drawerChips,
            ]}
            onClearAll={() => {
              setPickedDay(null);
              setFilters(EMPTY_FILTERS);
            }}
          />
        )}
        {view === "calendar" ? (
          <CalendarView
            cells={calendar.data?.cells ?? []}
            loading={calendar.loading}
            windowDaysParam={daysParam}
            onPickDay={(day) => {
              // Filter table to the picked day via a SERVER-side
              // from_date/to_date filter, then swap to table view. A
              // local substring filter against the loaded page would
              // silently miss any day outside the page-50 slice.
              setPickedDay(day);
              setLocalQuery("");
              setView("table");
            }}
          />
        ) : (
          <>
            <ChartState
              loading={sessions.loading && !sessions.data}
              error={sessions.error}
              empty={!sessions.loading && filtered.length === 0}
              emptyHint={
                pickedDay
                  ? `No sessions on ${pickedDay}. Clear the day filter to see all sessions.`
                  : query
                    ? `No sessions match "${query}". Clear the search or widen filters.`
                    : "No sessions in window. Run `observer init` to register hooks with your AI tools."
              }
              height={240}
            >
              <DataTable<SessionRow>
                data={filtered}
                columns={columns}
                onRowClick={(r) => setSelected(r.id)}
                rowKey={(r) => r.id}
                minWidth={840}
                loading={sessions.loading}
                sorting={sorting}
                onSortingChange={setSorting}
              />
            </ChartState>

            {sessions.data && (
              <div className="flex items-center justify-between gap-3">
                <Pagination
                  page={sessions.data.page}
                  limit={sessions.data.limit}
                  total={sessions.data.total}
                  onPage={setPage}
                  loading={sessions.loading}
                />
                {(sessions.data.page_cost_usd ?? 0) > 0 && (
                  <span className="shrink-0 pt-3 text-[11px] tabular-nums text-fg-3">
                    page cost{" "}
                    <span className="text-fg-1">
                      {fmtUSD(sessions.data.page_cost_usd ?? 0)}
                    </span>
                  </span>
                )}
              </div>
            )}
          </>
        )}
      </ChartShell>

      <SessionDetailPanel
        sessionId={selected}
        open={selected != null}
        onClose={() => setSelected(null)}
      />

      <SessionsFiltersDrawer
        open={drawerOpen}
        onClose={() => setDrawerOpen(false)}
        value={filters}
        onApply={(next) => {
          setFilters(next);
          setDrawerOpen(false);
        }}
        availableModels={availableModels}
      />
    </div>
  );
}

function rangeLabel(min: string, max: string, prefix: string): string {
  const lo = min.trim();
  const hi = max.trim();
  if (lo && hi) return `${prefix}${lo}–${prefix}${hi}`;
  if (lo) return `≥ ${prefix}${lo}`;
  if (hi) return `≤ ${prefix}${hi}`;
  return "—";
}

function durationLabel(d: SessionFilters["duration"]): string {
  switch (d) {
    case "short":
      return "<5m";
    case "medium":
      return "5–30m";
    case "long":
      return "30m–2h";
    case "xlong":
      return ">2h";
    default:
      return "any";
  }
}

function ScoringHintBanner() {
  return (
    <div className="flex items-start gap-3 rounded-3 border border-warn/30 bg-warn-soft/60 px-4 py-2.5 text-[11.5px]">
      <span className="mt-0.5 grid h-4 w-4 shrink-0 place-items-center rounded-full border border-warn/40 text-warn">
        i
      </span>
      <div className="text-fg-2">
        <b className="text-fg-1">
          Quality / Errors / Redundancy scoring is hidden.
        </b>{" "}
        Run{" "}
        <code className="rounded-1 border border-line-3 bg-bg-3 px-1.5 py-0.5 font-mono text-[11px] text-fg-1">
          observer score
        </code>{" "}
        to populate the columns. Sessions get a 0–100 quality score and a
        redundancy index that flags repeat-work patterns.
      </div>
    </div>
  );
}

// CalendarView — per-day grid heat-mapped by session count + total
// cost. Sources data from /api/sessions/calendar (server-side GROUP
// BY date(started_at) over the configured Window) so the cells
// reflect activity across the entire window, not just the most
// recent paginated slice. Days with no activity render as empty
// grey cells so the window's span is always visible. Clicking a
// day filters the table view to that day's ISO prefix.
function CalendarView({
  cells,
  loading,
  windowDaysParam,
  onPickDay,
}: {
  cells: { day: string; session_count: number; cost_usd: number }[];
  loading: boolean;
  windowDaysParam: number;
  onPickDay?: (day: string) => void;
}) {
  const byDay = useMemo(() => {
    const m = new Map<string, { count: number; cost: number }>();
    for (const c of cells) {
      m.set(c.day, { count: c.session_count, cost: c.cost_usd });
    }
    return m;
  }, [cells]);

  // Bracket the visible range to the configured Window — today back
  // `windowDaysParam` days — instead of the min/max day in the loaded
  // data. This way switching the Window to 90d always shows a 90-day
  // calendar regardless of whether earlier days had activity. Falls
  // back to a 30-day window when the caller passes a non-positive
  // value. The "all" Window passes 36500; cap the displayed span at
  // 365 so the calendar doesn't render thousands of empty cells.
  const span = clampSpanDays(windowDaysParam);
  const today = new Date();
  today.setUTCHours(0, 0, 0, 0);
  const last = today;
  const first = new Date(today);
  first.setUTCDate(first.getUTCDate() - (span - 1));
  const start = startOfWeek(first);
  const end = endOfWeek(last);
  const gridCells: Date[] = [];
  for (let cur = start; cur <= end; cur = nextDay(cur)) gridCells.push(cur);
  const maxCost = Math.max(...[...byDay.values()].map((v) => v.cost), 0);
  const totalCost = [...byDay.values()].reduce((a, v) => a + v.cost, 0);
  const totalSessions = [...byDay.values()].reduce((a, v) => a + v.count, 0);
  // Date-range header — "May 1 – May 30" rendered above the grid so
  // the user has anchoring context. Falls back to UTC since cells
  // themselves are UTC-indexed.
  const fmtHeader = (d: Date) =>
    d.toLocaleDateString("en-US", {
      month: "short",
      day: "numeric",
      timeZone: "UTC",
    });

  return (
    <div className="rounded-3 border border-line-2 bg-bg-2 p-4">
      <header className="mb-3 flex flex-wrap items-baseline justify-between gap-2 border-b border-line-1 pb-2.5">
        <div className="flex items-baseline gap-2">
          <span className="text-[12px] font-semibold uppercase tracking-[0.06em] text-fg-1">
            {fmtHeader(first)} – {fmtHeader(last)}
          </span>
          <span className="font-mono text-[10.5px] text-fg-3">
            {byDay.size} active day{byDay.size === 1 ? "" : "s"}
          </span>
          {loading && (
            <span
              aria-label="loading"
              className="inline-block h-2 w-2 animate-pulse rounded-full bg-accent"
            />
          )}
        </div>
        <div className="flex items-baseline gap-3 font-mono text-[10.5px] text-fg-3">
          <span>
            {fmtInt(totalSessions)} session{totalSessions === 1 ? "" : "s"}
          </span>
          <span className="font-semibold text-fg-1">
            {fmtUSD(totalCost)}
          </span>
        </div>
      </header>
      <div className="mb-2 grid grid-cols-7 gap-1.5 text-center text-[10px] font-semibold uppercase tracking-[0.06em] text-fg-3">
        {["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"].map((d) => (
          <div key={d}>{d}</div>
        ))}
      </div>
      <div className="grid grid-cols-7 gap-1.5">
        {gridCells.map((d) => {
          const key = d.toISOString().slice(0, 10);
          const slot = byDay.get(key);
          const cost = slot?.cost ?? 0;
          // Log scale + brighter floor so even small days read.
          const intensity = maxCost > 0
            ? Math.min(1, Math.log1p(cost) / Math.log1p(maxCost))
            : 0;
          const hasData = !!slot;
          const bgPct = intensity > 0 ? (12 + intensity * 60).toFixed(0) : "0";
          return (
            <Tooltip
              key={key}
              content={
                hasData
                  ? `${key} · ${slot!.count} sessions · ${fmtUSD(cost)}`
                  : key
              }
            >
            <button
              type="button"
              onClick={hasData && onPickDay ? () => onPickDay(key) : undefined}
              disabled={!hasData}
              className={
                "group/cal relative flex h-[94px] flex-col items-stretch overflow-hidden rounded-2 border px-2 py-1.5 text-left transition-all " +
                (hasData
                  ? "cursor-pointer border-line-2 hover:-translate-y-0.5 hover:border-accent/70 hover:shadow-drawer"
                  : "cursor-default border-line-1 bg-bg-3/30 text-fg-4")
              }
              style={
                hasData
                  ? {
                      background: `linear-gradient(155deg, color-mix(in srgb, var(--accent) ${bgPct}%, var(--bg-2)) 0%, var(--bg-2) 100%)`,
                    }
                  : undefined
              }
            >
              <div className="flex items-baseline justify-between">
                <span className="font-mono text-[10.5px] font-semibold text-fg-2">
                  {d.getUTCDate()}
                </span>
                {hasData && (
                  <span
                    aria-hidden
                    className="h-1.5 w-1.5 rounded-full bg-accent opacity-90"
                  />
                )}
              </div>
              {hasData ? (
                <div className="mt-auto flex flex-col gap-0.5">
                  <span className="font-mono text-[20px] font-bold leading-none tabular-nums text-fg-0">
                    {slot!.count}
                  </span>
                  <span className="font-mono text-[10.5px] tabular-nums text-fg-2">
                    {fmtUSD(cost)}
                  </span>
                </div>
              ) : (
                <span className="mt-auto text-[10px] text-fg-4">—</span>
              )}
            </button>
            </Tooltip>
          );
        })}
      </div>
      <div className="mt-3 flex items-center justify-between gap-3 text-[10.5px] text-fg-3">
        <p>Click a day to switch to Table view filtered to that day.</p>
        <div className="flex items-center gap-1.5">
          <span className="text-fg-4">less</span>
          {[0.15, 0.35, 0.55, 0.75, 0.95].map((s) => (
            <span
              key={s}
              className="h-2.5 w-3 rounded-[2px]"
              style={{
                background: `color-mix(in srgb, var(--accent) ${(12 + s * 60).toFixed(0)}%, var(--bg-2))`,
              }}
            />
          ))}
          <span className="text-fg-4">more</span>
        </div>
      </div>
    </div>
  );
}

function clampSpanDays(n: number): number {
  if (!Number.isFinite(n) || n <= 0) return 30;
  return Math.min(365, Math.round(n));
}

function startOfWeek(d: Date): Date {
  const out = new Date(d);
  out.setUTCHours(0, 0, 0, 0);
  out.setUTCDate(out.getUTCDate() - out.getUTCDay());
  return out;
}

function endOfWeek(d: Date): Date {
  const out = new Date(d);
  out.setUTCHours(0, 0, 0, 0);
  out.setUTCDate(out.getUTCDate() + (6 - out.getUTCDay()));
  return out;
}

function nextDay(d: Date): Date {
  const out = new Date(d);
  out.setUTCDate(out.getUTCDate() + 1);
  return out;
}

function buildColumns(showScoring: boolean): ColumnDef<SessionRow, unknown>[] {
  // Column order matches design/page-sessions.jsx exactly:
  // Session / Tool / Project / Model(s) / Started / Elapsed / Actions /
  // Sub / Input / Cache R / Cache W / Output / API $ / Tool $ / Total $
  // Reliability and scoring (optional) come after.
  const cols: ColumnDef<SessionRow, unknown>[] = [
    {
      id: "session",
      header: () => <>Session<HelpInd id="column.sessions.id" /></>,
      accessorKey: "id",
      cell: ({ row }) => (
        <CopyOnClick
          value={row.original.id}
          className="font-mono text-[11px] text-accent hover:text-accent-strong"
        >
          {row.original.id.slice(0, 12)}…
        </CopyOnClick>
      ),
    },
    {
      id: "tool",
      header: () => <>Tool<HelpInd id="column.sessions.tool" /></>,
      accessorKey: "tool",
      cell: ({ row }) => <ToolBadge tool={row.original.tool} />,
    },
    {
      id: "project",
      header: () => <>Project<HelpInd id="column.sessions.project" /></>,
      accessorKey: "project",
      cell: ({ row }) =>
        row.original.project ? (
          <Tooltip
            content={<span className="break-all font-mono">{row.original.project}</span>}
            maxWidth={420}
          >
            <span
              tabIndex={0}
              className="block max-w-[240px] cursor-help truncate font-mono text-[11px] text-fg-3 focus:outline-none"
            >
              {shortPath(row.original.project)}
            </span>
          </Tooltip>
        ) : (
          <Pill>none</Pill>
        ),
    },
    {
      id: "models",
      header: "Model(s)",
      // Not server-sortable (no single ordering key); keep the header inert
      // rather than offering a sort affordance the backend would ignore.
      enableSorting: false,
      cell: ({ row }) => {
        const ms = row.original.models ?? [];
        if (ms.length === 0) {
          return <span className="text-fg-4">—</span>;
        }
        const primary = ms[0];
        const extras = ms.slice(1);
        return (
          <Tooltip
            content={
              <span className="block whitespace-pre-line break-all font-mono">
                {ms.join("\n")}
              </span>
            }
            maxWidth={360}
          >
            <span
              tabIndex={0}
              className="inline-flex cursor-help items-center gap-1.5 focus:outline-none"
            >
              <ModelDot model={primary} />
              <span className="max-w-[160px] truncate font-mono text-[10.5px] text-fg-1">
                {shortModel(primary)}
              </span>
              {extras.length > 0 && (
                <span className="shrink-0 font-mono text-[10.5px] text-fg-3">
                  + {extras.length}
                </span>
              )}
            </span>
          </Tooltip>
        );
      },
    },
    {
      id: "started_at",
      header: () => <>Started<HelpInd id="column.sessions.started" /></>,
      accessorKey: "started_at",
      cell: ({ row }) => (
        <Tooltip content={row.original.started_at}>
          <span
            tabIndex={0}
            className="cursor-help font-mono text-[11px] text-fg-3 focus:outline-none"
          >
            {fmtTimestamp(row.original.started_at)}
          </span>
        </Tooltip>
      ),
    },
    {
      id: "elapsed",
      header: () => <>Elapsed<HelpInd id="column.sessions.elapsed" /></>,
      accessorKey: "duration_seconds",
      meta: { align: "right" },
      cell: ({ row }) => (
        <span className="tabular-nums text-fg-2">
          {fmtDuration(row.original.duration_seconds * 1000)}
        </span>
      ),
    },
    {
      id: "actions",
      header: () => <>Actions<HelpInd id="column.sessions.actions" /></>,
      accessorKey: "total_actions",
      meta: { align: "right" },
      cell: ({ row }) => (
        <span className="tabular-nums text-fg-1">
          {fmtInt(row.original.total_actions)}
        </span>
      ),
    },
    {
      id: "input",
      header: () => <>Input<HelpInd id="column.sessions.input_tokens" /></>,
      accessorKey: "input_tokens",
      meta: { align: "right", mono: true },
      cell: ({ row }) =>
        row.original.input_tokens > 0 ? (
          <span className="tabular-nums text-fg-1">
            {fmtCompact(row.original.input_tokens)}
          </span>
        ) : (
          <span className="text-fg-4">—</span>
        ),
    },
    {
      id: "cache_r",
      header: () => (
        <>Cache R<HelpInd id="column.sessions.cache_read_tokens" /></>
      ),
      accessorKey: "cache_read_tokens",
      meta: { align: "right", mono: true },
      cell: ({ row }) =>
        row.original.cache_read_tokens > 0 ? (
          <span className="tabular-nums text-fg-1">
            {fmtCompact(row.original.cache_read_tokens)}
          </span>
        ) : (
          <span className="text-fg-4">—</span>
        ),
    },
    {
      id: "cache_w",
      header: () => (
        <>Cache W<HelpInd id="column.sessions.cache_creation_tokens" /></>
      ),
      accessorKey: "cache_creation_tokens",
      meta: { align: "right", mono: true },
      cell: ({ row }) => {
        const total = row.original.cache_creation_tokens;
        if (total <= 0) return <span className="text-fg-4">—</span>;
        const tier1h = row.original.cache_creation_1h_tokens || 0;
        const tier5m = Math.max(0, total - tier1h);
        const pct1h = total > 0 ? (tier1h / total) * 100 : 0;
        const pct5m = total > 0 ? (tier5m / total) * 100 : 0;
        const tipBody = (
          <div className="space-y-1">
            <div className="flex items-baseline justify-between gap-4 font-mono">
              <span className="text-fg-3">5m tier</span>
              <span className="tabular-nums">
                {fmtCompact(tier5m)} <span className="text-fg-3">({pct5m.toFixed(0)}%)</span>
              </span>
            </div>
            <div className="flex items-baseline justify-between gap-4 font-mono">
              <span className="text-fg-3">1h tier</span>
              <span className="tabular-nums">
                {fmtCompact(tier1h)} <span className="text-fg-3">({pct1h.toFixed(0)}%)</span>
              </span>
            </div>
            {tier1h > 0 && (
              <div className="border-t border-line-2 pt-1 text-[10.5px] text-fg-3">
                1h tier bills at 2× input rate; 5m tier at 1.25×.
              </div>
            )}
          </div>
        );
        return (
          <Tooltip content={tipBody} maxWidth={260}>
            <span tabIndex={0} className="cursor-help tabular-nums text-fg-1 focus:outline-none">
              {fmtCompact(total)}
            </span>
          </Tooltip>
        );
      },
    },
    {
      id: "output",
      header: () => <>Output<HelpInd id="column.sessions.output_tokens" /></>,
      accessorKey: "output_tokens",
      meta: { align: "right", mono: true },
      cell: ({ row }) =>
        row.original.output_tokens > 0 ? (
          <span className="tabular-nums text-fg-1">
            {fmtCompact(row.original.output_tokens)}
          </span>
        ) : (
          <span className="text-fg-4">—</span>
        ),
    },
    {
      id: "cost",
      // Only Total $ in the table — API $ + Tool $ live on the
      // SessionDetailPanel's CostStat tile per operator feedback that
      // the Sessions table had too many cost cols. Hover shows the
      // 3-way split as a tooltip so the data isn't hidden.
      header: () => <>Total $<HelpInd id="column.sessions.cost" /></>,
      accessorKey: "cost_usd",
      meta: { align: "right" },
      cell: ({ row }) => {
        const total = row.original.cost_usd;
        if (total <= 0) {
          return <span className="text-fg-4">—</span>;
        }
        const color =
          total >= 50
            ? "text-danger"
            : total >= 10
              ? "text-warn"
              : "text-fg-0";
        const api = row.original.ai_cost_usd;
        const tool = row.original.tool_cost_usd;
        const split =
          api > 0 || tool > 0
            ? `api ${fmtUSD(api)} · tool ${fmtUSD(tool)}`
            : "";
        return (
          <Tooltip
            content={
              <span className="block whitespace-pre-line">
                {fmtUSD(total, true)}
                {split ? `\n${split}` : ""}
              </span>
            }
          >
            <span
              tabIndex={0}
              className={`cursor-help font-semibold tabular-nums focus:outline-none ${color}`}
            >
              {fmtUSD(total)}
            </span>
          </Tooltip>
        );
      },
    },
  ];

  if (showScoring) {
    cols.push(
      {
        id: "quality",
        header: () => <>Quality<HelpInd id="column.sessions.quality" /></>,
        accessorFn: (r) => r.quality_score ?? -1,
        meta: { align: "right" },
        cell: ({ row }) =>
          row.original.quality_score != null
            ? fmtPct(row.original.quality_score)
            : "—",
      },
      {
        id: "errors",
        header: () => <>Errors<HelpInd id="column.sessions.errors" /></>,
        accessorFn: (r) => r.error_rate ?? -1,
        meta: { align: "right" },
        cell: ({ row }) =>
          row.original.error_rate != null ? (
            <span
              className={
                row.original.error_rate > 0.05 ? "text-warn" : "text-fg-2"
              }
            >
              {fmtPct(row.original.error_rate)}
            </span>
          ) : (
            "—"
          ),
      },
      {
        id: "redundancy",
        header: () => <>Redund.<HelpInd id="column.sessions.redundancy" /></>,
        accessorFn: (r) => r.redundancy_ratio ?? -1,
        meta: { align: "right" },
        cell: ({ row }) => {
          const r = row.original;
          if (r.redundancy_ratio == null) return "—";
          // Spec §14.1 wasteful subset rendered as
          // "0.30 (0.20 wasteful)" when present. Sessions
          // without cache_events keep the legacy single value.
          if (r.redundancy_ratio_wasteful != null) {
            return (
              <span title="total / wasteful subset (spec §14.1)">
                {fmtPct(r.redundancy_ratio)}
                <span className="ml-1 text-fg-3">
                  ({fmtPct(r.redundancy_ratio_wasteful)} wasteful)
                </span>
              </span>
            );
          }
          return fmtPct(r.redundancy_ratio);
        },
      },
    );
  }

  return cols;
}

function shortPath(p: string): string {
  const parts = p.split("/").filter(Boolean);
  return parts.length <= 2 ? p : ".../" + parts.slice(-2).join("/");
}

// Format an ISO timestamp as "MMM DD HH:MM" — matches design's
// dim mono treatment for the Started column.
function fmtTimestamp(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString("en-US", {
    month: "short",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  });
}

// CSV export of the currently-visible (post-filter) Sessions table.
// Columns mirror the table 1:1 (minus Models, which the backend
// doesn't expose per-session yet).
function exportSessionsCsv(rows: SessionRow[]) {
  if (!rows.length) return;
  const header = [
    "session_id",
    "tool",
    "project",
    "started_at",
    "duration_seconds",
    "total_actions",
    "sidechain_action_count",
    "input_tokens",
    "cache_read_tokens",
    "cache_creation_tokens",
    "output_tokens",
    "ai_cost_usd",
    "tool_cost_usd",
    "cost_usd",
    "cost_reliability",
  ].join(",");
  const lines = rows.map((r) =>
    [
      escapeCsv(r.id),
      escapeCsv(r.tool),
      escapeCsv(r.project),
      escapeCsv(r.started_at),
      r.duration_seconds,
      r.total_actions,
      r.sidechain_action_count,
      r.input_tokens,
      r.cache_read_tokens,
      r.cache_creation_tokens,
      r.output_tokens,
      r.ai_cost_usd,
      r.tool_cost_usd,
      r.cost_usd,
      escapeCsv(r.cost_reliability),
    ].join(","),
  );
  const blob = new Blob([[header, ...lines].join("\n")], {
    type: "text/csv;charset=utf-8",
  });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = `sessions-${new Date().toISOString().slice(0, 10)}.csv`;
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
}

function escapeCsv(s: string | undefined | null): string {
  if (!s) return "";
  return /[",\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
}

