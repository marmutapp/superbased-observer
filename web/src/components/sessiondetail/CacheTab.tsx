import { useState } from "react";
import clsx from "clsx";
import { Pill } from "@/components/primitives";
import { ChartState } from "@/components/ChartState";
import { CacheExpiryCard } from "@/components/CacheExpiryCard";
import { useApi } from "@/lib/useApi";
import { fmtCompact, fmtInt } from "@/lib/format";
import type {
  SessionCacheAnnotation,
  SessionCacheResponse,
  SessionDetail,
} from "@/lib/types";

// Cache tab — "is this session's prompt cache working, and is it about to go
// cold?". CacheExpiryCard + CachePanel, both extracted VERBATIM from
// SessionDetailPanel.tsx during the tab split.
//
// HONEST EMPTY STATE. Both blocks can render nothing: CacheExpiryCard returns
// null when /api/cache/status reports no live windows for this session, and
// CachePanel is gated on d.cache_summary (absent when the cache-tracking
// engine never graded a turn for this session). Rather than leave a blank
// tab, the tab names the exact missing dependency — the same rule the
// disabled Jump in / Resume controls follow.
export function CacheTab({ d }: { d: SessionDetail }) {
  return (
    <div className="space-y-5">
      <CacheExpiryCard sessionId={d.id} />
      {d.cache_summary ? (
        <CachePanel sessionId={d.id} summary={d.cache_summary} />
      ) : (
        <p className="rounded-3 border border-dashed border-line-2 px-4 py-3 text-[11.5px] text-fg-3">
          No cache statistics for this session — the cache-tracking engine
          graded no turns here (<span className="font-mono">cache_summary</span>{" "}
          is absent from{" "}
          <span className="font-mono">/api/session/{d.id}</span>). That happens
          when <span className="font-mono">[cachetrack]</span> is disabled, or
          when the session predates it, or when the provider returns no cache
          token counts. Nothing is hidden here.
        </p>
      )}
      <p className="text-[10.5px] text-fg-4">
        Cache expiry lists only caches that are still live (or recently cold);
        a finished session usually shows none.
      </p>
    </div>
  );
}

// ----- Cache panel (C16) ------------------------------------------

// CachePanel renders the SessionDetailPanel Cache tab content. Two
// layers:
//
//   - the "summary" rail (always rendered): tier badge, hit/write/
//     rewrite counts, ratio, flagged pill. Loads from
//     detail.cache_summary (already in the SessionDetail payload, C15).
//
//   - the "timeline" (lazy-loaded on first expand): baseline roll-up
//     rows + anomaly items. Loads /api/session/<id>/cache only when
//     the operator clicks "Show timeline" so a closed-by-default
//     SessionDetailPanel makes only one round-trip on open.
//
// Operator UI steers:
//
//   #1: aggregate suffix_growth/hit as a count in the timeline.
//       The backend already returns a single baseline roll-up per
//       contiguous run; this component renders it as one row
//       saying "N normal warm growth events" with the token sums
//       and time range. The 141-baseline-events session collapses
//       to ONE timeline row.
//
//   #2: render tools_changed as flagged (neutral), not alarm-red.
//       The backend marks anomaly items with flagged=true; this
//       component uses the neutral "flagged" Pill tone instead of
//       "warn" / "danger". The rest of the cause vocabulary
//       (system_changed / model_changed / expiry_rewrite / etc.)
//       gets the standard warn tone.
function CachePanel({
  sessionId,
  summary,
}: {
  sessionId: string;
  summary: SessionCacheAnnotation;
}) {
  const [showTimeline, setShowTimeline] = useState(false);
  const timeline = useApi<SessionCacheResponse>(
    showTimeline ? `/api/session/${sessionId}/cache` : null,
    undefined,
    [sessionId, showTimeline],
  );

  const ratioLabel =
    summary.ratio > 0 ? `${summary.ratio.toFixed(1)}× R/W` : "—";

  return (
    <section className="mt-5 space-y-2">
      <h3 className="flex items-center justify-between gap-2">
        <span className="flex items-center gap-2 text-[11px] font-semibold uppercase tracking-[0.06em] text-fg-3">
          Cache
          <CacheTierBadge tier={summary.tier} />
        </span>
        <button
          type="button"
          onClick={() => setShowTimeline((v) => !v)}
          className="text-[10.5px] text-accent hover:underline focus:outline-none"
        >
          {showTimeline ? "Hide timeline" : "Show timeline"}
        </button>
      </h3>

      <div className="rounded-3 border bg-bg-2 px-4 py-3">
        <div className="grid grid-cols-2 gap-3 text-[12px] sm:grid-cols-4 lg:grid-cols-7">
          <CacheStat label="Events" value={fmtInt(summary.event_count)} />
          <CacheStat label="Hits" value={fmtInt(summary.hit_count)} />
          <CacheStat label="Writes" value={fmtInt(summary.write_count)} />
          <CacheStat
            label="Rewrites"
            value={fmtInt(summary.rewrite_count)}
            warn={summary.rewrite_count > 0 && !summary.has_flagged_rewrites}
          />
          <CacheStat label="Ratio" value={ratioLabel} />
          <CacheStat
            label="Mispredicts"
            value={
              summary.zero_usage_count > 0 &&
              summary.mispredict_count === summary.zero_usage_count
                ? `${fmtInt(summary.mispredict_count)} (zero-usage)`
                : fmtInt(summary.mispredict_count)
            }
            muted={summary.mispredict_count === 0}
          />
          <CacheStat
            label="Tokens R / W"
            value={`${fmtCompact(summary.tokens_read)} / ${fmtCompact(summary.tokens_written)}`}
          />
        </div>
        {summary.has_flagged_rewrites && (
          <div className="mt-2 flex items-center gap-2 text-[10.5px] text-fg-3">
            <Pill variant="neutral">flagged</Pill>
            One or more rewrites carry a flagged cause (e.g. tools_changed
            on MCP server toggle). Real signal — not alarm-worthy unless
            it dominates.
          </div>
        )}
      </div>

      {showTimeline && (
        <ChartState
          loading={timeline.loading}
          error={timeline.error}
          empty={!timeline.data?.timeline?.length}
          emptyHint="No cache events recorded for this session."
          height={120}
        >
          {timeline.data && <CacheTimelineList timeline={timeline.data.timeline} />}
        </ChartState>
      )}
    </section>
  );
}

function CacheTierBadge({ tier }: { tier: SessionCacheAnnotation["tier"] }) {
  const map: Record<SessionCacheAnnotation["tier"], { label: string; variant: "info" | "neutral" | "success" }> = {
    proxy: { label: "Tier 1 · proxy", variant: "success" },
    transcript: { label: "Tier 2 · transcript", variant: "info" },
    mixed: { label: "Mixed", variant: "info" },
    none: { label: "None", variant: "neutral" },
  };
  const { label, variant } = map[tier];
  return <Pill variant={variant}>{label}</Pill>;
}

function CacheStat({
  label,
  value,
  warn,
  muted,
}: {
  label: string;
  value: string;
  warn?: boolean;
  muted?: boolean;
}) {
  return (
    <div className={clsx("flex flex-col", muted && "opacity-60")}>
      <span className="text-[10px] font-medium uppercase tracking-[0.05em] text-fg-3">
        {label}
      </span>
      <span
        className={clsx(
          "tabular-nums text-[13px] font-semibold",
          warn ? "text-warn" : "text-fg-1",
        )}
      >
        {value}
      </span>
    </div>
  );
}

function CacheTimelineList({
  timeline,
}: {
  timeline: SessionCacheResponse["timeline"];
}) {
  return (
    <ol className="space-y-1.5 text-[11px]">
      {timeline.map((item, i) => (
        <li key={i}>
          {item.kind === "baseline" ? (
            <BaselineRow item={item} />
          ) : item.event ? (
            <AnomalyRow event={item.event} flagged={!!item.flagged} />
          ) : null}
        </li>
      ))}
    </ol>
  );
}

function BaselineRow({
  item,
}: {
  item: SessionCacheResponse["timeline"][number];
}) {
  return (
    <div className="rounded-3 border bg-bg-2 px-3 py-1.5 text-fg-3">
      <span className="font-semibold text-fg-2">
        {item.count ?? 0} normal warm growth events
      </span>
      {item.baseline_read_sum != null && item.baseline_write_sum != null && (
        <>
          {" · "}
          <span className="tabular-nums">
            R {fmtCompact(item.baseline_read_sum)} / W {fmtCompact(item.baseline_write_sum)}
          </span>
        </>
      )}
      {item.first_at && item.last_at && (
        <>
          {" · "}
          <span className="tabular-nums">
            {fmtTimeShort(item.first_at)} – {fmtTimeShort(item.last_at)}
          </span>
        </>
      )}
    </div>
  );
}

function AnomalyRow({
  event,
  flagged,
}: {
  event: NonNullable<SessionCacheResponse["timeline"][number]["event"]>;
  flagged: boolean;
}) {
  return (
    <div className="rounded-3 border border-fg-3/30 bg-bg-2 px-3 py-1.5">
      <div className="flex flex-wrap items-center gap-2">
        <CacheKindPill kind={event.kind} cause={event.cause} flagged={flagged} />
        <span className="font-mono text-[10.5px] text-fg-3">
          {fmtTimeShort(event.timestamp)}
        </span>
        <span className="text-[11px] text-fg-2">{event.cause || "(no cause)"}</span>
        <span className="tabular-nums text-[10.5px] text-fg-3">
          R {fmtCompact(event.tokens_read)} / W {fmtCompact(event.tokens_written)}
        </span>
        {event.predicted_kind && event.predicted_kind !== event.kind && (
          <span className="text-[10.5px] text-fg-3">
            predicted: {event.predicted_kind}
          </span>
        )}
        {event.zero_usage && (
          <Pill variant="neutral">zero-usage · excluded from rate</Pill>
        )}
      </div>
    </div>
  );
}

function CacheKindPill({
  kind,
  cause,
  flagged,
}: {
  kind: string;
  cause: string;
  flagged: boolean;
}) {
  if (flagged) {
    return <Pill variant="neutral">{kind}</Pill>;
  }
  switch (kind) {
    case "hit":
      return <Pill variant="success">{kind}</Pill>;
    case "reanchor":
      return <Pill variant="info">{kind}</Pill>;
    case "mispredict":
      return <Pill variant="warn">{kind}</Pill>;
    case "invalidation_rewrite":
    case "expiry_rewrite":
    case "model_switch_rewrite":
    case "compaction_reset":
      return <Pill variant="warn">{kind}</Pill>;
    default:
      void cause;
      return <Pill variant="info">{kind}</Pill>;
  }
}

function fmtTimeShort(iso: string): string {
  // Render HH:MM:SS from an RFC3339 timestamp. The Cache timeline
  // is per-session — same date for every event — so the date prefix
  // is uninformative. Short form keeps the row compact.
  const t = iso.slice(11, 19);
  return t || iso;
}
