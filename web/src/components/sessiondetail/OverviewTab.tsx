import { useMemo, useState } from "react";
import { SegmentedControl, Tooltip } from "@/components/primitives";
import { VerbosityCard } from "@/components/VerbosityCard";
import { actionMeta } from "@/lib/actions";
import { fmtCompact, fmtInt, fmtUSD } from "@/lib/format";
import type {
  ActionBucket,
  SessionDetail,
  SessionModelBucket,
} from "@/lib/types";

// Overview tab — "what shape was this session?".
//
// Action breakdown + token buckets + models used + output composition, all
// extracted VERBATIM from SessionDetailPanel.tsx during the tab split. No
// block's internals changed; only the three-column grid wrapper moved here
// from the shell.

export function OverviewTab({ d }: { d: SessionDetail }) {
  return (
    <div className="space-y-5">
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2 xl:grid-cols-3">
        <ActionBreakdownDonut rows={d.tool_breakdown} total={d.total_actions} />
        <TokenBucketsPanel tokens={d.tokens} />
        <ModelsUsedPanel rows={d.per_model} totalCost={d.cost_usd} />
      </div>
      <VerbosityCard sessionId={d.id} />
    </div>
  );
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
