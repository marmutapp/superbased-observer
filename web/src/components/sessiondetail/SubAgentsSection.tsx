import { useCallback, useState } from "react";
import { ChartState } from "@/components/ChartState";
import { useApi } from "@/lib/useApi";
import { fmtCompact, fmtInt, fmtUSD } from "@/lib/format";
import { fmtDate } from "@/components/sessiondetail/shared";

// SubAgentsSection — per-sub-agent breakdown for sessions whose sub-agent
// activity rides the PARENT's session row flagged is_sidechain (the
// claude-code same-session model, migration 010). Codex child sessions
// surface through the LineageBanner instead (separate session rows).
//
// Token/cost rollups (input_tokens/output_tokens/cost_usd) ride the same
// windows since migration 087 flagged token_usage.is_sidechain; they are
// omitted (zero) until a post-087 ingest or a `observer scan --force`
// re-parse heals pre-existing transcripts.
//
// Same lazy UX as ProcessesSection: collapsed by default, the open/closed
// state persists in localStorage, and a collapsed section makes NO request.

const SECTION_OPEN_KEY = "sb_subagents_section_open";

type SubagentSummary = {
  id?: string;
  label: string;
  type?: string;
  start: string;
  end?: string;
  open: boolean;
  action_count: number;
  error_count: number;
  input_tokens?: number;
  output_tokens?: number;
  cache_read_tokens?: number;
  cost_usd?: number;
};

type SessionSubagentsResponse = {
  session_id: string;
  total: number;
  subagents: SubagentSummary[];
};

export function SubAgentsSection({ sessionId }: { sessionId: string | null }) {
  const [open, setOpen] = useState<boolean>(() => {
    try {
      return localStorage.getItem(SECTION_OPEN_KEY) === "1";
    } catch {
      return false;
    }
  });
  const toggleOpen = useCallback(() => {
    setOpen((v) => {
      const next = !v;
      try {
        localStorage.setItem(SECTION_OPEN_KEY, next ? "1" : "0");
      } catch {
        /* ignore */
      }
      return next;
    });
  }, []);

  // Lazy-load: only fetch once the section is open. A closed section makes
  // no request (useApi skips null paths).
  const subs = useApi<SessionSubagentsResponse>(
    open && sessionId ? `/api/session/${sessionId}/subagents` : null,
    undefined,
    [sessionId, open],
  );
  const data = subs.data;
  const rows = data?.subagents ?? [];

  const summary = data
    ? `${fmtInt(data.total)} sub-agent${data.total === 1 ? "" : "s"}${
        rows.some((r) => r.open) ? " · some still running" : ""
      }`
    : open
      ? "Loading…"
      : "click to load inline sub-agent activity";

  return (
    <section className="space-y-2">
      <h3>
        <button
          type="button"
          onClick={toggleOpen}
          className="flex w-full items-center justify-between gap-2 text-left focus:outline-none"
          aria-expanded={open}
        >
          <span className="flex items-center gap-1.5 text-[11px] font-semibold uppercase tracking-[0.06em] text-fg-3">
            <span className="select-none text-fg-3">{open ? "▾" : "▸"}</span>
            Sub-agents
          </span>
          <span className="text-[10.5px] text-fg-3">{summary}</span>
        </button>
      </h3>

      {open && (
        <ChartState
          loading={subs.loading && !data}
          error={subs.error}
          empty={Boolean(data) && rows.length === 0}
          emptyHint="No inline sub-agent activity on this session."
        >
          <ul className="flex flex-col gap-1.5">
            {rows.map((r, i) => (
              <li
                key={`${r.id ?? "window"}-${i}`}
                className="rounded-md border border-line-2 bg-bg-2 px-3 py-2"
              >
                <div className="flex items-center justify-between gap-2">
                  <span className="flex items-center gap-2 text-[12px] font-medium text-fg-1">
                    {r.label}
                    {r.type && (
                      <span className="rounded-pill border border-line-2 bg-bg-3 px-[7px] py-[1px] text-[10px] font-semibold text-fg-2">
                        {r.type}
                      </span>
                    )}
                    {r.open && (
                      <span className="rounded-pill border border-accent/30 bg-accent-soft px-[7px] py-[1px] text-[10px] font-semibold text-accent">
                        running
                      </span>
                    )}
                  </span>
                  <span className="text-[10.5px] text-fg-3">
                    {r.action_count} action{r.action_count === 1 ? "" : "s"}
                    {r.error_count > 0 && (
                      <span className="ml-1 text-warn">· {r.error_count} failed</span>
                    )}
                  </span>
                </div>
                <div className="mt-0.5 text-[10.5px] text-fg-3">
                  {fmtDate(r.start)}
                  {r.end ? ` → ${fmtDate(r.end)}` : " → …"}
                  {((r.input_tokens ?? 0) > 0 || (r.output_tokens ?? 0) > 0) && (
                    <span className="ml-2">
                      · {fmtCompact(r.input_tokens)} in / {fmtCompact(r.output_tokens)} out
                    </span>
                  )}
                  {(r.cost_usd ?? 0) > 0 && (
                    <span className="ml-1.5" title={fmtUSD(r.cost_usd, true)}>
                      · ≈{fmtUSD(r.cost_usd)}
                    </span>
                  )}
                </div>
              </li>
            ))}
          </ul>
        </ChartState>
      )}
    </section>
  );
}
