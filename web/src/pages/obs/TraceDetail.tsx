import { useMemo, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { ChartShell, PageHeader, Pill, StatCard } from "@/components/primitives";
import { HelpInd } from "@/components/HelpInd";
import {
  ClockIcon,
  CoinsIcon,
  DatabaseIcon,
  LayersIcon,
} from "@/components/icons";
import { useApi } from "@/lib/useApi";
import { fmtDuration, fmtInt, fmtUSD } from "@/lib/format";
import type { ObsSpanRow, ObsTraceDetail } from "./types";

const ms = (iso: string): number => {
  const t = new Date(iso).getTime();
  return Number.isNaN(t) ? 0 : t;
};

// TraceDetailPage renders one trace as a coordinated tree + timeline + a
// selected-span drawer over a single /api/obs/trace/{id} payload. No graph
// library — the tree is a bespoke recursive component (the ProcessesSection
// precedent), the timeline is proportional flex bars. Panels render through
// the platform ChartShell so the surface matches every other detail page.
export function TraceDetailPage() {
  const { id = "" } = useParams();
  const { data, loading, error } = useApi<ObsTraceDetail>(id ? `/api/obs/trace/${id}` : null);
  const [selected, setSelected] = useState<string | null>(null);

  const childrenOf = useMemo(() => {
    const m = new Map<string, ObsSpanRow[]>();
    const ids = new Set((data?.spans ?? []).map((s) => s.span_id));
    for (const s of data?.spans ?? []) {
      const parent = s.parent_span_id && ids.has(s.parent_span_id) ? s.parent_span_id : "";
      const arr = m.get(parent) ?? [];
      arr.push(s);
      m.set(parent, arr);
    }
    return m;
  }, [data]);

  const window = useMemo(() => {
    const spans = data?.spans ?? [];
    let lo = Infinity;
    let hi = -Infinity;
    for (const s of spans) {
      const a = ms(s.started_at);
      const b = s.ended_at ? ms(s.ended_at) : a;
      if (a > 0) lo = Math.min(lo, a);
      if (b > 0) hi = Math.max(hi, b);
    }
    return { lo, span: hi > lo ? hi - lo : 1 };
  }, [data]);

  const selectedSpan = (data?.spans ?? []).find((s) => s.span_id === selected) ?? null;
  const selEvents = (data?.events ?? []).filter((e) => e.span_id === selected);
  const selLinks = (data?.links ?? []).filter((l) => l.span_id === selected);
  const selContent = (data?.content ?? []).filter((c) => c.span_id === selected);
  const selEnrich = selected ? data?.enrichments?.[selected] : undefined;

  return (
    <div className="space-y-6 p-6">
      <PageHeader
        title={data?.trace.root_name || "Trace"}
        sub={id}
        helpId="glossary.trajectory"
        right={
          <Link to="/trajectories" className="text-sm text-accent hover:underline">
            ← All trajectories
          </Link>
        }
      />

      {loading && <div className="text-sm text-fg-3">Loading…</div>}
      {error && <div className="text-sm text-fg-2">Trace not found.</div>}

      {data && (
        <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
          <StatCard
            label="Spans"
            helpId="tile.obs.spans"
            icon={<LayersIcon />}
            value={fmtInt(data.trace.span_count)}
          />
          <StatCard
            label="Tokens"
            helpId="tile.obs.tokens"
            icon={<DatabaseIcon />}
            value={data.trace.total_tokens > 0 ? fmtInt(data.trace.total_tokens) : "—"}
            sub={traceTokenSub(data.trace)}
          />
          <StatCard
            label="Cost"
            helpId="tile.obs.cost"
            icon={<CoinsIcon />}
            value={data.trace.cost_usd > 0 ? fmtUSD(data.trace.cost_usd, true) : "—"}
            accent
          />
          <StatCard
            label="Duration"
            icon={<ClockIcon />}
            value={fmtDuration(data.trace.duration_ms)}
          />
        </div>
      )}

      {data && (
        <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
          {/* Tree + timeline */}
          <div className="space-y-4 lg:col-span-2">
            <ChartShell
              title={
                <span className="flex items-center gap-2">
                  Span tree
                  <HelpInd id="glossary.span" />
                </span>
              }
            >
              <SpanTree
                parentId=""
                depth={0}
                childrenOf={childrenOf}
                selected={selected}
                onSelect={setSelected}
              />
            </ChartShell>
            <ChartShell title="Timeline">
              <div className="space-y-1">
                {(data.spans ?? []).map((s) => {
                  const start = ms(s.started_at);
                  const end = s.ended_at ? ms(s.ended_at) : start;
                  const left = window.span ? ((start - window.lo) / window.span) * 100 : 0;
                  const width = window.span ? Math.max(((end - start) / window.span) * 100, 1) : 1;
                  return (
                    <div
                      key={s.span_id}
                      className="group flex cursor-pointer items-center gap-2"
                      onClick={() => setSelected(s.span_id)}
                    >
                      <div className="w-40 shrink-0 truncate text-xs text-fg-2" title={s.name}>
                        {s.name || s.kind}
                      </div>
                      <div className="relative h-3 flex-1 rounded bg-bg-3">
                        <div
                          className={`absolute h-3 rounded ${
                            s.status === "error" ? "bg-danger/70" : "bg-accent/70"
                          } ${selected === s.span_id ? "ring-1 ring-accent" : ""}`}
                          style={{ left: `${left}%`, width: `${width}%` }}
                        />
                      </div>
                      <div className="w-16 shrink-0 text-right text-xs text-fg-3">
                        {fmtDuration(s.duration_ms)}
                      </div>
                    </div>
                  );
                })}
              </div>
            </ChartShell>
          </div>

          {/* Selected-span drawer */}
          <ChartShell title="Span detail">
            {!selectedSpan && <div className="text-sm text-fg-3">Select a span.</div>}
            {selectedSpan && (
              <div className="space-y-3 text-sm">
                <div className="flex items-center gap-2">
                  <Pill variant="neutral">{selectedSpan.kind}</Pill>
                  <span className="font-medium text-fg-1">{selectedSpan.name}</span>
                </div>
                <DetailRow label="Status" value={selectedSpan.status} />
                <DetailRow label="Duration" value={fmtDuration(selectedSpan.duration_ms)} />
                {selectedSpan.model && <DetailRow label="Model" value={selectedSpan.model} />}
                {selectedSpan.provider && <DetailRow label="Provider" value={selectedSpan.provider} />}
                {/* Span-reported tokens/cost are suppressed when the proxy saw
                    this turn — the Proxy-verified block below is authoritative
                    (Gap E: prefer proxy-exact, fall back to span-reported).
                    Reasoning stays: the proxy block doesn't surface it. */}
                {!selEnrich?.found && selectedSpan.input_tokens != null && (
                  <DetailRow label="Input tokens" value={fmtInt(selectedSpan.input_tokens)} />
                )}
                {!selEnrich?.found && selectedSpan.output_tokens != null && (
                  <DetailRow label="Output tokens" value={fmtInt(selectedSpan.output_tokens)} />
                )}
                {!selEnrich?.found && selectedSpan.cache_read_tokens != null && (
                  <DetailRow label="Cache read" value={fmtInt(selectedSpan.cache_read_tokens)} />
                )}
                {!selEnrich?.found && selectedSpan.cache_write_tokens != null && (
                  <DetailRow label="Cache write" value={fmtInt(selectedSpan.cache_write_tokens)} />
                )}
                {selectedSpan.reasoning_tokens != null && (
                  <DetailRow label="Reasoning" value={fmtInt(selectedSpan.reasoning_tokens)} />
                )}
                {!selEnrich?.found && selectedSpan.cost_usd != null && (
                  <DetailRow
                    label={selectedSpan.cost_source === "computed" ? "Cost (estimated)" : "Cost"}
                    value={fmtUSD(selectedSpan.cost_usd, true)}
                  />
                )}
                {!selEnrich?.found && selectedSpan.cost_detail && (
                  <div className="ml-2 space-y-0.5 border-l border-line-2 pl-2">
                    {costComponentRows(selectedSpan.cost_detail)}
                  </div>
                )}
                {selectedSpan.request_id && (
                  <DetailRow label="request_id" value={selectedSpan.request_id} mono />
                )}
                {selEnrich?.found && (
                  <div className="rounded-2 border border-accent/40 bg-accent-soft p-2">
                    <div className="mb-1 flex items-center gap-2">
                      <Pill variant="success">Proxy-verified</Pill>
                      <span className="text-xs text-fg-3">exact cost &amp; cache</span>
                    </div>
                    <div className="space-y-1">
                      <DetailRow label="Cost (proxy)" value={fmtUSD(selEnrich.cost_usd, true)} />
                      <DetailRow
                        label="Tokens (in / out)"
                        value={`${fmtInt(selEnrich.input_tokens)} / ${fmtInt(selEnrich.output_tokens)}`}
                      />
                      {(selEnrich.cache_read_tokens > 0 || selEnrich.cache_creation_tokens > 0) && (
                        <DetailRow
                          label="Cache (read / write)"
                          value={`${fmtInt(selEnrich.cache_read_tokens)} / ${fmtInt(
                            selEnrich.cache_creation_tokens,
                          )}`}
                        />
                      )}
                      {selEnrich.routing_reason && (
                        <DetailRow label="Routing" value={selEnrich.routing_reason} />
                      )}
                      {selEnrich.guard_verdict && (
                        <DetailRow label="Guard" value={selEnrich.guard_verdict} />
                      )}
                    </div>
                  </div>
                )}
                {selEvents.length > 0 && (
                  <div>
                    <div className="mb-1 text-xs uppercase tracking-wide text-fg-3">Events</div>
                    {selEvents.map((e, i) => (
                      <div key={i} className="text-xs text-fg-2">
                        <span className="text-fg-1">{e.name}</span>
                        {e.attributes && <span className="text-fg-3"> {e.attributes}</span>}
                      </div>
                    ))}
                  </div>
                )}
                {selLinks.length > 0 && (
                  <div>
                    <div className="mb-1 text-xs uppercase tracking-wide text-fg-3">Links</div>
                    {selLinks.map((l, i) => (
                      <Link
                        key={i}
                        to={`/trajectories/${l.linked_trace}`}
                        className="block text-xs text-accent hover:underline"
                      >
                        ↗ {l.linked_trace.slice(0, 12)}
                      </Link>
                    ))}
                  </div>
                )}
                {selContent.length > 0 && (
                  <div>
                    <div className="mb-1 text-xs uppercase tracking-wide text-fg-3">
                      {selectedSpan.kind === "tool" ? "Tool I/O" : "Messages"}
                    </div>
                    <div className="space-y-2">
                      {selContent.map((c, i) => (
                        <div key={i} className="rounded-2 border border-line-2 bg-bg-1 p-2">
                          <Pill variant="neutral">{contentLabel(c.kind)}</Pill>
                          {c.content ? (
                            <pre className="mt-1 max-h-64 overflow-auto whitespace-pre-wrap break-words text-xs text-fg-2">
                              {c.content}
                            </pre>
                          ) : (
                            <div className="mt-1 text-xs text-fg-3">
                              Raw content disabled — body hashed only.{" "}
                              <span className="font-mono">{c.content_hash.slice(0, 12)}</span>
                            </div>
                          )}
                        </div>
                      ))}
                    </div>
                  </div>
                )}
              </div>
            )}
          </ChartShell>
        </div>
      )}
    </div>
  );
}

function SpanTree({
  parentId,
  depth,
  childrenOf,
  selected,
  onSelect,
}: {
  parentId: string;
  depth: number;
  childrenOf: Map<string, ObsSpanRow[]>;
  selected: string | null;
  onSelect: (id: string) => void;
}) {
  const kids = childrenOf.get(parentId) ?? [];
  return (
    <>
      {kids.map((s) => (
        <div key={s.span_id}>
          <div
            className={`flex cursor-pointer items-center gap-2 rounded px-1 py-1 hover:bg-bg-3 ${
              selected === s.span_id ? "bg-bg-3" : ""
            }`}
            style={{ paddingLeft: depth * 16 + 4 }}
            onClick={() => onSelect(s.span_id)}
          >
            <Pill variant={s.status === "error" ? "danger" : "neutral"}>{s.kind}</Pill>
            <span className="truncate text-sm text-fg-1">{s.name || s.span_id.slice(0, 8)}</span>
            <span className="ml-auto shrink-0 text-xs text-fg-3">{fmtDuration(s.duration_ms)}</span>
            {s.cost_usd != null && s.cost_usd > 0 && (
              <span className="shrink-0 text-xs text-fg-3">{fmtUSD(s.cost_usd, true)}</span>
            )}
          </div>
          <SpanTree
            parentId={s.span_id}
            depth={depth + 1}
            childrenOf={childrenOf}
            selected={selected}
            onSelect={onSelect}
          />
        </div>
      ))}
    </>
  );
}

// traceTokenSub builds the "in / out (+cache, +reasoning)" sub-line for the
// trace token stat, surfacing cache/reasoning above the span drawer (Gap C).
function traceTokenSub(t: ObsTraceDetail["trace"]): string {
  if (t.total_tokens <= 0) return "";
  const parts = [`${fmtInt(t.input_tokens)} in / ${fmtInt(t.output_tokens)} out`];
  if (t.cache_read_tokens > 0) parts.push(`${fmtInt(t.cache_read_tokens)} cache`);
  if (t.reasoning_tokens > 0) parts.push(`${fmtInt(t.reasoning_tokens)} reasoning`);
  return parts.join(" · ");
}

// costComponentRows renders the present per-component cost lines (USD) under the
// total — only the components the instrumentor reported or the engine charged.
function costComponentRows(d: import("./types").ObsCostBreakdown) {
  const rows: Array<[string, number | undefined]> = [
    ["Input", d.input],
    ["Output", d.output],
    ["Cache read", d.cache_read],
    ["Cache write", d.cache_write],
    ["Reasoning", d.reasoning],
    ["Tool", d.tool],
  ];
  return rows
    .filter(([, v]) => v != null)
    .map(([label, v]) => (
      <div key={label} className="flex justify-between gap-3 text-xs text-fg-3">
        <span>{label}</span>
        <span className="text-fg-2">{fmtUSD(v as number, true)}</span>
      </div>
    ));
}

// contentLabel maps a stored content kind to a human label for the span drawer.
function contentLabel(kind: string): string {
  switch (kind) {
    case "prompt":
      return "Prompt";
    case "response":
      return "Response";
    case "tool_io":
      return "Tool I/O";
    default:
      return kind;
  }
}

function DetailRow({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="flex justify-between gap-3">
      <span className="text-fg-3">{label}</span>
      <span className={`text-right text-fg-1 ${mono ? "font-mono text-xs" : ""}`}>{value}</span>
    </div>
  );
}
