import { useMemo, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { api, type ObsContentEntry, type ObsSpanDetail, type ObsTraceDetailResult } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { useDetailCrumb } from "@/lib/crumb";
import { compact, ms, usd } from "@/lib/format";
import { Card, ErrorState, PageHeader } from "@/components/ui";
import { ChartShell, Pill, StatCard, StatStripSkeleton } from "@/components/primitives";

// Org trace detail (obs-org-tier T2): the span tree + per-span detail with the
// proxy-exact WEDGE (cost/cache joined from api_turns by request_id) — exact
// cost on an org-scale agent span, which no pure OTel backend can show.
export function ObsTraceDetailPage() {
  const { id = "" } = useParams();
  const { data, error, loading, reload } = useApi(() => api.obsTrace(id), [id]);
  const [sel, setSel] = useState<string | null>(null);
  useDetailCrumb(data?.trace.root_name);

  return (
    <>
      <div className="mb-4 flex items-center justify-between">
        <PageHeader title={data?.trace.root_name || "Trace"} subtitle={id} />
        <Link to="/trajectories" className="text-sm text-accent hover:underline">
          ← All trajectories
        </Link>
      </div>
      {error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : loading || !data ? (
        <StatStripSkeleton count={4} />
      ) : (
        <Detail data={data} sel={sel} setSel={setSel} />
      )}
    </>
  );
}

function Detail({
  data,
  sel,
  setSel,
}: {
  data: ObsTraceDetailResult;
  sel: string | null;
  setSel: (id: string) => void;
}) {
  const { id = "" } = useParams();
  const roots = useMemo(() => {
    const ids = new Set(data.spans.map((s) => s.span_id));
    return data.spans.filter((s) => !s.parent_span_id || !ids.has(s.parent_span_id));
  }, [data.spans]);
  // Flatten the span tree into a DFS-ordered list (parent immediately above its
  // children) and derive the shared time window for the gantt timeline.
  const timeline = useMemo(() => buildTimeline(data.spans, roots), [data.spans, roots]);
  const selected = data.spans.find((s) => s.span_id === sel);

  // Audited content (T3): lazy-loaded on explicit request (writes a
  // view_span_content audit row server-side). Mapped span_id → entries.
  const [content, setContent] = useState<Record<string, ObsContentEntry[]> | null>(null);
  const [contentErr, setContentErr] = useState<string | null>(null);
  const [loadingContent, setLoadingContent] = useState(false);
  async function loadContent() {
    setLoadingContent(true);
    setContentErr(null);
    try {
      const res = await api.obsTraceContent(id);
      const by: Record<string, ObsContentEntry[]> = {};
      for (const e of res.entries) (by[e.span_id] ??= []).push(e);
      setContent(by);
    } catch (e) {
      setContentErr(e instanceof Error ? e.message : String(e));
    } finally {
      setLoadingContent(false);
    }
  }

  return (
    <div className="space-y-5">
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <StatCard label="Spans" value={String(data.trace.span_count)} sub={data.trace.source} helpId="tile.obs.spans" />
        <StatCard label="Tokens" value={compact(data.trace.total_tokens)} sub="trace total" helpId="tile.obs.tokens" />
        <StatCard label="Cost" value={usd(data.trace.cost_usd)} accent sub="obs-local" helpId="tile.obs.cost" />
        <StatCard label="Duration" value={ms(data.trace.duration_ms)} sub={data.trace.status} helpId="tile.obs.duration" />
      </div>

      <ChartShell
        title="Timeline"
        sub={
          timeline.hasAxis
            ? `spans over ${ms(timeline.span)} · click a bar to inspect`
            : "positioned by duration — span timestamps unavailable"
        }
      >
        <Timeline timeline={timeline} sel={sel} setSel={setSel} />
      </ChartShell>

      <div className="grid gap-5 lg:grid-cols-2">
        <ChartShell title="Span tree">
          <div className="px-1 py-1">
            {roots.map((r) => (
              <SpanNode key={r.span_id} span={r} all={data.spans} depth={0} sel={sel} setSel={setSel} />
            ))}
          </div>
        </ChartShell>

        <Card className="p-4">
          <div className="mb-3 flex items-center justify-between">
            <span className="text-sm font-medium text-fg-1">Span detail</span>
            {!content && (
              <button
                onClick={loadContent}
                disabled={loadingContent}
                className="rounded border border-line-2 bg-bg-2 px-2 py-1 text-[11px] text-fg-2 hover:bg-bg-3 disabled:opacity-50"
                title="Reads the captured prompt/response/tool-io bodies — writes an audit record"
              >
                {loadingContent ? "Loading…" : "View content (audited)"}
              </button>
            )}
          </div>
          {contentErr && <p className="mb-2 text-[12px] text-danger">{contentErr}</p>}
          {!selected ? (
            <p className="text-[13px] text-fg-3">Select a span.</p>
          ) : (
            <SpanDetailPanel span={selected} content={content?.[selected.span_id]} />
          )}
        </Card>
      </div>
    </div>
  );
}

function SpanNode({
  span,
  all,
  depth,
  sel,
  setSel,
}: {
  span: ObsSpanDetail;
  all: ObsSpanDetail[];
  depth: number;
  sel: string | null;
  setSel: (id: string) => void;
}) {
  const children = all.filter((s) => s.parent_span_id === span.span_id);
  const active = sel === span.span_id;
  return (
    <div>
      <button
        onClick={() => setSel(span.span_id)}
        className={`flex w-full items-center gap-2 rounded px-2 py-1 text-left text-[13px] hover:bg-bg-2 ${
          active ? "bg-bg-2" : ""
        }`}
        style={{ paddingLeft: `${depth * 16 + 8}px` }}
      >
        <KindPill kind={span.kind} />
        <span className="flex-1 truncate text-fg-1">{span.name || "(unnamed)"}</span>
        {span.cost_usd > 0 && <span className="font-mono text-[11px] text-fg-3">{usd(span.cost_usd)}</span>}
        <span className="font-mono text-[11px] text-fg-3">{ms(span.duration_ms)}</span>
      </button>
      {children.map((c) => (
        <SpanNode key={c.span_id} span={c} all={all} depth={depth + 1} sel={sel} setSel={setSel} />
      ))}
    </div>
  );
}

function KindPill({ kind }: { kind: string }) {
  const variant =
    kind === "llm" ? "accent" : kind === "tool" ? "info" : kind === "agent" ? "success" : "neutral";
  return (
    <Pill variant={variant as any} className="font-mono text-[10px] uppercase">
      {kind}
    </Pill>
  );
}

function SpanDetailPanel({ span, content }: { span: ObsSpanDetail; content?: ObsContentEntry[] }) {
  const e = span.enrichment;
  return (
    <div className="space-y-3 text-[13px]">
      <DetailRow label="Status" value={span.status || "unset"} />
      <DetailRow label="Duration" value={ms(span.duration_ms)} />
      {span.model && <DetailRow label="Model" value={span.model} mono />}
      {span.provider && <DetailRow label="Provider" value={span.provider} />}
      {span.request_id && <DetailRow label="request_id" value={span.request_id} mono />}

      {content && content.length > 0 && (
        <div className="space-y-2">
          {content.map((c, i) => (
            <div key={i} className="rounded-md border border-line-2 bg-bg-2 p-2">
              <div className="mb-1 text-[11px] uppercase tracking-wide text-fg-3">{contentLabel(c.kind)}</div>
              {c.has_raw ? (
                <pre className="max-h-48 overflow-auto whitespace-pre-wrap break-words font-mono text-[11px] leading-relaxed text-fg-1">
                  {c.content}
                </pre>
              ) : (
                <p className="text-[11px] text-fg-3">raw content disabled by the node — body hashed only</p>
              )}
            </div>
          ))}
        </div>
      )}

      {e?.found ? (
        // The proxy-exact wedge: authoritative over span-reported numbers.
        <div className="rounded-md border border-success/30 bg-success-soft/40 p-3">
          <div className="mb-2 flex items-center gap-2">
            <Pill variant="success">Proxy-verified</Pill>
            <span className="text-[11px] text-fg-3">exact cost &amp; cache</span>
          </div>
          <DetailRow label="Cost (proxy)" value={usd(e.cost_usd)} mono />
          <DetailRow label="Tokens (in / out)" value={`${num0(e.input_tokens)} / ${num0(e.output_tokens)}`} />
          <DetailRow label="Cache (read / write)" value={`${num0(e.cache_read_tokens)} / ${num0(e.cache_creation_tokens)}`} />
        </div>
      ) : (
        // Span-reported (no matching proxy turn).
        (span.input_tokens > 0 || span.output_tokens > 0 || span.cost_usd > 0) && (
          <div className="rounded-md border border-line-2 bg-bg-2 p-3">
            <div className="mb-2 text-[11px] uppercase tracking-wide text-fg-3">
              Span-reported{span.cost_source ? ` · ${span.cost_source}` : ""}
            </div>
            <DetailRow label="Cost" value={usd(span.cost_usd)} mono />
            <DetailRow label="Tokens (in / out)" value={`${num0(span.input_tokens)} / ${num0(span.output_tokens)}`} />
            {span.cache_read_tokens > 0 && <DetailRow label="Cache read" value={num0(span.cache_read_tokens)} />}
            {span.reasoning_tokens > 0 && <DetailRow label="Reasoning" value={num0(span.reasoning_tokens)} />}
          </div>
        )
      )}
    </div>
  );
}

// ---- Gantt timeline ---------------------------------------------------------

type TimelineRow = { span: ObsSpanDetail; depth: number };
type TimelineModel = {
  rows: TimelineRow[];
  lo: number;
  span: number;
  hasAxis: boolean;
  maxDur: number;
};

// epochMs parses an ISO timestamp to epoch millis, NaN when absent/unparseable.
function epochMs(iso: string): number {
  if (!iso) return NaN;
  const t = new Date(iso).getTime();
  return Number.isNaN(t) ? NaN : t;
}

// buildTimeline flattens the span tree into DFS order (a parent immediately
// above its children) and computes the absolute time window every bar shares.
// When no span carries a parseable start it falls back to a duration-
// proportional axis (hasAxis=false) so the gantt still renders honestly.
function buildTimeline(spans: ObsSpanDetail[], roots: ObsSpanDetail[]): TimelineModel {
  const childrenOf = new Map<string, ObsSpanDetail[]>();
  for (const s of spans) {
    if (!s.parent_span_id) continue;
    const arr = childrenOf.get(s.parent_span_id) ?? [];
    arr.push(s);
    childrenOf.set(s.parent_span_id, arr);
  }
  const rows: TimelineRow[] = [];
  const seen = new Set<string>();
  const walk = (s: ObsSpanDetail, depth: number) => {
    if (seen.has(s.span_id)) return; // guard against cyclic parent refs
    seen.add(s.span_id);
    rows.push({ span: s, depth });
    for (const c of childrenOf.get(s.span_id) ?? []) walk(c, depth + 1);
  };
  for (const r of roots) walk(r, 0);
  // Any span the DFS didn't reach (dangling parent ref) still gets a row.
  for (const s of spans) if (!seen.has(s.span_id)) rows.push({ span: s, depth: 0 });

  let lo = Infinity;
  let hi = -Infinity;
  let maxDur = 0;
  for (const s of spans) {
    const a = epochMs(s.started_at);
    if (!Number.isNaN(a)) {
      const eb = epochMs(s.ended_at);
      const b = Number.isNaN(eb) ? a + Math.max(s.duration_ms, 0) : eb;
      lo = Math.min(lo, a);
      hi = Math.max(hi, b);
    }
    if (s.duration_ms > maxDur) maxDur = s.duration_ms;
  }
  const hasAxis = Number.isFinite(lo) && Number.isFinite(hi) && hi > lo;
  return { rows, lo, span: hasAxis ? hi - lo : Math.max(maxDur, 1), hasAxis, maxDur };
}

// barGeom maps one span onto the shared axis. On the time axis the bar is
// absolute (start offset + measured width); a span with no parseable start on
// an otherwise-timed trace renders faint full-width to signal "position
// unknown". Without any time axis the bar is duration-proportional from the
// left edge.
function barGeom(s: ObsSpanDetail, t: TimelineModel): { left: number; width: number; faint: boolean } {
  if (t.hasAxis) {
    const a = epochMs(s.started_at);
    if (!Number.isNaN(a)) {
      const eb = epochMs(s.ended_at);
      const b = Number.isNaN(eb) ? a + Math.max(s.duration_ms, 0) : eb;
      const left = Math.min(Math.max(((a - t.lo) / t.span) * 100, 0), 100);
      const width = Math.min(Math.max(((b - a) / t.span) * 100, 0.5), 100 - left);
      return { left, width, faint: false };
    }
    return { left: 0, width: 100, faint: true };
  }
  const width = t.maxDur > 0 ? Math.max((s.duration_ms / t.maxDur) * 100, 0.5) : 100;
  return { left: 0, width, faint: false };
}

function Timeline({
  timeline,
  sel,
  setSel,
}: {
  timeline: TimelineModel;
  sel: string | null;
  setSel: (id: string) => void;
}) {
  if (timeline.rows.length === 0) {
    return <p className="text-[13px] text-fg-3">No spans.</p>;
  }
  return (
    <div className="space-y-0.5">
      {timeline.rows.map(({ span, depth }) => {
        const geom = barGeom(span, timeline);
        const active = sel === span.span_id;
        return (
          <button
            key={span.span_id}
            onClick={() => setSel(span.span_id)}
            className={`flex w-full items-center gap-2 rounded px-1 py-0.5 text-left hover:bg-bg-3 ${
              active ? "bg-bg-3" : ""
            }`}
          >
            <span
              className="w-36 shrink-0 truncate text-[11px] text-fg-2 sm:w-44"
              style={{ paddingLeft: `${depth * 12}px` }}
              title={span.name || span.kind}
            >
              {span.name || span.kind || "(unnamed)"}
            </span>
            <span className="relative h-3 flex-1 rounded bg-fg-3/15">
              <span
                className={`absolute h-3 rounded ${
                  span.status === "error" ? "bg-danger/70" : "bg-accent/70"
                } ${active ? "ring-1 ring-accent" : ""} ${geom.faint ? "opacity-40" : ""}`}
                style={{ left: `${geom.left}%`, width: `${geom.width}%` }}
                title={geom.faint ? "position unknown — no span start timestamp" : undefined}
              />
            </span>
            <span className="w-14 shrink-0 text-right font-mono text-[11px] text-fg-3">{ms(span.duration_ms)}</span>
          </button>
        );
      })}
    </div>
  );
}

function DetailRow({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="flex items-center justify-between gap-3">
      <span className="text-fg-3">{label}</span>
      <span className={mono ? "font-mono text-[12px] text-fg-1" : "text-fg-1"}>{value}</span>
    </div>
  );
}

function num0(n: number): string {
  return compact(n);
}

function contentLabel(kind: string): string {
  if (kind === "prompt") return "Prompt";
  if (kind === "response") return "Response";
  if (kind === "tool_io") return "Tool I/O";
  return kind || "Content";
}
