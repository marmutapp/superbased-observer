import { useMemo, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { api, type ObsContentEntry, type ObsSpanDetail, type ObsTraceDetailResult } from "@/lib/api";
import { useApi } from "@/lib/useApi";
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
