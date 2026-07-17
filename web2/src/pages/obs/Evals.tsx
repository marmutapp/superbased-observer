import { Link } from "react-router-dom";
import { api, type ObsEvalRunGroup, type ObsEvalRunListRow } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { useFilters } from "@/lib/filters";
import { num, pct1, shortDate } from "@/lib/format";
import { Card, ErrorState, PageHeader } from "@/components/ui";
import { Pill, StatCard, StatStripSkeleton, TableSkeleton } from "@/components/primitives";

// Org eval-health (obs-org-tier T4 + T7). Two independently-gated surfaces:
// the T4 run-health AGGREGATE (regression trends over obs_eval_summaries under
// [org_client.share].obs_eval_summary), and the T7 per-item runs list (drill
// into one run + diff two, over obs_eval_items under
// [org_client.share.obs].eval_items). Admin-only.
export function ObsEvalsPage() {
  const { days } = useFilters();
  const { data, error, loading, reload } = useApi(() => api.obsEvals(days), [days]);

  return (
    <>
      <PageHeader
        title="Eval health"
        subtitle="Evaluation runs shared by enrolled nodes. Aggregate pass rates + per-scorer regression are content-free (obs_eval_summary); opt a node into eval_items for the per-item drill-in below (item bodies only under full-content)."
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
      <div className="mt-5">
        <PerItemRuns days={days} />
      </div>
    </>
  );
}

// PerItemRuns is the T7 drill-in feed: each shared run links to the per-item
// EvalRun page. Independently gated from the T4 aggregate above — a node can
// share either, both, or neither. Honest empty state names the exact opt-in.
function PerItemRuns({ days }: { days: number }) {
  const { data, error, loading } = useApi(() => api.obsEvalRuns(days), [days]);
  return (
    <Card className="p-3">
      <div className="mb-2 flex items-center justify-between gap-3 px-1">
        <div className="text-sm font-medium text-fg-1">
          Per-item runs
          <span className="ml-2 text-[11px] font-normal text-fg-3">click a run to drill into its item scores + diff two runs</span>
        </div>
      </div>
      {loading ? (
        <TableSkeleton rows={4} />
      ) : error ? (
        <div className="px-1 py-2 text-[12px] text-danger">{error}</div>
      ) : !data || !data.configured || data.runs.length === 0 ? (
        <PerItemEmpty />
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full min-w-[640px] text-sm">
            <thead>
              <tr className="border-b border-line-2 text-left text-[10px] font-semibold uppercase tracking-[0.06em] text-fg-3">
                <th className="px-3 py-2">Run</th>
                <th className="px-3 py-2">Dataset</th>
                <th className="px-3 py-2 text-right">Pass</th>
                <th className="px-3 py-2 text-right">Items</th>
                <th className="px-3 py-2">Scorers</th>
                <th className="px-3 py-2 text-right">When</th>
              </tr>
            </thead>
            <tbody>
              {data.runs.map((r: ObsEvalRunListRow) => (
                <tr key={r.ref} className="border-b border-line-2/60 last:border-0 hover:bg-bg-2/50">
                  <td className="px-3 py-2">
                    <Link
                      to={`/trajectories/evals/${encodeURIComponent(r.ref)}`}
                      className="font-medium text-accent hover:underline"
                    >
                      {r.run_name || r.ref}
                    </Link>
                  </td>
                  <td className="px-3 py-2">
                    <Pill variant="neutral">{r.dataset_name || "—"}</Pill>
                  </td>
                  <td className="px-3 py-2 text-right tabular-nums text-fg-1">
                    {pct1(r.pass_rate)} <span className="text-fg-3">({num(r.passed)}/{num(r.scores)})</span>
                  </td>
                  <td className="px-3 py-2 text-right tabular-nums text-fg-2">{num(r.items)}</td>
                  <td className="px-3 py-2 font-mono text-[11px] text-fg-3">{r.scorers.join(", ") || "—"}</td>
                  <td className="px-3 py-2 text-right text-[12px] text-fg-3">{shortDate(r.started_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Card>
  );
}

function PerItemEmpty() {
  return (
    <div className="px-1 py-6 text-sm leading-relaxed text-fg-2">
      <p className="max-w-2xl">
        No per-item eval scores shared for this window. This drill-in is a separate{" "}
        <b className="text-fg-1">node-side opt-in</b> from the aggregate above — an operator sets{" "}
        <span className="font-mono text-fg-2">[org_client.share.obs].eval_items = true</span> for the
        content-free per-item scores (span/scorer/score/pass, never the item body) to reach this
        server. The item input/expected/output excerpts additionally require{" "}
        <span className="font-mono text-fg-2">full_content = true</span>, and reading them is a
        separate, server-audited action. There is <b className="text-fg-2">no remote toggle</b>.
      </p>
    </div>
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
