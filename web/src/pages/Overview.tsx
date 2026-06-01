import { useState } from "react";
import { Link } from "react-router-dom";
import {
  ActionsAreaChart,
  CostAreaChart,
  type CostAreaMode,
  TopToolsDonut,
} from "@/components/charts";
import { ChartState } from "@/components/ChartState";
import {
  ChartShell,
  PageHeader,
  Pill,
  SegmentedControl,
  StatCard,
  ToolBadge,
  Tooltip,
} from "@/components/primitives";
import { HelpInd, TitleWithHelp } from "@/components/HelpInd";
import {
  AlertIcon,
  BoltIcon,
  DatabaseIcon,
  LayersIcon,
} from "@/components/icons";
import { useFilters, windowDays } from "@/lib/filters";
import { useApi } from "@/lib/useApi";
import {
  fmtCompact,
  fmtDuration,
  fmtInt,
  fmtUSD,
} from "@/lib/format";
import type {
  ActionsTimeseries,
  CostSummary,
  CostTimeseries,
  DiscoverResponse,
  SessionsResponse,
  StatusScoped,
  StatusSnapshot,
  ToolsResponse,
} from "@/lib/types";

export function OverviewPage() {
  const [costMode, setCostMode] = useState<CostAreaMode>("tokens");
  const { win, tool, project } = useFilters();
  const days = windowDays(win);
  const daysParam = days === "all" ? 36500 : days;
  const toolParam = tool === "all" ? undefined : tool;
  const projectParam = project === "all" ? undefined : project;

  const status = useApi<StatusSnapshot>("/api/status");
  const scoped = useApi<StatusScoped>(
    "/api/status/scoped",
    { days: daysParam, tool: toolParam, project: projectParam },
    [win, tool, project],
  );
  const costTs = useApi<CostTimeseries>(
    "/api/timeseries/cost",
    { days: daysParam, tool: toolParam, project: projectParam },
    [win, tool, project],
  );
  const actionsTs = useApi<ActionsTimeseries>(
    "/api/timeseries/actions",
    { days: daysParam, tool: toolParam, project: projectParam },
    [win, tool, project],
  );
  const models = useApi<CostSummary>(
    "/api/models",
    { days: daysParam, tool: toolParam, project: projectParam },
    [win, tool, project],
  );
  const tools = useApi<ToolsResponse>(
    "/api/tools",
    { days: daysParam, project: projectParam },
    [win, project],
  );
  const sessions = useApi<SessionsResponse>(
    "/api/sessions",
    { limit: 6, page: 1, tool: toolParam, project: projectParam },
    [tool, project],
  );
  // Discover is only used for the stale-reads KPI tile; pull a thin
  // slice with no pagination payload.
  const discover = useApi<DiscoverResponse>(
    "/api/discover",
    {
      days: daysParam,
      stale_limit: 1,
      repeated_limit: 1,
      tool: toolParam,
      project: projectParam,
    },
    [win, tool, project],
  );

  const kpis = deriveKpis(costTs.data, actionsTs.data, status.data);

  return (
    <div className="space-y-6 p-6">
      <PageHeader
        title="Overview"
        sub="High-level snapshot — KPI tiles, daily cost and activity, plus top-N models and tools across the selected window."
        helpId="tab.overview"
      />
      {/* KPI grid */}
      <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
        <StatCard
          label="Sessions"
          helpId="tile.sessions"
          icon={<LayersIcon />}
          loading={scoped.loading}
          value={fmtInt(scoped.data?.sessions)}
          cornerPill={
            <span className="rounded-pill border border-success/40 bg-success-soft px-1.5 py-0.5 text-[9.5px] font-medium text-success">
              window {win}
            </span>
          }
          sub={
            status.data?.last_action_at
              ? `last activity ${relativeTime(status.data.last_action_at)}`
              : "no activity yet"
          }
          spark={kpis.actionsSpark}
          sparkColor="var(--accent)"
        />
        <StatCard
          label="API Turns (proxy)"
          helpId="tile.api_turns"
          icon={<BoltIcon />}
          loading={scoped.loading}
          value={fmtInt(scoped.data?.api_turns)}
          sub={
            (scoped.data?.api_turns ?? 0) === 0
              ? "proxy not engaged · accurate-token source"
              : `accurate token source · ${fmtInt(scoped.data?.token_usage)} rows`
          }
          warn={(scoped.data?.api_turns ?? 0) === 0}
          spark={kpis.turnsSpark}
          sparkColor="var(--tok-net)"
        />
        <StatCard
          label="Token Rows (jsonl)"
          helpId="tile.token_rows"
          icon={<DatabaseIcon />}
          loading={scoped.loading}
          value={fmtCompact(scoped.data?.token_usage)}
          spark={kpis.costSpark}
          sparkColor="var(--tok-read)"
          sub="plentiful but unreliable · de-duped at cost time"
        />
        <StatCard
          label="Stale re-reads"
          helpId="metric.stale_count"
          icon={<AlertIcon />}
          linkTo="/discovery"
          loading={discover.loading}
          value={
            discover.data ? fmtInt(discover.data.summary.stale_read_count) : "—"
          }
          warn={
            (discover.data?.summary.stale_read_count ?? 0) > 0
          }
          cornerPill={
            (discover.data?.summary.stale_read_count ?? 0) === 0 ? (
              <span className="rounded-pill border border-info/40 bg-info-soft px-1.5 py-0.5 text-[9.5px] font-medium text-info">
                all systems nominal
              </span>
            ) : undefined
          }
          sub={
            discover.data?.summary.cross_thread_stale_count
              ? `${fmtInt(discover.data.summary.cross_thread_stale_count)} cross-thread`
              : "from Discovery tab"
          }
        />
      </div>

      {/* Two time-series charts side by side on wide screens */}
      <div className="grid grid-cols-1 gap-4 xl:grid-cols-2">
        <ChartShell
          title={<TitleWithHelp text="Cost over time" helpId="chart.cost_over_time" />}
          sub={
            costMode === "cost"
              ? `Daily cost in dollars · ${win}`
              : `Tokens by Anthropic billing bucket · ${win}`
          }
          right={
            <SegmentedControl<CostAreaMode>
              options={[
                { value: "tokens", label: "Tokens" },
                { value: "cost", label: "Cost $" },
              ]}
              value={costMode}
              onChange={setCostMode}
              size="sm"
            />
          }
        >
          <ChartState
            loading={costTs.loading}
            error={costTs.error}
            empty={!costTs.data?.series?.length}
            emptyHint="No cost data in this window."
          >
            {costTs.data && (
              <CostAreaChart data={costTs.data.series} mode={costMode} />
            )}
          </ChartState>
        </ChartShell>

        <ChartShell
          title={<TitleWithHelp text="Actions over time" helpId="chart.actions_over_time" />}
          sub={`Stacked by tool · ${win}`}
        >
          <ChartState
            loading={actionsTs.loading}
            error={actionsTs.error}
            empty={!actionsTs.data?.series?.length}
            emptyHint="No actions in this window."
          >
            {actionsTs.data && (
              <ActionsAreaChart data={actionsTs.data.series} />
            )}
          </ChartState>
        </ChartShell>
      </div>

      {/* Top models + Top tools side by side */}
      <div className="grid grid-cols-1 gap-4 xl:grid-cols-2">
        <ChartShell
          title={<TitleWithHelp text="Top models by tokens" helpId="chart.top_models" />}
          sub={`Net input + cache read + output · ${win}`}
        >
          <ChartState
            loading={models.loading}
            error={models.error}
            empty={!models.data?.rows?.length}
            emptyHint="No model data yet."
          >
            {models.data && <TopModelsBars rows={models.data.rows} />}
          </ChartState>
        </ChartShell>

        <ChartShell
          title={<TitleWithHelp text="Top tools by actions" helpId="chart.top_tools" />}
          sub={`Donut + success rate · ${win}`}
        >
          <ChartState
            loading={tools.loading}
            error={tools.error}
            empty={!tools.data?.tools?.length}
            emptyHint="No tools active in window."
          >
            {tools.data && <TopToolsDonut tools={tools.data.tools} />}
          </ChartState>
        </ChartShell>
      </div>

      {/* Recent sessions mini-list */}
      <ChartShell
        title="Recent sessions"
        sub="Most recent 6 — click through for the full list"
        right={
          <Link
            to="/sessions"
            className="text-[11px] font-medium text-accent hover:text-accent-strong"
          >
            All sessions →
          </Link>
        }
      >
        <ChartState
          loading={sessions.loading}
          error={sessions.error}
          empty={!sessions.data?.rows?.length}
          emptyHint="No sessions yet — adapters wire up via observer init / observer start."
        >
          {sessions.data && <RecentSessions rows={sessions.data.rows} />}
        </ChartState>
      </ChartShell>
    </div>
  );
}

// --------------------------------------------------------------- helpers

function deriveKpis(
  cost?: CostTimeseries | null,
  actions?: ActionsTimeseries | null,
  _status?: StatusSnapshot | null,
) {
  const series = cost?.series ?? [];
  const cost_usd = series.reduce((acc, p) => acc + (p.cost_usd || 0), 0);
  const turns = series.reduce((acc, p) => acc + (p.turn_count || 0), 0);
  const actionsSeries = actions?.series ?? [];
  return {
    cost: cost_usd,
    turns,
    costSpark: series.map((p) => p.cost_usd || 0),
    turnsSpark: series.map((p) => p.turn_count || 0),
    actionsSpark: actionsSeries.map(
      (p) =>
        Object.entries(p)
          .filter(([k]) => k !== "bucket")
          .reduce((a, [, v]) => a + (typeof v === "number" ? v : 0), 0),
    ),
  };
}

function relativeTime(iso: string): string {
  const t = new Date(iso).getTime();
  if (!Number.isFinite(t)) return "—";
  const diffMs = Date.now() - t;
  if (diffMs < 0) return "in the future";
  return `${fmtDuration(diffMs)} ago`;
}

// Per-model palette. Pulled from design tokens so themes follow.
const MODEL_PALETTE = [
  "var(--tok-net)",
  "var(--tok-read)",
  "var(--tok-out)",
  "var(--tok-write)",
  "var(--info)",
  "var(--success)",
  "var(--warn)",
  "var(--accent)",
];

function TopModelsBars({ rows }: { rows: CostSummary["rows"] }) {
  const top = rows.slice(0, 8);
  const max = Math.max(
    1,
    ...top.map(
      (r) =>
        r.tokens.input + r.tokens.cache_read + r.tokens.output,
    ),
  );
  return (
    <ul className="space-y-1.5">
      {top.map((r, i) => {
        const total =
          r.tokens.input + r.tokens.cache_read + r.tokens.output;
        const pct = (total / max) * 100;
        const color = MODEL_PALETTE[i % MODEL_PALETTE.length];
        return (
          <li key={r.key} className="space-y-0.5">
            <div className="flex items-baseline justify-between gap-2">
              <span className="flex items-center gap-1.5 truncate">
                <span
                  className="h-1.5 w-1.5 shrink-0 rounded-pill"
                  style={{ background: color }}
                />
                <span className="truncate font-mono text-[11px] text-fg-1">
                  {r.key}
                </span>
              </span>
              <span className="shrink-0 text-[11px] text-fg-3 tabular-nums">
                {fmtCompact(total)} · {fmtUSD(r.cost_usd)}
              </span>
            </div>
            <div className="h-2 w-full overflow-hidden rounded-pill bg-bg-3">
              <span
                className="block h-full"
                style={{ width: `${pct}%`, background: color }}
              />
            </div>
          </li>
        );
      })}
    </ul>
  );
}

function RecentSessions({ rows }: { rows: SessionsResponse["rows"] }) {
  return (
    <table className="w-full text-left text-[12px]">
      <thead className="text-[10.5px] uppercase tracking-[0.06em] text-fg-3">
        <tr>
          <th className="py-1.5 font-medium">Tool<HelpInd id="column.sessions.tool" /></th>
          <th className="py-1.5 font-medium">Project<HelpInd id="column.sessions.project" /></th>
          <th className="py-1.5 font-medium">Started<HelpInd id="column.sessions.started" /></th>
          <th className="py-1.5 text-right font-medium">Actions<HelpInd id="column.sessions.actions" /></th>
          <th className="py-1.5 text-right font-medium">Tokens<HelpInd id="column.sessions.tokens" /></th>
          <th className="py-1.5 text-right font-medium">Cost<HelpInd id="column.sessions.cost" /></th>
          <th className="py-1.5 text-right font-medium">Elapsed<HelpInd id="column.sessions.elapsed" /></th>
        </tr>
      </thead>
      <tbody>
        {rows.slice(0, 6).map((s) => (
          <tr key={s.id} className="border-t border-line-1 hover:bg-bg-3/40">
            <td className="py-1.5">
              <ToolBadge tool={s.tool} />
            </td>
            <td className="max-w-[280px] truncate py-1.5 font-mono text-[11px] text-fg-2">
              {s.project ? shortProject(s.project) : (
                <Pill>no project</Pill>
              )}
            </td>
            <Tooltip content={s.started_at}>
              <td tabIndex={0} className="cursor-help py-1.5 text-[11px] text-fg-3 focus:outline-none">
                {relativeTime(s.started_at)}
              </td>
            </Tooltip>
            <td className="py-1.5 text-right tabular-nums text-fg-1">
              {fmtInt(s.total_actions)}
            </td>
            <td className="py-1.5 text-right tabular-nums text-fg-2">
              {fmtCompact(s.total_tokens)}
            </td>
            <td className="py-1.5 text-right tabular-nums text-fg-1">
              {fmtUSD(s.cost_usd)}
            </td>
            <td className="py-1.5 text-right tabular-nums text-fg-3">
              {fmtDuration(s.duration_seconds * 1000)}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function shortProject(p: string): string {
  const parts = p.split("/").filter(Boolean);
  return parts.length <= 2 ? p : ".../" + parts.slice(-2).join("/");
}
