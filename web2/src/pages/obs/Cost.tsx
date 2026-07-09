import { useMemo } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { api, type ObsCostBucket } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { useFilters } from "@/lib/filters";
import { compact, num, pct1, usd } from "@/lib/format";
import { Card, Empty, ErrorState, PageHeader } from "@/components/ui";
import { StatCard, StatStripSkeleton, TableSkeleton } from "@/components/primitives";
import { DataTable } from "@/components/DataTable";

// Org observability cost attribution (obs-org-tier OP6). Who and what is
// spending on custom-app / agent trajectories — by developer, project, model —
// over the content-free obs_summaries aggregate. Distinct from the budget/cost
// surfaces (which count api_turns). Admin-only.
export function ObsCostPage() {
  const { days } = useFilters();
  const { data, error, loading, reload } = useApi(() => api.obsCost(days), [days]);

  return (
    <>
      <PageHeader
        title="Trajectory cost"
        subtitle="Custom-app / agent trajectory cost attributed by developer, project and model. Includes non-proxied spans — a separate view from the api_turns-based budgets."
      />
      {error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : loading || !data ? (
        <div className="space-y-5">
          <StatStripSkeleton count={2} />
          <Card className="p-4">
            <TableSkeleton rows={6} />
          </Card>
        </div>
      ) : !data.configured ? (
        <NotConfigured />
      ) : (
        <div className="space-y-5">
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-3">
            <StatCard label="Total cost" value={usd(data.total_cost_usd)} accent sub="trajectories, window" helpId="tile.obs.cost" />
            <StatCard label="Developers" value={String(data.by_developer.length)} sub="attributed" helpId="tile.obs.developers" />
            <StatCard label="Projects" value={String(data.by_project.length)} sub="attributed" helpId="tile.obs.projects" />
          </div>
          <div className="grid gap-5 lg:grid-cols-2">
            <CostCard title="By developer" keyHeader="Developer" rows={data.by_developer} />
            <CostCard title="By model" keyHeader="Model" rows={data.by_model} />
          </div>
          <CostCard title="By project" keyHeader="Project" rows={data.by_project} />
        </div>
      )}
    </>
  );
}

function NotConfigured() {
  return (
    <Card className="p-6">
      <h3 className="text-[15px] font-semibold text-fg-0">No trajectory cost shared</h3>
      <p className="mt-2 max-w-2xl text-sm leading-relaxed text-fg-2">
        No enrolled node has shared an observability summary for this window.
        Cost attribution rides the same <b className="text-fg-1">node-side opt-in</b> as the
        analytics floor — <span className="font-mono text-fg-2">[org_client.share].obs_summary = true</span> —
        and is content-free (cost / tokens / traces by developer, project_hash
        and model; never a prompt or response).
      </p>
    </Card>
  );
}

function CostCard({ title, keyHeader, rows }: { title: string; keyHeader: string; rows: ObsCostBucket[] }) {
  const columns = useMemo<ColumnDef<ObsCostBucket, any>[]>(
    () => [
      { accessorKey: "label", header: keyHeader, cell: (c) => <span className="text-fg-1">{c.row.original.label}</span> },
      { accessorKey: "cost_usd", header: "Cost", cell: (c) => usd(c.row.original.cost_usd), meta: { align: "right", mono: true } },
      { accessorKey: "cost_share", header: "Share", cell: (c) => pct1(c.row.original.cost_share), meta: { align: "right" } },
      { accessorKey: "tokens", header: "Tokens", cell: (c) => compact(c.row.original.tokens), meta: { align: "right" } },
      { accessorKey: "traces", header: "Traces", cell: (c) => num(c.row.original.traces), meta: { align: "right" } },
    ],
    [keyHeader],
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
          rowKey={(r) => r.key || r.label}
          initialSort={[{ id: "cost_usd", desc: true }]}
          zebra
          minWidth={460}
        />
      )}
    </Card>
  );
}
