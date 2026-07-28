import { Fragment, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { AnimatePresence, motion } from "framer-motion";
import { createPortal } from "react-dom";
import clsx from "clsx";
import {
  ComboChip,
  Pill,
  SegmentedControl,
  SlideOver,
  ToolBadge,
  Tooltip,
  TruncatedPath,
  type ComboOption,
} from "@/components/primitives";
import { ChartState } from "@/components/ChartState";
import { CopyOnClick } from "@/components/CopyOnClick";
import { ProcessesSection } from "@/components/ProcessesSection";
import { CacheExpiryCard } from "@/components/CacheExpiryCard";
import { VerbosityCard } from "@/components/VerbosityCard";
import { HandoffCard } from "@/components/HandoffCard";
import { JumpInButton } from "@/components/JumpInButton";
import { ResumeButton } from "@/components/ResumeButton";
import { HelpInd } from "@/components/HelpInd";
import { Pagination } from "@/components/DataTable";
import { actionMeta, mcpIdentity } from "@/lib/actions";
import {
  DollarIcon,
  LightningIcon,
} from "@/components/icons";
import { fetchJSON } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import {
  fmtClock,
  fmtCompact,
  fmtDuration,
  fmtInt,
  fmtUSD,
} from "@/lib/format";
import type {
  ActionBucket,
  ActionFullText,
  CacheForecastResponse,
  CacheForecastWarning,
  MessageRow,
  PredictBand,
  PredictResponse,
  PredictWarning,
  PricingDefaultsResponse,
  SessionCacheAnnotation,
  SessionCacheResponse,
  SessionDetail,
  SessionMessages,
  SessionRawEvents,
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
const RAW_EVENTS_LIMIT = 50;

// ----- Messages table sort ----------------------------------------
//
// Sorting is SERVER-SIDE (/api/session/<id>/messages?sort_by&sort_dir) so it
// addresses the whole timeline rather than the current page — the point of
// the feature is that "time descending" puts the newest message at row 1 of
// page 1 while a live session is running, instead of appending it to the
// last page.
type MessageSortKey =
  | "seq"
  | "timestamp"
  | "message_id"
  | "role"
  | "model"
  | "effort_level"
  | "input"
  | "cache_read"
  | "cache_creation"
  | "output"
  | "elapsed_ms"
  | "tokens_per_sec"
  | "tool_call_count"
  | "ai_cost_usd"
  | "tool_cost_usd"
  | "cost_usd"
  | "content";
type SortDir = "asc" | "desc";

// The default reproduces the endpoint's historical chronological order; the
// server treats it as the identity permutation.
const DEFAULT_MSG_SORT: MessageSortKey = "seq";
const DEFAULT_MSG_DIR: SortDir = "asc";

// MESSAGE_COLUMNS is the single source of truth for the Messages table
// header: one row per column, in table order, carrying its sort key, label,
// alignment and optional tooltip. Adding a column is one row here plus one
// row in the server's messageSortKeys table — never a new conditional.
const MESSAGE_COLUMNS: {
  key: MessageSortKey;
  label: string;
  right?: boolean;
  className?: string;
  tooltip?: string;
  tooltipMaxWidth?: number;
}[] = [
  { key: "seq", label: "#", className: "pl-3" },
  { key: "timestamp", label: "Time" },
  { key: "message_id", label: "Msg ID" },
  { key: "role", label: "Role" },
  { key: "model", label: "Model" },
  {
    key: "effort_level",
    label: "Effort",
    tooltip:
      "Reasoning effort — codex collaboration_mode.settings.reasoning_effort, antigravity SKU-encoded low/medium/high, etc. Empty for adapters that don't expose an effort knob (Anthropic models, copilot, etc.); those rows sort to the bottom in both directions. Sorts by intensity (low → high), not alphabetically.",
    tooltipMaxWidth: 420,
  },
  { key: "input", label: "In", right: true },
  { key: "cache_read", label: "Cache R", right: true },
  { key: "cache_creation", label: "Cache W", right: true },
  { key: "output", label: "Out", right: true },
  { key: "elapsed_ms", label: "Elapsed", right: true },
  {
    key: "tokens_per_sec",
    label: "Tok/s",
    right: true,
    tooltip:
      "Output tokens per second — this turn's output tokens ÷ its elapsed time. Only output (generated) tokens count. Blank for turns with no output or no timing; those rows sort to the bottom in both directions.",
    tooltipMaxWidth: 420,
  },
  { key: "tool_call_count", label: "Tools", right: true },
  { key: "ai_cost_usd", label: "API $", right: true },
  { key: "tool_cost_usd", label: "Tool $", right: true },
  { key: "cost_usd", label: "Total $", right: true },
  { key: "content", label: "Content", className: "pl-3 pr-3" },
];

// watchFollowEdge decides which edge of the table live watch-mode should
// track under the active sort. The chat-follow default assumes "newest is
// last" — true only for the chronological ascending orders. Under a
// chronological DESCENDING sort the newest row is at the TOP (page 1), which
// is the whole point of sorting time descending during a live session. Under
// any other sort the position of a new row is unpredictable, so we follow
// nothing rather than yank the operator's viewport around.
function watchFollowEdge(
  sortBy: MessageSortKey,
  sortDir: SortDir,
): "bottom" | "top" | "none" {
  if (sortBy !== "seq" && sortBy !== "timestamp") return "none";
  return sortDir === "asc" ? "bottom" : "top";
}

export function SessionDetailPanel({
  sessionId,
  open,
  watch = false,
  onClose,
  onOpenSession,
}: {
  sessionId: string | null;
  open: boolean;
  // watch = open the panel in read-only "watch live" mode: the messages
  // stream tail-follows the newest page, polling faster (4s vs 8s), for
  // bare (non-attachable) live sessions. Poll-based v1 on the existing
  // /messages endpoint — no WS, no after_id cursor (noted follow-up).
  watch?: boolean;
  onClose: () => void;
  // onOpenSession navigates the host page to another session's detail —
  // reused by the codex fork/subagent lineage banner to open a parent or
  // spawned session. Absent → lineage links render non-interactive.
  onOpenSession?: (id: string) => void;
}) {
  // watchMode is seeded from the `watch` prop but locally toggleable —
  // "Stop" in the indicator (and "Watch instead" on JumpInButton) flip it
  // without needing a URL round-trip. Re-seeds on prop / session change.
  const [watchMode, setWatchMode] = useState(watch);
  useEffect(() => {
    setWatchMode(watch);
  }, [watch, sessionId]);
  // stickToEdge drives chat-follow etiquette: auto-scroll to the newest
  // message only while the reader is pinned to the followed edge (the bottom
  // under the chronological default, the TOP under a time-descending sort —
  // see watchFollowEdge). Scrolling away releases the stick; scrolling back
  // re-engages it. Reset on session change, when (re)entering watch mode, and
  // on any SORT change — a sort change re-windows the whole timeline (it also
  // resets to page 1), and under follow="none" sorts handleScroll early-returns
  // so the flag would otherwise freeze at whatever it held when the operator
  // left the chronological order and never re-engage on the way back.
  const [stickToEdge, setStickToEdge] = useState(true);
  // Live-capture refresh while the slide-over is open: 8s on both
  // the detail rollup and the message stream. Calmer than the 2s
  // first-pass cadence — 2s was visibly choppy because every
  // background refetch toggled useApi's loading state, blanking the
  // rendered content. With useApi's silent-refetch tweak in place,
  // 8s gives steady progress without UI churn. Pauses when the tab
  // is hidden via the default visibility gate in useApi.
  // Watch mode polls faster (4s) so the tail stays close to live; the
  // calm 8s cadence remains the default for normal reading.
  const liveRefresh = open
    ? { refreshMs: watchMode ? 4000 : 8000 }
    : undefined;
  const detail = useApi<SessionDetail>(
    sessionId ? `/api/session/${sessionId}` : null,
    undefined,
    [sessionId],
    liveRefresh,
  );
  const [msgPage, setMsgPage] = useState(1);
  // tokenDetail controls per-message token grouping:
  //   - "turn": one message row per user-turn (claudecode msg.ID for
  //     Anthropic, codex turn boundary). Tokens for all model inferences
  //     within a turn are summed into that row.
  //   - "inference": one message row per token_count event (codex
  //     v1.7.24+). Each row carries the per-inference last_token_usage.
  //     For claudecode this is identical to "turn" since the watcher
  //     already emits one row per Anthropic msg_xxx.
  // null = no explicit operator choice yet → derive the default from the
  // tool (see effectiveDetail): codex defaults to "inference" (its
  // per-inference rows are the informative grain — turn collapses them),
  // every other tool defaults to "turn". An explicit toggle pins the
  // choice. Reset to null + page 1 on session navigation.
  const [tokenDetail, setTokenDetail] = useState<"turn" | "inference" | null>(
    null,
  );
  // focusMid is the message a Processes-panel link asked to jump to; the
  // MessagesTable scrolls to + highlights the row with this message_id.
  const [focusMid, setFocusMid] = useState<string | null>(null);
  const [showRawEvents, setShowRawEvents] = useState(false);
  const [rawPage, setRawPage] = useState(1);
  // Messages-table sort. Lifted here (next to msgPage) so it survives the
  // auto-refresh poll and so it can be fed into the request params. Held as
  // one object so the click handler's updater stays pure (a nested setState
  // would double-toggle under StrictMode).
  const [msgSort, setMsgSort] = useState<{ by: MessageSortKey; dir: SortDir }>({
    by: DEFAULT_MSG_SORT,
    dir: DEFAULT_MSG_DIR,
  });
  // See stickToEdge above: re-arm chat-follow on session / watch-mode / sort
  // change. Declared here because it depends on msgSort.
  useEffect(() => {
    setStickToEdge(true);
  }, [sessionId, watchMode, msgSort.by, msgSort.dir]);
  useEffect(() => {
    setMsgPage(1);
    setTokenDetail(null);
    setFocusMid(null);
    setShowRawEvents(false);
    setRawPage(1);
    setMsgSort({ by: DEFAULT_MSG_SORT, dir: DEFAULT_MSG_DIR });
  }, [sessionId]);
  // onSortMessages — click a header: a new column selects it (ascending,
  // except Time which starts descending so "newest first" is one click
  // away); clicking the active column toggles direction. Any change resets
  // to page 1, since the sort re-windows the whole timeline.
  const onSortMessages = useCallback((key: MessageSortKey) => {
    setMsgSort((s) =>
      s.by === key
        ? { by: key, dir: s.dir === "asc" ? "desc" : "asc" }
        : { by: key, dir: key === "timestamp" ? "desc" : "asc" },
    );
    setMsgPage(1);
  }, []);
  const followEdge = watchFollowEdge(msgSort.by, msgSort.dir);
  // Effective grain: an explicit toggle wins; otherwise codex defaults to
  // per-inference, everything else to turn rollup.
  const effectiveDetail: "turn" | "inference" =
    tokenDetail ?? (detail.data?.tool === "codex" ? "inference" : "turn");
  // browserSession gates the chat-bubble rendering of assistant_message /
  // user_prompt rows to browser-captured chat sessions (every browserchat
  // tool — chatgpt-web / claude-web / perplexity-web / gemini-web /
  // copilot-web — ends in "-web"). For those the response text lives in
  // raw_tool_output and IS the on-screen answer to render inline; coding-
  // agent rows keep their existing dense-table rendering untouched.
  const browserSession = (detail.data?.tool ?? "").endsWith("-web");
  const offset = (msgPage - 1) * MESSAGES_LIMIT;
  const messageParams: Record<string, string | number> = {
    limit: MESSAGES_LIMIT,
    offset,
  };
  if (effectiveDetail === "inference") {
    messageParams.detail = "inference";
  }
  // Sort travels with every request (including the auto-refresh poll) so the
  // live stream honours the operator's chosen order. Omitted entirely on the
  // default so the request stays byte-identical to the pre-sort client.
  if (msgSort.by !== DEFAULT_MSG_SORT || msgSort.dir !== DEFAULT_MSG_DIR) {
    messageParams.sort_by = msgSort.by;
    messageParams.sort_dir = msgSort.dir;
  }
  const messages = useApi<SessionMessages>(
    sessionId ? `/api/session/${sessionId}/messages` : null,
    messageParams,
    [sessionId, msgPage, effectiveDetail, msgSort.by, msgSort.dir],
    liveRefresh,
  );
  const rawOffset = (rawPage - 1) * RAW_EVENTS_LIMIT;
  const rawEvents = useApi<SessionRawEvents>(
    showRawEvents && sessionId ? `/api/session/${sessionId}/raw-events` : null,
    { limit: RAW_EVENTS_LIMIT, offset: rawOffset },
    [sessionId, showRawEvents, rawPage],
    undefined,
  );

  // onFocusMessage — a Processes-panel msg link: ensure the page containing the
  // message is loaded (the /messages ?locate= override returns its offset),
  // then highlight it. The highlight auto-clears after 3s.
  //
  // The probe MUST ask in the same shape as the rendered table, or the offset
  // it returns addresses a page the table never shows: the server resolves
  // ?locate= against its EFFECTIVE (post-sort, post-grain) row list, so a
  // chronological probe under an active sort — or a turn-grain probe while the
  // table is on inference grain — hands back an offset for a different row
  // set, the target row is absent from the page that loads, and the
  // scrollIntoView silently no-ops. Reuse the exact params the table's own
  // request carries, minus offset (locate replaces it).
  const onFocusMessage = useCallback(
    async (mid: string) => {
      if (!sessionId) return;
      const onPage = messages.data?.messages.some((m) => m.message_id === mid);
      if (!onPage) {
        try {
          const sorted =
            msgSort.by !== DEFAULT_MSG_SORT || msgSort.dir !== DEFAULT_MSG_DIR;
          const r = await fetchJSON<SessionMessages>(
            `/api/session/${sessionId}/messages`,
            {
              locate: mid,
              limit: MESSAGES_LIMIT,
              // undefined params are dropped by buildUrl, so the default
              // grain / default sort still send the historical query.
              detail: effectiveDetail === "inference" ? "inference" : undefined,
              sort_by: sorted ? msgSort.by : undefined,
              sort_dir: sorted ? msgSort.dir : undefined,
            },
          );
          setMsgPage(Math.floor((r.offset ?? 0) / MESSAGES_LIMIT) + 1);
        } catch {
          /* ignore — still try to highlight on the current page */
        }
      }
      setFocusMid(mid);
    },
    [sessionId, messages.data, effectiveDetail, msgSort.by, msgSort.dir],
  );
  useEffect(() => {
    if (!focusMid) return;
    const t = setTimeout(() => setFocusMid(null), 3000);
    return () => clearTimeout(t);
  }, [focusMid]);

  // Tail-follow: in watch mode, keep the messages view pinned to the page new
  // rows land on — but only while the reader is stuck to the followed edge
  // (paginating away releases it). Which page that is depends on the sort:
  // the LAST page under the chronological default, page 1 under a
  // time-descending sort (newest first). Under any other sort a new row's
  // page is unpredictable, so we pin nothing. total is the whole-session
  // count the endpoint returns regardless of the current offset.
  const totalMsgs = messages.data?.total ?? 0;
  const lastMsgPage = Math.max(1, Math.ceil(totalMsgs / MESSAGES_LIMIT));
  const followPage = followEdge === "top" ? 1 : lastMsgPage;
  useEffect(() => {
    if (!watchMode || !stickToEdge || totalMsgs === 0) return;
    if (followEdge === "none") return;
    setMsgPage((p) => (p === followPage ? p : followPage));
  }, [watchMode, stickToEdge, totalMsgs, followPage, followEdge]);

  return (
    <SlideOver
      open={open}
      onClose={onClose}
      width={1680}
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
      subtitle={
        detail.data?.project ? (
          <TruncatedPath value={detail.data.project} className="text-[11.5px]" />
        ) : undefined
      }
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
              <LineageBanner d={detail.data} onOpenSession={onOpenSession} />
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
              <CacheExpiryCard sessionId={detail.data.id} />
              {detail.data.cache_summary && (
                <CachePanel
                  sessionId={detail.data.id}
                  summary={detail.data.cache_summary}
                />
              )}
              <PredictorCard sessionId={detail.data.id} />
              <VerbosityCard sessionId={detail.data.id} />
              <JumpInButton
                sessionId={detail.data.id}
                tool={detail.data.tool}
                watchable={watch === true || sessionRecentlyActive(detail.data)}
                onWatch={() => setWatchMode(true)}
              />
              <ResumeButton
                sessionId={detail.data.id}
                tool={detail.data.tool}
                resume={detail.data.resume}
              />
              <HandoffCard
                sessionId={detail.data.id}
                tool={detail.data.tool}
              />
              {detail.data.model && (
                <ForecastWidget sessionId={detail.data.id} />
              )}
            </>
          )}
        </ChartState>

        <ProcessesSection
          sessionId={sessionId}
          onFocusMessage={onFocusMessage}
        />

        <RawEventsPanel
          open={showRawEvents}
          onToggle={() => {
            setShowRawEvents((v) => !v);
            setRawPage(1);
          }}
          data={rawEvents.data}
          loading={rawEvents.loading}
          error={rawEvents.error}
          page={rawPage}
          onPage={setRawPage}
        />

        <section className="space-y-2">
          <h3 className="flex items-center justify-between gap-2">
            <span className="text-[11px] font-semibold uppercase tracking-[0.06em] text-fg-3">
              Messages
            </span>
            <span className="flex items-center gap-3">
              {watchMode && (
                <span className="flex items-center gap-1.5 rounded-pill border border-success/30 bg-success-soft px-2 py-0.5 text-[10px] font-semibold lowercase tracking-[0.02em] text-success">
                  <span className="relative h-1.5 w-1.5 rounded-full bg-success">
                    <span className="absolute inset-0 animate-ping rounded-full bg-success/50" />
                  </span>
                  Watching live — read-only
                  <button
                    type="button"
                    onClick={() => setWatchMode(false)}
                    className="ml-1 text-fg-3 underline hover:text-fg-1 focus:outline-none"
                    title="Stop watching — return to normal 8s refresh and free navigation."
                  >
                    stop
                  </button>
                </span>
              )}
              {/* Group toggle is meaningful for codex (v1.7.24+ emits
                  one token row per model inference). Codex DEFAULTS to
                  "inference" (its informative grain — turn collapses
                  the per-inference rows). For Anthropic adapters
                  (claudecode et al.) "inference" is identical to "turn"
                  — the watcher already emits one row per upstream
                  msg_xxx — so the toggle is a no-op and stays hidden. */}
              {detail.data?.tool === "codex" && (
                <>
                  <Tooltip content="Group token rows by model inference (default for codex — one row per token_count event) or by user-turn (sums each turn's inferences). Tool calls always stay grouped at the turn level. Tok/s is more accurate in Turn view: a single inference has no measured duration, so per-inference rows show “—”, while a turn spans its inferences' timestamps.">
                    <span>
                      <SegmentedControl
                        size="sm"
                        value={effectiveDetail}
                        onChange={(v) =>
                          setTokenDetail(v as "turn" | "inference")
                        }
                        options={[
                          { value: "turn", label: "Turn" },
                          { value: "inference", label: "Inference" },
                        ]}
                      />
                    </span>
                  </Tooltip>
                  {effectiveDetail === "inference" && (
                    <button
                      type="button"
                      onClick={() => setTokenDetail("turn")}
                      className="text-[10.5px] text-accent hover:underline focus:outline-none"
                      title="Per-inference rows have no measured duration; switch to Turn for accurate Tok/s."
                    >
                      Tok/s? use Turn
                    </button>
                  )}
                </>
              )}
              <span className="text-[10.5px] text-fg-3">
                {messages.data
                  ? `${fmtInt(messages.data.total)} total · click row to expand tool calls`
                  : "Loading…"}
              </span>
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
              <MessagesTable
                rows={messages.data.messages}
                focusMid={focusMid}
                browser={browserSession}
                watch={watchMode}
                stick={stickToEdge}
                onStickChange={setStickToEdge}
                follow={followEdge}
                sortBy={msgSort.by}
                sortDir={msgSort.dir}
                onSort={onSortMessages}
              />
            )}
          </ChartState>
          {messages.data && messages.data.total > MESSAGES_LIMIT && (
            <Pagination
              page={msgPage}
              limit={MESSAGES_LIMIT}
              total={messages.data.total}
              onPage={(p) => {
                setMsgPage(p);
                // In watch mode, paginating off the followed page releases
                // the tail-follow stick; landing back on it re-engages.
                // The followed page is page 1 under a time-descending sort.
                if (watchMode) {
                  setStickToEdge(
                    followEdge === "top" ? p === 1 : p >= lastMsgPage,
                  );
                }
              }}
              loading={messages.loading}
            />
          )}
        </section>
      </div>
    </SlideOver>
  );
}

function RawEventsPanel({
  open,
  onToggle,
  data,
  loading,
  error,
  page,
  onPage,
}: {
  open: boolean;
  onToggle: () => void;
  data: SessionRawEvents | null;
  loading: boolean;
  error: Error | null;
  page: number;
  onPage: (page: number) => void;
}) {
  return (
    <section className="space-y-2">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 className="text-[11px] font-semibold uppercase tracking-[0.06em] text-fg-3">
          Source rows
        </h3>
        <div className="flex items-center gap-3">
          {data && (
            <span className="text-[10.5px] text-fg-3">
              {fmtInt(data.total)} rows · {fmtInt(data.sources.length)} sources
            </span>
          )}
          <button
            type="button"
            onClick={onToggle}
            className="rounded-2 border border-line-2 bg-bg-2 px-2.5 py-1 text-[11px] font-medium text-fg-1 hover:border-accent/40 hover:text-accent focus:outline-none focus:ring-2 focus:ring-accent-ring"
          >
            {open ? "Hide all data" : "View all data"}
          </button>
        </div>
      </div>
      {open && (
        <>
          <ChartState
            loading={loading && !data}
            error={error}
            empty={!data?.rows.length}
            emptyHint="No source JSONL rows found for this session."
            height={140}
          >
            {data && (
              <div className="overflow-hidden rounded-2 border border-line-2 bg-bg-1">
                <div className="border-b border-line-2 bg-bg-2 px-3 py-2">
                  <div className="flex flex-wrap gap-2">
                    {data.sources.map((source, i) => (
                      <Tooltip key={`${source.path}-${i}`} content={source.path}>
                        <span
                          tabIndex={0}
                          className={clsx(
                            "max-w-full truncate rounded-2 border px-2 py-0.5 font-mono text-[10.5px] focus:outline-none",
                            source.error
                              ? "border-warn/40 text-warn"
                              : "border-line-2 text-fg-2",
                          )}
                        >
                          {i + 1}: {source.path.split("/").pop()} · {fmtInt(source.rows)}
                        </span>
                      </Tooltip>
                    ))}
                  </div>
                </div>
                <div className="max-h-[520px] overflow-auto">
                  <table className="w-full min-w-[980px] text-left text-[12px]">
                    <thead className="sticky top-0 z-[1] border-b border-line-2 bg-bg-2 text-[10px] uppercase tracking-[0.06em] text-fg-3">
                      <tr>
                        <th className="w-[86px] px-3 py-2">Line</th>
                        <th className="w-[170px] px-2 py-2">Type</th>
                        <th className="w-[160px] px-2 py-2">ID</th>
                        <th className="w-[180px] px-2 py-2">Time</th>
                        <th className="px-2 py-2">Row</th>
                      </tr>
                    </thead>
                    <tbody className="divide-y divide-line-1">
                      {data.rows.map((row) => (
                        <tr key={`${row.source_index}:${row.line}:${row.byte_offset}`} className="align-top">
                          <td className="px-3 py-2 font-mono text-[11px] text-fg-3">
                            {row.source_index + 1}:{row.line}
                          </td>
                          <td className="px-2 py-2">
                            <div className="flex flex-wrap gap-1">
                              {row.type && <Pill variant="neutral">{row.type}</Pill>}
                              {row.payload_type && row.payload_type !== row.type && (
                                <Pill variant="neutral">{row.payload_type}</Pill>
                              )}
                              {row.role && <Pill variant="neutral">{row.role}</Pill>}
                              {!row.valid_json && <Pill variant="warn">not json</Pill>}
                            </div>
                          </td>
                          <td className="max-w-[160px] truncate px-2 py-2 font-mono text-[11px] text-fg-2">
                            {row.event_id || "—"}
                          </td>
                          <td className="px-2 py-2 font-mono text-[11px] text-fg-3">
                            {row.timestamp || "—"}
                          </td>
                          <td className="px-2 py-2">
                            <CopyOnClick value={row.excerpt} className="block">
                              <pre className="max-h-[160px] overflow-auto whitespace-pre-wrap break-words rounded-1 bg-bg-0 p-2 font-mono text-[11px] leading-5 text-fg-1">
                                {row.excerpt}
                                {row.excerpt_truncated ? "\n...(truncated)" : ""}
                              </pre>
                            </CopyOnClick>
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              </div>
            )}
          </ChartState>
          {data && data.total > RAW_EVENTS_LIMIT && (
            <Pagination
              page={page}
              limit={RAW_EVENTS_LIMIT}
              total={data.total}
              onPage={onPage}
              loading={loading}
            />
          )}
        </>
      )}
    </section>
  );
}

// ----- Codex fork/subagent lineage (migration 069) -----------------

// shortSid is the panel's short-session-id convention (mirrors the
// header's `id.slice(0, 8)`), used for the lineage badge labels.
function shortSid(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id;
}

// lineagePillCls matches the Pill primitive's visual tokens so the
// clickable lineage badges read as chips consistent with the rest of the
// panel (border + soft bg + 10px semibold), while remaining <button>s.
const lineagePillCls =
  "inline-flex items-center gap-1 rounded-pill border px-[7px] py-[1px] text-[10px] font-semibold leading-[1.4] tracking-[0.02em] transition-colors";

// LineageBanner surfaces a codex session's fork/subagent lineage: an
// upward "parent" badge (subagent-of / forked-from) and a downward list
// of the sessions spawned from this one. Renders nothing for a normal
// (non-lineage) session. Reuses onOpenSession to navigate.
function LineageBanner({
  d,
  onOpenSession,
}: {
  d: SessionDetail;
  onOpenSession?: (id: string) => void;
}) {
  const [expanded, setExpanded] = useState(false);
  const isSubagent = d.thread_source === "subagent";
  const parentId = d.forked_from_id || d.parent_thread_id || "";
  const hasParent = parentId !== "";
  const children = d.children ?? [];
  if (!hasParent && children.length === 0) return null;

  const canOpenParent = hasParent && d.parent_in_db && !!onOpenSession;
  const parentLabel = isSubagent
    ? `Subagent of ${shortSid(parentId)}`
    : `⑂ Forked from ${shortSid(parentId)}`;

  return (
    <div className="mb-3 flex flex-col gap-2">
      {hasParent && (
        <div>
          <button
            type="button"
            disabled={!canOpenParent}
            onClick={
              canOpenParent ? () => onOpenSession!(parentId) : undefined
            }
            title={
              !d.parent_in_db
                ? "Parent session not in this database"
                : !onOpenSession
                  ? "Session navigation unavailable here"
                  : `Open parent session ${shortSid(parentId)}`
            }
            className={clsx(
              lineagePillCls,
              isSubagent
                ? "border-accent/30 bg-accent-soft text-accent"
                : "border-info/30 bg-info-soft text-info",
              canOpenParent
                ? "cursor-pointer hover:brightness-110 focus:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent-ring)]"
                : "cursor-not-allowed opacity-55",
            )}
          >
            {parentLabel}
          </button>
        </div>
      )}
      {children.length > 0 && (
        <div className="rounded-md border border-line-2 bg-bg-2 px-3 py-2">
          <button
            type="button"
            onClick={() => setExpanded((v) => !v)}
            className="flex w-full items-center justify-between text-left text-[11px] font-medium text-fg-2 hover:text-fg-1 focus:outline-none"
          >
            <span>
              Spawned {children.length}{" "}
              {children.every((c) => c.thread_source === "subagent")
                ? "subagent"
                : children.some((c) => c.thread_source === "subagent")
                  ? "subagent/fork"
                  : "fork"}{" "}
              {children.length === 1 ? "session" : "sessions"}
            </span>
            <span className="font-mono text-fg-3">{expanded ? "−" : "+"}</span>
          </button>
          {expanded && (
            <ul className="mt-2 flex flex-col gap-1">
              {children.map((c) => {
                const openable = !!onOpenSession;
                return (
                  <li key={c.id}>
                    <button
                      type="button"
                      disabled={!openable}
                      onClick={
                        openable ? () => onOpenSession!(c.id) : undefined
                      }
                      title={
                        openable
                          ? `Open session ${shortSid(c.id)}`
                          : "Session navigation unavailable here"
                      }
                      className={clsx(
                        "flex w-full items-center gap-2 rounded px-2 py-1 text-left text-[11px] transition-colors",
                        openable
                          ? "cursor-pointer hover:bg-bg-3 focus:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent-ring)]"
                          : "cursor-default",
                      )}
                    >
                      <span
                        className={clsx(
                          lineagePillCls,
                          c.thread_source === "subagent"
                            ? "border-accent/30 bg-accent-soft text-accent"
                            : "border-info/30 bg-info-soft text-info",
                        )}
                      >
                        {c.thread_source === "subagent" ? "subagent" : "fork"}
                      </span>
                      <span className="font-mono text-fg-2">
                        {shortSid(c.id)}…
                      </span>
                    </button>
                  </li>
                );
              })}
            </ul>
          )}
        </div>
      )}
    </div>
  );
}

// ----- KPI band ----------------------------------------------------

function KpiBand({ d }: { d: SessionDetail }) {
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

// PredictorCard — the Next-Message Cost & Limit Predictor. Two composed
// halves: the cost band (always on where the session has token data) and
// the limit gauge (proxy-gated; renders a help-icon "needs proxy" state
// until the proxy captures the provider's rate-limit headers).
// Backed by GET /api/session/<id>/predict (pure read-side math over
// token_usage — no new tables for the estimate half).
function PredictorCard({ sessionId }: { sessionId: string }) {
  const predict = useApi<PredictResponse>(
    `/api/session/${sessionId}/predict`,
    undefined,
    [sessionId],
  );

  return (
    <section className="mt-5 rounded-3 border bg-bg-2 px-4 py-3">
      <div className="flex items-center justify-between gap-2">
        <span className="flex items-center gap-1.5 text-[11px] font-semibold uppercase tracking-[0.06em] text-fg-3">
          Next-message cost predictor
          <HelpInd id="glossary.cost_predictor" />
        </span>
      </div>
      <p className="mt-1 text-[10.5px] text-fg-3">
        Estimated cost of your next message on this session's current model —
        a low / typical / high range over the message's likely turn fan-out.
      </p>

      <ChartState
        loading={predict.loading && !predict.data}
        error={predict.error}
        empty={!predict.data}
        emptyHint="Loading estimate…"
        height={80}
      >
        {predict.data && <PredictorBody data={predict.data} />}
      </ChartState>
    </section>
  );
}

function PredictorBody({ data }: { data: PredictResponse }) {
  const est = data.estimate;
  return (
    <div className="mt-2 space-y-3">
      {est.has_estimate ? (
        <>
          <div className="grid grid-cols-3 gap-2">
            <PredictBandStat
              label="Low"
              sub="quick reply"
              band={est.low}
              prefixTokens={est.prefix_tokens}
            />
            <PredictBandStat
              label="Typical"
              sub="median message"
              band={est.mid}
              prefixTokens={est.prefix_tokens}
              highlight
            />
            <PredictBandStat
              label="High"
              sub="agentic loop"
              band={est.high}
              prefixTokens={est.prefix_tokens}
            />
          </div>
          <div className="flex flex-wrap items-center gap-1.5">
            <PredictTurnsTierPill tier={est.turns_tier} />
            {(est.warnings ?? [])
              .filter((w) => w !== "turns_inferred_prior" && w !== "turns_inferred_default")
              .map((w) => (
                <PredictWarningPill key={w} kind={w} />
              ))}
          </div>
          <div className="text-[10.5px] text-fg-3">
            <span className="font-mono text-fg-2">{est.model}</span> · cached
            prefix {fmtCompact(est.prefix_tokens)} tok re-read — and billed —
            each turn ·{" "}
            {est.turns_tier === "observed"
              ? `${fmtInt(est.sample_messages)} messages observed`
              : "fan-out inferred (no per-message boundaries on this session)"}
          </div>
        </>
      ) : (
        <div className="rounded-3 border border-dashed bg-bg-1 px-3 py-2.5 text-[11px] text-fg-2">
          <span className="font-medium text-fg-1">No cost estimate yet.</span>{" "}
          {data.reason ||
            "This session has no model/token data captured."}{" "}
          Route this client through the observer proxy (<code className="font-mono text-[10.5px] text-fg-2">observer init</code>) so future
          messages capture token &amp; cost data.
        </div>
      )}

      {/* Limit gauge — proxy-gated half. */}
      <PredictLimitSection limit={data.limit} />
    </div>
  );
}

// fmtPredictUSD keeps the three band headlines decimal-consistent: 2
// decimals normally (so $0.66 sits cleanly next to $3.82), escalating to
// finer precision only for sub-cent amounts that would otherwise round to
// $0.00.
function fmtPredictUSD(n: number): string {
  if (!Number.isFinite(n)) return "—";
  if (n > 0 && n < 0.01) return fmtUSD(n, true);
  return fmtUSD(n);
}

// predictBilledTokens is the token counterpart of the priced formula
// (per_turn = P·r_cache_read + S·r_input + O·r_output; message = T ×
// per_turn). It counts exactly the dimensions the price counts, so the
// tokens and the dollars agree: the cached prefix P is re-read — and
// billed at the cache-read rate — on EVERY one of the T turns, so it is
// counted T times. `fresh` is the genuinely-new half (S + O per turn).
// Cache-WRITE tokens are not modelled by the estimator, so they are not
// counted here either (disclosed in the tooltip + glossary).
function predictBilledTokens(band: PredictBand, prefixTokens: number) {
  const turns = Math.max(0, band.turns);
  const cached = Math.round(turns * Math.max(0, prefixTokens));
  const fresh = Math.round(
    turns * (Math.max(0, band.fresh_input) + Math.max(0, band.output)),
  );
  return { billed: cached + fresh, cached, fresh };
}

function PredictBandStat({
  label,
  sub,
  band,
  prefixTokens,
  highlight,
}: {
  label: string;
  sub: string;
  band: PredictBand;
  prefixTokens: number;
  highlight?: boolean;
}) {
  const tok = predictBilledTokens(band, prefixTokens);
  return (
    <div
      className={clsx(
        "flex flex-col rounded-3 border px-2.5 py-2",
        highlight ? "border-accent bg-accent-soft" : "bg-bg-1",
      )}
    >
      <span className="text-[10px] font-medium uppercase tracking-[0.05em] text-fg-3">
        {label}
      </span>
      <span
        className={clsx(
          "tabular-nums text-[15px] font-semibold leading-tight",
          highlight ? "text-accent" : "text-fg-1",
        )}
      >
        {fmtPredictUSD(band.message_usd)}
      </span>
      <span className="mt-0.5 text-[10px] text-fg-3">
        ~{band.turns} turns · {fmtCompact(band.output)} out/turn
      </span>
      <span
        className="text-[10px] tabular-nums text-fg-3"
        title={
          `Billed tokens = turns × (cached prefix + fresh input + output) = ` +
          `${band.turns} × (${fmtInt(prefixTokens)} + ${fmtInt(band.fresh_input)} + ${fmtInt(band.output)}) ` +
          `≈ ${fmtInt(tok.billed)}. Of that, ${fmtInt(tok.cached)} is the SAME cached prefix ` +
          `re-read (and billed at the cache-read rate) on every turn — only ${fmtInt(tok.fresh)} is new. ` +
          `This is throughput, not context size. Cache-WRITE tokens are not included.`
        }
      >
        <span className="text-fg-2">{fmtCompact(tok.billed)} tok</span> billed ·{" "}
        {fmtCompact(tok.fresh)} new
      </span>
      <span className="text-[10px] text-fg-3">{sub}</span>
    </div>
  );
}

function PredictTurnsTierPill({ tier }: { tier: PredictResponse["estimate"]["turns_tier"] }) {
  if (tier === "observed") {
    return <Pill variant="success">fan-out: observed</Pill>;
  }
  if (tier === "prior") {
    return <Pill variant="info">fan-out: inferred (similar sessions)</Pill>;
  }
  if (tier === "default") {
    return <Pill variant="neutral">fan-out: inferred (default)</Pill>;
  }
  return null;
}

function PredictWarningPill({ kind }: { kind: PredictWarning }) {
  const labels: Partial<Record<PredictWarning, string>> = {
    empty_prefix: "no cache prefix yet",
    fast_mode_active: "model in fast tier (2×)",
    no_session_history: "no history",
  };
  const label = labels[kind];
  if (!label) return null;
  const variant: "warn" | "neutral" = kind === "fast_mode_active" ? "warn" : "neutral";
  return <Pill variant={variant}>{label}</Pill>;
}

// PredictLimitSection renders the 5h/weekly limit gauge. Source is either
// the proxy (Anthropic response headers) or the tool's own session log
// (codex token_count rate_limits — "from session log"). When neither is
// available it shows the help-icon "route through the proxy to unlock"
// state (Anthropic) or the "provider exposes no window" note.
function PredictLimitSection({ limit }: { limit: PredictResponse["limit"] }) {
  return (
    <div className="rounded-3 border bg-bg-1 px-3 py-2.5">
      <div className="flex items-center gap-1.5">
        <span className="text-[10.5px] font-semibold uppercase tracking-[0.06em] text-fg-3">
          5-hour / weekly limit
        </span>
        <HelpInd id="glossary.limit_gauge" />
        {limit.needs_proxy && <Pill variant="neutral">needs proxy</Pill>}
        {limit.available && limit.source === "transcript" && (
          <Pill variant="neutral">from session log</Pill>
        )}
        {limit.available && limit.observed_age && (
          <span className="text-[10px] text-fg-3">· {limit.observed_age}</span>
        )}
      </div>
      {limit.available ? (
        <div className="mt-1.5 grid grid-cols-2 gap-3 text-[11px]">
          <LimitWindowStat
            label="5-hour window"
            util={limit.window_5h_util}
            reset={limit.window_5h_reset}
          />
          <LimitWindowStat
            label="Weekly cap"
            util={limit.window_7d_util}
            reset={limit.window_7d_reset}
          />
        </div>
      ) : limit.no_window ? (
        <p className="mt-1 text-[10.5px] text-fg-3">
          This provider doesn't expose a 5-hour / weekly subscription window in
          its response headers, so only the cost estimate above is available.
        </p>
      ) : (
        <p className="mt-1 text-[10.5px] text-fg-3">
          Route this client through the observer proxy (
          <code className="font-mono text-[10.5px] text-fg-2">observer init</code>
          ) to see how much of your 5-hour and weekly subscription window each
          message consumes. These numbers live only in the provider's response
          headers, which the proxy is the only component able to read.
        </p>
      )}
    </div>
  );
}

function LimitWindowStat({
  label,
  util,
  reset,
}: {
  label: string;
  util?: number;
  reset?: number;
}) {
  const pct = util != null ? Math.round(util * 100) : null;
  const remaining = pct != null ? 100 - pct : null;
  return (
    <div className="flex flex-col">
      <span className="text-[10px] font-medium uppercase tracking-[0.05em] text-fg-3">
        {label}
      </span>
      <span className="tabular-nums text-[12.5px] font-semibold text-fg-1">
        {remaining != null ? `${remaining}% left` : "—"}
      </span>
      {reset != null && (
        <span className="mt-0.5 text-[10px] text-fg-3">
          resets {fmtResetClock(reset)}
        </span>
      )}
    </div>
  );
}

// fmtResetClock renders a unix-seconds reset timestamp as a short
// relative countdown ("in 1h52m") falling back to a local time.
function fmtResetClock(unixSec: number): string {
  const ms = unixSec * 1000;
  const delta = ms - Date.now();
  if (delta <= 0) return "now";
  const mins = Math.round(delta / 60000);
  if (mins < 60) return `in ${mins}m`;
  const h = Math.floor(mins / 60);
  const m = mins % 60;
  return `in ${h}h${m > 0 ? `${m}m` : ""}`;
}

// ForecastWidget — operator-facing surface for the model-switch
// cost forecaster. Pick a candidate model from the dropdown
// (populated from the cost engine's baked-in pricing table via
// /api/config/pricing/defaults), fetch /api/session/<id>/cache/
// forecast?model=X, render the headline numbers (switch_cost,
// break_even, savings/turn, net_savings) + the closed-set warning
// list as pills.
//
// The picker is a ComboChip — searchable single-select with
// keyboard nav over ~180 pricing-table entries. Family-grouped
// labels surface the model family ("claude / opus 4.7") so the
// operator doesn't need to memorize the exact 20251001 suffix.
// Right-aligned per-row metadata shows the input rate ($/M) so
// the operator sees the price gradient at a glance during the
// pick (the forecast itself is the source of truth for the
// downstream numbers; this is just an anchor to inform the choice).
function ForecastWidget({ sessionId }: { sessionId: string }) {
  const [candidate, setCandidate] = useState("");
  const [submitted, setSubmitted] = useState<string | null>(null);
  // Pricing defaults — cached at module level via useApi's
  // built-in identity; the dropdown only needs the model id list,
  // not the per-bucket rates (the forecast call re-derives those
  // server-side). We DO surface the input rate as right-aligned
  // metadata so the operator sees cheap vs expensive at a glance
  // during the pick.
  const pricing = useApi<PricingDefaultsResponse>(
    "/api/config/pricing/defaults",
    undefined,
    [],
  );

  const forecast = useApi<CacheForecastResponse>(
    submitted ? `/api/session/${sessionId}/cache/forecast?model=${encodeURIComponent(submitted)}` : null,
    undefined,
    [sessionId, submitted],
  );

  // Build the dropdown options once per pricing fetch. Sort by
  // (family, model id) so the operator sees claude-/gpt-/gemini-/
  // … grouped naturally — ComboChip's flat list + the per-row
  // family prefix in the label reads like an optgroup without
  // needing a separate primitive.
  const options = useMemo<ComboOption[]>(() => {
    if (!pricing.data?.defaults) return [];
    const entries = Object.entries(pricing.data.defaults);
    entries.sort((a, b) => a[0].localeCompare(b[0]));
    return entries.map(([modelId, p]) => {
      const inputRate = p?.input ?? 0;
      const family = familyOf(modelId);
      return {
        value: modelId,
        // Label shows the family as a muted prefix + the bare model
        // id so a scan column-aligns: "claude   opus-4-7-20251001".
        label: (
          <span className="flex w-full items-baseline gap-1.5">
            {family && (
              <span className="text-[10px] uppercase tracking-[0.05em] text-fg-3">
                {family}
              </span>
            )}
            <span className="font-mono">{stripFamilyPrefix(modelId, family)}</span>
          </span>
        ),
        searchable: modelId.toLowerCase(),
        rightMeta: inputRate > 0 ? `$${inputRate}/M` : undefined,
        title: `Input ${inputRate}/M · Output ${p?.output ?? 0}/M · Cache R ${p?.cache_read ?? 0}/M · Cache W ${p?.cache_creation ?? 0}/M`,
      };
    });
  }, [pricing.data]);

  const onForecast = () => {
    if (candidate) setSubmitted(candidate);
  };

  return (
    <section className="mt-5 rounded-3 border bg-bg-2 px-4 py-3">
      <div className="flex items-center justify-between gap-2">
        <span className="flex items-center gap-1.5 text-[11px] font-semibold uppercase tracking-[0.06em] text-fg-3">
          Next-message cost forecast · model switch
          <HelpInd id="glossary.forecaster_math" />
        </span>
      </div>
      <p className="mt-1 text-[10.5px] text-fg-3">
        Per-turn cost of switching this session's model — works for any session
        with token data; observed cache (claude-code/codex) additionally refines
        the one-time switch cost.
      </p>
      <div className="mt-2 flex flex-wrap items-center gap-2">
        <label className="text-[11px] text-fg-2">Switch to:</label>
        <ComboChip
          label="model"
          value={candidate}
          onChange={setCandidate}
          options={options}
          popoverWidth={360}
          placeholder={
            pricing.loading
              ? "loading pricing table…"
              : `search ${options.length} models…`
          }
          emptyHint={
            pricing.error
              ? "Pricing table failed to load."
              : pricing.loading
                ? "loading pricing…"
                : "No models match."
          }
          buttonValueRender={(selected) =>
            selected ? (
              <b className="font-mono text-fg-0">{selected.value}</b>
            ) : (
              <span className="font-mono text-fg-3">pick a model…</span>
            )
          }
        />
        <button
          type="button"
          onClick={onForecast}
          disabled={!candidate}
          className="rounded-3 border border-accent bg-accent-soft px-2.5 py-1 text-[11px] font-medium text-accent hover:bg-accent disabled:cursor-not-allowed disabled:opacity-50"
        >
          Forecast
        </button>
      </div>

      {submitted && (
        <ChartState
          loading={forecast.loading}
          error={forecast.error}
          empty={!forecast.data}
          emptyHint="Forecast unavailable."
          height={80}
        >
          {forecast.data && <ForecastResultPanel data={forecast.data} />}
        </ChartState>
      )}
    </section>
  );
}

// familyOf maps a model id to its family prefix so the dropdown
// can label the row with a muted family chip. Returns "" when the
// id doesn't match a known family — the row renders without the
// chip and the full id shows mono-spaced.
function familyOf(modelId: string): string {
  if (modelId.startsWith("claude-")) return "claude";
  if (modelId.startsWith("gpt-")) return "gpt";
  if (modelId.startsWith("gemini-")) return "gemini";
  if (modelId.startsWith("deepseek")) return "deepseek";
  if (modelId.startsWith("o1") || modelId.startsWith("o3") || modelId.startsWith("o4")) {
    return "openai-o";
  }
  if (modelId.startsWith("babbage") || modelId.startsWith("davinci")) return "openai-legacy";
  if (modelId.startsWith("text-")) return "openai-text";
  if (modelId.startsWith("kilo-")) return "kilo";
  return "";
}

// stripFamilyPrefix removes the family prefix from the model id so
// the dropdown row reads as `claude  opus-4-7-20251001` rather than
// `claude  claude-opus-4-7-20251001`. When family is empty the
// full id is returned untouched.
function stripFamilyPrefix(modelId: string, family: string): string {
  if (!family) return modelId;
  // openai-o / openai-legacy / openai-text are virtual groupings —
  // their model ids don't actually start with the family label. Skip
  // the strip in those cases.
  if (family.startsWith("openai")) return modelId;
  const prefix = family + "-";
  return modelId.startsWith(prefix) ? modelId.slice(prefix.length) : modelId;
}

function ForecastResultPanel({ data }: { data: CacheForecastResponse }) {
  const neverPays = (data.warnings ?? []).includes("switch_never_pays_off");
  return (
    <div className="mt-2 space-y-2 text-[11px]">
      <div className="grid grid-cols-2 gap-3 lg:grid-cols-5">
        <ForecastStat label="Switch cost" value={fmtUSD(data.switch_cost_usd)} />
        <ForecastStat
          label="Break-even"
          value={neverPays ? "never" : `${fmtInt(data.break_even_turns)} turns`}
          warn={neverPays}
        />
        <ForecastStat
          label="Savings / turn"
          value={fmtUSD(data.savings_per_turn_usd)}
          warn={data.savings_per_turn_usd < 0}
        />
        <ForecastStat
          label="Est. net savings"
          value={fmtUSD(data.estimated_net_savings_usd)}
          warn={data.estimated_net_savings_usd < 0}
          sub={`over ${fmtInt(data.estimated_remaining_turns)} more turns`}
        />
        <ForecastStat
          label="Prefix tokens"
          value={fmtCompact(data.current_prefix_tokens)}
          sub={`avg suffix ${fmtCompact(data.avg_suffix_tokens)} / out ${fmtCompact(data.avg_output_tokens)}`}
        />
      </div>
      {(data.warnings ?? []).length > 0 && (
        <div className="flex flex-wrap items-center gap-1.5 pt-1">
          {(data.warnings ?? []).map((w) => (
            <ForecastWarningPill key={w} kind={w} />
          ))}
        </div>
      )}
      <div className="text-[10.5px] text-fg-3">
        {data.current_model} → {data.candidate_model} · per-turn{" "}
        {fmtUSD(data.per_turn_before_usd)} → {fmtUSD(data.per_turn_after_usd)}
      </div>
    </div>
  );
}

function ForecastStat({
  label,
  value,
  sub,
  warn,
}: {
  label: string;
  value: string;
  sub?: string;
  warn?: boolean;
}) {
  return (
    <div className="flex flex-col">
      <span className="text-[10px] font-medium uppercase tracking-[0.05em] text-fg-3">
        {label}
      </span>
      <span
        className={clsx(
          "tabular-nums text-[12.5px] font-semibold",
          warn ? "text-warn" : "text-fg-1",
        )}
      >
        {value}
      </span>
      {sub && (
        <span className="mt-0.5 text-[10px] text-fg-3">{sub}</span>
      )}
    </div>
  );
}

function ForecastWarningPill({ kind }: { kind: CacheForecastWarning }) {
  const labels: Record<CacheForecastWarning, string> = {
    cache_wont_engage: "candidate cache won't engage yet",
    fast_mode_active: "current model in fast tier (2×)",
    try_1h_tier: "session has > 5m gaps — try 1h TTL",
    switch_never_pays_off: "switch never recoups cold-write cost",
    empty_prefix: "no cache built yet on this session",
  };
  const variant: "warn" | "neutral" =
    kind === "switch_never_pays_off" || kind === "cache_wont_engage" ? "warn" : "neutral";
  return <Pill variant={variant}>{labels[kind]}</Pill>;
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

// ----- Messages table ---------------------------------------------

// tokensPerSec is OUTPUT tokens per second: the turn's output_tokens
// divided by its elapsed time. Only output tokens count (input / cache
// tokens are not generated, so they don't belong in a generation-rate).
// Returns null when there's no output or no/zero timing (don't fabricate
// a rate).
// tokensPerSec is OUTPUT tokens per second. The denominator (tps_ms) is
// chosen server-side from the best available timing source — proxy
// measured response time, codex intra-turn span, or the gap-to-next
// fallback — and is absent when none applies (then we show "—").
function tokensPerSec(m: MessageRow): number | null {
  if (m.output <= 0 || m.tps_ms == null || m.tps_ms <= 0) return null;
  return m.output / (m.tps_ms / 1000);
}

// tpsBasisLabel describes which timing source backed a Tok/s value, for
// the column's tooltip.
function tpsBasisLabel(basis: MessageRow["tps_basis"]): string {
  switch (basis) {
    case "measured":
      return "measured response time (proxy)";
    case "intra-turn":
      return "intra-turn generation span";
    default:
      return "elapsed (to next message)";
  }
}

// fmtTps keeps one decimal under 10 tok/s (where precision matters) and
// rounds to a whole number above it, suffixed "/s".
function fmtTps(tps: number): string {
  return `${tps >= 10 ? Math.round(tps).toString() : tps.toFixed(1)}/s`;
}

// SortableTh renders one Messages-table header as a control: clickable and
// keyboard-operable (Enter / Space), carrying the DataTable indicator
// convention (↑ active-ascending, ↓ active-descending, · inactive) and
// aria-sort for assistive tech. A tooltip-bearing column keeps its tooltip by
// wrapping the raw <th> — Tooltip clones its child, so it must stay a DOM
// element rather than this component.
function SortableTh({
  col,
  active,
  dir,
  onSort,
}: {
  col: (typeof MESSAGE_COLUMNS)[number];
  active: boolean;
  dir: SortDir;
  onSort: (key: MessageSortKey) => void;
}) {
  const indicator = active ? (dir === "asc" ? "↑" : "↓") : "·";
  const hint = `Sort by ${col.label}${
    active ? (dir === "asc" ? " (ascending)" : " (descending)") : ""
  }`;
  const th = (
    <th
      tabIndex={0}
      aria-sort={active ? (dir === "asc" ? "ascending" : "descending") : "none"}
      // A tooltip-bearing column gets the hint through the Tooltip below; the
      // native title= would fire a SECOND, overlapping popup on hover.
      title={col.tooltip ? undefined : hint}
      onClick={() => onSort(col.key)}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onSort(col.key);
        }
      }}
      className={clsx(
        "cursor-pointer select-none py-1.5 font-medium hover:text-fg-1 focus:outline-none",
        col.right && "text-right",
        col.className,
      )}
    >
      {col.label}
      <span className="ml-1 text-fg-4">{indicator}</span>
    </th>
  );
  if (!col.tooltip) return th;
  return (
    <Tooltip
      content={
        <>
          {col.tooltip}
          <span className="mt-1 block text-fg-3">{hint}.</span>
        </>
      }
      maxWidth={col.tooltipMaxWidth}
    >
      {th}
    </Tooltip>
  );
}

function MessagesTable({
  rows,
  focusMid,
  browser = false,
  watch = false,
  stick = true,
  onStickChange,
  follow = "bottom",
  sortBy = DEFAULT_MSG_SORT,
  sortDir = DEFAULT_MSG_DIR,
  onSort,
}: {
  rows: MessageRow[];
  focusMid?: string | null;
  // browser = the session is a browser-captured chat (browserchat *-web
  // tool). Turns on the chat-bubble rendering of user_prompt /
  // assistant_message rows in the expander. Absent/false for every
  // coding-agent session → rendering is unchanged.
  browser?: boolean;
  // watch = read-only tail-follow mode. Caps the table in a vertical
  // scroll region and auto-pins to the newest row while `stick`; reports
  // scroll-position changes back via onStickChange.
  watch?: boolean;
  stick?: boolean;
  onStickChange?: (atEdge: boolean) => void;
  // follow names which edge live watch-mode tracks under the active sort:
  // "bottom" (chronological ascending — newest last), "top" (chronological
  // descending — newest first), or "none" (any other sort, where a new row's
  // position is unpredictable and yanking the viewport would be hostile).
  follow?: "bottom" | "top" | "none";
  sortBy?: MessageSortKey;
  sortDir?: SortDir;
  onSort?: (key: MessageSortKey) => void;
}) {
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});
  const scrollRef = useRef<HTMLDivElement | null>(null);
  // Scroll the focused message into view once it (and its page) are present.
  useEffect(() => {
    if (!focusMid) return;
    const el = document.getElementById(`msg-row-${focusMid}`);
    el?.scrollIntoView({ behavior: "smooth", block: "center" });
  }, [focusMid, rows]);
  // Chat-follow: after each rows update, if watching and stuck to the
  // followed edge, pin the scroll container to the newest message — the
  // BOTTOM under the chronological default, the TOP under a time-descending
  // sort. follow="none" (any non-chronological sort) never force-scrolls.
  useEffect(() => {
    if (!watch || !stick || follow === "none") return;
    const el = scrollRef.current;
    if (el) el.scrollTop = follow === "top" ? 0 : el.scrollHeight;
  }, [rows, watch, stick, follow]);
  function handleScroll() {
    if (!watch || follow === "none") return;
    const el = scrollRef.current;
    if (!el) return;
    // 40px slack so a near-edge position still counts as "stuck".
    const atEdge =
      follow === "top"
        ? el.scrollTop < 40
        : el.scrollHeight - el.scrollTop - el.clientHeight < 40;
    onStickChange?.(atEdge);
  }
  return (
    <div
      ref={scrollRef}
      onScroll={watch ? handleScroll : undefined}
      className={clsx(
        "overflow-x-auto rounded-2 border border-line-1",
        watch && "max-h-[60vh] overflow-y-auto",
      )}
    >
      <table className="w-full min-w-[1460px] text-left text-[11px]">
        <thead className="bg-bg-3/40 text-[10px] uppercase tracking-[0.06em] text-fg-3">
          <tr className="whitespace-nowrap border-b border-line-2">
            {MESSAGE_COLUMNS.map((col) => (
              <SortableTh
                key={col.key}
                col={col}
                active={sortBy === col.key}
                dir={sortDir}
                onSort={onSort ?? (() => {})}
              />
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((m) => {
            // Key on row IDENTITY, never on the page index: `expanded` is
            // keyed by this, and an index-derived key hands one row's
            // expanded state to whatever row later occupies that slot — which
            // reordering (sort, page, live poll) now does routinely. seq is
            // the server's whole-timeline ordinal, unique per row and carried
            // through every reorder, so it is a safe fallback for the (rare)
            // row with no message_id.
            const key = m.message_id || `seq-${m.seq}`;
            const isOpen = !!expanded[key];
            const hasToolCalls = (m.tool_calls?.length ?? 0) > 0;
            return (
              <Fragment key={key}>
                <tr
                  id={m.message_id ? `msg-row-${m.message_id}` : undefined}
                  className={clsx(
                    "whitespace-nowrap border-b border-line-1 last:border-b-0",
                    hasToolCalls && "cursor-pointer hover:bg-bg-3/40",
                    focusMid &&
                      m.message_id === focusMid &&
                      "bg-accent-soft/40 ring-2 ring-inset ring-accent-ring",
                  )}
                  onClick={() =>
                    hasToolCalls &&
                    setExpanded((s) => ({ ...s, [key]: !s[key] }))
                  }
                >
                  {/* Chronological ordinal from the server — NOT the page
                      index, which would renumber under a non-default sort. */}
                  <td className="py-1 pl-3 tabular-nums text-fg-3">{m.seq}</td>
                  <td className="py-1 whitespace-nowrap tabular-nums text-fg-2" title={m.timestamp}>
                    {fmtClock(m.timestamp)}
                  </td>
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
                  <td className="max-w-[160px] py-1 font-mono text-fg-2">
                    <div className="flex items-center gap-1">
                      {m.model ? (
                        <Tooltip
                          content={<span className="break-all font-mono">{m.model}</span>}
                          maxWidth={360}
                        >
                          <span
                            tabIndex={0}
                            className="block max-w-[120px] truncate cursor-help focus:outline-none"
                          >
                            {m.model}
                          </span>
                        </Tooltip>
                      ) : (
                        "—"
                      )}
                      {m.fast && (
                        <Tooltip
                          content={
                            <span>
                              Served in the provider's low-latency fast tier —
                              Anthropic Opus 4.8 (<code>speed:&quot;fast&quot;</code>) or
                              OpenAI/Codex (<code>service_tier:&quot;priority&quot;</code>).
                              Billed at the model's fast-mode premium (Opus 4.8 &amp;
                              gpt-5.4 2×, gpt-5.5 2.5×).
                            </span>
                          }
                          maxWidth={320}
                        >
                          <span tabIndex={0} className="shrink-0 cursor-help focus:outline-none">
                            <Pill variant="info" title="fast tier (premium price)">
                              ⚡ fast
                            </Pill>
                          </span>
                        </Tooltip>
                      )}
                      {m.service_tier &&
                        !["standard", "default", "auto"].includes(m.service_tier) && (
                          <Pill variant="accent" title="requested/served processing tier">
                            {m.service_tier}
                          </Pill>
                        )}
                      {m.stop_reason && (
                        <Pill
                          // Show every turn's stop_reason; routine completions
                          // (end_turn / tool_use) stay subdued (neutral) while
                          // abnormal ends (max_tokens / refusal / stop_sequence
                          // / pause_turn) stand out in warn.
                          variant={
                            m.stop_reason === "end_turn" || m.stop_reason === "tool_use"
                              ? "neutral"
                              : "warn"
                          }
                          title="how this turn ended (stop_reason)"
                        >
                          {m.stop_reason}
                        </Pill>
                      )}
                    </div>
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
                  <td className="py-1 text-right tabular-nums text-fg-3">
                    {(() => {
                      const tps = tokensPerSec(m);
                      return tps != null ? (
                        <Tooltip
                          content={`${fmtInt(m.output)} output tokens over ${fmtDuration(
                            m.tps_ms ?? 0,
                          )} ${tpsBasisLabel(m.tps_basis)}`}
                        >
                          <span tabIndex={0} className="cursor-help focus:outline-none">
                            {fmtTps(tps)}
                          </span>
                        </Tooltip>
                      ) : (
                        "—"
                      );
                    })()}
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
                    <td colSpan={17} className="px-3 py-2 pl-[50px]">
                      <div className="flex flex-col gap-1.5">
                        {m.tool_calls.map((tc, j) => (
                          <ToolCallRowView
                            key={`${key}-tc-${j}`}
                            tc={tc}
                            browser={browser}
                          />
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

// parseMcpName splits a `mcp__<server>__<tool>` raw_tool_name into its
// server + tool so MCP calls (codeIntel search_symbols/get_relations, chrome,
// workspace, …) can render as a distinct badge instead of hiding as a plain
// tool name. Returns null for non-MCP tools.
// McpBadge highlights an MCP tool call in the timeline (server / tool). It
// takes the already-resolved identity (see mcpIdentity) so it lights up for
// every adapter — the mcp__ names, the colon server:tool forms, and rows
// carrying only the action_type="mcp_call" signal.
function McpBadge({ id, copy }: { id: { server: string; tool: string }; copy: string }) {
  return (
    <CopyOnClick value={copy} className="shrink-0" title="MCP tool call">
      <span className="inline-flex items-center gap-1 rounded-pill border border-accent/40 bg-accent/10 px-2 py-0.5 font-mono text-[10px] font-medium leading-none text-accent">
        <span className="uppercase tracking-[0.06em] opacity-80">MCP</span>
        <span>
          {id.server}
          {id.tool ? ` / ${id.tool}` : ""}
        </span>
      </span>
    </CopyOnClick>
  );
}

type EditHunk = { old: string; neu: string };

// parseEditInput turns an edit_file/write_file raw_tool_input into a renderable
// change: Edit → old→new hunks (incl. MultiEdit's edits[]); Write → the new
// content. Falls back to the raw text/JSON when the shape is unfamiliar so a
// change is never silently hidden.
function parseEditInput(
  actionType: string,
  raw: string,
): { file?: string; hunks: EditHunk[]; content?: string; fallback?: string } {
  let j: Record<string, unknown>;
  try {
    j = JSON.parse(raw) as Record<string, unknown>;
  } catch {
    return { hunks: [], fallback: raw };
  }
  const file = typeof j.file_path === "string" ? j.file_path : undefined;
  if (actionType === "write_file") {
    const content =
      typeof j.content === "string"
        ? j.content
        : typeof j.new_string === "string"
          ? j.new_string
          : undefined;
    return { file, hunks: [], content };
  }
  const hunks: EditHunk[] = [];
  if (Array.isArray(j.edits)) {
    for (const e of j.edits as Array<Record<string, unknown>>) {
      if (e && typeof e.old_string === "string" && typeof e.new_string === "string") {
        hunks.push({ old: e.old_string, neu: e.new_string });
      }
    }
  } else if (typeof j.old_string === "string" && typeof j.new_string === "string") {
    hunks.push({ old: j.old_string, neu: j.new_string });
  }
  if (hunks.length === 0) {
    return { file, hunks: [], fallback: JSON.stringify(j, null, 2) };
  }
  return { file, hunks };
}

// DiffBlock renders one old→new hunk as a minimal line diff: common leading /
// trailing lines shown dim as context, the differing middle as removed (−) /
// added (+). Claude's Edit already passes a minimal changed region, so context
// stays small.
function DiffBlock({ oldText, newText }: { oldText: string; newText: string }) {
  const o = oldText.split("\n");
  const n = newText.split("\n");
  let pre = 0;
  while (pre < o.length && pre < n.length && o[pre] === n[pre]) pre++;
  let suf = 0;
  while (
    suf < o.length - pre &&
    suf < n.length - pre &&
    o[o.length - 1 - suf] === n[n.length - 1 - suf]
  ) {
    suf++;
  }
  const ctxPre = o.slice(0, pre);
  const removed = o.slice(pre, o.length - suf);
  const added = n.slice(pre, n.length - suf);
  const ctxSuf = suf > 0 ? o.slice(o.length - suf) : [];
  const line = (t: string, kind: "ctx" | "del" | "add", key: string) => (
    <div
      key={key}
      className={
        kind === "del"
          ? "bg-danger/10 text-danger"
          : kind === "add"
            ? "bg-ok/10 text-ok"
            : "text-fg-3"
      }
    >
      <span className="select-none opacity-60">
        {kind === "del" ? "- " : kind === "add" ? "+ " : "  "}
      </span>
      {t || " "}
    </div>
  );
  return (
    <pre className="max-h-[360px] overflow-auto px-2 py-1 font-mono text-[10.5px] leading-snug">
      {ctxPre.map((t, i) => line(t, "ctx", "p" + i))}
      {removed.map((t, i) => line(t, "del", "d" + i))}
      {added.map((t, i) => line(t, "add", "a" + i))}
      {ctxSuf.map((t, i) => line(t, "ctx", "s" + i))}
    </pre>
  );
}

// EditChangeView renders the parsed edit/write change (file header + diff or
// full content). Lazily fed the raw_tool_input by ToolCallRowView on expand.
function EditChangeView({ actionType, rawInput }: { actionType: string; rawInput: string }) {
  const parsed = parseEditInput(actionType, rawInput);
  return (
    <div className="ml-6 mt-1 overflow-hidden rounded-1 border border-line-2 bg-bg-1">
      {parsed.file && (
        <div className="border-b border-line-2 px-2 py-1 font-mono text-[10px] text-fg-3">
          {parsed.file}
        </div>
      )}
      {parsed.content != null && (
        <pre className="max-h-[360px] overflow-auto whitespace-pre-wrap break-words px-2 py-1 font-mono text-[10.5px] leading-snug text-ok">
          {parsed.content || " "}
        </pre>
      )}
      {parsed.hunks.map((h, i) => (
        <div key={i} className={i > 0 ? "border-t border-line-2" : ""}>
          <DiffBlock oldText={h.old} newText={h.neu} />
        </div>
      ))}
      {parsed.fallback != null && (
        <pre className="max-h-[360px] overflow-auto whitespace-pre-wrap break-words px-2 py-1 font-mono text-[10.5px] text-fg-2">
          {parsed.fallback}
        </pre>
      )}
    </div>
  );
}

// BrowserChatBubble renders a browser-captured chat turn (browserchat
// *-web adapters) as an actual chat bubble: the user's prompt or the
// assistant's answer as wrapped, readable text — the on-screen content —
// rather than the dense tool-call row used for coding-agent tools. The
// assistant bubble also surfaces the per-turn API-call details
// (request_url / id_source / granularity / token estimates / latency)
// captured in actions.metadata. Falls back to the target preview when a
// turn carried no content (usage_only granularity), and offers the same
// lazy "View full" affordance for bodies that exceeded the inline cap.
function BrowserChatBubble({ tc }: { tc: ToolCallRow }) {
  const isUser = tc.action_type === "user_prompt";
  const [viewOpen, setViewOpen] = useState(false);
  const { fetchOnce } = useActionFullText(tc.action_id);
  // The captured on-screen text: full_text is the (inline-capped) prompt
  // for user rows and the response body for assistant rows. target is the
  // one-line preview the adapter stored; used as a fallback when no
  // content was captured (usage_only granularity).
  const body = tc.full_text || "";
  const preview = tc.target || "";
  const hasBody = body.trim() !== "";
  // "View full" is offered when the inline body was truncated OR a fuller
  // raw_tool_output exists server-side.
  const showView = tc.full_text_elided === true || tc.has_full_output === true;
  // API-call detail chips — only on assistant rows, only where captured.
  const details: { label: string; value: string; title?: string }[] = [];
  if (!isUser) {
    if (tc.request_url)
      details.push({
        label: "url",
        value: tc.request_url,
        title: tc.request_url,
      });
    if (tc.id_source)
      details.push({ label: "id via", value: tc.id_source });
    if (tc.granularity)
      details.push({ label: "granularity", value: tc.granularity });
    if (tc.prompt_tokens_est)
      details.push({
        label: "prompt~",
        value: `${fmtInt(tc.prompt_tokens_est)} tok`,
      });
    if (tc.response_tokens_est)
      details.push({
        label: "response~",
        value: `${fmtInt(tc.response_tokens_est)} tok`,
      });
    if (tc.duration_ms && tc.duration_ms > 0)
      details.push({ label: "latency", value: fmtDuration(tc.duration_ms) });
  }
  return (
    <div
      className={clsx(
        "flex flex-col gap-1.5 rounded-2 px-3 py-2",
        isUser
          ? "border border-accent/30 bg-accent-soft/40"
          : "border border-success/25 bg-success-soft/30",
      )}
      onClick={(e) => e.stopPropagation()}
    >
      <div className="flex items-center gap-2">
        <Pill variant={isUser ? "accent" : "success"}>
          {isUser ? "user" : "assistant"}
        </Pill>
        {tc.raw_tool_name && (
          <span className="font-mono text-[10px] text-fg-3">
            {tc.raw_tool_name}
          </span>
        )}
        {showView && (
          <button
            type="button"
            onClick={(e) => {
              e.stopPropagation();
              setViewOpen(true);
            }}
            title="View full text"
            className="ml-auto shrink-0 rounded-1 border border-line-2 bg-bg-3 px-1.5 py-0.5 font-mono text-[10px] text-fg-3 transition-colors hover:border-accent hover:text-accent"
          >
            View full
          </button>
        )}
      </div>
      {hasBody ? (
        <CopyOnClick
          value={body}
          resolveValue={
            showView
              ? async () => {
                  const v = await fetchOnce();
                  return (isUser ? v.raw_tool_input : v.raw_tool_output) || body;
                }
              : undefined
          }
          className="block"
          title="Click to copy full text"
        >
          <div className="max-h-[420px] overflow-auto whitespace-pre-wrap break-words text-[12px] leading-relaxed text-fg-1">
            {body}
            {tc.full_text_elided ? (
              <span className="text-fg-3"> …(truncated — View full)</span>
            ) : null}
          </div>
        </CopyOnClick>
      ) : preview ? (
        <div className="whitespace-pre-wrap break-words text-[12px] leading-relaxed text-fg-2">
          {preview}
        </div>
      ) : (
        <div className="text-[11px] italic text-fg-3">
          No content captured for this turn (usage-only granularity).
        </div>
      )}
      {details.length > 0 && (
        <div className="mt-0.5 flex flex-wrap items-center gap-x-3 gap-y-1 border-t border-line-1 pt-1.5 text-[10px] text-fg-3">
          {details.map((d) => (
            <span
              key={d.label}
              className="inline-flex items-center gap-1"
              title={d.title}
            >
              <span className="uppercase tracking-[0.05em] text-fg-4">
                {d.label}
              </span>
              <span className="max-w-[280px] truncate font-mono text-fg-2">
                {d.value}
              </span>
            </span>
          ))}
        </div>
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
function ToolCallRowView({
  tc,
  browser = false,
}: {
  tc: ToolCallRow;
  browser?: boolean;
}) {
  // Browser-captured chat: render the user prompt / assistant answer as a
  // proper chat bubble (the on-screen text) rather than the dense tool-call
  // row. Gated on `browser` so coding-agent assistant_message rows keep the
  // existing rendering below untouched.
  if (
    browser &&
    (tc.action_type === "assistant_message" || tc.action_type === "user_prompt")
  ) {
    return <BrowserChatBubble tc={tc} />;
  }
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

  // Edit/Write "View change" — a lazy inline diff. The messages payload
  // carries only the filename for these rows; the actual change lives in
  // raw_tool_input, fetched on demand (same cached fetch as the copy/View
  // buttons) so a long session doesn't preload every diff.
  const isEditWrite = tc.action_type === "edit_file" || tc.action_type === "write_file";
  const [changeOpen, setChangeOpen] = useState(false);
  const [changeInput, setChangeInput] = useState<string | null>(null);
  const [changeErr, setChangeErr] = useState<string | null>(null);
  const toggleChange = (e: { stopPropagation: () => void }) => {
    e.stopPropagation();
    const next = !changeOpen;
    setChangeOpen(next);
    if (next && changeInput == null && changeErr == null) {
      fetchOnce()
        .then((v) => setChangeInput(v.raw_tool_input || ""))
        .catch((err) => setChangeErr(err instanceof Error ? err.message : String(err)));
    }
  };

  // For write/edit rows the inline `excerpt` is the harness result string
  // ("File created successfully at …"), which says nothing about the change.
  // When the input was captured inline (full_text — the raw_tool_input,
  // already returned inline-capped), show a short preview of the new content
  // / first hunk instead, so the actual change is visible without expanding.
  const changePreview = (() => {
    if (!isEditWrite || !tc.full_text) return null;
    const p = parseEditInput(tc.action_type, tc.full_text);
    const body = p.content ?? p.hunks[0]?.neu ?? p.fallback ?? "";
    const lines = body.split("\n");
    const head = lines.slice(0, 3).join("\n").replace(/\s+$/, "");
    if (!head.trim()) return null;
    return { head, more: lines.length > 3 };
  })();

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
        {(() => {
          const mcp = mcpIdentity(tc);
          if (mcp) return <McpBadge id={mcp} copy={tc.raw_tool_name || tc.target || ""} />;
          return tc.raw_tool_name ? (
            <CopyOnClick
              value={tc.raw_tool_name}
              className="font-mono text-[10.5px] text-fg-3"
            >
              {tc.raw_tool_name}
            </CopyOnClick>
          ) : null;
        })()}
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
        {isEditWrite && (
          <button
            type="button"
            onClick={toggleChange}
            title={changeOpen ? "Hide change" : "View the file change"}
            className="shrink-0 rounded-1 border border-line-2 bg-bg-3 px-1.5 py-0.5 font-mono text-[10px] text-fg-3 transition-colors hover:border-accent hover:text-accent"
          >
            {changeOpen ? "▾ Change" : "▸ Change"}
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
      {changePreview ? (
        <div className="ml-6 block font-mono text-[10.5px] leading-snug text-fg-3">
          <span className="block whitespace-pre-wrap break-words opacity-90">
            {changePreview.head}
            {changePreview.more ? "\n…" : ""}
          </span>
        </div>
      ) : tc.excerpt ? (
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
      ) : null}
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
      {changeOpen &&
        (changeErr != null ? (
          <div className="ml-6 mt-1 font-mono text-[10.5px] text-danger">
            failed to load change: {changeErr}
          </div>
        ) : changeInput == null ? (
          <div className="ml-6 mt-1 font-mono text-[10.5px] text-fg-3">loading change…</div>
        ) : changeInput === "" ? (
          <div className="ml-6 mt-1 font-mono text-[10.5px] text-fg-3">
            no change content captured for this action
          </div>
        ) : (
          <EditChangeView actionType={tc.action_type} rawInput={changeInput} />
        ))}
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
    // Highlight an MCP call in the COLLAPSED row too — the McpBadge otherwise
    // only shows once a message is expanded, so mcp__ calls (incl. the
    // observer codeintel tools: search_symbols / get_symbols / get_relations)
    // looked un-highlighted in the default timeline. Surface the first MCP
    // tool call's server/tool badge inline; fall back to the plain label.
    const mcpTc = row.tool_calls.find((t) => mcpIdentity(t));
    if (mcpTc) {
      const id = mcpIdentity(mcpTc)!;
      return (
        <span className="inline-flex min-w-0 items-center gap-1.5">
          <McpBadge id={id} copy={mcpTc.raw_tool_name || mcpTc.target || ""} />
          {mcpTc.target ? (
            <span className="truncate font-mono text-[11px] text-fg-3">
              {truncate(mcpTc.target, 60)}
            </span>
          ) : null}
        </span>
      );
    }
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

// elapsedMillis measures start→end. `end` is ended_at when the session was
// cleanly closed, else last_activity_at (COALESCE'd server-side to the last
// action's timestamp); Date.now() is only the last resort for a session with
// no end AND no recorded activity. This stops a never-closed session from
// reporting start→now (the 583h bug).
function elapsedMillis(start: string, end?: string): number | null {
  const s = new Date(start).getTime();
  if (!Number.isFinite(s)) return null;
  const e = end ? new Date(end).getTime() : Date.now();
  if (!Number.isFinite(e)) return null;
  return Math.max(0, e - s);
}

// sessionRecentlyActive — the cheapest in-file honest signal for whether a
// bare session is worth offering a read-only "Watch" on: NOT cleanly ended,
// and last activity within 15 minutes (matching /api/live's window). The
// ended_at exclusion matters because the server sets last_activity_at to
// ended_at for closed sessions — without it a just-ended session would offer
// "Watch" on a conversation that can no longer move. Uses last_activity_at
// (server COALESCE of the newest action timestamp), falling back to
// started_at for a brand-new session that hasn't logged an action yet. A
// dead/stale session returns false → no watch affordance (honest-disabled).
// Known residual: a session active only through api_turns/token_usage rows
// (no recent actions) can under-report here; the Sessions page's watch pill
// uses the canonical /api/live signal and passes watch=true, which overrides.
function sessionRecentlyActive(d: SessionDetail): boolean {
  if (d.ended_at) return false;
  const iso = d.last_activity_at ?? d.started_at;
  if (!iso) return false;
  const t = new Date(iso).getTime();
  if (!Number.isFinite(t)) return false;
  return Date.now() - t <= 15 * 60 * 1000;
}

// elapsedSub is the Elapsed tile's sub-label. Cleanly closed → the end date.
// Never closed but with recent activity (< 10 min ago) → "session in
// progress". Never closed and stale → the last-activity date, so an old
// unfinished session doesn't misleadingly read as still running.
function elapsedSub(d: SessionDetail): string {
  if (d.ended_at) return fmtDate(d.ended_at);
  const last = d.last_activity_at;
  if (!last) return "session in progress";
  const lastMs = new Date(last).getTime();
  if (Number.isFinite(lastMs) && Date.now() - lastMs < 10 * 60 * 1000) {
    return "session in progress";
  }
  return `last activity ${fmtDate(last)}`;
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
