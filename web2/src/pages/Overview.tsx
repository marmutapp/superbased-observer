import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { api } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { useFilters } from "@/lib/filters";
import { compact, ms, num, pct, pct1, usd } from "@/lib/format";
import { ErrorState, PageHeader } from "@/components/ui";
import { ChartShell, ChartSkeleton, Pill, StatCard, StatStripSkeleton } from "@/components/primitives";
import { CostChart } from "@/components/CostChart";
import { ToolDonut } from "@/components/charts/ToolDonut";
import { ModelBar } from "@/components/charts/ModelBar";
import { HourBars } from "@/components/charts/HourBars";

export function OverviewPage() {
  const { days } = useFilters();
  const { data, error, loading, reload } = useApi(() => api.overview(days), [days]);

  const spark = data?.cost_by_day.map((p) => p.cost_usd) ?? [];
  const d = data?.deltas;
  const delta = (v: number) => (d?.has_prior ? v : undefined);

  return (
    <>
      <PageHeader
        title="Overview"
        subtitle="Organisation-wide cost and activity. Aggregate only — no per-developer detail."
        right={data ? <CaptureTierPill apiTurns={data.total_api_turns} /> : undefined}
      />
      {error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : loading || !data ? (
        <div className="space-y-5">
          <StatStripSkeleton />
          <StatStripSkeleton />
          <ChartSkeleton height={280} />
        </div>
      ) : (
        <div className="space-y-5">
          {/* Hero KPIs */}
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
            <StatCard
              label="Spend"
              value={usd(data.total_cost_usd)}
              sub={`last ${data.window_days}d`}
              delta={delta(d?.cost_usd ?? 0)}
              deltaPrior={d?.has_prior ? `vs prior ${usd(d.prior_cost_usd)}` : undefined}
              spark={spark.length >= 2 ? spark : undefined}
              accent
            />
            <StatCard label="Active developers" value={num(data.active_developers)} />
            <StatCard label="Sessions" value={num(data.total_sessions)} delta={delta(d?.sessions ?? 0)} />
            <StatCard label="Actions" value={num(data.total_actions)} delta={delta(d?.actions ?? 0)} />
          </div>

          {/* Volume + reliability */}
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
            <StatCard label="Teams" value={num(data.team_count)} />
            <StatCard label="Projects" value={num(data.project_count)} />
            <StatCard
              label="API turns"
              value={num(data.total_api_turns)}
              sub={data.total_api_turns === 0 ? "no proxy capture" : "proxy-observed"}
            />
            {data.reliability && (
              <StatCard
                label="Proxy-measured"
                value={pct(data.reliability.proxy_share)}
                sub={`${usd(data.reliability.proxy_cost_usd)} proxy · ${usd(data.reliability.estimated_cost_usd)} est`}
              />
            )}
          </div>

          {/* Token usage + cache efficiency */}
          {data.tokens && (
            <ChartShell
              title="Token usage"
              sub={
                data.cache
                  ? `Cache hit ${pct1(data.cache.hit_ratio)} · read/write ${data.cache.read_write_ratio.toFixed(1)}×`
                  : undefined
              }
            >
              <div className="grid grid-cols-2 gap-3 sm:grid-cols-5">
                <Mini label="Net input" value={compact(data.tokens.net_input)} dot="var(--tok-net)" />
                <Mini label="Cache read" value={compact(data.tokens.cache_read)} dot="var(--tok-read)" />
                <Mini label="Cache write" value={compact(data.tokens.cache_write)} dot="var(--tok-write)" />
                <Mini label="Output" value={compact(data.tokens.output)} dot="var(--tok-out)" />
                <Mini label="Reasoning" value={compact(data.tokens.reasoning)} dot="var(--fg-3)" />
              </div>
            </ChartShell>
          )}

          {/* Quality: errors + latency (latency degrades honestly) */}
          {data.errors && (
            <ChartShell title="Reliability & latency">
              <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
                <Mini
                  label="Action error rate"
                  value={pct1(data.errors.action_error_rate)}
                  sub={`${num(data.errors.failed_actions)} / ${num(data.errors.total_actions)} failed`}
                />
                <Mini
                  label="HTTP errors"
                  value={num(data.errors.http_errors)}
                  sub={data.errors.api_turns === 0 ? "needs proxy capture" : `${num(data.errors.api_turns)} turns`}
                />
                {data.latency ? (
                  <>
                    <Mini
                      label="Median latency"
                      value={ms(data.latency.median_total_ms)}
                      sub={`TTFT ${ms(data.latency.median_ttft_ms)} · n=${num(data.latency.sample_size)}`}
                    />
                    <Mini label="p95 latency" value={ms(data.latency.p95_total_ms)} />
                  </>
                ) : (
                  <div className="col-span-2 grid place-items-center rounded-3 border border-dashed border-line-2 p-3 text-center text-[11px] text-fg-3">
                    Latency needs proxy capture — route this org's tools through
                    the observer proxy to populate timing.
                  </div>
                )}
              </div>
            </ChartShell>
          )}

          <ChartShell title="Spend over time" sub={`Daily cost, last ${data.window_days}d`}>
            <CostChart data={data.cost_by_day} />
          </ChartShell>

          {/* Tool mix + model mix */}
          {(data.tool_mix?.length || data.model_mix?.length) && (
            <div className="grid grid-cols-1 gap-3 lg:grid-cols-2">
              {data.tool_mix && (
                <ChartShell title="Tool mix" sub="Spend share by AI tool">
                  <ToolDonut tools={data.tool_mix} />
                </ChartShell>
              )}
              {data.model_mix && (
                <ChartShell title="Model mix" sub="Spend by model">
                  <ModelBar models={data.model_mix} />
                </ChartShell>
              )}
            </div>
          )}

          {data.hour_of_day && data.hour_of_day.length > 0 && (
            <ChartShell title="Activity by hour" sub="Actions by hour of day (UTC)">
              <HourBars buckets={data.hour_of_day} />
            </ChartShell>
          )}

          {/* Top teams / projects */}
          <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
            <ChartShell title="Top teams">
              {data.top_teams.length === 0 ? (
                <div className="py-4 text-sm text-fg-3">No team spend.</div>
              ) : (
                <ul className="divide-y divide-line-1">
                  {data.top_teams.map((t) => (
                    <li key={t.team_id} className="flex items-center justify-between py-2 text-sm">
                      <Link to={`/teams/${t.team_id}`} className="text-fg-1 hover:text-accent">
                        {t.display_name}
                      </Link>
                      <span className="font-mono text-fg-2">{usd(t.cost_usd)}</span>
                    </li>
                  ))}
                </ul>
              )}
            </ChartShell>
            <ChartShell title="Top projects">
              {data.top_projects.length === 0 ? (
                <div className="py-4 text-sm text-fg-3">No project spend.</div>
              ) : (
                <ul className="divide-y divide-line-1">
                  {data.top_projects.map((p) => (
                    <li key={p.project_id} className="flex items-center justify-between py-2 text-sm">
                      <Link
                        to={`/projects/${p.project_id}`}
                        className="truncate text-fg-1 hover:text-accent"
                        title={p.project_root}
                      >
                        {p.project_root}
                      </Link>
                      <span className="ml-3 shrink-0 font-mono text-fg-2">{usd(p.cost_usd)}</span>
                    </li>
                  ))}
                </ul>
              )}
            </ChartShell>
          </div>
        </div>
      )}
    </>
  );
}

// Mini is a compact labeled metric cell used inside the token/quality grids.
function Mini({
  label,
  value,
  sub,
  dot,
}: {
  label: string;
  value: ReactNode;
  sub?: ReactNode;
  dot?: string;
}) {
  return (
    <div className="rounded-3 border border-line-2 bg-bg-1 px-3 py-2.5">
      <div className="flex items-center gap-1.5 text-[10px] font-semibold uppercase tracking-[0.06em] text-fg-3">
        {dot && <span className="h-2 w-2 rounded-pill" style={{ background: dot }} />}
        {label}
      </div>
      <div className="mt-1 text-[20px] font-bold leading-tight tracking-tight text-fg-0">
        {value}
      </div>
      {sub && <div className="mt-0.5 text-[11px] text-fg-3">{sub}</div>}
    </div>
  );
}

// CaptureTierPill is the honest "which capture tier is feeding this org" badge:
// proxy-observed metrics (latency, cache, per-turn error class) exist only when
// the org's tools routed through the proxy. With no api_turns in the window we
// say so plainly rather than implying those metrics are merely empty.
function CaptureTierPill({ apiTurns }: { apiTurns: number }) {
  return apiTurns > 0 ? (
    <Pill variant="success" title="Proxy turns observed — latency, cache, and per-turn error metrics are live.">
      Proxy + JSONL
    </Pill>
  ) : (
    <Pill variant="warn" title="No proxy turns in window — latency & cache metrics are unavailable; cost/tokens/actions still come from the watcher (JSONL).">
      JSONL only · latency/cache unavailable
    </Pill>
  );
}
