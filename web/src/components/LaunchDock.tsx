import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import type { ReactNode } from "react";
import { LaunchTerminal } from "@/components/LaunchTerminal";
import type { Status } from "@/components/LaunchTerminal";

// LaunchDock owns every live embedded terminal at the APP level, so a
// terminal survives the "Continue in…" modal closing and can be minimized
// without being destroyed. The core discipline: a launched `LaunchTerminal`
// is mounted ONCE at a stable position in this provider's tree and NEVER
// conditionally re-rendered into a different parent — because unmounting it
// tears down the websocket, and the server reaps the child on ws-disconnect.
// "Minimize" therefore only toggles CSS (the panel goes off-screen, the ws +
// process stay alive); "Stop & close" is the sole destructive path.
//
// UX (docs/session-handoff.md launch section, Tier 1):
//   - one session shown expanded at a time (a dim backdrop, like the modal);
//   - backdrop click, or Escape when the TUI isn't focused, → MINIMIZE;
//   - Escape while the live terminal is focused → goes to the TUI (the
//     embedded tool needs Escape — we never hijack it);
//   - Cmd/Ctrl-. → minimize from the keyboard even while focused;
//   - minimized sessions live as pills bottom-right; click to restore, × to
//     stop & close;
//   - a beforeunload guard warns before a tab-close/refresh kills a live
//     session (true detach/reattach across reloads is the Tier 2 follow-up).

/** A launched session the dock owns. token is the opaque launch handle. */
export type DockSession = { token: string; tool: string; sessionId: string };

/** One row of GET /api/launch/sessions (dashboard.LaunchInfo wire shape). */
type LaunchInfoWire = {
  token: string;
  subcommand?: string;
  session_id?: string;
  exited?: boolean;
};

type LaunchDockCtx = {
  /** Register a freshly launched session and show it expanded. */
  launch: (s: DockSession) => void;
};

const Ctx = createContext<LaunchDockCtx | null>(null);

export function LaunchDockProvider({ children }: { children: ReactNode }) {
  const [sessions, setSessions] = useState<DockSession[]>([]);
  const [activeToken, setActiveToken] = useState<string | null>(null);
  const [statuses, setStatuses] = useState<Record<string, Status>>({});

  const launch = useCallback((s: DockSession) => {
    setSessions((prev) =>
      prev.some((p) => p.token === s.token) ? prev : [...prev, s],
    );
    setActiveToken(s.token);
  }, []);

  const minimize = useCallback(() => setActiveToken(null), []);
  const restore = useCallback((token: string) => setActiveToken(token), []);

  const closeSession = useCallback((token: string) => {
    // Drop the session → its LaunchTerminal unmounts → ws closes → server
    // reaps the child. This is the ONLY destructive path.
    setSessions((prev) => prev.filter((p) => p.token !== token));
    setStatuses((prev) => {
      const next = { ...prev };
      delete next[token];
      return next;
    });
    setActiveToken((cur) => (cur === token ? null : cur));
  }, []);

  const setStatus = useCallback((token: string, s: Status) => {
    setStatuses((prev) => (prev[token] === s ? prev : { ...prev, [token]: s }));
  }, []);

  // Rehydrate on load: re-discover live terminal sessions the server kept
  // alive across a tab-close/refresh (Tier 2). Each becomes a MINIMIZED pill
  // (we don't steal focus by auto-expanding); its TerminalHost mounts the
  // LaunchTerminal off-screen, which reconnects the ws and replays the recent
  // output ring in the background. Dedup by handle so this never duplicates a
  // session already added via launch().
  useEffect(() => {
    let cancelled = false;
    fetch("/api/launch/sessions")
      .then((r) => (r.ok ? r.json() : null))
      .then((data: { sessions?: LaunchInfoWire[] } | null) => {
        if (cancelled || !data?.sessions) return;
        const live = data.sessions.filter((s) => s && !s.exited && s.token);
        if (live.length === 0) return;
        setSessions((prev) => {
          const known = new Set(prev.map((p) => p.token));
          const add: DockSession[] = live
            .filter((s) => !known.has(s.token))
            .map((s) => ({
              token: s.token,
              tool: s.subcommand || "terminal",
              sessionId: s.session_id || "",
            }));
          return add.length ? [...prev, ...add] : prev;
        });
      })
      .catch(() => {
        /* dashboard without the launch seam (503) — nothing to rehydrate */
      });
    return () => {
      cancelled = true;
    };
  }, []);

  // Warn before a tab-close/refresh silently kills a still-running session.
  useEffect(() => {
    const anyLive = sessions.some((s) => {
      const st = statuses[s.token];
      return st === "open" || st === "connecting";
    });
    if (!anyLive) return;
    const onBeforeUnload = (e: BeforeUnloadEvent) => {
      e.preventDefault();
      e.returnValue = "";
    };
    window.addEventListener("beforeunload", onBeforeUnload);
    return () => window.removeEventListener("beforeunload", onBeforeUnload);
  }, [sessions, statuses]);

  const value = useMemo<LaunchDockCtx>(() => ({ launch }), [launch]);

  return (
    <Ctx.Provider value={value}>
      {children}
      {sessions.map((s) => (
        <TerminalHost
          key={s.token}
          session={s}
          expanded={s.token === activeToken}
          status={statuses[s.token]}
          onMinimize={minimize}
          onClose={() => closeSession(s.token)}
          onStatus={(st) => setStatus(s.token, st)}
        />
      ))}
      <Dock
        sessions={sessions}
        activeToken={activeToken}
        statuses={statuses}
        onRestore={restore}
        onClose={closeSession}
      />
    </Ctx.Provider>
  );
}

export function useLaunchDock(): LaunchDockCtx {
  const v = useContext(Ctx);
  if (!v) throw new Error("useLaunchDock must be used within LaunchDockProvider");
  return v;
}

// TerminalHost renders a single session's LaunchTerminal at a STABLE tree
// position. Expanded = a centered panel over a dim backdrop; minimized =
// the same element parked off-screen (kept mounted + full-size so the PTY
// dimensions and scrollback survive, and restore is instant). Only the
// wrapper className changes between the two — LaunchTerminal itself never
// remounts, so the ws + child process persist across minimize/restore.
function TerminalHost({
  session,
  expanded,
  status,
  onMinimize,
  onClose,
  onStatus,
}: {
  session: DockSession;
  expanded: boolean;
  status: Status | undefined;
  onMinimize: () => void;
  onClose: () => void;
  onStatus: (s: Status) => void;
}) {
  const boxRef = useRef<HTMLDivElement | null>(null);
  const isLive = status === "open" || status === "connecting";

  // Keyboard: while expanded, Cmd/Ctrl-. always minimizes; Escape minimizes
  // UNLESS the live terminal is focused (then it belongs to the TUI).
  useEffect(() => {
    if (!expanded) return;
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === ".") {
        e.preventDefault();
        onMinimize();
        return;
      }
      if (e.key === "Escape") {
        const box = boxRef.current;
        const focusedInTerminal =
          !!box && box.contains(document.activeElement);
        if (focusedInTerminal && isLive) return; // let the TUI have Escape
        onMinimize();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [expanded, isLive, onMinimize]);

  return (
    <div
      className={
        expanded
          ? "fixed inset-0 z-[80] flex items-center justify-center bg-black/50 p-6"
          : "pointer-events-none fixed left-[-100000px] top-0 h-[60vh] w-[880px]"
      }
      onClick={expanded ? onMinimize : undefined}
      role={expanded ? "dialog" : undefined}
      aria-modal={expanded ? true : undefined}
      aria-hidden={expanded ? undefined : true}
      aria-label={expanded ? `${session.tool} terminal` : undefined}
    >
      <div
        ref={boxRef}
        className="pointer-events-auto flex w-[880px] max-w-[95vw] flex-col"
        onClick={(e) => e.stopPropagation()}
      >
        <LaunchTerminal
          token={session.token}
          tool={session.tool}
          expanded={expanded}
          onMinimize={onMinimize}
          onClose={onClose}
          onStatus={onStatus}
        />
      </div>
    </div>
  );
}

// Dock renders a pill per minimized session bottom-right. The currently
// expanded session has no pill (it's on screen). Click a pill to restore;
// × stops & closes it.
function Dock({
  sessions,
  activeToken,
  statuses,
  onRestore,
  onClose,
}: {
  sessions: DockSession[];
  activeToken: string | null;
  statuses: Record<string, Status>;
  onRestore: (token: string) => void;
  onClose: (token: string) => void;
}) {
  const pills = sessions.filter((s) => s.token !== activeToken);
  if (pills.length === 0) return null;
  return (
    <div className="fixed bottom-4 right-4 z-[70] flex flex-col-reverse gap-2">
      {pills.map((s) => (
        <DockPill
          key={s.token}
          session={s}
          status={statuses[s.token]}
          onRestore={() => onRestore(s.token)}
          onClose={() => onClose(s.token)}
        />
      ))}
    </div>
  );
}

function DockPill({
  session,
  status,
  onRestore,
  onClose,
}: {
  session: DockSession;
  status: Status | undefined;
  onRestore: () => void;
  onClose: () => void;
}) {
  const dot: Record<Status, string> = {
    connecting: "bg-fg-3 animate-pulse",
    open: "bg-ok",
    exited: "bg-fg-3",
    error: "bg-danger",
  };
  const label: Record<Status, string> = {
    connecting: "connecting…",
    open: "live",
    exited: "exited",
    error: "error",
  };
  const st = status ?? "connecting";
  return (
    <div className="flex items-center gap-2 rounded-full border bg-bg-1 py-1 pl-3 pr-1.5 shadow-lg">
      <button
        type="button"
        onClick={onRestore}
        title={`Restore ${session.tool} terminal`}
        className="flex items-center gap-2 text-[11px] text-fg-2 hover:text-fg-1 focus:outline-none"
      >
        <span className={`h-2 w-2 rounded-full ${dot[st]}`} />
        <span className="font-mono text-fg-1">{session.tool}</span>
        <span className="text-[9.5px] uppercase tracking-[0.05em] text-fg-3">
          {label[st]}
        </span>
        <span aria-hidden className="text-fg-3">
          ▴
        </span>
      </button>
      <button
        type="button"
        onClick={() => {
          // Closing kills the process — confirm while it's still live, same
          // as the terminal header's "Stop & close".
          if (
            (st === "open" || st === "connecting") &&
            !window.confirm(
              `Stop the running ${session.tool} session? This ends the process.`,
            )
          ) {
            return;
          }
          onClose();
        }}
        title={
          st === "open" ? "Stop the running process and close" : "Close"
        }
        className="rounded-full px-1.5 text-[11px] text-fg-3 hover:bg-white/10 hover:text-fg-1 focus:outline-none"
      >
        ✕
      </button>
    </div>
  );
}
