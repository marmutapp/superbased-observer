import { api, type ObsEvalRunGroup } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { useFilters } from "@/lib/filters";
import { num, pct1, shortDate } from "@/lib/format";
import { Card, ErrorState, PageHeader } from "@/components/ui";
import { Pill, StatCard, StatStripSkeleton, TableSkeleton } from "@/components/primitives";

// Org eval-health (obs-org-tier T4). Run history + regression tracking over the
// content-free obs_eval_summaries member nodes push under
// [org_client.share].obs_eval_summary. Admin-only.
export function ObsEvalsPage() {
  const { days } = useFilters();
  const { data, error, loading, reload } = useApi(() => api.obsEvals(days), [days]);

  return (
    <>
      <PageHeader
        title="Eval health"
        subtitle="Evaluation runs shared by enrolled nodes. Org-aggregate, content-free — pass rates, mean scores, and per-scorer regression vs the prior run. No per-item bodies."
      />
      {error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : loading || !data ? (
        <div className="space-y-5">
          <StatStripSkeleton count={3} />
          <Card className="p-4">
            <TableSkeleton rows={6} />
          </Card>
        </div>
      ) : !data.configured ? (
        <NotConfigured />
      ) : (
        <Configured runs={data.runs} />
      )}
    </>
  );
}

function NotConfigured() {
  return (
    <Card className="p-6">
      <h3 className="text-[15px] font-semibold text-fg-0">No eval runs shared</h3>
      <p className="mt-2 max-w-2xl text-sm leading-relaxed text-fg-2">
        No enrolled node has shared an eval-run summary for this window. Eval
        health is a <b className="text-fg-1">node-side opt-in</b>: an operator must
        set <span className="font-mono text-fg-2">[org_client.share].obs_eval_summary = true</span> for
        the content-free run aggregates (pass counts + mean/min score per scorer,
        never the reference or output text) to reach this server.
      </p>
      <p className="mt-4 text-[12px] text-fg-3">
        Runs come from <span className="font-mono text-fg-2">observer eval run</span> on each node. See{" "}
        <span className="font-mono text-fg-2">docs/observability.md</span>.
      </p>
    </Card>
  );
}

function Configured({ runs }: { runs: ObsEvalRunGroup[] }) {
  const regressed = runs.filter((r) => r.regressed).length;
  const avgPass = runs.length ? runs.reduce((a, r) => a + r.pass_rate, 0) / runs.length : 0;
  return (
    <div className="space-y-5">
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3">
        <StatCard label="Runs" value={String(runs.length)} sub="in window" helpId="tile.obs.runs" />
        <StatCard label="Avg pass rate" value={pct1(avgPass)} sub="across runs" accent helpId="tile.obs.pass_rate" />
        <StatCard label="Regressed" value={String(regressed)} sub="runs with a scorer drop" helpId="tile.obs.regressed" />
      </div>
      <div className="space-y-3">
        {runs.map((r, i) => (
          <RunCard key={`${r.day}-${r.dataset_name}-${r.run_name}-${i}`} run={r} />
        ))}
      </div>
    </div>
  );
}

function RunCard({ run }: { run: ObsEvalRunGroup }) {
  return (
    <Card className="p-4">
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <span className="text-sm font-semibold text-fg-1">{run.run_name || run.dataset_name || "(run)"}</span>
        <Pill variant="neutral">{run.dataset_name || "—"}</Pill>
        {run.source === "online" && <Pill variant="info">online</Pill>}
        {run.regressed && <Pill variant="danger">regression</Pill>}
        <span className="ml-auto text-[12px] text-fg-3">{shortDate(run.day)}</span>
      </div>
      <div className="mb-3 flex gap-6 text-[13px]">
        <span className="text-fg-2">
          Pass <b className="text-fg-1">{pct1(run.pass_rate)}</b> ({num(run.passed)}/{num(run.total)})
        </span>
        <span className="text-fg-2">
          Mean <b className="text-fg-1">{run.mean_score.toFixed(2)}</b>
        </span>
      </div>
      <table className="w-full text-[12px]">
        <thead>
          <tr className="text-left text-fg-3">
            <th className="py-1 font-medium">Scorer</th>
            <th className="py-1 text-right font-medium">Pass</th>
            <th className="py-1 text-right font-medium">Mean</th>
            <th className="py-1 text-right font-medium">Δ vs prev</th>
          </tr>
        </thead>
        <tbody>
          {run.scorers.map((sc) => (
            <tr key={sc.scorer_name} className="border-t border-line-2">
              <td className="py-1 font-mono text-fg-2">{sc.scorer_name}</td>
              <td className="py-1 text-right text-fg-1">{pct1(sc.pass_rate)}</td>
              <td className="py-1 text-right text-fg-1">{sc.mean_score.toFixed(2)}</td>
              <td
                className={`py-1 text-right font-mono ${
                  sc.pass_rate_delta < 0 ? "text-danger" : sc.pass_rate_delta > 0 ? "text-success" : "text-fg-3"
                }`}
              >
                {sc.pass_rate_delta === 0 ? "—" : `${sc.pass_rate_delta > 0 ? "+" : ""}${pct1(sc.pass_rate_delta)}`}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </Card>
  );
}
