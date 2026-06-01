import { Fragment, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { AnimatePresence, motion } from "framer-motion";
import { createPortal } from "react-dom";
import clsx from "clsx";
import {
  Pill,
  SegmentedControl,
  SlideOver,
  ToolBadge,
  Tooltip,
} from "@/components/primitives";
import { ChartState } from "@/components/ChartState";
import { CopyOnClick } from "@/components/CopyOnClick";
import { Pagination } from "@/components/DataTable";
import { actionMeta } from "@/lib/actions";
import {
  DollarIcon,
  LightningIcon,
} from "@/components/icons";
import { fetchJSON } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import {
  fmtCompact,
  fmtDuration,
  fmtInt,
  fmtUSD,
} from "@/lib/format";
import type {
  ActionBucket,
  ActionFullText,
  MessageRow,
  SessionDetail,
  SessionMessages,
  SessionModelBucket,
  ToolCallRow,
} from "@/lib/types";

// SessionDetailPanel — right-side slide-over showing one session's
// 4-tile KPI band, action breakdown donut, token buckets panel, and
// the full per-message log with expandable tool calls.
//
// Panel width is 1480px (was 1400; 1200 before that; 880 originally).
// Each bump unlocked another column the messages table needed without
// horizontal scroll. The 2026-05-19 bump (1400 → 1480) adds the
// per-turn Effort column so reasoning-effort (low/medium/high) is
// visible at a glance for codex / antigravity sessions where the user
// (or the SKU encoding) selected an effort tier.
const MESSAGES_LIMIT = 25;

export function SessionDetailPanel({
  sessionId,
  open,
  onClose,
}: {
  sessionId: string | null;
  open: boolean;
  onClose: () => void;
}) {
  // Live-capture refresh while the slide-over is open: 8s on both
  // the detail rollup and the message stream. Calmer than the 2s
  // first-pass cadence — 2s was visibly choppy because every
  // background refetch toggled useApi's loading state, blanking the
  // rendered content. With useApi's silent-refetch tweak in place,
  // 8s gives steady progress without UI churn. Pauses when the tab
  // is hidden via the default visibility gate in useApi.
  const liveRefresh = open ? { refreshMs: 8000 } : undefined;
  const detail = useApi<SessionDetail>(
    sessionId ? `/api/session/${sessionId}` : null,
    undefined,
    [sessionId],
    liveRefresh,
  );
  const [msgPage, setMsgPage] = useState(1);
  // Reset paging when the user navigates to a different session.
  useEffect(() => {
    setMsgPage(1);
  }, [sessionId]);
  const offset = (msgPage - 1) * MESSAGES_LIMIT;
  const messages = useApi<SessionMessages>(
    sessionId ? `/api/session/${sessionId}/messages` : null,
    { limit: MESSAGES_LIMIT, offset },
    [sessionId, msgPage],
    liveRefresh,
  );

  return (
    <SlideOver
      open={open}
      onClose={onClose}
      width={1480}
      title={
        detail.data ? (
          <span className="flex items-center gap-2">
            <ToolBadge tool={detail.data.tool} />
            <CopyOnClick
              value={detail.data.id}
              className="font-mono text-[12px] text-fg-2"
            >
              {detail.data.id.slice(0, 8)}…{detail.data.id.slice(-4)}
            </CopyOnClick>
          </span>
        ) : sessionId ? (
          <span className="font-mono text-[12px] text-fg-3">
            {sessionId.slice(0, 8)}…
          </span>
        ) : (
          "Session detail"
        )
      }
      subtitle={detail.data?.project || undefined}
    >
      <div className="space-y-5 px-5 pb-5 pt-3">
        <ChartState
          loading={detail.loading && !detail.data}
          error={detail.error}
          empty={!detail.data}
          emptyHint="Loading session…"
          height={120}
        >
          {detail.data && (
            <>
              <KpiBand d={detail.data} />
              <div className="mt-5 grid grid-cols-1 gap-4 lg:grid-cols-2 xl:grid-cols-3">
                <ActionBreakdownDonut
                  rows={detail.data.tool_breakdown}
                  total={detail.data.total_actions}
                />
                <TokenBucketsPanel tokens={detail.data.tokens} />
                <ModelsUsedPanel
                  rows={detail.data.per_model}
                  totalCost={detail.data.cost_usd}
                />
              </div>
            </>
          )}
        </ChartState>

        <section className="space-y-2">
          <h3 className="flex items-baseline justify-between gap-2">
            <span className="text-[11px] font-semibold uppercase tracking-[0.06em] text-fg-3">
              Messages
            </span>
            <span className="text-[10.5px] text-fg-3">
              {messages.data
                ? `${fmtInt(messages.data.total)} total · click row to expand tool calls`
                : "Loading…"}
            </span>
          </h3>
          <ChartState
            loading={messages.loading}
            error={messages.error}
            empty={!messages.data?.messages.length}
            emptyHint="No messages indexed for this session."
            height={160}
          >
            {messages.data && (
              <MessagesTable rows={messages.data.messages} />
            )}
          </ChartState>
          {messages.data && messages.data.total > MESSAGES_LIMIT && (
            <Pagination
              page={msgPage}
              limit={MESSAGES_LIMIT}
              total={messages.data.total}
              onPage={setMsgPage}
              loading={messages.loading}
            />
          )}
        </section>
      </div>
    </SlideOver>
  );
}

// ----- KPI band ----------------------------------------------------

function KpiBand({ d }: { d: SessionDetail }) {
  const totalTokens =
    d.tokens.input + d.tokens.output + d.tokens.cache_read + d.tokens.cache_creation;
  const elapsedMs = elapsedMillis(d.started_at, d.ended_at);
  const ungraded =
    d.total_actions - d.success_actions - d.failure_actions;
  const hasProxyCost = d.cost_usd > 0 || d.ai_cost_usd > 0;
  const hasTokens = totalTokens > 0;
  return (
    <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
      <CostStat
        d={d}
        muted={!hasProxyCost}
      />
      <BigStat
        label="Actions"
        icon={<LightningIcon size={12} />}
        value={fmtInt(d.total_actions)}
        sub={renderActionsSub(d.success_actions, d.failure_actions, ungraded)}
        warn={d.failure_actions > 0}
      />
      <BigStat
        label="Elapsed"
        icon={<ClockIcon />}
        value={elapsedMs != null ? fmtDuration(elapsedMs) : "(open)"}
        sub={d.ended_at ? fmtDate(d.ended_at) : "session in progress"}
      />
      <BigStat
        label="Tokens"
        icon={<LayersIcon />}
        value={fmtCompact(totalTokens)}
        sub={
          hasTokens
            ? "net + cache R/W + output"
            : "no proxy capture for this session"
        }
        muted={!hasTokens}
      />
    </div>
  );
}

// CostStat — Total cost hero tile with explicit API + Tool sub-lines
// rendered as a two-row grid below the headline number. Per design's
// page-sessions.jsx mockup the slide-over makes the 3-way split
// visible at-a-glance rather than buried in a single "api X · tool Y"
// caption line.
function CostStat({
  d,
  muted,
}: {
  d: SessionDetail;
  muted?: boolean;
}) {
  const hasSplit = d.ai_cost_usd > 0 || d.tool_cost_usd > 0;
  return (
    <div
      className={clsx(
        "relative flex flex-col gap-1 overflow-hidden rounded-3 border bg-bg-2 px-4 py-3.5",
        "border-accent/40 ring-1 ring-accent-ring",
      )}
    >
      <span
        aria-hidden
        className="pointer-events-none absolute inset-0"
        style={{
          background:
            "radial-gradient(circle at 100% 0%, var(--accent-soft), transparent 60%)",
        }}
      />
      <div className="relative">
        <span className="flex items-center gap-1.5 text-[10px] font-semibold uppercase tracking-[0.08em] text-fg-3">
          <DollarIcon size={12} />
          Total cost
        </span>
        <Tooltip content={fmtUSD(d.cost_usd, true)}>
          <span
            tabIndex={0}
            className={clsx(
              "mt-0.5 block cursor-help text-[34px] font-bold leading-[1.05] tracking-[-0.02em] focus:outline-none",
              muted ? "text-fg-2" : "text-fg-0",
            )}
          >
            {fmtUSD(d.cost_usd)}
          </span>
        </Tooltip>
        {hasSplit ? (
          <div className="mt-1 grid grid-cols-2 gap-x-3 gap-y-0.5 text-[10.5px]">
            <span className="text-fg-3">API</span>
            <Tooltip content={fmtUSD(d.ai_cost_usd, true)}>
              <span
                tabIndex={0}
                className="cursor-help text-right font-mono tabular-nums text-fg-1 focus:outline-none"
              >
                {fmtUSD(d.ai_cost_usd)}
              </span>
            </Tooltip>
            <span className="text-fg-3">Tool</span>
            <Tooltip content={fmtUSD(d.tool_cost_usd, true)}>
              <span
                tabIndex={0}
                className="cursor-help text-right font-mono tabular-nums text-fg-2 focus:outline-none"
              >
                {fmtUSD(d.tool_cost_usd)}
              </span>
            </Tooltip>
          </div>
        ) : (
          <span className="mt-1 block text-[10.5px] text-fg-3">
            no proxy capture for this session
          </span>
        )}
      </div>
    </div>
  );
}

// BigStat — heavier-weight variant of StatCard used inside the
// SessionDetailPanel header. Design treats these 4 tiles as the
// hero of the slide-over, so they bump up to 34px / 700 with
// extra padding compared to the page-level KPI grid.
function BigStat({
  label,
  value,
  sub,
  warn,
  accent,
  muted,
  icon,
}: {
  label: string;
  value: React.ReactNode;
  sub?: React.ReactNode;
  warn?: boolean;
  accent?: boolean;
  muted?: boolean;
  icon?: React.ReactNode;
}) {
  return (
    <div
      className={clsx(
        "relative flex flex-col gap-1 overflow-hidden rounded-3 border bg-bg-2 px-4 py-3.5",
        accent
          ? "border-accent/40 ring-1 ring-accent-ring"
          : warn
            ? "border-warn/40"
            : "border-line-2",
      )}
    >
      <span
        aria-hidden
        className="pointer-events-none absolute inset-0"
        style={{
          background: warn
            ? "radial-gradient(circle at 100% 0%, var(--warn-soft), transparent 60%)"
            : accent
              ? "radial-gradient(circle at 100% 0%, var(--accent-soft), transparent 60%)"
              : "radial-gradient(circle at 100% 0%, color-mix(in srgb, var(--accent-soft) 30%, transparent), transparent 70%)",
        }}
      />
      <div className="relative">
        <span className="flex items-center gap-1.5 text-[10px] font-semibold uppercase tracking-[0.08em] text-fg-3">
          {icon && <span className="text-fg-3">{icon}</span>}
          {label}
        </span>
        <span
          className={clsx(
            "mt-0.5 block text-[34px] font-bold leading-[1.05] tracking-[-0.02em]",
            muted ? "text-fg-2" : "text-fg-0",
          )}
        >
          {value}
        </span>
        {sub && (
          <span
            className={clsx(
              "mt-1 block text-[11px]",
              muted ? "text-fg-4" : "text-fg-3",
            )}
          >
            {sub}
          </span>
        )}
      </div>
    </div>
  );
}

function ClockIcon() {
  return (
    <svg width={12} height={12} viewBox="0 0 16 16" fill="none" aria-hidden>
      <circle cx="8" cy="8" r="6" stroke="currentColor" strokeWidth="1.4" />
      <path
        d="M8 4.5V8l2.5 1.5"
        stroke="currentColor"
        strokeWidth="1.4"
        strokeLinecap="round"
      />
    </svg>
  );
}

function LayersIcon() {
  return (
    <svg width={12} height={12} viewBox="0 0 16 16" fill="none" aria-hidden>
      <path
        d="M2 5l6-3 6 3-6 3-6-3ZM2 8l6 3 6-3M2 11l6 3 6-3"
        stroke="currentColor"
        strokeWidth="1.4"
        strokeLinejoin="round"
      />
    </svg>
  );
}

function renderActionsSub(ok: number, fail: number, ungraded: number): string {
  // Most observer events (user_prompt, post_tool_batch, instructions_loaded)
  // aren't success/fail-graded, so the bare ok/fail split would be
  // misleading. Surface the ungraded count explicitly when significant.
  const parts: string[] = [];
  if (ok > 0) parts.push(`${fmtInt(ok)} ok`);
  if (fail > 0) parts.push(`${fmtInt(fail)} fail`);
  if (ungraded > 0) parts.push(`${fmtInt(ungraded)} event`);
  return parts.length ? parts.join(" · ") : "no graded outcomes";
}

// ----- Action breakdown donut -------------------------------------

function ActionBreakdownDonut({
  rows,
  total,
}: {
  rows: ActionBucket[];
  total: number;
}) {
  // Collapse below-1% slices into "other" so the legend stays tight.
  const sorted = [...rows].sort((a, b) => b.count - a.count);
  const minShare = Math.max(1, total) * 0.01;
  const main = sorted.filter((r) => r.count >= minShare);
  const otherCount = sorted
    .filter((r) => r.count < minShare)
    .reduce((a, r) => a + r.count, 0);
  const slices = otherCount > 0
    ? [...main, { action_type: "other", count: otherCount, failures: 0 }]
    : main;

  const sum = Math.max(1, slices.reduce((a, r) => a + r.count, 0));
  // Bumped from 128 to 144 + thicker ring (r48/inner26 → r56/inner30)
  // so the slices read with more presence per the operator's "more
  // vibrant" note on the donut.
  const cx = 72;
  const cy = 72;
  const r = 56;
  const inner = 32;
  let cursor = 0;

  return (
    <section className="rounded-3 border border-line-2 bg-bg-2 p-4">
      <h4 className="text-[12px] font-semibold text-fg-1">Action breakdown</h4>
      <p className="mt-0.5 text-[10.5px] text-fg-3">
        {fmtInt(total)} actions in this session
      </p>
      <div className="mt-3 flex items-center gap-4">
        <svg
          width={144}
          height={144}
          viewBox="0 0 144 144"
          style={{ filter: "drop-shadow(0 1px 3px rgba(0,0,0,0.35))" }}
        >
          {slices.length === 0 && (
            <circle
              cx={cx}
              cy={cy}
              r={r}
              fill="none"
              stroke="var(--line-2)"
              strokeWidth={r - inner}
            />
          )}
          {slices.map((s) => {
            const meta = actionMeta(s.action_type);
            const frac = s.count / sum;
            const a0 = (cursor / sum) * Math.PI * 2 - Math.PI / 2;
            cursor += s.count;
            const a1 = (cursor / sum) * Math.PI * 2 - Math.PI / 2;
            const largeArc = frac > 0.5 ? 1 : 0;
            const x0 = cx + Math.cos(a0) * r;
            const y0 = cy + Math.sin(a0) * r;
            const x1 = cx + Math.cos(a1) * r;
            const y1 = cy + Math.sin(a1) * r;
            const ix0 = cx + Math.cos(a0) * inner;
            const iy0 = cy + Math.sin(a0) * inner;
            const ix1 = cx + Math.cos(a1) * inner;
            const iy1 = cy + Math.sin(a1) * inner;
            const d = `M ${x0} ${y0} A ${r} ${r} 0 ${largeArc} 1 ${x1} ${y1} L ${ix1} ${iy1} A ${inner} ${inner} 0 ${largeArc} 0 ${ix0} ${iy0} Z`;
            return (
              <path
                key={s.action_type}
                d={d}
                fill={meta.colorVar}
                stroke="var(--bg-2)"
                strokeWidth={2}
              />
            );
          })}
          <text
            x={cx}
            y={cy + 6}
            textAnchor="middle"
            fontSize="22"
            fontWeight="700"
            fill="var(--fg-0)"
            style={{ letterSpacing: "-0.02em" }}
          >
            {fmtCompact(total)}
          </text>
        </svg>
        <ul className="flex-1 divide-y divide-line-1 text-[11.5px]">
          {slices.slice(0, 8).map((s) => {
            const meta = actionMeta(s.action_type);
            const pct = (s.count / sum) * 100;
            return (
              <li
                key={s.action_type}
                className="flex items-center justify-between gap-2 py-1"
              >
                <span className="flex items-center gap-2 truncate">
                  <span
                    className="h-2.5 w-2.5 shrink-0 rounded-sm"
                    style={{ background: meta.colorVar }}
                  />
                  <span className="truncate text-fg-1">{meta.label}</span>
                </span>
                <span className="flex shrink-0 items-baseline gap-2 font-mono tabular-nums">
                  <span className="text-fg-3">{fmtCompact(s.count)}</span>
                  <span className="w-[44px] text-right font-semibold text-fg-0">
                    {pct.toFixed(1)}%
                  </span>
                </span>
              </li>
            );
          })}
        </ul>
      </div>
    </section>
  );
}

// ----- Token buckets bars -----------------------------------------

function TokenBucketsPanel({
  tokens,
}: {
  tokens: SessionDetail["tokens"];
}) {
  // Cache-write splits into 5m and 1h ephemeral tiers; the 1h sub-row
  // only renders when the session actually carried 1h-tier writes
  // (every non-Anthropic provider stays at 0 — irrelevant noise to
  // show as a perpetual "—" row). Same colour family as 5m so the
  // two read as siblings; slight opacity drop on 1h so the visual
  // hierarchy still leads with the dominant tier.
  const cache1h = tokens.cache_creation_1h || 0;
  const cache5m = Math.max(0, tokens.cache_creation - cache1h);
  const cacheRows: {
    label: string;
    value: number;
    color: string;
    help: string;
  }[] =
    cache1h > 0
      ? [
          {
            label: "Cache Write (5m)",
            value: cache5m,
            color: "var(--tok-write)",
            help: "Prompt prefix written to Anthropic's 5-minute ephemeral cache (default tier). Charged at the model's cache_creation rate (≈125% of input). The TTL is sliding — every cache hit refreshes it for another 5 minutes from the read.",
          },
          {
            label: "Cache Write (1h)",
            value: cache1h,
            color: "color-mix(in oklab, var(--tok-write) 70%, var(--bg-3))",
            help: "Prompt prefix written to Anthropic's 1-hour ephemeral cache (cache_control.ttl = '1h'). Charged at 2× input rate — 60% premium over the 5m tier. The TTL is fixed (no sliding refresh). Worth the premium when the cached prefix is stable for the full session.",
          },
        ]
      : [
          {
            label: "Cache Write",
            value: tokens.cache_creation,
            color: "var(--tok-write)",
            help: "Prompt prefix written into Anthropic's cache. Charged at the model's cache_creation rate (≈125% of input).",
          },
        ];
  const rows: {
    label: string;
    value: number;
    color: string;
    help: string;
  }[] = [
    {
      label: "Net Input",
      value: tokens.input,
      color: "var(--tok-net)",
      help: "Fresh prompt tokens (uncached). Charged at the model's input rate.",
    },
    {
      label: "Cache Read",
      value: tokens.cache_read,
      color: "var(--tok-read)",
      help: "Prompt prefix served from Anthropic's prefix cache. Charged at the model's cache_read rate (≈10% of input).",
    },
    ...cacheRows,
    {
      label: "Output",
      value: tokens.output,
      color: "var(--tok-out)",
      help: "Assistant response tokens. Charged at the model's output rate (typically 5× input).",
    },
  ];
  const total = rows.reduce((a, r) => a + r.value, 0);
  const max = Math.max(1, ...rows.map((r) => r.value));
  return (
    <section className="rounded-3 border border-line-2 bg-bg-2 p-4">
      <h4 className="text-[12px] font-semibold text-fg-1">Token buckets</h4>
      <p className="mt-0.5 text-[10.5px] text-fg-3">
        {fmtCompact(total)} total · net input · cache read · cache write · output
      </p>
      <ul className="mt-3 space-y-2.5 text-[11.5px]">
        {rows.map((r) => {
          const pct = total > 0 ? (r.value / total) * 100 : 0;
          return (
            <Tooltip key={r.label} content={r.help} maxWidth={360}>
              <li
                tabIndex={0}
                className="flex cursor-help items-center gap-3 focus:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent-ring)]"
              >
              <span className="w-[88px] shrink-0 text-fg-2">{r.label}</span>
              <span className="relative h-2 flex-1 overflow-hidden rounded-pill bg-bg-3">
                <span
                  className="block h-full"
                  style={{
                    width: `${(r.value / max) * 100}%`,
                    background: r.color,
                  }}
                />
              </span>
              <span className="w-[70px] shrink-0 text-right font-mono tabular-nums text-fg-1">
                {r.value > 0 ? fmtCompact(r.value) : "—"}
              </span>
              <span className="w-[44px] shrink-0 text-right font-mono tabular-nums text-fg-3">
                {r.value > 0 ? `${pct.toFixed(1)}%` : "—"}
              </span>
              </li>
            </Tooltip>
          );
        })}
      </ul>
      {tokens.reasoning > 0 && (
        <div className="mt-2 border-t border-line-1 pt-2 text-[10.5px] text-fg-3">
          plus {fmtCompact(tokens.reasoning)} reasoning tokens (billed at output rate)
        </div>
      )}
    </section>
  );
}

// ----- Models used panel ------------------------------------------

// MODELS_USED_VISIBLE caps how many bars render before the "+N more"
// footer kicks in. Matches the ActionBreakdownDonut + TokenBucketsPanel
// row caps so the three side-by-side tiles have roughly equal height.
const MODELS_USED_VISIBLE = 6;

// BUCKETS is the canonical 4-bucket split used in both $ and Tokens
// modes. Colors mirror TokenBucketsPanel so the same hue means the
// same thing across the whole session-detail slide-over.
type BucketKey = "input" | "output" | "cache_read" | "cache_creation";
const BUCKETS: { key: BucketKey; label: string; color: string }[] = [
  { key: "input", label: "Net Input", color: "var(--tok-net)" },
  { key: "cache_read", label: "Cache Read", color: "var(--tok-read)" },
  { key: "cache_creation", label: "Cache Write", color: "var(--tok-write)" },
  { key: "output", label: "Output", color: "var(--tok-out)" },
];

type Mode = "cost" | "tokens";

function bucketValue(
  r: SessionModelBucket,
  bucket: BucketKey,
  mode: Mode,
): number {
  if (mode === "tokens") {
    switch (bucket) {
      case "input":
        return r.input;
      case "output":
        return r.output + (r.reasoning ?? 0);
      case "cache_read":
        return r.cache_read;
      case "cache_creation":
        return r.cache_creation;
    }
  }
  switch (bucket) {
    case "input":
      return r.input_cost_usd ?? 0;
    case "output":
      return r.output_cost_usd ?? 0;
    case "cache_read":
      return r.cache_read_cost_usd ?? 0;
    case "cache_creation":
      return r.cache_creation_cost_usd ?? 0;
  }
}

function modelTotal(r: SessionModelBucket, mode: Mode): number {
  return BUCKETS.reduce((a, b) => a + bucketValue(r, b.key, mode), 0);
}

// ModelsUsedPanel — third tile in the session-detail 2-up band.
// Style: one horizontal stacked bar per model with the bar LENGTH
// encoding magnitude (not normalized to 100%); segments colored by
// token bucket (input / cache read / cache write / output). A
// `$ / Tokens` SegmentedControl toggles the encoded metric so users
// can flip between cost share and raw token volume per bucket — same
// 4 colors, same model rows. Empty bar = $0/0 tok (e.g. recorded-cost
// adapters that don't surface a per-bucket split: the bar collapses
// when the toggle is on $, the tokens view always renders).
function ModelsUsedPanel({
  rows,
  totalCost,
}: {
  rows: SessionModelBucket[];
  totalCost: number;
}) {
  const [mode, setMode] = useState<Mode>("cost");

  // Filter zero-everything rows. Rank by cost first (then turn count as
  // tiebreak — useful for sessions where pricing isn't tied yet).
  const ranked = useMemo(() => {
    return [...rows]
      .filter(
        (r) =>
          r.cost_usd > 0 ||
          r.input + r.output + r.cache_read + r.cache_creation > 0,
      )
      .sort((a, b) => b.cost_usd - a.cost_usd || b.turn_count - a.turn_count);
  }, [rows]);

  // The maximum model total in the current mode is the reference for
  // bar widths — the top model takes the full track, everyone else is
  // proportional. Recompute when mode flips because the proportions
  // typically change (cost is dominated by output tokens, raw token
  // counts are dominated by cache reads).
  const maxTotal = Math.max(
    1,
    ...ranked.map((r) => modelTotal(r, mode)),
  );
  const grandTotalCost = useMemo(
    () => ranked.reduce((a, r) => a + r.cost_usd, 0),
    [ranked],
  );
  const grandTotalTokens = useMemo(
    () =>
      ranked.reduce(
        (a, r) => a + r.input + r.output + r.cache_read + r.cache_creation,
        0,
      ),
    [ranked],
  );

  const visible = ranked.slice(0, MODELS_USED_VISIBLE);
  const hidden = ranked.length - visible.length;

  const subtitle =
    ranked.length === 0
      ? "no model attribution captured for this session"
      : mode === "cost"
        ? `${ranked.length} model${ranked.length === 1 ? "" : "s"} · ${fmtUSD(totalCost > 0 ? totalCost : grandTotalCost)} total · bar length = $ spent`
        : `${ranked.length} model${ranked.length === 1 ? "" : "s"} · ${fmtCompact(grandTotalTokens)} tok · bar length = tokens used`;

  return (
    <section className="rounded-3 border border-line-2 bg-bg-2 p-4">
      <div className="flex items-baseline justify-between gap-2">
        <div>
          <h4 className="text-[12px] font-semibold text-fg-1">Models used</h4>
          <p className="mt-0.5 text-[10.5px] text-fg-3">{subtitle}</p>
        </div>
        {ranked.length > 0 && (
          <SegmentedControl<Mode>
            options={[
              { value: "cost", label: "$" },
              { value: "tokens", label: "tokens" },
            ]}
            value={mode}
            onChange={setMode}
            size="sm"
          />
        )}
      </div>
      {ranked.length === 0 ? (
        <div className="mt-3 text-[10.5px] text-fg-3">
          No tokens or cost are attributed to any model on this session yet.
        </div>
      ) : (
        <>
          <ul className="mt-3 space-y-2 text-[11.5px]">
            {visible.map((r) => {
              const total = modelTotal(r, mode);
              const widthPct = (total / maxTotal) * 100;
              const tokenTotal =
                r.input + r.output + r.cache_read + r.cache_creation;
              const rightLabel =
                mode === "cost" ? fmtUSD(r.cost_usd) : fmtCompact(tokenTotal);
              return (
                <li key={r.model} className="space-y-1">
                  <div className="flex items-baseline justify-between gap-2">
                    <Tooltip content={<span className="break-all font-mono">{r.model}</span>} maxWidth={360}>
                      <span
                        tabIndex={0}
                        className="cursor-help truncate text-fg-1 focus:outline-none"
                      >
                        {r.model}
                      </span>
                    </Tooltip>
                    <Tooltip
                      content={
                        mode === "cost"
                          ? fmtUSD(r.cost_usd, true)
                          : `${fmtInt(tokenTotal)} tokens`
                      }
                    >
                      <span
                        tabIndex={0}
                        className="cursor-help shrink-0 font-mono tabular-nums font-semibold text-fg-0 focus:outline-none"
                      >
                        {rightLabel}
                      </span>
                    </Tooltip>
                  </div>
                  <div className="flex h-2.5 w-full overflow-hidden rounded-pill bg-bg-3">
                    <div
                      className="flex h-full"
                      style={{ width: `${widthPct}%` }}
                    >
                      {BUCKETS.map((b) => {
                        const v = bucketValue(r, b.key, mode);
                        if (v <= 0) return null;
                        const segPct = total > 0 ? (v / total) * 100 : 0;
                        const tip =
                          mode === "cost"
                            ? `${b.label}: ${fmtUSD(v, true)}`
                            : `${b.label}: ${fmtInt(v)} tokens`;
                        return (
                          <Tooltip key={b.key} content={tip}>
                            <span
                              style={{
                                width: `${segPct}%`,
                                background: b.color,
                              }}
                            />
                          </Tooltip>
                        );
                      })}
                    </div>
                  </div>
                  <div className="font-mono tabular-nums text-[10.5px] text-fg-3">
                    {fmtInt(r.turn_count)} turn
                    {r.turn_count === 1 ? "" : "s"} ·{" "}
                    {mode === "cost"
                      ? `${fmtCompact(tokenTotal)} tok`
                      : fmtUSD(r.cost_usd)}
                    {r.tool_cost_usd > 0 &&
                      ` · tool ${fmtUSD(r.tool_cost_usd)}`}
                  </div>
                </li>
              );
            })}
            {hidden > 0 && (
              <li className="pt-1 text-center text-[10.5px] text-fg-3">
                +{hidden} more
              </li>
            )}
          </ul>
          <div className="mt-3 border-t border-line-1 pt-2">
            <ul className="flex flex-wrap items-center gap-x-3 gap-y-1 text-[10.5px] text-fg-2">
              {BUCKETS.map((b) => (
                <li key={b.key} className="flex items-center gap-1.5">
                  <span
                    aria-hidden
                    className="h-2 w-2 rounded-pill"
                    style={{ background: b.color }}
                  />
                  <span>{b.label}</span>
                </li>
              ))}
            </ul>
          </div>
        </>
      )}
    </section>
  );
}

// ----- Messages table ---------------------------------------------

function MessagesTable({ rows }: { rows: MessageRow[] }) {
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});
  return (
    <div className="overflow-x-auto rounded-2 border border-line-1">
      <table className="w-full min-w-[1260px] text-left text-[11px]">
        <thead className="bg-bg-3/40 text-[10px] uppercase tracking-[0.06em] text-fg-3">
          <tr className="whitespace-nowrap border-b border-line-2">
            <th className="py-1.5 pl-3 font-medium">#</th>
            <th className="py-1.5 font-medium">Msg ID</th>
            <th className="py-1.5 font-medium">Role</th>
            <th className="py-1.5 font-medium">Model</th>
            <Tooltip
              content="Reasoning effort — codex collaboration_mode.settings.reasoning_effort, antigravity SKU-encoded low/medium/high, etc. Empty for adapters that don't expose an effort knob (Anthropic models, copilot, etc.)."
              maxWidth={420}
            >
              <th tabIndex={0} className="cursor-help py-1.5 font-medium focus:outline-none">
                Effort
              </th>
            </Tooltip>
            <th className="py-1.5 text-right font-medium">In</th>
            <th className="py-1.5 text-right font-medium">Cache R</th>
            <th className="py-1.5 text-right font-medium">Cache W</th>
            <th className="py-1.5 text-right font-medium">Out</th>
            <th className="py-1.5 text-right font-medium">Elapsed</th>
            <th className="py-1.5 text-right font-medium">Tools</th>
            <th className="py-1.5 text-right font-medium">API $</th>
            <th className="py-1.5 text-right font-medium">Tool $</th>
            <th className="py-1.5 text-right font-medium">Total $</th>
            <th className="py-1.5 pl-3 pr-3 font-medium">Content</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((m, i) => {
            const key = m.message_id || `m${i}`;
            const isOpen = !!expanded[key];
            const hasToolCalls = (m.tool_calls?.length ?? 0) > 0;
            return (
              <Fragment key={key}>
                <tr
                  className={clsx(
                    "whitespace-nowrap border-b border-line-1 last:border-b-0",
                    hasToolCalls && "cursor-pointer hover:bg-bg-3/40",
                  )}
                  onClick={() =>
                    hasToolCalls &&
                    setExpanded((s) => ({ ...s, [key]: !s[key] }))
                  }
                >
                  <td className="py-1 pl-3 tabular-nums text-fg-3">{i + 1}</td>
                  <td className="py-1 font-mono text-[10.5px]">
                    {m.message_id ? (
                      <CopyOnClick
                        value={m.message_id}
                        className="text-accent hover:text-accent-strong"
                      >
                        {m.message_id.slice(0, 10)}…
                      </CopyOnClick>
                    ) : (
                      <span className="text-fg-4">—</span>
                    )}
                  </td>
                  <td className="py-1">
                    <RolePill role={m.role} />
                  </td>
                  <td className="max-w-[140px] truncate py-1 font-mono text-fg-2">
                    {m.model ? (
                      <Tooltip
                        content={<span className="break-all font-mono">{m.model}</span>}
                        maxWidth={360}
                      >
                        <span tabIndex={0} className="cursor-help focus:outline-none">
                          {m.model}
                        </span>
                      </Tooltip>
                    ) : (
                      "—"
                    )}
                  </td>
                  <td className="py-1 text-fg-2">
                    {m.effort_level ? (
                      <span className="font-mono text-[10.5px] uppercase tracking-tight">
                        {m.effort_level}
                      </span>
                    ) : (
                      <span className="text-fg-4">—</span>
                    )}
                  </td>
                  <td className="py-1 text-right tabular-nums text-fg-2">
                    {m.input > 0 ? fmtCompact(m.input) : "—"}
                  </td>
                  <td className="py-1 text-right tabular-nums text-fg-2">
                    {m.cache_read > 0 ? fmtCompact(m.cache_read) : "—"}
                  </td>
                  <td className="py-1 text-right tabular-nums text-fg-2">
                    {m.cache_creation > 0 ? fmtCompact(m.cache_creation) : "—"}
                  </td>
                  <td className="py-1 text-right tabular-nums text-fg-2">
                    {m.output > 0 ? fmtCompact(m.output) : "—"}
                  </td>
                  <td className="py-1 text-right tabular-nums text-fg-3">
                    {m.elapsed_ms != null ? fmtDuration(m.elapsed_ms) : "—"}
                  </td>
                  <td className="py-1 text-right tabular-nums text-fg-2">
                    {hasToolCalls ? (
                      <Tooltip content="Click row to expand tool calls">
                        <span
                          tabIndex={0}
                          className="inline-flex cursor-help items-center gap-1 focus:outline-none"
                        >
                          {fmtInt(m.tool_call_count)}
                          <Caret open={isOpen} />
                        </span>
                      </Tooltip>
                    ) : (
                      "—"
                    )}
                  </td>
                  <td className="py-1 text-right tabular-nums text-fg-1">
                    {m.ai_cost_usd > 0 ? (
                      <Tooltip content={fmtUSD(m.ai_cost_usd, true)}>
                        <span tabIndex={0} className="cursor-help focus:outline-none">
                          {fmtUSD(m.ai_cost_usd)}
                        </span>
                      </Tooltip>
                    ) : (
                      "—"
                    )}
                  </td>
                  <td className="py-1 text-right tabular-nums text-fg-3">
                    {m.tool_cost_usd > 0 ? (
                      <Tooltip content={fmtUSD(m.tool_cost_usd, true)}>
                        <span tabIndex={0} className="cursor-help focus:outline-none">
                          {fmtUSD(m.tool_cost_usd)}
                        </span>
                      </Tooltip>
                    ) : (
                      "—"
                    )}
                  </td>
                  <td className="py-1 text-right tabular-nums text-fg-0">
                    {m.cost_usd > 0 ? (
                      <Tooltip content={fmtUSD(m.cost_usd, true)}>
                        <span tabIndex={0} className="cursor-help font-semibold focus:outline-none">
                          {fmtUSD(m.cost_usd)}
                        </span>
                      </Tooltip>
                    ) : (
                      "—"
                    )}
                  </td>
                  <td className="max-w-[320px] truncate py-1 pl-3 pr-3 text-fg-2">
                    <ContentSnippet row={m} />
                  </td>
                </tr>
                {isOpen && m.tool_calls && m.tool_calls.length > 0 && (
                  <tr className="bg-bg-1">
                    <td colSpan={14} className="px-3 py-2 pl-[50px]">
                      <div className="flex flex-col gap-1.5">
                        {m.tool_calls.map((tc, j) => (
                          <ToolCallRowView key={`${key}-tc-${j}`} tc={tc} />
                        ))}
                      </div>
                    </td>
                  </tr>
                )}
              </Fragment>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

// useActionFullText caches the on-demand /api/action/<id>/full_text
// fetch per action_id within a single row's lifetime. The cache is a
// ref (not state) because we only want the cached value to influence
// what gets copied, never to trigger re-renders. Returns a resolver
// for CopyOnClick + an open-modal callback that loads on first click
// and stores the result in component state so the modal can render.
function useActionFullText(actionId: number) {
  const cacheRef = useRef<ActionFullText | null>(null);
  const inflightRef = useRef<Promise<ActionFullText> | null>(null);

  const fetchOnce = useCallback(async (): Promise<ActionFullText> => {
    if (cacheRef.current) return cacheRef.current;
    if (inflightRef.current) return inflightRef.current;
    const p = fetchJSON<ActionFullText>(`/api/action/${actionId}/full_text`)
      .then((v) => {
        cacheRef.current = v;
        return v;
      })
      .finally(() => {
        inflightRef.current = null;
      });
    inflightRef.current = p;
    return p;
  }, [actionId]);

  return { fetchOnce };
}

// ToolCallRowView — one expanded tool-call row inside a message. The
// design's expand row puts the action pill + target + status + any
// excerpt / error inline; we render each text piece via CopyOnClick
// so the operator can grab any subset (the target path, an output
// snippet, the error message) without selecting through the row's
// click-to-collapse handler. Legacy parity per `tmp/legacy/index.html:
// 2977-2990` (sessionToolPrimaryText + expandable copy).
//
// v1.6.29 on-demand full-text: when tc.full_text_elided or
// tc.has_full_output is set, the inline `full_text` / `excerpt` is a
// truncated preview. The copy buttons fetch the untruncated body via
// /api/action/<id>/full_text on click; a separate "View" button next
// to the primary opens a modal showing the full text + a copy button
// inside. The fetch is lazy + cached per row so a session with 1000
// large rows doesn't preload megabytes the operator may never inspect.
function ToolCallRowView({ tc }: { tc: ToolCallRow }) {
  // `primary` is what gets rendered (truncated at 140 chars in the
  // narrow case). `primaryFull` is what gets copied — the backend
  // serves `full_text` from actions.raw_tool_input (preview capped at
  // 4 KiB inline; longer rows carry full_text_elided=true and require
  // an /api/action/<id>/full_text fetch for the untruncated body),
  // while `target` is capped at 200 chars at store time. Preferring
  // full_text means the copy button hands the operator the actual
  // text they typed, not the dashboard's display-truncated version.
  const primary = tc.target || tc.raw_tool_name || "";
  const primaryFull = tc.full_text || primary;
  const hasLazyFullText = tc.full_text_elided === true;
  const hasLazyOutput = tc.has_full_output === true;
  const [viewOpen, setViewOpen] = useState(false);
  const { fetchOnce } = useActionFullText(tc.action_id);

  // Resolver for the primary CopyOnClick. Returns the untruncated
  // raw_tool_input when full_text_elided; otherwise null so
  // CopyOnClick falls back to the inline `value`.
  const resolvePrimary = hasLazyFullText
    ? async () => {
        const v = await fetchOnce();
        return v.raw_tool_input || primaryFull;
      }
    : undefined;

  // Resolver for the excerpt CopyOnClick. Returns the untruncated
  // raw_tool_output when has_full_output; otherwise null so
  // CopyOnClick falls back to the inline excerpt.
  const resolveOutput = hasLazyOutput
    ? async () => {
        const v = await fetchOnce();
        return v.raw_tool_output || tc.excerpt || "";
      }
    : undefined;

  const showViewButton = hasLazyFullText || hasLazyOutput;

  return (
    <div
      className="flex flex-col gap-1 rounded-1 bg-bg-2 px-2.5 py-1.5"
      style={{
        borderLeft: `2px solid ${
          tc.success ? "var(--accent)" : "var(--danger)"
        }`,
      }}
      onClick={(e) => e.stopPropagation()}
    >
      <div className="flex items-center gap-2.5">
        <span
          className="inline-flex items-center gap-1 rounded-pill border border-line-2 bg-bg-3 px-2 py-0.5 font-mono text-[10.5px] font-medium leading-none"
          style={{ color: "var(--act-cmd)" }}
        >
          {actionMeta(tc.action_type).label}
        </span>
        {tc.raw_tool_name && (
          <CopyOnClick
            value={tc.raw_tool_name}
            className="font-mono text-[10.5px] text-fg-3"
          >
            {tc.raw_tool_name}
          </CopyOnClick>
        )}
        {primary && (
          <CopyOnClick
            value={primaryFull}
            resolveValue={resolvePrimary}
            className="min-w-0 flex-1 font-mono text-[11px] text-fg-1"
            title={
              hasLazyFullText
                ? "Click to fetch and copy full text"
                : "Click to copy full text"
            }
          >
            {primary.length > 140 || primaryFull.length > primary.length ? (
              <Tooltip
                content={<span className="break-all font-mono">{primaryFull}</span>}
                maxWidth={420}
              >
                <span tabIndex={0} className="block cursor-help truncate focus:outline-none">
                  {truncate(primary, 140)}
                </span>
              </Tooltip>
            ) : (
              <span className="block truncate">{primary}</span>
            )}
          </CopyOnClick>
        )}
        {showViewButton && (
          <button
            type="button"
            onClick={(e) => {
              e.stopPropagation();
              setViewOpen(true);
            }}
            title={
              hasLazyFullText && hasLazyOutput
                ? "View full input + output"
                : hasLazyFullText
                  ? "View full input"
                  : "View full output"
            }
            className="shrink-0 rounded-1 border border-line-2 bg-bg-3 px-1.5 py-0.5 font-mono text-[10px] text-fg-3 transition-colors hover:border-accent hover:text-accent"
          >
            View
          </button>
        )}
        {tc.duration_ms != null && tc.duration_ms > 0 && (
          <span className="shrink-0 font-mono text-[10px] tabular-nums text-fg-3">
            {fmtDuration(tc.duration_ms)}
          </span>
        )}
        {!tc.success && (
          <Pill variant="danger">
            {tc.error_message ? truncate(tc.error_message, 32) : "failed"}
          </Pill>
        )}
      </div>
      {tc.excerpt && (
        <CopyOnClick
          value={tc.full_text || tc.excerpt}
          resolveValue={resolveOutput}
          className="ml-6 block font-mono text-[10.5px] text-fg-3"
          title={
            hasLazyOutput
              ? "Click to fetch and copy full output"
              : "Click to copy full output"
          }
        >
          <span className="block whitespace-pre-wrap break-words">
            {tc.excerpt}
          </span>
        </CopyOnClick>
      )}
      {tc.error_message && (
        <CopyOnClick
          value={tc.error_message}
          className="ml-6 block font-mono text-[10.5px] text-danger"
        >
          <span className="block whitespace-pre-wrap break-words">
            {tc.error_message}
          </span>
        </CopyOnClick>
      )}
      {viewOpen && (
        <FullTextModal
          actionId={tc.action_id}
          fetcher={fetchOnce}
          actionType={tc.action_type}
          rawToolName={tc.raw_tool_name}
          onClose={() => setViewOpen(false)}
        />
      )}
    </div>
  );
}

// FullTextModal — centered overlay that loads the untruncated
// raw_tool_input + raw_tool_output for one action and renders both
// with copy buttons. Mounted above the SessionDetailPanel slide-over
// (z-[60]) so the operator can drill into a single row's full content
// without losing the messages timeline behind it. Closes on Escape or
// backdrop click; the fetch is the same one CopyOnClick uses behind
// the scenes so opening View immediately after copying is free.
function FullTextModal({
  actionId,
  fetcher,
  actionType,
  rawToolName,
  onClose,
}: {
  actionId: number;
  fetcher: () => Promise<ActionFullText>;
  actionType: string;
  rawToolName: string;
  onClose: () => void;
}) {
  const [data, setData] = useState<ActionFullText | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    let cancelled = false;
    fetcher()
      .then((v) => {
        if (!cancelled) setData(v);
      })
      .catch((e: unknown) => {
        if (!cancelled) setError(e instanceof Error ? e.message : String(e));
      });
    return () => {
      cancelled = true;
    };
  }, [fetcher]);

  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") onClose();
    }
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onClose]);

  const meta = actionMeta(actionType);
  const inputBody = data?.raw_tool_input ?? "";
  const outputBody = data?.raw_tool_output ?? "";

  return createPortal(
    <AnimatePresence>
      <motion.div
        key="ft-backdrop"
        initial={{ opacity: 0 }}
        animate={{ opacity: 1 }}
        exit={{ opacity: 0 }}
        transition={{ duration: 0.15, ease: "easeOut" }}
        onClick={onClose}
        className="fixed inset-0 z-[60] flex items-center justify-center bg-black/70 p-6"
      >
        <motion.div
          key="ft-panel"
          initial={{ scale: 0.97, opacity: 0 }}
          animate={{ scale: 1, opacity: 1 }}
          exit={{ scale: 0.97, opacity: 0 }}
          transition={{ duration: 0.18, ease: "easeOut" }}
          onClick={(e) => e.stopPropagation()}
          className="flex max-h-[88vh] w-full max-w-[1000px] flex-col overflow-hidden rounded-2 border border-line-2 bg-bg-1 shadow-drawer"
        >
          <header className="flex items-center justify-between gap-3 border-b border-line-1 px-5 py-3">
            <div className="flex min-w-0 items-center gap-2">
              <span
                className="inline-flex items-center gap-1 rounded-pill border border-line-2 bg-bg-3 px-2 py-0.5 font-mono text-[10.5px] font-medium leading-none"
                style={{ color: "var(--act-cmd)" }}
              >
                {meta.label}
              </span>
              {rawToolName && (
                <span className="truncate font-mono text-[11px] text-fg-3">
                  {rawToolName}
                </span>
              )}
              <span className="shrink-0 font-mono text-[10px] text-fg-3">
                action #{actionId}
              </span>
            </div>
            <button
              type="button"
              onClick={onClose}
              className="shrink-0 rounded-1 border border-line-2 bg-bg-3 px-2 py-0.5 font-mono text-[10.5px] text-fg-2 transition-colors hover:border-accent hover:text-accent"
            >
              Close
            </button>
          </header>
          <div className="flex-1 overflow-y-auto px-5 py-4">
            {!data && !error && (
              <div className="font-mono text-[11px] text-fg-3">Loading…</div>
            )}
            {error && (
              <div className="font-mono text-[11px] text-danger">
                Failed to load full text: {error}
              </div>
            )}
            {data && (
              <div className="flex flex-col gap-4">
                {inputBody && (
                  <FullTextSection label="raw_tool_input" body={inputBody} />
                )}
                {outputBody && (
                  <FullTextSection label="raw_tool_output" body={outputBody} />
                )}
                {!inputBody && !outputBody && (
                  <div className="font-mono text-[11px] text-fg-3">
                    No captured content for this action.
                  </div>
                )}
              </div>
            )}
          </div>
        </motion.div>
      </motion.div>
    </AnimatePresence>,
    document.body,
  );
}

function FullTextSection({ label, body }: { label: string; body: string }) {
  return (
    <section className="flex flex-col gap-1">
      <div className="flex items-center justify-between">
        <span className="font-mono text-[10px] uppercase tracking-wide text-fg-3">
          {label}{" "}
          <span className="text-fg-3/70">({body.length.toLocaleString()} chars)</span>
        </span>
        <CopyOnClick
          value={body}
          className="rounded-1 border border-line-2 bg-bg-3 px-2 py-0.5 font-mono text-[10.5px] text-fg-2"
        >
          Copy
        </CopyOnClick>
      </div>
      <pre className="max-h-[55vh] overflow-auto rounded-1 border border-line-1 bg-bg-2 p-3 font-mono text-[11px] leading-relaxed text-fg-1 whitespace-pre-wrap break-words">
        {body}
      </pre>
    </section>
  );
}

function ContentSnippet({ row }: { row: MessageRow }) {
  // Backend doesn't index user/assistant body text, so we surface
  // the most informative substitute: for a tool-call-bearing
  // assistant row, the first tool call's target. For a tool-result
  // row, the action_type + truncated target. Otherwise the role.
  if (row.tool_calls?.length) {
    const tc = row.tool_calls[0];
    const label = (
      <>
        {actionMeta(tc.action_type).label}
        {tc.target ? ` · ${truncate(tc.target, 60)}` : ""}
      </>
    );
    if (!tc.target) return <span>{label}</span>;
    return (
      <Tooltip content={<span className="break-all font-mono">{tc.target}</span>} maxWidth={420}>
        <span tabIndex={0} className="cursor-help focus:outline-none">
          {label}
        </span>
      </Tooltip>
    );
  }
  return <span className="text-fg-4">—</span>;
}

function RolePill({ role }: { role: string }) {
  // Design's role color mapping (page-sessions.jsx:222-224):
  //   user      → accent (purple)
  //   assistant → success (green)
  //   tool      → warn (yellow)
  switch (role) {
    case "user":
      return <Pill variant="accent">user</Pill>;
    case "assistant":
      return <Pill variant="success">assistant</Pill>;
    case "tool":
      return <Pill variant="warn">tool</Pill>;
    case "system":
      return <Pill>system</Pill>;
    default:
      return <Pill>{role}</Pill>;
  }
}

function Caret({ open }: { open: boolean }) {
  return (
    <svg
      width={9}
      height={9}
      viewBox="0 0 12 12"
      fill="none"
      className={clsx(
        "transition-transform",
        open ? "rotate-180" : "rotate-0",
      )}
      aria-hidden
    >
      <path
        d="m3 4.5 3 3 3-3"
        stroke="currentColor"
        strokeWidth="1.4"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  );
}

// ----- helpers -----------------------------------------------------

function elapsedMillis(start: string, end?: string): number | null {
  const s = new Date(start).getTime();
  if (!Number.isFinite(s)) return null;
  const e = end ? new Date(end).getTime() : Date.now();
  if (!Number.isFinite(e)) return null;
  return Math.max(0, e - s);
}

function fmtDate(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString("en-US", {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

function truncate(s: string, n: number): string {
  if (!s) return "";
  return s.length <= n ? s : s.slice(0, n - 1) + "…";
}

// Re-export `ToolCallRow` so its type is preserved if consumers need it.
export type { ToolCallRow };
