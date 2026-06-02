import { useMemo, useState } from "react";
import {
  ActionsAreaChart,
} from "@/components/charts";
import {
  ChartShell,
  PageHeader,
  SegmentedControl,
  StatCard,
  ToolBadge,
  ToolDot,
  Tooltip,
} from "@/components/primitives";
import { HelpInd, TitleWithHelp } from "@/components/HelpInd";
import { ChartState } from "@/components/ChartState";
import { useFilters, windowDays } from "@/lib/filters";
import { useApi } from "@/lib/useApi";
import { actionMeta } from "@/lib/actions";
import { toolMeta } from "@/lib/tools";
import {
  BoltIcon,
  FlameIcon,
  LayersIcon,
  PercentIcon,
} from "@/components/icons";
import { fmtInt, fmtPct } from "@/lib/format";
import type {
  ActionsTimeseries,
  ToolsBreakdownResponse,
  ToolsResponse,
} from "@/lib/types";

export function ToolsPage() {
  const { win, tool, project } = useFilters();
  const days = windowDays(win);
  const daysParam = days === "all" ? 36500 : days;
  const projectParam = project === "all" ? undefined : project;
  const toolParam = tool === "all" ? undefined : tool;

  const tools = useApi<ToolsResponse>(
    "/api/tools",
    { days: daysParam, project: projectParam },
    [win, project],
  );
  const breakdown = useApi<ToolsBreakdownResponse>(
    "/api/tools/breakdown",
    { days: daysParam, tool: toolParam, project: projectParam },
    [win, tool, project],
  );
  const ts = useApi<ActionsTimeseries>(
    "/api/timeseries/actions",
    { days: daysParam, tool: toolParam, project: projectParam },
    [win, tool, project],
  );

  const summary = useMemo(() => summarize(tools.data), [tools.data]);
  const actionsSpark = useMemo<number[]>(() => {
    const series = ts.data?.series ?? [];
    return series.map((p: Record<string, unknown>) =>
      Object.entries(p)
        .filter(([k]) => k !== "bucket")
        .reduce(
          (a, [, v]) => a + (typeof v === "number" ? v : 0),
          0,
        ),
    );
  }, [ts.data]);

  return (
    <div className="space-y-6 p-6">
      <PageHeader
        title="Tools"
        sub="Per-tool aggregates with charts showing when each AI client was active and what kind of work it did — four KPIs, activity-over-time stack, action-type mix per tool, and the per-tool aggregates table."
        helpId="tab.tools"
      />
      {/* 4-KPI header */}
      <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
        <StatCard
          label="Total actions"
          icon={<BoltIcon />}
          loading={tools.loading}
          value={fmtInt(summary.totalActions)}
          sub={`${fmtInt(summary.totalSessions)} sessions`}
          spark={actionsSpark}
          sparkColor="var(--accent)"
        />
        <StatCard
          label="Distinct tools"
          icon={<LayersIcon />}
          loading={tools.loading}
          value={fmtInt(summary.distinctTools)}
          sub={`${win} window`}
        />
        <StatCard
          label="Overall success"
          icon={<PercentIcon />}
          loading={tools.loading}
          value={fmtPct(summary.overallSuccess)}
          warn={summary.overallSuccess < 0.95}
          sub={
            summary.totalFailures > 0
              ? `${fmtInt(summary.totalFailures)} failures`
              : "no failures"
          }
        />
        <StatCard
          label="Busiest tool"
          icon={<FlameIcon />}
          loading={tools.loading}
          value={
            summary.busiestTool ? (
              <span className="flex items-center gap-2">
                <ToolDot tool={summary.busiestTool} size={10} />
                <span className="text-[18px] font-semibold">
                  {toolMeta(summary.busiestTool).label}
                </span>
              </span>
            ) : (
              "—"
            )
          }
          sub={
            summary.busiestTool
              ? `${fmtInt(summary.busiestCount)} actions`
              : undefined
          }
        />
      </div>

      {/* Activity + Mix side-by-side */}
      <div className="grid grid-cols-1 gap-4 xl:grid-cols-2">
        <ChartShell
          title={<TitleWithHelp text="Activity over time" helpId="chart.tools_activity" />}
          sub={`Stacked actions by tool · ${win}`}
        >
          <ChartState
            loading={ts.loading && !ts.data}
            error={ts.error}
            empty={!ts.data?.series.length}
            emptyHint="No actions in window."
            height={300}
          >
            {ts.data && <ActionsAreaChart data={ts.data.series} />}
          </ChartState>
        </ChartShell>

        <ChartShell
          title={<TitleWithHelp text="Action-type mix per tool" helpId="chart.tools_breakdown" />}
          sub="What each tool actually does — 100% horizontal stack per tool, colored by action category."
        >
          <ChartState
            loading={breakdown.loading && !breakdown.data}
            error={breakdown.error}
            empty={!breakdown.data?.tools.length}
            emptyHint="No actions to break down."
            height={300}
          >
            {breakdown.data && (
              <ActionMixPanel rows={breakdown.data.tools} />
            )}
          </ChartState>
        </ChartShell>
      </div>

      {/* Per-tool aggregates */}
      <ChartShell
        title="Per-tool aggregates"
        sub="Action volume, success rate, distinct sessions, first/last seen — sorted by volume DESC."
      >
        <ChartState
          loading={tools.loading && !tools.data}
          error={tools.error}
          empty={!tools.data?.tools.length}
          emptyHint="No tools active in window."
          height={160}
        >
          {tools.data && <PerToolTable rows={tools.data.tools} />}
        </ChartState>
      </ChartShell>
    </div>
  );
}

// --------------------------------------------------------------- helpers

function summarize(data?: ToolsResponse | null) {
  if (!data) {
    return {
      totalActions: 0,
      distinctTools: 0,
      totalSessions: 0,
      totalFailures: 0,
      overallSuccess: 1,
      busiestTool: "",
      busiestCount: 0,
    };
  }
  const totalActions = data.tools.reduce((a, t) => a + t.action_count, 0);
  const totalFailures = data.tools.reduce((a, t) => a + t.failure_count, 0);
  const totalSessions = data.tools.reduce((a, t) => a + t.session_count, 0);
  const busiest = data.tools.reduce(
    (acc, t) => (t.action_count > acc.count ? { tool: t.tool, count: t.action_count } : acc),
    { tool: "", count: 0 },
  );
  return {
    totalActions,
    distinctTools: data.tools.length,
    totalSessions,
    totalFailures,
    overallSuccess: totalActions > 0 ? 1 - totalFailures / totalActions : 1,
    busiestTool: busiest.tool,
    busiestCount: busiest.count,
  };
}

// --------------------------------------------------------------- ActionMix

function ActionMixPanel({ rows }: { rows: ToolsBreakdownResponse["tools"] }) {
  // Each row gets its own 100% stacked bar with one segment per
  // action_type, colored via the category registry. Action types
  // below a 1% threshold collapse into an "other" segment so the
  // legend doesn't explode for tools with 20+ types.
  //
  // The legend below the panel is a SINGLE shared legend — mirrors
  // the legacy Chart.js single-instance behavior. Collected across
  // every tool row, deduped, ordered by total share.
  //
  // `display` toggles the right-aligned per-tool caption + legend
  // between % share (default) and raw count, mirroring design's
  // mix-mode toggle (`design/page-tools.jsx:84-85`).
  const [display, setDisplay] = useState<"share" | "count">("share");
  const sharedLegend = useMemo(() => {
    const tot: Record<string, number> = {};
    let grandTotal = 0;
    for (const t of rows) {
      for (const [type, n] of Object.entries(t.by_type)) {
        tot[type] = (tot[type] ?? 0) + n;
        grandTotal += n;
      }
    }
    return Object.entries(tot)
      .sort((a, b) => b[1] - a[1])
      .map(([type, n]) => ({
        type,
        n,
        share: grandTotal > 0 ? n / grandTotal : 0,
      }));
  }, [rows]);

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-end">
        <SegmentedControl<"share" | "count">
          options={[
            { value: "share", label: "%" },
            { value: "count", label: "count" },
          ]}
          value={display}
          onChange={setDisplay}
          size="sm"
        />
      </div>
      <ul className="space-y-2">
        {rows.map((t) => {
          const sorted = Object.entries(t.by_type).sort((a, b) => b[1] - a[1]);
          return (
            <li
              key={t.tool}
              className="rounded-2 border border-line-1 bg-bg-2 px-3 py-2"
            >
              <div className="mb-1.5 flex items-baseline justify-between gap-2">
                <span className="flex items-center gap-2">
                  <ToolBadge tool={t.tool} />
                  <span className="text-[11px] text-fg-3">
                    {fmtInt(t.total)} actions · {sorted.length} types
                  </span>
                </span>
                <span className="text-[10px] text-fg-3">
                  top:{" "}
                  <span className="font-mono text-fg-2">
                    {sorted[0]?.[0] ?? "—"}
                  </span>{" "}
                  ({display === "share"
                    ? fmtPct((sorted[0]?.[1] ?? 0) / Math.max(1, t.total))
                    : fmtInt(sorted[0]?.[1] ?? 0)})
                </span>
              </div>
              <div className="flex h-3 w-full overflow-hidden rounded-pill bg-bg-3">
                {sorted.map(([type, n]) => {
                  const pct = (n / Math.max(1, t.total)) * 100;
                  if (pct < 0.5) return null;
                  const meta = actionMeta(type);
                  return (
                    <Tooltip
                      key={type}
                      content={`${meta.label}: ${fmtInt(n)} (${fmtPct(n / Math.max(1, t.total))})`}
                    >
                      <span
                        style={{
                          width: `${pct}%`,
                          background: meta.colorVar,
                          opacity: 0.75,
                        }}
                      />
                    </Tooltip>
                  );
                })}
              </div>
            </li>
          );
        })}
      </ul>
      {sharedLegend.length > 0 && (
        <div className="border-t border-line-1 pt-2">
          <ul className="flex flex-wrap items-center gap-x-3 gap-y-1 text-[10.5px] text-fg-2">
            {sharedLegend.map(({ type, n, share }) => {
              const meta = actionMeta(type);
              return (
                <Tooltip
                  key={type}
                  content={`${fmtInt(n)} (${fmtPct(share)})`}
                >
                  <li
                    tabIndex={0}
                    className="flex cursor-help items-center gap-1.5 focus:outline-none"
                  >
                    <span
                      aria-hidden
                      className="h-2 w-2 rounded-pill"
                      style={{ background: meta.colorVar }}
                    />
                    <span>{meta.label}</span>
                    <span className="font-mono tabular-nums text-fg-3">
                      {display === "share" ? fmtPct(share) : fmtInt(n)}
                    </span>
                  </li>
                </Tooltip>
              );
            })}
          </ul>
        </div>
      )}
    </div>
  );
}

// --------------------------------------------------------------- PerToolTable

function PerToolTable({ rows }: { rows: ToolsResponse["tools"] }) {
  const maxActions = Math.max(1, ...rows.map((r) => r.action_count));
  return (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[760px] text-left text-[11.5px]">
        <thead className="text-[10px] uppercase tracking-[0.06em] text-fg-3">
          <tr className="border-b border-line-2">
            <th className="py-1.5 pl-2 font-medium">Tool<HelpInd id="column.tools.tool" /></th>
            <th className="py-1.5 font-medium">Actions<HelpInd id="column.tools.actions" /></th>
            <th className="py-1.5 text-right font-medium">Failures<HelpInd id="column.tools.failures" /></th>
            <th className="py-1.5 font-medium">Success rate<HelpInd id="column.tools.success_rate" /></th>
            <th className="py-1.5 text-right font-medium">Sessions<HelpInd id="column.tools.sessions" /></th>
            <th className="py-1.5 font-medium">First seen<HelpInd id="column.tools.first_seen" /></th>
            <th className="py-1.5 font-medium">Last seen<HelpInd id="column.tools.last_seen" /></th>
          </tr>
        </thead>
        <tbody>
          {rows.map((t) => {
            const pct = (t.action_count / maxActions) * 100;
            const meta = toolMeta(t.tool);
            const succPct = Math.max(0, Math.min(1, t.success_rate)) * 100;
            const succColor =
              t.success_rate >= 0.95
                ? "var(--success)"
                : t.success_rate >= 0.8
                  ? "var(--warn)"
                  : "var(--danger)";
            return (
              <tr
                key={t.tool}
                className="border-b border-line-1 last:border-b-0 hover:bg-bg-3/40"
              >
                <td className="py-1.5 pl-2">
                  <ToolBadge tool={t.tool} />
                </td>
                <td className="py-1.5">
                  <div className="flex items-center gap-2">
                    <div className="h-1.5 w-[120px] overflow-hidden rounded-pill bg-bg-3">
                      <span
                        style={{
                          display: "block",
                          height: "100%",
                          width: `${pct}%`,
                          background: meta.colorVar,
                          opacity: 0.75,
                        }}
                      />
                    </div>
                    <span className="tabular-nums text-fg-1">
                      {fmtInt(t.action_count)}
                    </span>
                  </div>
                </td>
                <td className="py-1.5 text-right tabular-nums">
                  {t.failure_count > 0 ? (
                    <span className="text-danger">
                      {fmtInt(t.failure_count)}
                    </span>
                  ) : (
                    <span className="text-fg-4">—</span>
                  )}
                </td>
                <td className="py-1.5">
                  <div className="flex items-center gap-2">
                    <div className="h-1.5 w-[100px] overflow-hidden rounded-pill bg-bg-3">
                      <span
                        style={{
                          display: "block",
                          height: "100%",
                          width: `${succPct}%`,
                          background: succColor,
                        }}
                      />
                    </div>
                    <span
                      className={`tabular-nums ${
                        t.success_rate < 0.9 ? "text-warn" : "text-fg-1"
                      }`}
                    >
                      {fmtPct(t.success_rate)}
                    </span>
                  </div>
                </td>
                <td className="py-1.5 text-right tabular-nums text-fg-2">
                  {fmtInt(t.session_count)}
                </td>
                <Tooltip content={t.first_seen}>
                  <td tabIndex={0} className="cursor-help py-1.5 text-fg-3 focus:outline-none">
                    {fmtCompactDate(t.first_seen)}
                  </td>
                </Tooltip>
                <Tooltip content={t.last_seen}>
                  <td tabIndex={0} className="cursor-help py-1.5 text-fg-3 focus:outline-none">
                    {fmtCompactDate(t.last_seen)}
                  </td>
                </Tooltip>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

function fmtCompactDate(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleDateString("en-US", {
    month: "short",
    day: "numeric",
    year: "2-digit",
  });
}

