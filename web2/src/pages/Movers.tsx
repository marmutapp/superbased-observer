import { useState } from "react";
import { api, type MoverRow } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { useFilters } from "@/lib/filters";
import { usd } from "@/lib/format";
import { Card, Empty, ErrorState, PageHeader } from "@/components/ui";
import { SegmentedControl, TableSkeleton } from "@/components/primitives";

type Dim = "model" | "project" | "tool";

export function MoversPage() {
  const { days } = useFilters();
  const [dim, setDim] = useState<Dim>("model");
  const { data, error, loading, reload } = useApi(() => api.movers(days, dim), [days, dim]);

  const dimLabel = dim === "project" ? "project" : dim;

  return (
    <>
      <PageHeader
        title="Movers"
        subtitle="Period-over-period spend movement vs the prior window of equal length."
        right={
          <SegmentedControl<Dim>
            options={[
              { value: "model", label: "Model" },
              { value: "project", label: "Project" },
              { value: "tool", label: "Tool" },
            ]}
            value={dim}
            onChange={setDim}
          />
        }
      />
      {error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : loading || !data ? (
        <Card className="p-4">
          <TableSkeleton rows={6} />
        </Card>
      ) : (
        <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
          <MoverList title="Top increases" rows={data.increases} kind="delta" dim={dimLabel} />
          <MoverList title="Top decreases" rows={data.decreases} kind="delta" dim={dimLabel} />
          <MoverList title="New this period" rows={data.new_entrants} kind="current" dim={dimLabel} />
        </div>
      )}
    </>
  );
}

function MoverList({
  title,
  rows,
  kind,
  dim,
}: {
  title: string;
  rows: MoverRow[];
  kind: "delta" | "current";
  dim: string;
}) {
  return (
    <Card className="p-3">
      <div className="mb-2 px-1 text-sm font-medium text-fg-1">{title}</div>
      {rows.length === 0 ? (
        <Empty message={`No ${title.toLowerCase()} this period.`} />
      ) : (
        <ul className="space-y-1.5">
          {rows.map((r) => (
            <li key={r.key} className="flex items-center justify-between gap-2 rounded-2 px-2 py-1.5 text-[12px] hover:bg-bg-2">
              <span className="min-w-0 flex-1 truncate font-mono text-fg-2" title={`${dim}: ${r.key}`}>
                {r.key}
              </span>
              {kind === "delta" ? (
                <span className={`shrink-0 font-mono ${r.delta_usd >= 0 ? "text-bad" : "text-good"}`}>
                  {r.delta_usd >= 0 ? "+" : "−"}
                  {usd(Math.abs(r.delta_usd))}
                </span>
              ) : (
                <span className="shrink-0 font-mono text-fg-1">{usd(r.current_usd)}</span>
              )}
            </li>
          ))}
        </ul>
      )}
    </Card>
  );
}
