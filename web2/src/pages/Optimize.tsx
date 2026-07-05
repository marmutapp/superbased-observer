import { useMemo } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { api, type RoutingDimCount, type RoutingResult } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { useFilters } from "@/lib/filters";
import { num, pct1, usd } from "@/lib/format";
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
import { RoutingTrendChart } from "@/components/charts/RoutingTrendChart";

export function OptimizePage() {
  const { days } = useFilters();
  const { data, error, loading, reload } = useApi(() => api.routing(days), [days]);

  return (
    <>
      <PageHeader
        title="Routing"
        subtitle="Model-routing decisions shared by enrolled nodes (§R19). Org-aggregate, content-free — counts and decision-time estimates only, never a model id."
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

// NotConfigured is the honest empty state for the common case: no node has
// opted into routing-summary sharing, so routing_summaries holds no rows. It
// names the exact node-side dependency rather than implying data is "coming".
function NotConfigured() {
  return (
    <Card className="p-6">
      <h3 className="text-[15px] font-semibold text-fg-0">No routing summaries shared</h3>
      <p className="mt-2 max-w-2xl text-sm leading-relaxed text-fg-2">
        No enrolled node has shared a model-routing summary for this window.
        Routing data is a <b className="text-fg-1">node-side opt-in</b>: an operator must
        set <span className="font-mono text-fg-2">[org_client.share].routing_summary = true</span> in
        their local config for the aggregate (decision counts + decision-time
        dollar estimates by day × tier × reason × mode — never a model id, never
        content) to reach this server.
      </p>
      <p className="mt-4 text-[12px] text-fg-3">
        There is no remote enforce toggle and no way for the org to flip this on
        a node (§R23) — the share is the operator's choice. See{" "}
        <span className="font-mono text-fg-2">docs/model-routing.md</span>.
      </p>
    </Card>
  );
}

function Configured({ data }: { data: RoutingResult }) {
  const appliedRate = data.total_decisions > 0 ? data.total_applied / data.total_decisions : 0;
  const enforceShare = data.total_decisions > 0 ? data.enforce_decisions / data.total_decisions : 0;

  return (
    <div className="space-y-5">
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <StatCard label="Decisions" value={num(data.total_decisions)} sub={`${num(data.total_applied)} applied`} />
        <StatCard
          label="Net savings (est.)"
          value={usd(data.net_savings_usd)}
          sub={`${usd(data.est_savings_usd)} gross − ${usd(data.cache_forfeit_usd)} cache`}
          accent
          helpId="metric.routing_savings"
        />
        <StatCard label="Applied rate" value={pct1(appliedRate)} sub="rewritten ÷ decisions" />
        <StatCard
          label="Enforce share"
          value={pct1(enforceShare)}
          sub={`${num(data.advise_decisions)} advise · ${num(data.enforce_decisions)} enforce`}
          helpId="metric.routing_mode"
        />
      </div>

      <ChartShell title="Routing decisions over time">
        <RoutingTrendChart data={data.by_day} />
      </ChartShell>

      <div className="grid gap-5 lg:grid-cols-2">
        <DimCard title="By tier" rows={data.by_tier} emptyMsg="No tier data." />
        <DimCard title="By reason" rows={data.by_reason} emptyMsg="No reason data." />
      </div>
    </div>
  );
}

function DimCard({
  title,
  rows,
  emptyMsg,
}: {
  title: string;
  rows: RoutingDimCount[];
  emptyMsg: string;
}) {
  const columns = useMemo<ColumnDef<RoutingDimCount, any>[]>(
    () => [
      {
        accessorKey: "key",
        header: title === "By tier" ? "Tier" : "Reason",
        cell: (c) => <Pill variant="neutral">{c.row.original.key}</Pill>,
      },
      {
        accessorKey: "decisions",
        header: "Decisions",
        cell: (c) => num(c.row.original.decisions),
        meta: { align: "right" },
      },
      {
        accessorKey: "applied",
        header: "Applied",
        cell: (c) => num(c.row.original.applied),
        meta: { align: "right" },
      },
      {
        accessorKey: "est_savings_usd",
        header: "Est. savings",
        cell: (c) => usd(c.row.original.est_savings_usd),
        meta: { align: "right", mono: true },
      },
    ],
    [title],
  );

  return (
    <Card className="p-3">
      <div className="mb-2 px-1 text-sm font-medium text-fg-1">{title}</div>
      {rows.length === 0 ? (
        <Empty message={emptyMsg} />
      ) : (
        <DataTable
          data={rows}
          columns={columns}
          rowKey={(r) => r.key}
          initialSort={[{ id: "decisions", desc: true }]}
          zebra
          minWidth={420}
        />
      )}
    </Card>
  );
}
