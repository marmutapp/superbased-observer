import { useMemo } from "react";
import { useNavigate } from "react-router-dom";
import type { ColumnDef } from "@tanstack/react-table";
import { ChartShell, PageHeader, Pill, StatCard } from "@/components/primitives";
import { HelpInd } from "@/components/HelpInd";
import {
  AlertIcon,
  CoinsIcon,
  DatabaseIcon,
  LayersIcon,
  SparklesIcon,
} from "@/components/icons";
import { DataTable } from "@/components/DataTable";
import { ChartState } from "@/components/ChartState";
import { useApi } from "@/lib/useApi";
import { fmtClock, fmtDuration, fmtInt, fmtPct, fmtUSD } from "@/lib/format";
import { ObsOffNotice } from "./ObsOffNotice";
import type { ObsTraceRow, ObsTracesResponse } from "./types";

// TrajectoriesPage lists captured traces from the generalized-observability
// subsystem (/api/obs/traces). When the subsystem is disabled the endpoint is
// not served, so a fetch error renders an honest "disabled" state (no fake
// counts — the operator-honesty steer). Click a row for the span tree +
// timeline + proxy-verified detail. Rendered through the platform's
// ChartShell + DataTable + ChartState primitives like every other list page.
export function TrajectoriesPage() {
  const nav = useNavigate();
  const { data, loading, error } = useApi<ObsTracesResponse>("/api/obs/traces", { limit: 200 }, [], {
    refreshMs: 10000,
  });

  const columns = useMemo<ColumnDef<ObsTraceRow, unknown>[]>(
    () => [
      {
        header: "Root",
        accessorKey: "root_name",
        cell: ({ row }) => (
          <span className="font-medium text-fg-1">
            {row.original.root_name || row.original.trace_id.slice(0, 12)}
          </span>
        ),
      },
      {
        header: "Status",
        accessorKey: "status",
        cell: ({ row }) => (
          <Pill variant={row.original.status === "error" ? "danger" : "neutral"}>
            {row.original.status}
          </Pill>
        ),
      },
      {
        header: "Spans",
        accessorKey: "span_count",
        meta: { align: "right", mono: true },
        cell: ({ row }) => fmtInt(row.original.span_count),
      },
      {
        header: "Duration",
        accessorKey: "duration_ms",
        meta: { align: "right" },
        cell: ({ row }) => fmtDuration(row.original.duration_ms),
      },
      {
        header: "Tokens",
        accessorKey: "total_tokens",
        meta: { align: "right", mono: true },
        cell: ({ row }) =>
          row.original.total_tokens > 0 ? fmtInt(row.original.total_tokens) : "—",
      },
      {
        header: "Cost",
        accessorKey: "cost_usd",
        meta: { align: "right", mono: true },
        cell: ({ row }) =>
          row.original.cost_usd > 0 ? fmtUSD(row.original.cost_usd, true) : "—",
      },
      {
        header: "Source",
        accessorKey: "source",
        cell: ({ row }) => <span className="text-fg-3">{row.original.source}</span>,
      },
      {
        header: "Started",
        accessorKey: "started_at",
        cell: ({ row }) => <span className="text-fg-3">{fmtClock(row.original.started_at)}</span>,
      },
    ],
    [],
  );

  const traces = data?.traces ?? [];
  const totalSpans = traces.reduce((n, t) => n + t.span_count, 0);
  const totalCost = traces.reduce((n, t) => n + t.cost_usd, 0);
  const totalTokens = traces.reduce((n, t) => n + t.total_tokens, 0);
  const errors = traces.filter((t) => t.status === "error").length;
  const errRate = traces.length > 0 ? errors / traces.length : 0;

  return (
    <div className="space-y-6 p-6">
      <PageHeader
        title="Trajectories"
        sub="Trace/span graph for custom apps & agents captured via OTLP — enriched with the proxy's exact cost & cache where a span matches a proxied turn."
        helpId="tab.trajectories"
      />

      {!error && data && (
        <div className="grid grid-cols-2 gap-3 sm:grid-cols-5">
          <StatCard
            label="Traces"
            helpId="tile.obs.traces"
            icon={<SparklesIcon />}
            value={fmtInt(traces.length)}
            sub="captured via OTLP"
            loading={loading && !data}
          />
          <StatCard
            label="Spans"
            helpId="tile.obs.spans"
            icon={<LayersIcon />}
            value={fmtInt(totalSpans)}
            sub="across all traces"
          />
          <StatCard
            label="Tokens"
            helpId="tile.obs.tokens"
            icon={<DatabaseIcon />}
            value={fmtInt(totalTokens)}
            sub="LLM spans"
          />
          <StatCard
            label="Total cost"
            helpId="tile.obs.cost"
            icon={<CoinsIcon />}
            value={totalCost > 0 ? fmtUSD(totalCost, true) : "$0"}
            sub="proxy-verified where matched"
            accent
          />
          <StatCard
            label="Error rate"
            helpId="tile.obs.error_rate"
            icon={<AlertIcon />}
            value={fmtPct(errRate)}
            sub={`${errors}/${traces.length} traces`}
            warn={errors > 0}
          />
        </div>
      )}

      {error ? (
        <ObsOffNotice>
          The trajectory endpoints aren&apos;t being served. Enable{" "}
          <code className="rounded bg-bg-3 px-1">[observability] enabled = true</code> in your
          config (Settings → Observability) and point an OTLP exporter at{" "}
          <code className="rounded bg-bg-3 px-1">/v1/traces</code> on the OTLP receiver.
        </ObsOffNotice>
      ) : (
        <ChartShell
          title={
            <span className="flex items-center gap-2">
              Captured traces
              {data && <Pill>{fmtInt(traces.length)}</Pill>}
              <HelpInd id="chart.obs.captured_traces" />
            </span>
          }
          sub="click any row for the span tree, timeline & proxy-verified detail"
        >
          <ChartState
            loading={loading && !data}
            error={null}
            empty={!loading && traces.length === 0}
            emptyHint="No traces captured yet. Point an OTLP exporter at /v1/traces."
            height={220}
          >
            <DataTable<ObsTraceRow>
              data={traces}
              columns={columns}
              onRowClick={(t) => nav(`/trajectories/${t.trace_id}`)}
              rowKey={(t) => t.trace_id}
              minWidth={720}
              loading={loading}
              initialSort={[{ id: "started_at", desc: true }]}
            />
          </ChartState>
        </ChartShell>
      )}
    </div>
  );
}
