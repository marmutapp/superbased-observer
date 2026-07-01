import { useMemo } from "react";
import { useNavigate } from "react-router-dom";
import type { ColumnDef } from "@tanstack/react-table";
import { ChartShell, PageHeader, Pill, StatCard } from "@/components/primitives";
import { HelpInd } from "@/components/HelpInd";
import {
  BarChartIcon,
  LayersIcon,
  ListIcon,
  PercentIcon,
} from "@/components/icons";
import { DataTable } from "@/components/DataTable";
import { ChartState } from "@/components/ChartState";
import { useApi } from "@/lib/useApi";
import { fmtClock, fmtInt, fmtPct } from "@/lib/format";
import { ObsOffNotice } from "./ObsOffNotice";
import type { ObsDatasetsResponse, ObsEvalRunRow, ObsEvalRunsResponse } from "./types";

// EvalsPage lists the eval plane's runs (the primary artifact a user compares)
// plus a compact datasets summary, served from /api/obs/eval/*. When the
// observability subsystem is off the endpoints aren't served, so a fetch error
// renders an honest "disabled" state (no fake counts — the operator-honesty
// steer). Click a run to open its detail + run-vs-run comparison. Rendered
// through the platform's ChartShell + DataTable + ChartState primitives.
export function EvalsPage() {
  const nav = useNavigate();
  const runs = useApi<ObsEvalRunsResponse>("/api/obs/eval/runs", { limit: 200 }, [], {
    refreshMs: 15000,
  });
  const datasets = useApi<ObsDatasetsResponse>("/api/obs/eval/datasets", undefined, [], {
    refreshMs: 30000,
  });

  const columns = useMemo<ColumnDef<ObsEvalRunRow, unknown>[]>(
    () => [
      {
        header: "Run",
        accessorKey: "name",
        cell: ({ row }) => (
          <span className="font-medium text-fg-1">
            {row.original.name || `run #${row.original.id}`}
          </span>
        ),
      },
      {
        header: "Dataset",
        accessorKey: "dataset_name",
        cell: ({ row }) => <span className="text-fg-2">{row.original.dataset_name || "—"}</span>,
      },
      {
        header: "Status",
        accessorKey: "status",
        cell: ({ row }) => (
          <Pill variant={row.original.status === "done" ? "neutral" : "warn"}>
            {row.original.status}
          </Pill>
        ),
      },
      {
        header: "Pass rate",
        accessorKey: "passed",
        meta: { align: "right" },
        cell: ({ row }) => {
          const r = row.original;
          const rate = r.total > 0 ? r.passed / r.total : 0;
          return (
            <span className="tabular-nums">
              <span
                className={
                  rate >= 1 ? "text-success" : rate >= 0.5 ? "text-fg-2" : "text-danger"
                }
              >
                {fmtPct(rate)}
              </span>
              <span className="ml-1 text-xs text-fg-3">
                ({r.passed}/{r.total})
              </span>
            </span>
          );
        },
      },
      {
        header: "Mean",
        accessorKey: "mean_score",
        meta: { align: "right", mono: true },
        cell: ({ row }) => row.original.mean_score.toFixed(3),
      },
      {
        header: "Items",
        accessorKey: "total",
        meta: { align: "right", mono: true },
        cell: ({ row }) => fmtInt(row.original.total),
      },
      {
        header: "Started",
        accessorKey: "started_at",
        cell: ({ row }) => <span className="text-fg-3">{fmtClock(row.original.started_at)}</span>,
      },
    ],
    [],
  );

  const runRows = runs.data?.runs ?? [];
  const dsList = datasets.data?.datasets ?? [];
  const latest = runRows[0];
  const latestRate = latest && latest.total > 0 ? latest.passed / latest.total : 0;
  const meanOfMeans =
    runRows.length > 0 ? runRows.reduce((n, r) => n + r.mean_score, 0) / runRows.length : 0;

  return (
    <div className="space-y-6 p-6">
      <PageHeader
        title="Evals"
        sub="Scorer runs over trajectory datasets — regression gates & run-vs-run comparison"
        helpId="tab.evals"
      />

      {!runs.error && runs.data && (
        <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
          <StatCard
            label="Runs"
            helpId="tile.obs.runs"
            icon={<ListIcon />}
            value={fmtInt(runRows.length)}
            sub="scored passes"
            loading={runs.loading && !runs.data}
          />
          <StatCard
            label="Datasets"
            helpId="tile.obs.datasets"
            icon={<LayersIcon />}
            value={fmtInt(dsList.length)}
            sub="available to score"
          />
          <StatCard
            label="Latest pass rate"
            helpId="tile.obs.pass_rate"
            icon={<PercentIcon />}
            value={latest ? fmtPct(latestRate) : "—"}
            sub={latest ? latest.name || `run #${latest.id}` : "no runs yet"}
            warn={!!latest && latestRate < 0.5}
          />
          <StatCard
            label="Mean score"
            helpId="tile.obs.mean_score"
            icon={<BarChartIcon />}
            value={runRows.length > 0 ? meanOfMeans.toFixed(3) : "—"}
            sub="across recent runs"
          />
        </div>
      )}

      {runs.error ? (
        <ObsOffNotice>
          The eval endpoints aren&apos;t being served. Enable{" "}
          <code className="rounded bg-bg-3 px-1">[observability] enabled = true</code> in your
          config (Settings → Observability), capture traces, then build a dataset with{" "}
          <code className="rounded bg-bg-3 px-1">observer eval dataset create-from-traces</code>{" "}
          and score it with <code className="rounded bg-bg-3 px-1">observer eval run</code>.
        </ObsOffNotice>
      ) : (
        <>
          {dsList.length > 0 && (
            <div className="flex flex-wrap gap-2">
              {dsList.map((d) => (
                <div
                  key={d.id}
                  className="rounded-2 border border-line-2 bg-bg-2 px-3 py-1.5 text-xs text-fg-2"
                  title={d.description || undefined}
                >
                  <span className="font-medium text-fg-1">{d.name}</span>
                  <span className="ml-2 text-fg-3">{fmtInt(d.item_count)} items</span>
                </div>
              ))}
            </div>
          )}

          <ChartShell
            title={
              <span className="flex items-center gap-2">
                Runs
                {runs.data && <Pill>{fmtInt(runRows.length)}</Pill>}
                <HelpInd id="chart.obs.eval_runs" />
              </span>
            }
            sub="click a run for per-item scores & run-vs-run comparison"
          >
            <ChartState
              loading={runs.loading && !runs.data}
              error={null}
              empty={!runs.loading && runRows.length === 0}
              emptyHint="No eval runs yet. Run `observer eval run` against a dataset."
              height={220}
            >
              <DataTable<ObsEvalRunRow>
                data={runRows}
                columns={columns}
                onRowClick={(r) => nav(`/evals/${r.id}`)}
                rowKey={(r) => String(r.id)}
                minWidth={720}
                loading={runs.loading}
                initialSort={[{ id: "started_at", desc: true }]}
              />
            </ChartState>
          </ChartShell>
        </>
      )}
    </div>
  );
}
