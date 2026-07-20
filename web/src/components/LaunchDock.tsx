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
import { createPortal } from "react-dom";
import { useNavigate } from "react-router-dom";
import { isRemoteView } from "@/lib/remote";
import { LaunchTerminal } from "@/components/LaunchTerminal";
import type { Status } from "@/components/LaunchTerminal";
import { NewTerminalDialog } from "@/components/NewTerminalDialog";
import {
  useTerminalStatuses,
  AgentStatusBadge,
} from "@/components/useTerminalStatuses";
import {
  REMOTE_TERMINAL_OFF_MSG,
  useRemoteTerminalGate,
} from "@/lib/remoteTerminal";

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
  /** Open the New-terminal dialog (the same one the floating dock's + uses). */
  openNewTerminal: () => void;
  /** The dock-owned live sessions (the Terminal Workspace reads these). */
  sessions: DockSession[];
  /** Per-token connection status. */
  statuses: Record<string, Status>;
  /** Stop & close (destructive — unmount → ws close → server reap). */
  closeSession: (token: string) => void;
  /** Expand a session as the floating panel (undocked view). */
  restore: (token: string) => void;
  /**
   * The Terminal Workspace grid seam (dock-grid design D2): register a grid
   * cell ELEMENT as the docking target for a session. The provider stays the
   * SOLE owner of the mounted LaunchTerminal — its stable host element is
   * imperatively reparented into the cell (a DOM move preserves the live
   * xterm), and reparented back to the floating tree when the cell
   * unregisters (el = null). One cell per token; the grid is a VIEW, never a
   * second owner.
   */
  registerWorkspaceCell: (token: string, el: HTMLElement | null) => void;
  /** True once the boot session-rehydrate has settled (success or failure). */
  sessionsHydrated: boolean;
  /**
   * One-click docking from a floating terminal window. When the Terminal
   * Workspace is mounted its sink receives the token immediately; otherwise
   * the token is queued (pendingDock) and the app navigates to /terminals,
   * where the workspace consumes the queue on mount. Never auto-invoked —
   * docking is always an explicit user gesture (both the floating-window and
   * the grid workflows are first-class).
   */
  requestDock: (token: string) => void;
  /** Workspace-registered dock sink (null on unmount). */
  registerDockSink: (fn: ((token: string) => void) | null) => void;
  /** Tokens queued by requestDock while the workspace was unmounted. */
  pendingDock: string[];
  clearPendingDock: () => void;
};

const Ctx = createContext<LaunchDockCtx | null>(null);

export function LaunchDockProvider({ children }: { children: ReactNode }) {
  const [sessions, setSessions] = useState<DockSession[]>([]);
  const [activeToken, setActiveToken] = useState<string | null>(null);
  const [statuses, setStatuses] = useState<Record<string, Status>>({});
  const [newOpen, setNewOpen] = useState(false);
  // Workspace grid cells: token → the registered cell element the session's
  // stable host is reparented into (dock-grid design D2). Docked sessions
  // hide their floating pill; undocked ones behave exactly as before.
  const [cells, setCells] = useState<Record<string, HTMLElement>>({});
  // True once the boot rehydrate (GET /api/launch/sessions) has settled, so
  // the workspace can tell "still discovering sessions" from "this saved tile
  // is genuinely dead" (tombstone vs pending).
  const [sessionsHydrated, setSessionsHydrated] = useState(false);
  // Explicit-gesture docking plumbing (never automatic): the workspace
  // registers a sink while mounted; requestDock routes through it or queues +
  // navigates to /terminals.
  const dockSinkRef = useRef<((token: string) => void) | null>(null);
  const [pendingDock, setPendingDock] = useState<string[]>([]);
  const navigate = useNavigate();
  // A read-only remote paired device gets no dock gesture: the workspace
  // never registers a sink for it, so the button would minimize + queue an
  // unconsumable token (review MED). Stable per page load.
  const remoteViewer = isRemoteView();

  const launch = useCallback((s: DockSession) => {
    setSessions((prev) =>
      prev.some((p) => p.token === s.token) ? prev : [...prev, s],
    );
    setActiveToken(s.token);
  }, []);

  const minimize = useCallback(() => setActiveToken(null), []);
  const restore = useCallback((token: string) => setActiveToken(token), []);

  const closeSession = useCallback((token: string) => {
    // Reap the server-side process FIRST: since detach-replay, a ws close
    // only DETACHES (the child stays alive for reconnect / idle-reap), so the
    // destructive promise needs the explicit DELETE. Best-effort — the state
    // teardown below proceeds regardless; a failed DELETE leaves the session
    // to rehydrate honestly as a live pill on the next load.
    fetch(`/api/launch/${encodeURIComponent(token)}`, { method: "DELETE" }).catch(() => {
      /* daemon unreachable — nothing to reap */
    });
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

  const registerWorkspaceCell = useCallback(
    (token: string, el: HTMLElement | null) => {
      setCells((prev) => {
        if (el === null) {
          if (!(token in prev)) return prev;
          const next = { ...prev };
          delete next[token];
          return next;
        }
        if (prev[token] === el) return prev;
        return { ...prev, [token]: el };
      });
    },
    [],
  );

  const openNewTerminal = useCallback(() => setNewOpen(true), []);

  const registerDockSink = useCallback(
    (fn: ((token: string) => void) | null) => {
      dockSinkRef.current = fn;
    },
    [],
  );

  const requestDock = useCallback(
    (token: string) => {
      // Close the floating panel either way — the terminal is moving to the
      // grid (or its queue), not the modal.
      setActiveToken((cur) => (cur === token ? null : cur));
      const sink = dockSinkRef.current;
      if (sink) {
        sink(token);
        return;
      }
      setPendingDock((prev) => (prev.includes(token) ? prev : [...prev, token]));
      // Unique param per request: navigating to an identical URL would not
      // re-fire the page's [location.search] effect (review finding — a user
      // who locally switched to the Settings tab would strand the queued
      // dock), so each request carries a fresh nonce.
      navigate(`/terminals?tab=workspace&dock=${Date.now()}`);
    },
    [navigate],
  );

  const clearPendingDock = useCallback(() => setPendingDock([]), []);

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
      })
      .finally(() => {
        if (!cancelled) setSessionsHydrated(true);
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

  const value = useMemo<LaunchDockCtx>(
    () => ({
      launch,
      openNewTerminal,
      sessions,
      statuses,
      closeSession,
      restore,
      registerWorkspaceCell,
      sessionsHydrated,
      requestDock,
      registerDockSink,
      pendingDock,
      clearPendingDock,
    }),
    [
      launch,
      openNewTerminal,
      sessions,
      statuses,
      closeSession,
      restore,
      registerWorkspaceCell,
      sessionsHydrated,
      requestDock,
      registerDockSink,
      pendingDock,
      clearPendingDock,
    ],
  );

  return (
    <Ctx.Provider value={value}>
      {children}
      {sessions.map((s) => (
        <TerminalHost
          key={s.token}
          session={s}
          expanded={s.token === activeToken}
          cellEl={cells[s.token] ?? null}
          status={statuses[s.token]}
          onRequestDock={remoteViewer ? undefined : () => requestDock(s.token)}
          onMinimize={minimize}
          onClose={() => closeSession(s.token)}
          onStatus={(st) => setStatus(s.token, st)}
        />
      ))}
      <Dock
        sessions={sessions.filter((s) => !cells[s.token])}
        activeToken={activeToken}
        statuses={statuses}
        onRestore={restore}
        onClose={closeSession}
        onNew={() => setNewOpen(true)}
      />
      {newOpen && (
        <NewTerminalDialog
          onClose={() => setNewOpen(false)}
          onLaunched={(handle, tool) => {
            setNewOpen(false);
            launch({ token: handle, tool, sessionId: "" });
          }}
        />
      )}
    </Ctx.Provider>
  );
}

export function useLaunchDock(): LaunchDockCtx {
  const v = useContext(Ctx);
  if (!v) throw new Error("useLaunchDock must be used within LaunchDockProvider");
  return v;
}

// TerminalHost renders a single session's LaunchTerminal at a STABLE tree
// position, into a STABLE detached host element via a one-time portal whose
// container NEVER changes — so the terminal never remounts (ws + child
// process persist). Presentation is an imperative DOM MOVE of that host
// element between three placements (dock-grid design D2):
//   docked   → the Terminal Workspace grid cell registered for this token;
//   expanded → the centered floating panel over a dim backdrop;
//   parked   → off-screen (minimized pill), kept full-size so the PTY
//              dimensions and scrollback survive and restore is instant.
// A DOM move preserves the live xterm; switching a React portal's container
// would recreate the DOM and orphan it — hence the imperative reparent.
function TerminalHost({
  session,
  expanded,
  cellEl,
  status,
  onRequestDock,
  onMinimize,
  onClose,
  onStatus,
}: {
  session: DockSession;
  expanded: boolean;
  cellEl: HTMLElement | null;
  status: Status | undefined;
  onRequestDock: (() => void) | undefined;
  onMinimize: () => void;
  onClose: () => void;
  onStatus: (s: Status) => void;
}) {
  const boxRef = useRef<HTMLDivElement | null>(null);
  const isLive = status === "open" || status === "connecting";
  const docked = cellEl !== null;
  const [hostEl] = useState(() => {
    const el = document.createElement("div");
    el.className = "flex w-full flex-1 min-h-0 flex-col";
    return el;
  });
  // Floating-window size: user-resizable (native CSS resize grip) and
  // persisted, so the modal workflow is first-class alongside the grid — a
  // resize refits the PTY via LaunchTerminal's ResizeObserver. The parked
  // wrapper uses the same size so PTY dimensions survive minimize/restore.
  const [floatSize, setFloatSize] = useState(loadFloatSize);
  useEffect(() => {
    if (!expanded || docked) return;
    const el = boxRef.current;
    if (!el) return;
    let t: number | null = null;
    const persist = () => {
      const r = el.getBoundingClientRect();
      const next = { w: Math.round(r.width), h: Math.round(r.height) };
      if (next.w > 0 && next.h > 0) {
        setFloatSize(next);
        try {
          localStorage.setItem(FLOAT_SIZE_KEY, JSON.stringify(next));
        } catch {
          /* storage blocked — size still applies for this tab */
        }
      }
    };
    const ro = new ResizeObserver(() => {
      if (t) window.clearTimeout(t);
      t = window.setTimeout(() => {
        t = null;
        persist();
      }, 400);
    });
    ro.observe(el);
    return () => {
      // FLUSH a pending persist (review LOW): minimizing/docking within the
      // debounce window must not lose the final size across reloads.
      if (t) {
        window.clearTimeout(t);
        persist();
      }
      ro.disconnect();
    };
  }, [expanded, docked]);

  // Reparent the stable host: grid cell wins; otherwise the floating box.
  // Idempotent — appendChild moves the node (never clones), and the xterm
  // inside survives the move (its ResizeObserver refits to the new size).
  useEffect(() => {
    const target = cellEl ?? boxRef.current;
    if (target && hostEl.parentElement !== target) {
      target.appendChild(hostEl);
    }
  }, [cellEl, hostEl, expanded]);

  // Keyboard: while expanded (and NOT docked — the grid owns its own keys),
  // Cmd/Ctrl-. always minimizes; Escape minimizes UNLESS the live terminal
  // is focused (then it belongs to the TUI).
  useEffect(() => {
    if (!expanded || docked) return;
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === ".") {
        e.preventDefault();
        onMinimize();
        return;
      }
      if (e.key === "Escape") {
        const box = boxRef.current;
        const focusedInTerminal = !!box && box.contains(document.activeElement);
        if (focusedInTerminal && isLive) return; // let the TUI have Escape
        onMinimize();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [expanded, docked, isLive, onMinimize]);

  const showModal = !docked && expanded;
  // Backdrop-minimize must fire only when the PRESS started on the backdrop
  // itself: a resize-grip drag (or a text-selection drag) that releases over
  // the backdrop synthesizes a click on the wrapper — the common ancestor of
  // the mousedown and mouseup targets — which used to minimize the panel on
  // every enlarge (operator-reported).
  const backdropPress = useRef(false);
  return (
    <div
      className={
        docked
          ? "hidden"
          : showModal
            ? "fixed inset-0 z-[80] flex items-center justify-center bg-black/50 p-6"
            : "pointer-events-none fixed left-[-100000px] top-0"
      }
      onMouseDown={
        showModal
          ? (e) => {
              backdropPress.current = e.target === e.currentTarget;
            }
          : undefined
      }
      onClick={
        showModal
          ? (e) => {
              const startedOnBackdrop = backdropPress.current;
              backdropPress.current = false;
              if (startedOnBackdrop && e.target === e.currentTarget) onMinimize();
            }
          : undefined
      }
      role={showModal ? "dialog" : undefined}
      aria-modal={showModal ? true : undefined}
      aria-hidden={showModal ? undefined : true}
      aria-label={showModal ? `${session.tool} terminal` : undefined}
    >
      <div
        ref={boxRef}
        style={{ width: floatSize.w, height: floatSize.h }}
        className={
          showModal
            ? "pointer-events-auto flex max-h-[92vh] min-h-[280px] w-auto min-w-[480px] max-w-[96vw] resize flex-col overflow-hidden"
            : "flex flex-col"
        }
        onClick={(e) => e.stopPropagation()}
        title={showModal ? "Drag the corner to resize — the terminal refits" : undefined}
      />
      {createPortal(
        <LaunchTerminal
          token={session.token}
          tool={session.tool}
          expanded={expanded && !docked}
          fill
          onAddToGrid={docked ? undefined : onRequestDock}
          onMinimize={onMinimize}
          onClose={onClose}
          onStatus={onStatus}
        />,
        hostEl,
      )}
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
  onNew,
}: {
  sessions: DockSession[];
  activeToken: string | null;
  statuses: Record<string, Status>;
  onRestore: (token: string) => void;
  onClose: (token: string) => void;
  onNew: () => void;
}) {
  const pills = sessions.filter((s) => s.token !== activeToken);
  const agentStatuses = useTerminalStatuses();
  const drag = useDockDrag();
  // A paired remote device can only fresh-launch when the owner enabled
  // [remote].allow_terminal; when it's off we keep the button visible but
  // disabled with a reason (honest-disabled-control convention), rather than
  // opening a dialog that can only fail.
  const { blocked: remoteBlocked } = useRemoteTerminalGate();
  return (
    <div
      ref={drag.ref}
      style={drag.style}
      className="fixed bottom-4 right-4 z-[70] flex flex-col-reverse items-end gap-2"
    >
      <button type="button" onClick={onNew} disabled={remoteBlocked} title={remoteBlocked ? REMOTE_TERMINAL_OFF_MSG : "Start a fresh agent in the embedded terminal"} className="flex items-center gap-1.5 rounded-full border bg-bg-1 px-3 py-1.5 text-[11px] font-medium text-fg-2 shadow-lg hover:text-fg-1 focus:outline-none disabled:cursor-not-allowed disabled:opacity-50 disabled:hover:text-fg-2"><span aria-hidden className="text-[13px] leading-none">+</span>New terminal</button>
      {pills.map((s) => (
        <DockPill
          key={s.token}
          session={s}
          agent={agentStatuses[s.token]}
          status={statuses[s.token]}
          onRestore={() => onRestore(s.token)}
          onClose={() => onClose(s.token)}
        />
      ))}
      {/* Drag grip (rendered last so flex-col-reverse floats it to the TOP of
          the dock). Dragging only the grip keeps the "New terminal" button a
          plain click target — no click-vs-drag ambiguity. Double-click resets. */}
      <button
        type="button"
        aria-label="Drag to move the terminal dock (double-click to reset)"
        title="Drag to move · double-click to reset"
        onPointerDown={drag.gripHandlers.onPointerDown}
        onPointerMove={drag.gripHandlers.onPointerMove}
        onPointerUp={drag.gripHandlers.onPointerUp}
        onDoubleClick={drag.onReset}
        className="flex h-4 w-9 touch-none cursor-grab select-none items-center justify-center rounded-full border bg-bg-1 text-fg-4 shadow-lg hover:text-fg-2 active:cursor-grabbing"
      >
        <svg width="14" height="6" viewBox="0 0 14 6" fill="none" aria-hidden>
          <circle cx="2" cy="2" r="1" fill="currentColor" />
          <circle cx="7" cy="2" r="1" fill="currentColor" />
          <circle cx="12" cy="2" r="1" fill="currentColor" />
          <circle cx="2" cy="5" r="1" fill="currentColor" />
          <circle cx="7" cy="5" r="1" fill="currentColor" />
          <circle cx="12" cy="5" r="1" fill="currentColor" />
        </svg>
      </button>
    </div>
  );
}

// Dock position persistence (usability follow-up): drag the grip to move the
// whole dock so it stops covering content bottom-right. The offset is stored in
// localStorage and clamped to the viewport (so a resize can't strand it).
const DOCK_POS_KEY = "sb_dock_pos";

// Floating terminal-window size (px), user-resizable + persisted. One shared
// size: every floating terminal opens at the last size the user chose.
const FLOAT_SIZE_KEY = "sb_terminal_float_size";

function loadFloatSize(): { w: number; h: number } {
  try {
    const raw = localStorage.getItem(FLOAT_SIZE_KEY);
    if (raw) {
      const p = JSON.parse(raw);
      if (typeof p?.w === "number" && typeof p?.h === "number" && p.w >= 320 && p.h >= 200) {
        return { w: p.w, h: p.h };
      }
    }
  } catch {
    /* malformed — fall through to the default */
  }
  return { w: 880, h: Math.max(360, Math.round(window.innerHeight * 0.6)) };
}

function useDockDrag() {
  const ref = useRef<HTMLDivElement | null>(null);
  const [pos, setPos] = useState<{ dx: number; dy: number }>(() => {
    try {
      const raw = localStorage.getItem(DOCK_POS_KEY);
      if (raw) {
        const p = JSON.parse(raw);
        if (typeof p?.dx === "number" && typeof p?.dy === "number") return p;
      }
    } catch {
      /* ignore malformed */
    }
    return { dx: 0, dy: 0 };
  });
  const start = useRef<{ x: number; y: number; dx: number; dy: number } | null>(
    null,
  );

  // Clamp an offset so the dock stays fully on-screen. The element's current
  // rect already includes the current transform, so subtract the applied
  // offset to recover the un-transformed anchor, then bound the proposed one.
  const clamp = useCallback(
    (dx: number, dy: number) => {
      const el = ref.current;
      if (!el) return { dx, dy };
      const r = el.getBoundingClientRect();
      const m = 8;
      const baseLeft = r.left - pos.dx;
      const baseTop = r.top - pos.dy;
      const minDx = m - baseLeft;
      const maxDx = window.innerWidth - m - (baseLeft + r.width);
      const minDy = m - baseTop;
      const maxDy = window.innerHeight - m - (baseTop + r.height);
      return {
        dx: Math.min(Math.max(dx, minDx), Math.max(minDx, maxDx)),
        dy: Math.min(Math.max(dy, minDy), Math.max(minDy, maxDy)),
      };
    },
    [pos.dx, pos.dy],
  );

  const onPointerDown = useCallback(
    (e: React.PointerEvent) => {
      (e.target as HTMLElement).setPointerCapture?.(e.pointerId);
      start.current = { x: e.clientX, y: e.clientY, dx: pos.dx, dy: pos.dy };
    },
    [pos.dx, pos.dy],
  );

  const onPointerMove = useCallback(
    (e: React.PointerEvent) => {
      if (!start.current) return;
      setPos(
        clamp(
          start.current.dx + (e.clientX - start.current.x),
          start.current.dy + (e.clientY - start.current.y),
        ),
      );
    },
    [clamp],
  );

  const onPointerUp = useCallback((e: React.PointerEvent) => {
    if (!start.current) return;
    start.current = null;
    (e.target as HTMLElement).releasePointerCapture?.(e.pointerId);
    setPos((p) => {
      try {
        localStorage.setItem(DOCK_POS_KEY, JSON.stringify(p));
      } catch {
        /* storage full/blocked — position still applies for the session */
      }
      return p;
    });
  }, []);

  const onReset = useCallback(() => {
    setPos({ dx: 0, dy: 0 });
    try {
      localStorage.removeItem(DOCK_POS_KEY);
    } catch {
      /* ignore */
    }
  }, []);

  // Re-clamp on resize so a shrunk window can't strand the dock off-screen.
  useEffect(() => {
    const onResize = () => setPos((p) => clamp(p.dx, p.dy));
    window.addEventListener("resize", onResize);
    return () => window.removeEventListener("resize", onResize);
  }, [clamp]);

  return {
    ref,
    style: { transform: `translate(${pos.dx}px, ${pos.dy}px)` },
    gripHandlers: { onPointerDown, onPointerMove, onPointerUp },
    onReset,
  };
}

function DockPill({
  session,
  status,
  agent,
  onRestore,
  onClose,
}: {
  session: DockSession;
  status: Status | undefined;
  agent?: import("@/components/useTerminalStatuses").AgentStatusInfo;
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
        <AgentStatusBadge info={agent} />
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
