import { useMemo } from "react";
import { useNavigate } from "react-router-dom";
import type { ColumnDef } from "@tanstack/react-table";
import { api, type ObsTraceListRow } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { useFilters } from "@/lib/filters";
import { compact, ms, shortDate, usd } from "@/lib/format";
import { Card, ErrorState, PageHeader } from "@/components/ui";
import { EmptyState } from "@/components/EmptyState";
import { Pill, StatCard, StatStripSkeleton, TableSkeleton } from "@/components/primitives";
import { DataTable } from "@/components/DataTable";
import { Spline } from "lucide-react";

// Org trajectory explorer list (obs-org-tier T2). RBAC-scoped trace list over
// the obs_traces/obs_spans structure member nodes push under
// [org_client.share].obs_traces. Click a row → the span tree + proxy wedge.
export function ObsTrajectoriesPage() {
  const { days } = useFilters();
  const nav = useNavigate();
  const { data, error, loading, reload } = useApi(() => api.obsTrajectories(days), [days]);

  const columns = useMemo<ColumnDef<ObsTraceListRow, any>[]>(
    () => [
      {
        accessorKey: "root_name",
        header: "Trajectory",
        cell: (c) => <span className="font-medium text-fg-1">{c.row.original.root_name || "(unnamed)"}</span>,
      },
      {
        accessorKey: "status",
        header: "Status",
        cell: (c) => (
          <Pill variant={c.row.original.status === "error" ? "danger" : "success"}>
            {c.row.original.status || "ok"}
          </Pill>
        ),
      },
      { accessorKey: "span_count", header: "Spans", cell: (c) => c.row.original.span_count, meta: { align: "right" } },
      {
        accessorKey: "total_tokens",
        header: "Tokens",
        cell: (c) => compact(c.row.original.total_tokens),
        meta: { align: "right" },
      },
      {
        accessorKey: "cost_usd",
        header: "Cost",
        cell: (c) => usd(c.row.original.cost_usd),
        meta: { align: "right", mono: true },
      },
      { accessorKey: "duration_ms", header: "Duration", cell: (c) => ms(c.row.original.duration_ms), meta: { align: "right" } },
      { accessorKey: "source", header: "Source", cell: (c) => <span className="text-[12px] text-fg-3">{c.row.original.source}</span> },
      { accessorKey: "started_at", header: "Started", cell: (c) => shortDate(c.row.original.started_at), meta: { align: "right" } },
    ],
    [],
  );

  return (
    <>
      <PageHeader
        title="Trajectory explorer"
        subtitle="Custom-app / agent traces shared by enrolled nodes (structure only — kinds, names, durations, cost). Click a trace for the span tree and proxy-verified cost."
      />
      {error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : loading || !data ? (
        <div className="space-y-5">
          <StatStripSkeleton count={2} />
          <Card className="p-4">
            <TableSkeleton rows={8} />
          </Card>
        </div>
      ) : !data.configured ? (
        <NotConfigured />
      ) : (
        <div className="space-y-5">
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-3">
            <StatCard label="Traces" value={String(data.traces.length)} sub="in window" helpId="tile.obs.traces" />
            <StatCard label="Cost" value={usd(data.traces.reduce((a, t) => a + t.cost_usd, 0))} sub="across shared nodes" accent helpId="tile.obs.cost" />
            <StatCard label="Tokens" value={compact(data.traces.reduce((a, t) => a + t.total_tokens, 0))} sub="total" helpId="tile.obs.tokens" />
          </div>
          <Card className="p-3">
            <DataTable
              data={data.traces}
              columns={columns}
              rowKey={(r) => r.trace_id}
              onRowClick={(r) => nav(`/trajectories/${encodeURIComponent(r.trace_id)}`)}
              initialSort={[{ id: "started_at", desc: true }]}
              zebra
              minWidth={760}
            />
          </Card>
        </div>
      )}
    </>
  );
}

function NotConfigured() {
  return (
    <EmptyState
      icon={Spline}
      title="No trajectory structure shared"
      helpId="tab.obs_explorer"
      docHint="docs/observability.md"
      body={
        <>
          No enrolled node has shared trace structure for this window. The
          explorer is a <b className="text-fg-1">node-side opt-in</b>: an operator
          must set{" "}
          <span className="font-mono text-fg-2">
            [org_client.share].obs_traces = true
          </span>{" "}
          for the content-free span topology (kinds, names, durations, tokens,
          cost, request_id — never a prompt or response) to reach this server.
          The <span className="font-mono text-fg-2">obs_summary</span> aggregate
          (Trajectory analytics) is a lighter opt-in if you only need cost/volume
          trends.
        </>
      }
    />
  );
}
