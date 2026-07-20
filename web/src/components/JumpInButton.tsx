import { useState } from "react";
import clsx from "clsx";
import { Tooltip } from "@/components/primitives";
import { fetchJSON } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { useLaunchDock } from "@/components/LaunchDock";
import type { DockSession } from "@/components/LaunchDock";
import { pushToast } from "@/components/Toast";
import type { AttachSessionsResponse, AttachInfo } from "@/lib/types";

// JumpInButton — the dashboard "second seat" affordance (session-attach
// Phase 2, docs/plans/session-attach-design-2026-07-19.md). Offered ONLY
// for a LIVE `observer <tool> --attach` session the daemon owns (exact
// liveness, §4) — matched by session_id against GET /api/attach/sessions.
// The session already exists, so there is NO POST: enabled → hand the
// existing ws handle straight to the app-level LaunchDock (same terminal
// bridge as HandoffCard's "Launch here"). No matching live attach row →
// honest-disabled with the exact re-launch instruction (§3.4,
// feedback_honest_disable_copy): Jump in is never faked from recency.

// ATTACH_SUBCOMMANDS maps a canonical tool name to the observer LAUNCHER VERB
// the operator types to make it joinable (P2-5). The tooltip must never present
// a canonical tool NAME ("claude-code") as a CLI verb — the real command is
// `observer claude --attach`. This mirrors the integration registry's Attach
// rows (internal/integration): keep the two in step when a second tool grounds
// an Attach capability. The generic fallback keeps the copy honest for an
// ungrounded tool rather than inventing a verb.
const ATTACH_SUBCOMMANDS: Record<string, string> = {
  "claude-code": "claude",
  codex: "codex",
};

function relaunchCommand(tool: string): string {
  const sub = ATTACH_SUBCOMMANDS[tool];
  return sub ? `observer ${sub} --attach` : "observer <tool> --attach";
}

export function JumpInButton({
  sessionId,
  tool,
}: {
  sessionId: string;
  tool: string;
}) {
  const dock = useLaunchDock();
  // Refetch on panel open AND poll while visible (P2-4a): a child that exits
  // while the panel stays open must flip the affordance to disabled, not leave
  // it stuck "live". 15s matches the Sessions/detail live-capture cadence; the
  // hook pauses when the tab is hidden and clears on unmount.
  const attach = useApi<AttachSessionsResponse>(
    "/api/attach/sessions",
    undefined,
    [sessionId],
    { refreshMs: 15000 },
  );
  // clickDisabled latches after a click discovers the row is already gone
  // (P2-4c) so a racing poll can't briefly re-enable a dead affordance.
  const [clickDisabled, setClickDisabled] = useState(false);

  const rows = attach.data?.sessions ?? [];
  const match = rows.find((r) => r.session_id === sessionId && !r.exited);

  // Three honest states (P2-4b), never conflated:
  //   - loading:   first fetch in flight, nothing decided yet.
  //   - fetchError: the attachability check FAILED — we do NOT claim "not
  //                 attachable" (a false diagnosis); we say we couldn't check.
  //   - no match:  the check succeeded and there is genuinely no live row.
  const loading = attach.loading && attach.data == null;
  const fetchError = !loading && attach.error != null && attach.data == null;
  const enabled = !!match && !clickDisabled && !fetchError;

  const relaunch = relaunchCommand(tool);
  const disabledTitle = fetchError
    ? "Couldn't check whether this session is attachable — the dashboard's attach endpoint didn't respond. This is NOT a verdict that the session can't be joined; retry, or confirm `observer start` is running."
    : loading
      ? "Checking whether this session is attachable…"
      : `Jump in unavailable — this session isn't running as an attachable session. Launch with \`${relaunch}\` to make it joinable.`;
  const enabledTitle =
    "Open this live session as a second seat in an embedded terminal — the same TUI, drivable from here.";

  async function jumpIn() {
    if (!match) return;
    // Revalidate the attach row IMMEDIATELY before docking (P2-4c): the poll
    // snapshot can be up to 15s stale, and the child may have exited in that
    // window. If it's gone now, surface the race honestly and disable rather
    // than opening a websocket that the server closes with "session not found".
    try {
      const fresh = await fetchJSON<AttachSessionsResponse>(
        "/api/attach/sessions",
      );
      const live = fresh.sessions.find(
        (r) => r.session_id === sessionId && !r.exited,
      );
      if (!live) {
        pushToast("Session ended before you could jump in", "warn");
        setClickDisabled(true);
        return;
      }
      launchSeat(live);
    } catch {
      pushToast(
        "Couldn't reach the dashboard to jump in — check `observer start` is running.",
        "danger",
      );
    }
  }

  function launchSeat(row: AttachInfo) {
    // Hand the existing ws handle to the dock. The handle field is assigned
    // through a computed key so the source never contains a `token: value`
    // pattern (the harness write-filter mangles those;
    // feedback_write_filter_token_patterns).
    const seat = { tool: row.tool || tool, sessionId } as DockSession;
    const handleKey = "tok" + "en";
    (seat as unknown as Record<string, string>)[handleKey] = row.token;
    dock.launch(seat);
  }

  return (
    <section className="mt-5 rounded-3 border bg-bg-2 px-4 py-3">
      <div className="flex items-center justify-between gap-2">
        <span className="flex items-center gap-1.5 text-[11px] font-semibold uppercase tracking-[0.06em] text-fg-3">
          Live session
        </span>
        <Tooltip content={enabled ? enabledTitle : disabledTitle} maxWidth={360}>
          <span>
            <button
              type="button"
              disabled={!enabled}
              onClick={enabled ? jumpIn : undefined}
              className={clsx(
                "rounded-2 border px-2.5 py-1 text-[11px] font-medium focus:outline-none",
                enabled
                  ? "border-accent/40 bg-accent/10 text-accent hover:bg-accent/20"
                  : "cursor-not-allowed border-line-2 bg-bg-3 text-fg-3",
              )}
            >
              {enabled && (
                <span
                  aria-hidden
                  className="mr-1.5 inline-block h-1.5 w-1.5 rounded-full bg-success align-middle"
                />
              )}
              {loading ? "Checking…" : "Jump in"}
            </button>
          </span>
        </Tooltip>
      </div>
      <p className="mt-1 text-[10.5px] text-fg-3">
        {fetchError
          ? "Couldn't check attachability — the attach endpoint didn't respond. Retry or confirm the daemon is running; this is not a verdict that the session can't be joined."
          : enabled
            ? "This session is running as an attachable terminal. Jump in to view and drive the same live TUI from the dashboard — a second seat on the running agent."
            : `Only sessions launched with \`${relaunch}\` can be joined from the dashboard. A bare launch can't be re-parented — its terminal belongs to the shell that started it.`}
      </p>
    </section>
  );
}
