import { useMemo, useState } from "react";
import { Link, useParams } from "react-router-dom";
import type { ColumnDef } from "@tanstack/react-table";
import {
  api,
  type ObsEvalItemContentRow,
  type ObsEvalItemScoreRow,
  type ObsEvalRunListRow,
  type ObsEvalScorerDelta,
} from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { useFilters } from "@/lib/filters";
import { useDetailCrumb } from "@/lib/crumb";
import { dateTime, num, pct1 } from "@/lib/format";
import { Button, Card, Empty, ErrorState, PageHeader, Spinner } from "@/components/ui";
import { Pill, StatCard, StatStripSkeleton, TableSkeleton } from "@/components/primitives";
import { DataTable } from "@/components/DataTable";

// ObsEvalRunPage is the org per-item eval-run detail (obs-org-tier T7, gap-audit
// §1 / §2.2 / §6): one run's per-item scores, a run-vs-run per-scorer
// comparison, and — as a separate, server-audited lazy read — the item content
// excerpts. It complements the T4 aggregate Evals page (run-health trends);
// this is the drill-in that answers "which items regressed, and why". Admin-only.
export function ObsEvalRunPage() {
  const { runId = "" } = useParams();
  const ref = decodeURIComponent(runId);
  const { days } = useFilters();
  const { data, error, loading, reload } = useApi(() => api.obsEvalRun(ref, days), [ref, days]);

  const run = data?.run;
  useDetailCrumb(run?.run_name);
  return (
    <>
      <PageHeader
        title={run?.run_name || "Eval run"}
        subtitle={
          run
            ? `${run.dataset_name || "—"} · per-item scores shared by ${run.user_email || "a node"}. Content-free; item excerpts are a separate audited read.`
            : "Per-item eval-run detail — content-free scores, with an audited content read."
        }
        right={
          <Link to="/trajectories/evals" className="text-sm text-accent hover:underline">
            ← All evals
          </Link>
        }
      />
      {error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : loading || !data || !run ? (
        <div className="space-y-5">
          <StatStripSkeleton count={4} />
          <Card className="p-4">
            <TableSkeleton rows={8} />
          </Card>
        </div>
      ) : (
        <div className="space-y-5">
          <SummaryRow run={run} />
          <CompareCard run={run} days={days} />
          <ScoresCard runRef={ref} scores={data.scores} />
        </div>
      )}
    </>
  );
}

function SummaryRow({ run }: { run: ObsEvalRunListRow }) {
  return (
    <>
      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <StatCard
          label="Pass rate"
          value={pct1(run.pass_rate)}
          sub={`${num(run.passed)}/${num(run.scores)} scores`}
          warn={run.scores > 0 && run.pass_rate < 0.5}
          helpId="tile.obs.pass_rate"
        />
        <StatCard label="Mean score" value={run.mean_score.toFixed(3)} sub="0–1, higher is better" />
        <StatCard label="Items" value={num(run.items)} sub={`${num(run.scores)} scored`} />
        <StatCard label="Scorers" value={num(run.scorers.length)} sub={run.scorers.join(", ") || "—"} />
      </div>
      <div className="text-xs text-fg-3">
        started {dateTime(run.started_at)}
        {run.ended_at && <> · ended {dateTime(run.ended_at)}</>}
      </div>
    </>
  );
}

// --- run-vs-run comparison -------------------------------------------------

function CompareCard({ run, days }: { run: ObsEvalRunListRow; days: number }) {
  // Other runs of the SAME dataset are the compare candidates (a cell aligns
  // across two runs of one dataset). The bare run_id is node-local, so we match
  // on ref inequality + dataset_name.
  const { data: runsData } = useApi(() => api.obsEvalRuns(days), [days]);
  const candidates = useMemo(
    () => (runsData?.runs ?? []).filter((r) => r.dataset_name === run.dataset_name && r.ref !== run.ref),
    [runsData, run.dataset_name, run.ref],
  );

  const [compareRef, setCompareRef] = useState("");
  const compare = useApi(
    () => (compareRef ? api.obsEvalCompare(run.ref, compareRef) : Promise.resolve(null)),
    [run.ref, compareRef],
  );

  return (
    <Card className="p-3">
      <div className="mb-2 flex flex-wrap items-center gap-2 px-1 text-sm">
        <span className="font-medium text-fg-1">Compare runs</span>
        <span className="text-[11px] text-fg-3">per-scorer Base vs Compare vs Δ</span>
        <span className="ml-auto flex items-center gap-2">
          <span className="text-fg-3">Compare with</span>
          <select
            className="rounded-2 border border-line-2 bg-bg-2 px-2 py-1 text-fg-1 focus:border-accent focus:outline-none"
            value={compareRef}
            onChange={(e) => setCompareRef(e.target.value)}
          >
            <option value="">— none —</option>
            {candidates.map((r) => (
              <option key={r.ref} value={r.ref}>
                {r.run_name || r.ref} ({pct1(r.pass_rate)})
              </option>
            ))}
          </select>
        </span>
      </div>
      {candidates.length === 0 ? (
        <div className="px-1 pb-1 text-[12px] text-fg-3">No other runs on “{run.dataset_name || "—"}” in this window.</div>
      ) : compareRef === "" ? (
        <div className="px-1 pb-1 text-[12px] text-fg-3">Pick a run above to see per-scorer deltas.</div>
      ) : compare.loading ? (
        <Spinner label="Comparing…" />
      ) : compare.error ? (
        <div className="px-1 pb-1 text-[12px] text-danger">{compare.error}</div>
      ) : compare.data ? (
        <ScorerDeltaTable scorers={compare.data.scorers} />
      ) : null}
    </Card>
  );
}

function ScorerDeltaTable({ scorers }: { scorers: ObsEvalScorerDelta[] }) {
  if (scorers.length === 0) return <Empty message="No shared scorers between these runs." />;
  return (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[560px] text-sm">
        <thead>
          <tr className="border-b border-line-2 text-left text-[10px] font-semibold uppercase tracking-[0.06em] text-fg-3">
            <th className="px-3 py-2">Scorer</th>
            <th className="px-3 py-2 text-right">Base pass</th>
            <th className="px-3 py-2 text-right">Compare pass</th>
            <th className="px-3 py-2 text-right">Δ pass</th>
            <th className="px-3 py-2 text-right">Base mean</th>
            <th className="px-3 py-2 text-right">Compare mean</th>
            <th className="px-3 py-2 text-right">Δ mean</th>
          </tr>
        </thead>
        <tbody>
          {scorers.map((s) => (
            <tr key={s.scorer} className="border-b border-line-2/60 last:border-0">
              <td className="px-3 py-2 font-mono text-xs text-fg-2">{s.scorer}</td>
              <td className="px-3 py-2 text-right tabular-nums text-fg-2">{s.base_count > 0 ? pct1(s.base_pass_rate) : "—"}</td>
              <td className="px-3 py-2 text-right tabular-nums text-fg-2">{s.compare_count > 0 ? pct1(s.compare_pass_rate) : "—"}</td>
              <td className="px-3 py-2 text-right tabular-nums">
                <Delta v={s.pass_rate_delta} fmt={(x) => `${x > 0 ? "+" : ""}${pct1(x)}`} both={s.base_count > 0 && s.compare_count > 0} />
              </td>
              <td className="px-3 py-2 text-right tabular-nums text-fg-2">{s.base_count > 0 ? s.base_mean.toFixed(2) : "—"}</td>
              <td className="px-3 py-2 text-right tabular-nums text-fg-2">{s.compare_count > 0 ? s.compare_mean.toFixed(2) : "—"}</td>
              <td className="px-3 py-2 text-right tabular-nums">
                <Delta v={s.mean_delta} fmt={(x) => `${x > 0 ? "+" : ""}${x.toFixed(2)}`} both={s.base_count > 0 && s.compare_count > 0} />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function Delta({ v, fmt, both }: { v: number; fmt: (x: number) => string; both: boolean }) {
  if (!both) return <span className="text-[11px] text-fg-3">{v === 0 ? "new" : "—"}</span>;
  return <span className={v > 0 ? "text-success" : v < 0 ? "text-danger" : "text-fg-3"}>{fmt(v)}</span>;
}

// --- per-item scores + audited content -------------------------------------

function ScoresCard({ runRef, scores }: { runRef: string; scores: ObsEvalItemScoreRow[] }) {
  const [contentOpen, setContentOpen] = useState(false);
  const [content, setContent] = useState<ObsEvalItemContentRow[] | null>(null);
  const [contentLoading, setContentLoading] = useState(false);
  const [contentError, setContentError] = useState<string | null>(null);

  const loadContent = () => {
    setContentOpen(true);
    if (content !== null || contentLoading) return; // load once
    setContentLoading(true);
    setContentError(null);
    api
      .obsEvalRunContent(runRef)
      .then((r) => setContent(r.items))
      .catch((e: unknown) => setContentError(String(e)))
      .finally(() => setContentLoading(false));
  };

  const columns = useMemo<ColumnDef<ObsEvalItemScoreRow, any>[]>(
    () => [
      {
        accessorKey: "span_id",
        header: "Item",
        cell: (c) =>
          c.row.original.trace_id ? (
            <Link
              to={`/trajectories/${encodeURIComponent(c.row.original.trace_id)}`}
              className="font-mono text-xs text-accent hover:underline"
              title="Open trajectory"
            >
              {c.row.original.span_id.slice(0, 10) || `#${c.row.original.item_id}`}
            </Link>
          ) : (
            <span className="font-mono text-xs text-fg-2">{c.row.original.span_id.slice(0, 10) || `#${c.row.original.item_id}`}</span>
          ),
      },
      { accessorKey: "scorer", header: "Scorer", cell: (c) => <span className="text-fg-2">{c.row.original.scorer}</span> },
      {
        accessorKey: "passed",
        header: "Verdict",
        cell: (c) => <Pill variant={c.row.original.passed ? "success" : "warn"}>{c.row.original.passed ? "pass" : "fail"}</Pill>,
      },
      {
        accessorKey: "score",
        header: "Score",
        cell: (c) => c.row.original.score.toFixed(2),
        meta: { align: "right", mono: true },
      },
      {
        accessorKey: "duration_ms",
        header: "Duration",
        cell: (c) => (c.row.original.duration_ms > 0 ? `${num(c.row.original.duration_ms)}ms` : "—"),
        meta: { align: "right", mono: true },
      },
    ],
    [],
  );

  return (
    <Card className="p-3">
      <div className="mb-2 flex items-center justify-between gap-3 px-1">
        <div className="text-sm font-medium text-fg-1">
          Per-item scores
          <span className="ml-2 text-[11px] font-normal text-fg-3">content-free · {num(scores.length)} rows</span>
        </div>
        <Button onClick={loadContent} disabled={contentOpen && contentLoading}>
          {contentOpen ? "Content loaded" : "View item content (audited)"}
        </Button>
      </div>
      {scores.length === 0 ? (
        <Empty message="No per-item scores recorded for this run." />
      ) : (
        <DataTable
          data={scores}
          columns={columns}
          rowKey={(r) => `${r.item_id}:${r.span_id}:${r.scorer}`}
          initialSort={[{ id: "score", desc: false }]}
          zebra
          minWidth={640}
        />
      )}
      {contentOpen && <ContentPanel loading={contentLoading} error={contentError} items={content} />}
    </Card>
  );
}

function ContentPanel({
  loading,
  error,
  items,
}: {
  loading: boolean;
  error: string | null;
  items: ObsEvalItemContentRow[] | null;
}) {
  return (
    <div className="mt-3 space-y-3 rounded-2 border border-line-2 bg-bg-1 p-3">
      <div className="rounded-2 border border-warn/30 bg-warn-soft px-3 py-2 text-[12px] text-warn">
        Item content is a deeper disclosure — this read is <b>audited server-side</b> (a row is written
        before the excerpts are returned). Excerpts arrive only from nodes that opted into{" "}
        <span className="font-mono">[org_client.share.obs].eval_items</span> +{" "}
        <span className="font-mono">full_content</span>; hash-only nodes contribute nothing here.
      </div>
      {loading ? (
        <Spinner label="Loading item content…" />
      ) : error ? (
        <div className="text-[12px] text-danger">{error}</div>
      ) : !items || items.length === 0 ? (
        <Empty message="No item content shared for this run — nodes shipped score metadata only." />
      ) : (
        <ul className="space-y-2">
          {items.map((it, i) => (
            <li key={`${it.item_id}:${it.scorer}:${i}`} className="rounded-2 border border-line-2 bg-bg-2 p-2.5">
              <div className="mb-1 flex items-center gap-2 text-[11px] text-fg-3">
                <span className="font-mono text-fg-2">{it.scorer}</span>
                <span className="font-mono text-fg-4">{it.span_id.slice(0, 12) || `#${it.item_id}`}</span>
                <span>{dateTime(it.ts)}</span>
              </div>
              <Excerpt label="Input" text={it.input_excerpt} />
              <Excerpt label="Expected" text={it.expected_excerpt} />
              <Excerpt label="Output" text={it.output_excerpt} />
              <Excerpt label="Rationale" text={it.rationale} />
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function Excerpt({ label, text }: { label: string; text: string }) {
  if (!text) return null;
  return (
    <div className="mt-1.5">
      <div className="text-[10px] font-semibold uppercase tracking-[0.06em] text-fg-3">{label}</div>
      <pre className="mt-0.5 max-h-48 overflow-auto whitespace-pre-wrap break-words rounded-2 border border-line-2 bg-bg-3 p-2 font-mono text-[11.5px] text-fg-1">
        {text}
      </pre>
    </div>
  );
}
