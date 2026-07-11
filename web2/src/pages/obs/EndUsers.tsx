import { useMemo } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { api, type ObsEndUserBucket } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { useFilters } from "@/lib/filters";
import { compact, num, pct1, usd } from "@/lib/format";
import { Card, Empty, ErrorState, PageHeader } from "@/components/ui";
import { StatCard, StatStripSkeleton, TableSkeleton } from "@/components/primitives";
import { DataTable } from "@/components/DataTable";

// Org observability per-END-USER spend (T5, org-budget guardrails plan §2.1).
// CROSS-INSTANCE spend attributed to the hosted-app's end-user identity, summed
// across every node that observed that end-user. DISTINCT from Trajectory cost
// (which attributes to the developer/project/model over obs_summaries): this
// answers "which of MY app's END-USERS is driving spend". The end-user id is
// PII, so the tier rides the obs_summary opt-in AND node-side raw-content
// sharing — the honest empty state below names both. Admin-only.
export function ObsEndUsersPage() {
  const { days } = useFilters();
  const { data, error, loading, reload } = useApi(() => api.obsEndUserSpend(days), [days]);

  return (
    <>
      <PageHeader
        title="End-user spend"
        subtitle="Cross-instance spend attributed to your hosted app's end-users. Summed across every enrolled node that observed the end-user — a per-customer view the node-local budgets can't provide."
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
            <StatCard
              label="Total spend"
              value={usd(data.total_cost_usd)}
              accent
              sub="end-users, window"
              helpId="tile.obs.enduser_cost"
            />
            <StatCard
              label="End-users"
              value={String(data.users.length)}
              sub="attributed"
              helpId="tile.obs.endusers"
            />
          </div>
          <SpendCard rows={data.users} />
        </div>
      )}
    </>
  );
}

function NotConfigured() {
  return (
    <Card className="p-6">
      <h3 className="text-[15px] font-semibold text-fg-0">No end-user spend shared</h3>
      <p className="mt-2 max-w-2xl text-sm leading-relaxed text-fg-2">
        No enrolled node has shared per-end-user spend for this window. Because an
        end-user id is <b className="text-fg-1">personally identifying</b>, this tier
        rides both the observability opt-in and node-side raw-content sharing —
        a node ships it only when its operator has set{" "}
        <span className="font-mono text-fg-2">[org_client.share].obs_summary = true</span>{" "}
        <b className="text-fg-1">and</b>{" "}
        <span className="font-mono text-fg-2">full_content = true</span>{" "}
        (or <span className="font-mono text-fg-2">admin_managed = true</span> under a
        native-console deployment). There is no remote toggle the org admin can flip.
      </p>
    </Card>
  );
}

function SpendCard({ rows }: { rows: ObsEndUserBucket[] }) {
  const columns = useMemo<ColumnDef<ObsEndUserBucket, any>[]>(
    () => [
      {
        accessorKey: "end_user",
        header: "End-user",
        cell: (c) => <span className="text-fg-1">{c.row.original.end_user}</span>,
      },
      { accessorKey: "cost_usd", header: "Cost", cell: (c) => usd(c.row.original.cost_usd), meta: { align: "right", mono: true } },
      { accessorKey: "cost_share", header: "Share", cell: (c) => pct1(c.row.original.cost_share), meta: { align: "right" } },
      { accessorKey: "total_tokens", header: "Tokens", cell: (c) => compact(c.row.original.total_tokens), meta: { align: "right" } },
      { accessorKey: "traces", header: "Traces", cell: (c) => num(c.row.original.traces), meta: { align: "right" } },
    ],
    [],
  );
  return (
    <Card className="p-3">
      <div className="mb-2 px-1 text-sm font-medium text-fg-1">By end-user</div>
      {rows.length === 0 ? (
        <Empty message="No end-user data." />
      ) : (
        <DataTable
          data={rows}
          columns={columns}
          rowKey={(r) => r.end_user}
          initialSort={[{ id: "cost_usd", desc: true }]}
          zebra
          minWidth={460}
        />
      )}
    </Card>
  );
}
