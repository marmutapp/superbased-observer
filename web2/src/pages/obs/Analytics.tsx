import { useMemo } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { api, type ObsAnalyticsResult, type ObsDimCount } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { useFilters } from "@/lib/filters";
import { compact, ms, num, pct1, usd } from "@/lib/format";
import { Card, Empty, ErrorState, PageHeader } from "@/components/ui";
import {
  ChartShell,
  ChartSkeleton,
  Pill,
  StatCard,
  StatStripSkeleton,
  TableSkeleton,
} from "@/components/primitives";
import { DataTable } from "@/components/DataTable";
import { ObsTrendChart } from "@/components/charts/ObsTrendChart";

// Org-tier observability analytics (obs-org-tier T1). Content-free cost / token
// / latency / error rollups over the obs_summaries aggregate member nodes push
// under [org_client.share].obs_summary. Admin-only.
export function ObsAnalyticsPage() {
  const { days } = useFilters();
  const { data, error, loading, reload } = useApi(() => api.obsAnalytics(days), [days]);

  return (
    <>
      <PageHeader
        title="Trajectory analytics"
        subtitle="Custom-app / agent trajectories shared by enrolled nodes. Org-aggregate, content-free — cost, tokens, latency and error rates by model, project and source. No trace content."
      />
      {error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : loading || !data ? (
        <div className="space-y-5">
          <StatStripSkeleton count={4} />
          <Card className="p-4">
            <ChartSkeleton />
          </Card>
          <Card className="p-4">
            <TableSkeleton rows={5} />
          </Card>
        </div>
      ) : !data.configured ? (
        <NotConfigured />
      ) : (
        <Configured data={data} />
      )}
    </>
  );
}

// NotConfigured is the honest empty state: no node has opted into obs_summary
// sharing, so obs_summaries holds no rows. Names the exact node-side dependency.
function NotConfigured() {
  return (
    <Card className="p-6">
      <h3 className="text-[15px] font-semibold text-fg-0">No trajectory analytics shared</h3>
      <p className="mt-2 max-w-2xl text-sm leading-relaxed text-fg-2">
        No enrolled node has shared an observability summary for this window.
        Trajectory analytics is a <b className="text-fg-1">node-side opt-in</b>: an operator must
        set <span className="font-mono text-fg-2">[org_client.share].obs_summary = true</span> in
        their local config for the content-free aggregate (per day × model ×
        project × source — trace/span/token/cost/latency/error sums, never a
        prompt or response) to reach this server.
      </p>
      <p className="mt-4 text-[12px] text-fg-3">
        The underlying trace/span tables stay node-local; only the aggregate
        crosses the wire, and there is no remote toggle for the org to flip it on
        a node. See <span className="font-mono text-fg-2">docs/observability.md</span>.
      </p>
    </Card>
  );
}

function Configured({ data }: { data: ObsAnalyticsResult }) {
  return (
    <div className="space-y-5">
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <StatCard label="Traces" value={num(data.total_traces)} sub={`${num(data.total_spans)} spans`} helpId="tile.obs.traces" />
        <StatCard label="Tokens" value={compact(data.total_tokens)} sub={`${compact(data.input_tokens)} in · ${compact(data.output_tokens)} out`} helpId="tile.obs.tokens" />
        <StatCard label="Cost" value={usd(data.total_cost_usd)} sub="across shared nodes" accent helpId="tile.obs.cost" />
        <StatCard
          label="Error rate"
          value={pct1(data.error_rate)}
          sub={`${num(data.error_traces)} errored · ${ms(data.avg_duration_ms)} avg`}
          helpId="tile.obs.error_rate"
        />
      </div>

      <ChartShell title="Trajectories over time">
        <ObsTrendChart data={data.by_day} />
      </ChartShell>

      {data.latency_configured && (
        <div className="space-y-3">
          <div className="grid grid-cols-3 gap-3">
            <StatCard label="Latency P50" value={ms(data.latency_p50_ms)} sub="median span" helpId="tile.obs.latency" />
            <StatCard label="Latency P95" value={ms(data.latency_p95_ms)} sub="tail" helpId="tile.obs.latency" />
            <StatCard label="Latency P99" value={ms(data.latency_p99_ms)} sub="worst 1%" helpId="tile.obs.latency" />
          </div>
          <div className="grid gap-5 lg:grid-cols-2">
            <Card className="p-3">
              <div className="mb-2 px-1 text-sm font-medium text-fg-1">Latency by span kind</div>
              <table className="w-full text-[12px]">
                <thead>
                  <tr className="text-left text-fg-3">
                    <th className="py-1 font-medium">Kind</th>
                    <th className="py-1 text-right font-medium">Spans</th>
                    <th className="py-1 text-right font-medium">P50</th>
                    <th className="py-1 text-right font-medium">P95</th>
                    <th className="py-1 text-right font-medium">Avg</th>
                  </tr>
                </thead>
                <tbody>
                  {data.by_kind.map((k) => (
                    <tr key={k.kind} className="border-t border-line-2">
                      <td className="py-1"><Pill variant="neutral">{k.kind}</Pill></td>
                      <td className="py-1 text-right text-fg-1">{num(k.spans)}</td>
                      <td className="py-1 text-right text-fg-1">{ms(k.p50_ms)}</td>
                      <td className="py-1 text-right text-fg-1">{ms(k.p95_ms)}</td>
                      <td className="py-1 text-right text-fg-2">{ms(k.avg_ms)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </Card>
            <Card className="p-3">
              <div className="mb-2 px-1 text-sm font-medium text-fg-1">Error causes</div>
              {data.error_causes.length === 0 ? (
                <p className="px-1 py-3 text-[13px] text-fg-3">No errored spans in window.</p>
              ) : (
                <table className="w-full text-[12px]">
                  <thead>
                    <tr className="text-left text-fg-3">
                      <th className="py-1 font-medium">Operation</th>
                      <th className="py-1 text-right font-medium">Errors</th>
                    </tr>
                  </thead>
                  <tbody>
                    {data.error_causes.map((c) => (
                      <tr key={c.key} className="border-t border-line-2">
                        <td className="py-1 font-mono text-fg-2">{c.key}</td>
                        <td className="py-1 text-right text-danger">{num(c.traces)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </Card>
          </div>
        </div>
      )}

      <div className="grid gap-5 lg:grid-cols-2">
        <DimCard title="By model" rows={data.by_model} keyHeader="Model" />
        <DimCard title="By source" rows={data.by_source} keyHeader="Source" />
      </div>
      <DimCard title="By project" rows={data.by_project} keyHeader="Project" mono />
    </div>
  );
}

function DimCard({
  title,
  rows,
  keyHeader,
  mono,
}: {
  title: string;
  rows: ObsDimCount[];
  keyHeader: string;
  mono?: boolean;
}) {
  const columns = useMemo<ColumnDef<ObsDimCount, any>[]>(
    () => [
      {
        accessorKey: "key",
        header: keyHeader,
        cell: (c) =>
          mono ? (
            <span className="font-mono text-[12px] text-fg-2">{shortKey(c.row.original.key)}</span>
          ) : (
            <Pill variant="neutral">{c.row.original.key}</Pill>
          ),
      },
      {
        accessorKey: "traces",
        header: "Traces",
        cell: (c) => num(c.row.original.traces),
        meta: { align: "right" },
      },
      {
        accessorKey: "tokens",
        header: "Tokens",
        cell: (c) => compact(c.row.original.tokens),
        meta: { align: "right" },
      },
      {
        accessorKey: "cost_usd",
        header: "Cost",
        cell: (c) => usd(c.row.original.cost_usd),
        meta: { align: "right", mono: true },
      },
    ],
    [keyHeader, mono],
  );

  return (
    <Card className="p-3">
      <div className="mb-2 px-1 text-sm font-medium text-fg-1">{title}</div>
      {rows.length === 0 ? (
        <Empty message={`No ${keyHeader.toLowerCase()} data.`} />
      ) : (
        <DataTable
          data={rows}
          columns={columns}
          rowKey={(r) => r.key}
          initialSort={[{ id: "tokens", desc: true }]}
          zebra
          minWidth={420}
        />
      )}
    </Card>
  );
}

// shortKey trims a project_hash to a readable prefix (it is a 64-char sha256).
function shortKey(k: string): string {
  if (k === "(none)") return k;
  return k.length > 14 ? k.slice(0, 14) + "…" : k;
}
