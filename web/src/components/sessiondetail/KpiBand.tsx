import { Fragment } from "react";
import clsx from "clsx";
import { Tooltip } from "@/components/primitives";
import { DollarIcon, LightningIcon } from "@/components/icons";
import { fmtCompact, fmtDuration, fmtInt, fmtUSD } from "@/lib/format";
import type { SessionDetail } from "@/lib/types";
import { elapsedMillis, elapsedSub } from "./shared";

// KpiBand — the four always-visible tiles. Extracted from
// SessionDetailPanel.tsx during the tab split and kept ABOVE the tab strip:
// these four numbers are the session's headline and must not require a tab
// choice to read.

// ----- KPI band ----------------------------------------------------
//
// The annotation strip (favorite / tags / note) that used to live here moved
// to SessionActionHeader.tsx alongside the session verbs; the note is a chip +
// popover there instead of an always-expanded textarea.

export function KpiBand({ d }: { d: SessionDetail }) {
  const totalTokens =
    d.tokens.input + d.tokens.output + d.tokens.cache_read + d.tokens.cache_creation;
  // Prefer ended_at; fall back to last_activity_at (server COALESCE of the
  // last action's timestamp) so a never-closed session shows real elapsed,
  // not start→now.
  const elapsedMs = elapsedMillis(d.started_at, d.ended_at ?? d.last_activity_at);
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
        sub={elapsedSub(d)}
      />
      <BigStat
        label="Tokens"
        icon={<LayersIcon />}
        value={
          hasTokens
            ? fmtCompact(totalTokens)
            : d.context_budget_tokens
              ? `~${fmtCompact(d.context_budget_tokens)}`
              : fmtCompact(totalTokens)
        }
        sub={
          hasTokens
            ? "net + cache R/W + output"
            : d.context_budget_tokens
              ? "context budget (est.) · not billed"
              : "no billed usage"
        }
        subTitle={!hasTokens ? d.tokens_note : undefined}
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
  subTitle,
  warn,
  accent,
  muted,
  icon,
}: {
  label: string;
  value: React.ReactNode;
  sub?: React.ReactNode;
  /** Optional hover text on the sub-line (e.g. the full unbilled-tokens note). */
  subTitle?: string;
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
            // warn tints the headline number amber (the mock's `.kpi.hot .v`)
            // so "this tile is not clean" is readable without parsing the
            // sub-line. muted stays the weaker signal and loses to warn.
            warn ? "text-warn" : muted ? "text-fg-2" : "text-fg-0",
          )}
        >
          {value}
        </span>
        {sub && (
          <span
            className={clsx(
              "mt-1 block text-[11px]",
              muted ? "text-fg-4" : "text-fg-3",
              subTitle && "cursor-help",
            )}
            title={subTitle}
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

function renderActionsSub(
  ok: number,
  fail: number,
  ungraded: number,
): React.ReactNode {
  // Most observer events (user_prompt, post_tool_batch, instructions_loaded)
  // aren't success/fail-graded, so the bare ok/fail split would be
  // misleading. Surface the ungraded count explicitly when significant.
  //
  // STATE ENCODED IN FORM (design proposal, failure 05: "a session with 4
  // failed actions and one with none look the same until you read the
  // digits"). The failure count is the ONE segment here that changes what an
  // operator should do next, so it carries danger colour and weight while the
  // ok / ungraded counts stay neutral — colour marks the exception, not the
  // whole line. The tile itself already carried a warn border for
  // failure_actions > 0; the headline number now tints amber with it, so the
  // signal survives at a glance from across the panel.
  const parts: React.ReactNode[] = [];
  if (ok > 0) parts.push(<span key="ok">{fmtInt(ok)} ok</span>);
  if (fail > 0) {
    parts.push(
      <span key="fail" className="font-semibold text-danger">
        {fmtInt(fail)} failed
      </span>,
    );
  }
  if (ungraded > 0) parts.push(<span key="ev">{fmtInt(ungraded)} event</span>);
  if (parts.length === 0) return "no graded outcomes";
  return parts.map((p, i) => (
    <Fragment key={i}>
      {i > 0 && " · "}
      {p}
    </Fragment>
  ));
}
