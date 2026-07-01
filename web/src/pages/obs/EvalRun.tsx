import { useMemo, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { ChartShell, PageHeader, Pill, StatCard } from "@/components/primitives";
import { BarChartIcon, ListIcon, PercentIcon } from "@/components/icons";
import { useApi } from "@/lib/useApi";
import { fmtClock, fmtInt, fmtPct } from "@/lib/format";
import type { ObsEvalRunDetail, ObsEvalRunsResponse, ObsRunScoreRow } from "./types";

// scoreKey aligns a score across two runs of the same dataset: a span scored
// by a named scorer is the same cell in run A and run B.
const scoreKey = (s: ObsRunScoreRow): string => `${s.span_id}::${s.scorer}`;

// EvalRunPage renders one run's summary + per-item scores, with an optional
// run-vs-run comparison: pick another run of the same dataset and the scores
// table aligns cell-for-cell, flagging regressions (Δ<0) and improvements
// (Δ>0). Mirrors the TraceDetail single-payload + selected-state pattern, and
// the platform StatCard / ChartShell primitives.
export function EvalRunPage() {
  const { id = "" } = useParams();
  const base = useApi<ObsEvalRunDetail>(id ? `/api/obs/eval/run/${id}` : null);
  const datasetID = base.data?.run.dataset_id;

  // Other runs of the same dataset, for the compare picker.
  const allRuns = useApi<ObsEvalRunsResponse>("/api/obs/eval/runs", { limit: 200 });
  const compareCandidates = useMemo(
    () =>
      (allRuns.data?.runs ?? []).filter(
        (r) => r.dataset_id === datasetID && String(r.id) !== id,
      ),
    [allRuns.data, datasetID, id],
  );

  const [compareID, setCompareID] = useState<string>("");
  const compare = useApi<ObsEvalRunDetail>(
    compareID ? `/api/obs/eval/run/${compareID}` : null,
  );

  const run = base.data?.run;
  const baseScores = base.data?.scores ?? [];
  const compareMap = useMemo(() => {
    const m = new Map<string, ObsRunScoreRow>();
    for (const s of compare.data?.scores ?? []) m.set(scoreKey(s), s);
    return m;
  }, [compare.data]);
  const comparing = compareID !== "" && compare.data != null;

  return (
    <div className="space-y-6 p-6">
      <PageHeader
        title={run?.name || "Eval run"}
        sub={run ? `${run.dataset_name} · run #${run.id}` : id}
        helpId="glossary.eval_run"
        right={
          <Link to="/evals" className="text-sm text-accent hover:underline">
            ← All evals
          </Link>
        }
      />

      {base.loading && <div className="text-sm text-fg-3">Loading…</div>}
      {base.error && <div className="text-sm text-fg-2">Run not found.</div>}

      {run && (
        <>
          {/* Summary cards */}
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
            <StatCard
              label="Pass rate"
              helpId="tile.obs.pass_rate"
              icon={<PercentIcon />}
              value={fmtPct(run.total > 0 ? run.passed / run.total : 0)}
              sub={`${run.passed}/${run.total} passed`}
              warn={run.total > 0 && run.passed / run.total < 0.5}
            />
            <StatCard
              label="Mean score"
              helpId="tile.obs.mean_score"
              icon={<BarChartIcon />}
              value={run.mean_score.toFixed(3)}
              sub="0–1, higher is better"
            />
            <StatCard
              label="Items"
              icon={<ListIcon />}
              value={fmtInt(run.total)}
              sub="scored"
            />
            <StatCard label="Status" value={run.status} />
          </div>
          <div className="text-xs text-fg-3">
            Scorers: <span className="font-mono text-fg-2">{run.scorers}</span> · started{" "}
            {fmtClock(run.started_at)}
            {run.ended_at && <> · ended {fmtClock(run.ended_at)}</>}
          </div>

          {/* Compare picker */}
          <div className="flex items-center gap-2 text-sm">
            <span className="text-fg-3">Compare with</span>
            <select
              className="rounded-2 border border-line-2 bg-bg-2 px-2 py-1 text-fg-1 focus:border-accent focus:outline-none"
              value={compareID}
              onChange={(e) => setCompareID(e.target.value)}
            >
              <option value="">— none —</option>
              {compareCandidates.map((r) => (
                <option key={r.id} value={String(r.id)}>
                  {r.name || `run #${r.id}`} ({fmtPct(r.total > 0 ? r.passed / r.total : 0)})
                </option>
              ))}
            </select>
            {compareCandidates.length === 0 && (
              <span className="text-xs text-fg-3">no other runs on this dataset</span>
            )}
          </div>

          {/* Scores table */}
          <ChartShell
            title={comparing ? "Per-item scores · base vs compare" : "Per-item scores"}
            sub={comparing ? "Δ flags regressions (red) and improvements (green)" : undefined}
            bodyClassName="overflow-x-auto"
          >
            <table className="w-full min-w-[640px] text-sm">
              <thead>
                <tr className="border-b border-line-2 text-left text-[10px] font-semibold uppercase tracking-[0.06em] text-fg-3">
                  <th className="px-3 py-2">Span</th>
                  <th className="px-3 py-2">Scorer</th>
                  <th className="px-3 py-2">{comparing ? "Base" : "Score"}</th>
                  {comparing && <th className="px-3 py-2">Compare</th>}
                  {comparing && <th className="px-3 py-2 text-right">Δ</th>}
                  {!comparing && <th className="px-3 py-2">Rationale</th>}
                </tr>
              </thead>
              <tbody>
                {baseScores.length === 0 && (
                  <tr>
                    <td className="px-3 py-6 text-center text-fg-3" colSpan={comparing ? 5 : 4}>
                      No scores recorded for this run.
                    </td>
                  </tr>
                )}
                {baseScores.map((s) => {
                  const other = comparing ? compareMap.get(scoreKey(s)) : undefined;
                  const delta = other ? s.score - other.score : 0;
                  return (
                    <tr
                      key={scoreKey(s)}
                      className="border-b border-line-2/60 align-top last:border-0"
                    >
                      <td className="px-3 py-2 font-mono text-xs text-fg-2">
                        {s.trace_id ? (
                          <Link
                            to={`/trajectories/${s.trace_id}`}
                            className="text-accent hover:underline"
                            title="Open trajectory"
                          >
                            {s.span_id.slice(0, 10) || "—"}
                          </Link>
                        ) : (
                          s.span_id.slice(0, 10) || "—"
                        )}
                      </td>
                      <td className="px-3 py-2 text-fg-2">{s.scorer}</td>
                      <td className="px-3 py-2">
                        <ScoreCell score={s.score} passed={s.passed} />
                      </td>
                      {comparing && (
                        <td className="px-3 py-2">
                          {other ? (
                            <ScoreCell score={other.score} passed={other.passed} />
                          ) : (
                            <span className="text-xs text-fg-3">—</span>
                          )}
                        </td>
                      )}
                      {comparing && (
                        <td className="px-3 py-2 text-right tabular-nums">
                          {other ? (
                            <span
                              className={
                                delta > 0
                                  ? "text-success"
                                  : delta < 0
                                    ? "text-danger"
                                    : "text-fg-3"
                              }
                            >
                              {delta > 0 ? "+" : ""}
                              {delta.toFixed(2)}
                            </span>
                          ) : (
                            <span className="text-xs text-fg-3">new</span>
                          )}
                        </td>
                      )}
                      {!comparing && (
                        <td className="px-3 py-2 text-xs text-fg-3">{s.rationale || "—"}</td>
                      )}
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </ChartShell>
        </>
      )}
    </div>
  );
}

function ScoreCell({ score, passed }: { score: number; passed: boolean }) {
  return (
    <span className="flex items-center gap-1.5">
      <Pill variant={passed ? "success" : "warn"}>{passed ? "pass" : "fail"}</Pill>
      <span className="font-mono text-xs text-fg-2">{score.toFixed(2)}</span>
    </span>
  );
}
