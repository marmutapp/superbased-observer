import { useEffect, useMemo, useRef, useState } from "react";
import { isRemoteView } from "@/lib/remote";
import { pushToast } from "@/components/Toast";
import {
  STANDING_REMEMBER_RISK,
  STANDING_REVOKED_MSG,
  forgetStandingSecret,
  getStoredStandingSecret,
  storeStandingSecret,
} from "@/lib/remoteTerminal";

// LaunchTerminal renders the embedded web terminal for a launched session
// (Continue-in… → "Launch <tool> here"). It opens a websocket to
// /ws/launch/<token> — same-origin, so it passes the dashboard's
// browserGuard Host check AND coder/websocket's default cross-origin
// rejection — and bridges it to an xterm.js instance: terminal input rides
// BINARY frames (keystrokes), and a TEXT control frame carries resize
// ({"t":"resize",…}) out and exit ({"t":"exit","code"}) in.
//
// xterm is DYNAMIC-imported so its ~250 KB lands in the lazy `vendor-xterm`
// chunk — a user who never opens a terminal never downloads it (the same
// discipline as the other pinned vendor chunks).

type Props = {
  /** Opaque session handle minted by POST /api/session/<id>/launch. */
  token: string;
  /** Tool label for the header (e.g. "codex"). */
  tool: string;
  /** Kill the process + tear down the panel (destructive). */
  onClose: () => void;
  /** Collapse to the dock, keeping the ws + child process alive. */
  onMinimize: () => void;
  /** Report lifecycle status up to the dock (pill state + beforeunload guard). */
  onStatus?: (s: Status) => void;
  /** True while this session is the on-screen (expanded) panel. */
  expanded: boolean;
  /**
   * Fill the parent container instead of the fixed floating-panel height —
   * set by a Terminal Workspace grid tile so drag-resizing the tile actually
   * resizes the terminal (the ResizeObserver then refits the PTY). Additive:
   * existing callers keep the h-[60vh] floating panel.
   */
  fill?: boolean;
  /**
   * When set, the header shows "Add to grid" — one-click docking of this
   * floating terminal onto the Terminals workspace (the session keeps
   * running; the provider reparents the live xterm into a grid tile).
   * Absent for already-docked tiles and non-workspace hosts.
   */
  onAddToGrid?: () => void;
};

export type Status = "connecting" | "open" | "exited" | "error";

// keyboardApi returns the (Chromium-only) Keyboard Lock API surface without
// leaning on lib.dom types that don't yet declare `navigator.keyboard`.
type KeyboardLockApi = {
  lock?: (keys?: string[]) => Promise<void>;
  unlock?: () => void;
};
function keyboardApi(): KeyboardLockApi | undefined {
  return (navigator as unknown as { keyboard?: KeyboardLockApi }).keyboard;
}

// isBrowserReserved reports combos a normal browser tab cannot preventDefault
// (close-tab / new-tab / new-window). When the keyboard is LOCKED (focus mode)
// they DO reach the page, so they stop being reserved and we forward them to
// the TUI. Honest by construction: we never pretend to intercept Ctrl-W/T/N in
// a normal tab.
function isBrowserReserved(e: KeyboardEvent, locked: boolean): boolean {
  if (locked) return false;
  if (!(e.ctrlKey || e.metaKey)) return false;
  const k = e.key.toLowerCase();
  return k === "w" || k === "t" || k === "n";
}

// Control tracks who drives the PTY from THIS client's point of view.
//   - "local"      owner-loopback path: the WS acquires the local writer
//                  automatically (§4.α), so input is always live — no UI.
//   - "viewer"     remote device, read-only: input is dropped server-side
//                  (§4.β) until an execute capability is acquired.
//   - "requesting" an acquire-writer frame was sent; awaiting granted/denied.
//   - "writer"     remote device granted control (control_granted) — input live.
//   - "denied"     acquire was refused (control_denied) — stay a viewer.
//   - "revoked"    control was taken over / revoked (control_revoked) — input
//                  stops; the socket may then close on an admin kill.
export type Control =
  | "local"
  | "viewer"
  | "requesting"
  | "writer"
  | "denied"
  | "revoked";

export function LaunchTerminal({
  token,
  tool,
  onClose,
  onMinimize,
  onStatus,
  expanded,
  fill,
  onAddToGrid,
}: Props) {
  const hostRef = useRef<HTMLDivElement | null>(null);
  // Mirrors `expanded` for the ws handlers (closure-stable): only the
  // currently-expanded floating panel may steal focus on socket open — a
  // docked grid tile, a parked pill, or a background rehydrate must not
  // (review H2 residual).
  const expandedRef = useRef(expanded);
  useEffect(() => {
    expandedRef.current = expanded;
  }, [expanded]);
  // The outer panel element (header buttons + xterm host); the focus trap
  // treats anything inside it as a legitimate focus target.
  const rootRef = useRef<HTMLDivElement | null>(null);
  const termRef = useRef<import("@xterm/xterm").Terminal | null>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const [status, setStatus] = useState<Status>("connecting");
  const [exitCode, setExitCode] = useState<number | null>(null);
  const [errMsg, setErrMsg] = useState<string | null>(null);

  // Remote-writer control (§4). A remote-paired device starts read-only and
  // must acquire an execute capability; the owner-local loopback path is always
  // the writer, so it never shows control UI (the existing local UX is intact).
  const isRemote = useMemo(() => isRemoteView(), []);
  const [control, setControl] = useState<Control>(isRemote ? "viewer" : "local");
  // canWrite is read by the (long-lived) xterm onData handler via a ref so it
  // never sends keystrokes while this client is a viewer — the client half of
  // the §4.β server-side drop.
  const canWrite = control === "local" || control === "writer";
  const canWriteRef = useRef(canWrite);
  canWriteRef.current = canWrite;
  // Acquire-control form state. The capability + confirm are held ONLY in these
  // inputs, cleared the instant the frame is sent — never persisted, never put
  // in a URL/query (§8.1 #5).
  const [showAcquire, setShowAcquire] = useState(false);
  const [capInput, setCapInput] = useState("");
  const [confirmInput, setConfirmInput] = useState("");
  const [acqErr, setAcqErr] = useState<string | null>(null);
  // Standing terminal-control secret (§B, device side). A device may store a
  // single global standing secret in localStorage so writer control survives a
  // page refresh with NO re-approval. When one is stored we auto-present it on
  // socket open (exactly once per open — never a retry loop). If the server
  // rejects it (revoked/rotated), we clear it and fall back to the one-time
  // flow. With NO stored secret, behaviour is identical to today's single-use
  // flow — nothing is auto-enabled.
  const [standingStored, setStandingStored] = useState<boolean>(
    () => isRemoteView() && getStoredStandingSecret() !== null,
  );
  const [standingInput, setStandingInput] = useState("");
  // Default OFF: remembering the secret in localStorage is an explicit opt-in
  // (it is the risky part — the secret then lives in this browser).
  const [rememberStanding, setRememberStanding] = useState(false);
  // standingAttemptRef marks that the in-flight acquire used a standing secret,
  // so a control_denied can clear the stored secret (rather than blaming a
  // one-time capability) and show the revoked/rotated fallback message.
  const standingAttemptRef = useRef(false);
  // wasRevokedRef tracks whether THIS seat has lost control at least once, so a
  // later control_granted reads as REGAINING control (fire the "you have
  // control" toast) rather than a first-ever acquire (silent). Part of the
  // §6-decision-5 "silent takeover + visible toast in both seats" contract.
  const wasRevokedRef = useRef(false);
  // sawActivityRef records whether this socket ever delivered real session
  // activity — an "exit" control frame OR any terminal output byte. It gates the
  // Jump-in race UX (P2-4): the server closes /ws/launch/<handle> with a
  // POLICY-VIOLATION (1008) + "session not found" when the attach handle is gone
  // by the time the socket opens (the child exited between the attach-list fetch
  // and the click). Without this, onclose paints every close as a bare "exited"
  // terminal, hiding that the session ended BEFORE the second seat could join.
  const sawActivityRef = useRef(false);

  // ── Feature A: per-terminal size mode ────────────────────────────────
  // "fit" (default) tracks the container; "original" pins the PTY to the
  // native terminal's launch-time geometry so a TUI that garbled itself on a
  // resize can be restored. Persisted per handle in sessionStorage (never the
  // server). The ref is read inside the long-lived ResizeObserver closure.
  const sizeKey = `sb_term_sizemode_${token}`;
  const [sizeMode, setSizeMode] = useState<"fit" | "original">(() => {
    try {
      return sessionStorage.getItem(sizeKey) === "original" ? "original" : "fit";
    } catch {
      return "fit";
    }
  });
  const sizeModeRef = useRef(sizeMode);
  sizeModeRef.current = sizeMode;
  useEffect(() => {
    try {
      sessionStorage.setItem(sizeKey, sizeMode);
    } catch {
      /* sessionStorage unavailable (private mode) — mode stays in-memory */
    }
  }, [sizeKey, sizeMode]);
  // The native terminal's launch-time geometry, delivered once by the server's
  // {t:"pty_size"} frame right after the bridge starts. 0/unknown → the restore
  // control renders honest-disabled.
  const [initialDims, setInitialDims] = useState<{
    rows: number;
    cols: number;
  } | null>(null);
  const initialDimsRef = useRef<{ rows: number; cols: number } | null>(null);
  // Hoisted so the size-mode buttons (outside the setup effect) can refit.
  const fitRef = useRef<import("@xterm/addon-fit").FitAddon | null>(null);

  // ── Feature B: local re-acquire after a native-terminal reclaim ──────
  const controlRef = useRef(control);
  controlRef.current = control;
  // Assigned each render so the long-lived onData/onMouseDown closures can call
  // the latest version (wsRef is stable; this only needs a live target).
  const reacquireLocalRef = useRef<() => void>(() => {});

  // ── Feature C: focus mode (fullscreen + Keyboard Lock, Chromium-only) ─
  const [focusMode, setFocusMode] = useState(false);
  const keyboardLockedRef = useRef(false);
  const focusModeSupported = useMemo(
    () =>
      typeof document !== "undefined" &&
      !!document.fullscreenEnabled &&
      typeof navigator !== "undefined" &&
      !!keyboardApi()?.lock,
    [],
  );

  useEffect(() => {
    let disposed = false;
    let ws: WebSocket | null = null;
    let term: import("@xterm/xterm").Terminal | null = null;
    let fit: import("@xterm/addon-fit").FitAddon | null = null;
    let ro: ResizeObserver | null = null;

    (async () => {
      // Lazy chunk: JS + CSS only load when a terminal is actually opened.
      const [{ Terminal }, { FitAddon }] = await Promise.all([
        import("@xterm/xterm"),
        import("@xterm/addon-fit"),
        import("@xterm/xterm/css/xterm.css"),
      ]);
      if (disposed || !hostRef.current) return;

      term = new Terminal({
        convertEol: false, // the PTY already emits CRLF
        cursorBlink: true,
        fontFamily:
          'ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace',
        fontSize: 12,
        theme: { background: "#0b0b0f", foreground: "#e6e6e6" },
      });
      fit = new FitAddon();
      term.loadAddon(fit);
      fitRef.current = fit;
      term.open(hostRef.current);
      termRef.current = term;

      // Feature C1: route modified key combos to the TUI instead of the
      // browser/page where interception is possible; keep copy/paste native.
      term.attachCustomKeyEventHandler((e) => {
        if (e.type !== "keydown") return true;
        // Take-back gesture (Feature B): a REAL keyboard event in a revoked
        // local seat re-acquires control. This lives here — a trusted DOM
        // keydown — and deliberately NOT in term.onData: xterm also emits
        // onData for the protocol replies it auto-generates while parsing TUI
        // output (CPR/DA answers), and reclaiming on those would steal control
        // back from the native terminal with nobody at this keyboard — the
        // exact machine-reply hazard the server's ESC fence exists for.
        // Modifier-only presses don't count as intent.
        if (
          !isRemote &&
          controlRef.current === "revoked" &&
          !["Control", "Shift", "Alt", "Meta", "CapsLock"].includes(e.key)
        ) {
          reacquireLocalRef.current();
        }
        // Push-to-talk chords (Ctrl+Alt+*) / AltGr text: never touch.
        if (e.ctrlKey && e.altKey) return true;
        const mod = e.ctrlKey || e.metaKey;
        if (!mod) return true;
        // Copy WITH a selection stays a native browser copy (don't send ^C).
        if (
          !e.altKey &&
          !e.shiftKey &&
          (e.key === "c" || e.key === "C") &&
          termRef.current?.hasSelection()
        ) {
          return false;
        }
        // Paste rides the hidden textarea — leave it entirely alone.
        if (e.key === "v" || e.key === "V") return true;
        // Browser-reserved combos (Ctrl/Cmd-W/T/N): in a normal tab the browser
        // acts no matter what we do — return FALSE so xterm doesn't ALSO
        // transmit the sequence before the tab closes. Under an active focus-
        // mode keyboard lock they reach the page like any other combo and flow
        // to the TUI below.
        if (isBrowserReserved(e, keyboardLockedRef.current)) return false;
        // Ctrl combos (Ctrl-A, -E, -K, -., …): swallow the page default so the
        // control byte reaches the TUI.
        if (e.ctrlKey) {
          e.preventDefault();
          e.stopPropagation();
          return true;
        }
        // Cmd combos on macOS: xterm does NOT map Meta to terminal Ctrl, so
        // claiming these would break native browser behavior (Cmd-A select-all
        // etc.) without ever delivering a ^X to the TUI. Leave them alone.
        return true;
      });

      try {
        fit.fit();
      } catch {
        /* container not laid out yet — the ResizeObserver will fit shortly */
      }

      const proto = window.location.protocol === "https:" ? "wss" : "ws";
      ws = new WebSocket(`${proto}://${window.location.host}/ws/launch/${token}`);
      ws.binaryType = "arraybuffer";
      wsRef.current = ws;
      // A remote device is a read-only viewer until control is granted, so make
      // the terminal visually read-only up front (keystrokes are ALSO dropped
      // server-side; this is the client-side half + honest affordance).
      if (isRemote) term.options.disableStdin = true;

      const sendResize = () => {
        if (!ws || ws.readyState !== WebSocket.OPEN || !term) return;
        ws.send(
          JSON.stringify({ t: "resize", rows: term.rows, cols: term.cols }),
        );
      };

      ws.onopen = () => {
        if (disposed) return;
        setStatus("open");
        try {
          fit?.fit();
        } catch {
          /* ignore */
        }
        sendResize();
        if (expandedRef.current) term?.focus();
        // Standing-access auto-acquire (§B): on a remote device with a stored
        // standing secret, present it ONCE now (confirm empty — the server
        // routes it by its `standing.` prefix). This is the whole point of
        // standing access: control is re-acquired on every fresh socket with no
        // owner round-trip. Exactly one attempt per open — never a retry loop;
        // a rejection (revoked/rotated) is handled in onmessage below.
        if (isRemote && ws && ws.readyState === WebSocket.OPEN) {
          const stored = getStoredStandingSecret();
          if (stored) {
            standingAttemptRef.current = true;
            ws.send(JSON.stringify({ t: "acquire-writer", cap: stored, confirm: "" }));
            setControl("requesting");
          }
        }
      };

      ws.onmessage = (ev: MessageEvent) => {
        if (!term) return;
        if (typeof ev.data === "string") {
          // Text frame = control message.
          try {
            const m = JSON.parse(ev.data) as {
              t?: string;
              code?: number;
              rows?: number;
              cols?: number;
              initial_rows?: number;
              initial_cols?: number;
            };
            if (m.t === "exit") {
              sawActivityRef.current = true;
              setExitCode(typeof m.code === "number" ? m.code : 0);
              setStatus("exited");
            } else if (m.t === "pty_size") {
              // Feature A: the server's one-shot geometry frame. Record the
              // native terminal's launch-time dims (may be 0/unknown).
              const ir = m.initial_rows;
              const ic = m.initial_cols;
              if (typeof ir === "number" && typeof ic === "number" && ir > 0 && ic > 0) {
                const dims = { rows: ir, cols: ic };
                initialDimsRef.current = dims;
                setInitialDims(dims);
                // Persisted "original" from a prior mount: apply now that the
                // geometry is known (open-time fit already ran in fit mode).
                if (sizeModeRef.current === "original" && termRef.current) {
                  try {
                    termRef.current.resize(ic, ir);
                  } catch {
                    /* ignore */
                  }
                  sendResizeNow(ir, ic);
                }
              }
            } else if (m.t === "control_granted") {
              standingAttemptRef.current = false;
              // Regaining control after a prior revoke → surface it in this
              // seat (§6 decision 5). A first-ever acquire stays silent.
              if (wasRevokedRef.current) {
                wasRevokedRef.current = false;
                pushToast("You have terminal control", "success");
              }
              // Remote seats become the explicit "writer"; a local loopback seat
              // returns to its always-on "local" writer state (Feature B).
              setControl(isRemote ? "writer" : "local");
              setShowAcquire(false);
              setAcqErr(null);
              // Re-assert THIS seat's geometry now that its resizes are
              // unfenced: while demoted, every ResizeObserver resize was
              // dropped server-side, so the PTY still carries the other seat's
              // dims (e.g. the native terminal's after a reclaim). Original
              // mode re-pins the launch dims; fit mode re-fits the container.
              if (sizeModeRef.current === "original" && initialDimsRef.current) {
                const d = initialDimsRef.current;
                try {
                  term.resize(d.cols, d.rows);
                } catch {
                  /* ignore */
                }
                sendResizeNow(d.rows, d.cols);
              } else {
                try {
                  fit?.fit();
                } catch {
                  /* ignore */
                }
                sendResize();
              }
            } else if (m.t === "control_denied") {
              if (standingAttemptRef.current) {
                // A stored/entered standing secret was rejected — it was revoked
                // or rotated. Clear it and fall back to the one-time flow with an
                // honest message. We do NOT retry (one attempt per socket open).
                standingAttemptRef.current = false;
                forgetStandingSecret();
                setStandingStored(false);
                setControl("denied");
                setAcqErr(STANDING_REVOKED_MSG);
              } else {
                setControl("denied");
                setAcqErr("Control was denied — the capability or confirm code was wrong or already used. Ask the owner to approve again.");
              }
            } else if (m.t === "control_revoked") {
              // Taken over by another viewer, the owner/admin, OR — new daemon
              // feature — the NATIVE terminal reclaiming its own session (fires
              // for LOCAL seats too now). Input stops; toast it in this seat (§6
              // decision 5) and arm the regain toast for the next grant. A local
              // seat can take it back with a click/keystroke (Feature B).
              wasRevokedRef.current = true;
              pushToast(
                isRemote
                  ? "Terminal control passed to another viewer"
                  : "The native terminal reclaimed control",
                "warn",
              );
              setControl("revoked");
            }
          } catch {
            /* ignore malformed control frame */
          }
          return;
        }
        sawActivityRef.current = true;
        term.write(new Uint8Array(ev.data as ArrayBuffer));
      };

      ws.onerror = () => {
        if (disposed) return;
        setErrMsg("connection error");
        setStatus((s) => (s === "exited" ? s : "error"));
      };

      ws.onclose = (ev: CloseEvent) => {
        if (disposed) return;
        // A policy-violation close (1008) with NO prior activity means the
        // server refused the join outright — the handle was already gone (the
        // child exited in the fetch→click race) or the session was never
        // joinable. Surface that honestly instead of a bare "exited" terminal
        // the user might read as "my agent just quit" (P2-4). Any close AFTER
        // real output/exit is an ordinary session end. Copy is workflow-NEUTRAL
        // (F7): LaunchTerminal is shared by Jump-in, fresh, handoff, setup, and
        // resume, so it must not claim every failed launch was "Jump in".
        if (ev.code === 1008 && !sawActivityRef.current) {
          setStatus((s) => (s === "exited" ? s : "error"));
          setErrMsg("Session ended before the terminal could connect — it was no longer running.");
          return;
        }
        setStatus((s) => (s === "exited" ? s : "exited"));
      };

      // Keystrokes → binary frames. Gated on canWriteRef so a read-only remote
      // viewer never even transmits input (it would be dropped server-side by
      // §4.β regardless — this is the honest client-side half).
      term.onData((data) => {
        if (!canWriteRef.current) {
          // Dropped silently. NOTE: take-back intent is deliberately NOT
          // inferred here — onData also fires for xterm's auto-generated
          // protocol replies to TUI queries (CPR/DA), which carry no human
          // intent. The take-back triggers are real DOM gestures only: the
          // custom key handler's keydown, the terminal-body mousedown, and the
          // warn-bar click.
          return;
        }
        if (ws && ws.readyState === WebSocket.OPEN) {
          ws.send(new TextEncoder().encode(data));
        }
      });

      // Refit + notify the PTY on any container size change — UNLESS the size
      // mode is pinned to "original" (Feature A), in which case every RO tick is
      // inert so the fixed geometry never gets refit out from under the TUI.
      ro = new ResizeObserver(() => {
        if (sizeModeRef.current === "original") return;
        try {
          fit?.fit();
        } catch {
          /* ignore */
        }
        sendResize();
      });
      ro.observe(hostRef.current);
    })().catch((e) => {
      if (!disposed) {
        setErrMsg(e instanceof Error ? e.message : String(e));
        setStatus("error");
      }
    });

    return () => {
      disposed = true;
      ro?.disconnect();
      try {
        ws?.close();
      } catch {
        /* ignore */
      }
      term?.dispose();
      termRef.current = null;
      wsRef.current = null;
      fitRef.current = null;
    };
  }, [token, isRemote]);

  // Keep the xterm read-only state in sync with control: enable stdin + focus
  // when this client holds the writer, disable it otherwise. Local sessions are
  // always writers, so this only ever restricts a remote viewer.
  useEffect(() => {
    const term = termRef.current;
    if (!term) return;
    // Remote viewers are hard read-only (disableStdin). A LOCAL seat keeps stdin
    // ENABLED even when its lease was reclaimed, so a keystroke can trigger
    // take-back (Feature B) — the actual send is still gated by canWriteRef, so
    // no bytes (nor xterm's TUI-query auto-replies) reach the PTY meanwhile.
    term.options.disableStdin = isRemote ? !canWrite : false;
    if (canWrite && expanded) term.focus();
  }, [canWrite, expanded, isRemote]);

  // acquireControl sends the §4.δ acquire-writer frame with the owner-conveyed
  // capability + confirm over the ALREADY-open, cookie-authed, Origin-checked
  // websocket — the only channel that carries them (never a URL/query/header).
  // The inputs are cleared immediately so the secrets do not linger in state.
  function acquireControl() {
    const ws = wsRef.current;
    const cap = capInput.trim();
    const confirm = confirmInput.trim();
    if (!cap || !confirm) {
      setAcqErr("Paste both the capability and the confirm code the owner gave you.");
      return;
    }
    if (!ws || ws.readyState !== WebSocket.OPEN) {
      setAcqErr("Not connected — reopen the terminal and try again.");
      return;
    }
    ws.send(JSON.stringify({ t: "acquire-writer", cap, confirm }));
    setControl("requesting");
    setAcqErr(null);
    setCapInput("");
    setConfirmInput("");
  }

  // acquireStanding sends the acquire-writer frame with a STANDING secret in the
  // cap field (confirm empty — the server routes it by its `standing.` prefix).
  // If "Remember on this device" is checked, the secret is persisted to
  // localStorage so control survives future refreshes (see the risk copy). The
  // input is cleared immediately either way. standingAttemptRef is set so a
  // rejection clears the (possibly just-stored) secret and shows the fallback.
  function acquireStanding() {
    const ws = wsRef.current;
    const secret = standingInput.trim();
    if (!secret) {
      setAcqErr("Paste the standing secret the owner gave you.");
      return;
    }
    if (!secret.startsWith("standing.")) {
      setAcqErr('That is not a standing secret — it should start with "standing.". Use the capability + confirm fields above for a one-time approval.');
      return;
    }
    if (!ws || ws.readyState !== WebSocket.OPEN) {
      setAcqErr("Not connected — reopen the terminal and try again.");
      return;
    }
    if (rememberStanding) {
      storeStandingSecret(secret);
      setStandingStored(true);
    }
    standingAttemptRef.current = true;
    ws.send(JSON.stringify({ t: "acquire-writer", cap: secret, confirm: "" }));
    setControl("requesting");
    setAcqErr(null);
    setStandingInput("");
  }

  // forgetStandingDevice removes the browser-stored standing secret so this
  // device stops auto-acquiring control on refresh. It does not affect the live
  // lease (still held until the socket closes) — it only stops future auto-use.
  function forgetStandingDevice() {
    forgetStandingSecret();
    setStandingStored(false);
  }

  // Grab focus when this session becomes the on-screen panel (launch, or
  // restore from the dock) so the user can type immediately without an extra
  // click.
  useEffect(() => {
    if (expanded) termRef.current?.focus();
  }, [expanded]);

  // Focus trap (defense-in-depth). While this terminal is the on-screen
  // (expanded) modal, an opaque full-screen backdrop covers the app, so the
  // ONLY legitimate focus targets are inside this panel (the xterm textarea,
  // Minimize, Stop — all under rootRef). If focus lands anywhere OUTSIDE the
  // panel it was stolen by a background re-render (the root cause was fixed in
  // SlideOver, but any future stealer is caught here), so redirect it back.
  // Document-level `focusin` fires whatever the stealer's relatedTarget is —
  // the failure mode of the earlier relatedTarget-null-only watchdog.
  useEffect(() => {
    if (!expanded) return;
    const onFocusIn = (e: FocusEvent) => {
      const root = rootRef.current;
      const target = e.target as Node | null;
      if (!root || !target || root.contains(target)) return;
      termRef.current?.focus();
    };
    document.addEventListener("focusin", onFocusIn, true);
    return () => document.removeEventListener("focusin", onFocusIn, true);
  }, [expanded]);

  // Bubble lifecycle status up to the dock (drives the pill state and the
  // beforeunload guard). Idempotent — safe to fire on every status change.
  useEffect(() => {
    onStatus?.(status);
  }, [status, onStatus]);

  // Closing kills the child process tree (ws teardown → server reap), so
  // confirm when it's still live. Minimize is the non-destructive exit.
  function requestClose() {
    if (
      status === "open" &&
      !window.confirm(`Stop the running ${tool} session? This ends the process.`)
    ) {
      return;
    }
    onClose();
  }

  // sendResizeNow tells the PTY to adopt explicit dims over the control channel
  // (wsRef is stable, so this is safe to call from the setup-effect closure).
  function sendResizeNow(rows: number, cols: number) {
    const ws = wsRef.current;
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify({ t: "resize", rows, cols }));
    }
  }

  // enterOriginalSize (Feature A) pins the viewport to the native launch
  // geometry and stops the ResizeObserver from refitting (guarded by
  // sizeModeRef). The container is unchanged, so there's no RO feedback — and
  // any RO tick while in "original" is inert.
  function enterOriginalSize() {
    const term = termRef.current;
    const dims = initialDimsRef.current;
    if (!term || !dims) return;
    sizeModeRef.current = "original";
    setSizeMode("original");
    try {
      term.resize(dims.cols, dims.rows);
    } catch {
      /* ignore */
    }
    sendResizeNow(dims.rows, dims.cols);
  }

  // enterFitSize re-enables container-tracking and refits + resends at once.
  function enterFitSize() {
    sizeModeRef.current = "fit";
    setSizeMode("fit");
    const term = termRef.current;
    try {
      fitRef.current?.fit();
    } catch {
      /* ignore */
    }
    if (term) sendResizeNow(term.rows, term.cols);
  }

  // reacquireLocal (Feature B) re-takes the writer lease for a LOCAL seat after
  // the native terminal reclaimed control. The loopback path auto-grants on
  // connect; re-acquire mirrors that by sending an acquire-writer frame with an
  // empty capability (the server routes a loopback seat's request without one).
  reacquireLocalRef.current = () => {
    const ws = wsRef.current;
    if (isRemote || !ws || ws.readyState !== WebSocket.OPEN) return;
    if (controlRef.current !== "revoked") return;
    ws.send(JSON.stringify({ t: "acquire-writer", cap: "", confirm: "" }));
    setControl("requesting");
  };

  // enterFocusMode (Feature C4) fullscreens the WHOLE panel (chrome stays
  // visible so the exit button is reachable) and, on Chromium, locks the
  // keyboard so browser-reserved combos (Ctrl-W/T/N) reach the page → the TUI.
  async function enterFocusMode() {
    const el = rootRef.current;
    if (!el) return;
    try {
      await el.requestFullscreen();
    } catch {
      return;
    }
    try {
      await keyboardApi()?.lock?.();
      keyboardLockedRef.current = true;
    } catch {
      /* lock rejected — fullscreen still helps; reserved keys stay reserved */
    }
    setFocusMode(true);
  }

  // exitFocusMode releases the lock and leaves fullscreen. Escape is captured
  // by the lock, so this button is the primary exit affordance.
  async function exitFocusMode() {
    try {
      keyboardApi()?.unlock?.();
    } catch {
      /* ignore */
    }
    keyboardLockedRef.current = false;
    try {
      if (document.fullscreenElement) await document.exitFullscreen();
    } catch {
      /* ignore */
    }
    setFocusMode(false);
  }

  // Exiting fullscreen by ANY means (browser UI, OS chord) must release the
  // keyboard lock and clear focus-mode state.
  useEffect(() => {
    function onFsChange() {
      if (document.fullscreenElement) return;
      if (keyboardLockedRef.current) {
        try {
          keyboardApi()?.unlock?.();
        } catch {
          /* ignore */
        }
        keyboardLockedRef.current = false;
      }
      setFocusMode(false);
    }
    document.addEventListener("fullscreenchange", onFsChange);
    return () => document.removeEventListener("fullscreenchange", onFsChange);
  }, []);

  return (
    <div
      ref={rootRef}
      className={
        fill
          ? "flex min-h-0 w-full flex-1 flex-col overflow-hidden rounded-2 border bg-[#0b0b0f]"
          : "flex h-[60vh] min-h-[360px] flex-col overflow-hidden rounded-2 border bg-[#0b0b0f]"
      }
    >
      <div className="flex items-center justify-between gap-2 border-b border-white/10 bg-[#14141a] px-3 py-1.5">
        <span className="flex items-center gap-2 text-[11px] text-fg-2">
          <span className="font-mono text-fg-1">{tool}</span>
          <StatusPill status={status} exitCode={exitCode} />
          {errMsg && (
            <span className="text-[10.5px] text-danger" title={errMsg}>
              {errMsg}
            </span>
          )}
        </span>
        <span className="flex items-center gap-1">
          {/* Feature A: size-mode control. Resizing is a writer capability, so
              this hides for read-only remote viewers (mirrors the RemoteControlBar
              gating) and honest-disables when the original geometry is unknown. */}
          {canWrite && (
            <SizeModeControl
              mode={sizeMode}
              hasOriginal={!!initialDims}
              onOriginal={enterOriginalSize}
              onFit={enterFitSize}
            />
          )}
          {/* Feature C4: focus mode (Chromium-only; hidden where unsupported). */}
          {focusModeSupported && (
            <button
              type="button"
              onClick={focusMode ? exitFocusMode : enterFocusMode}
              title={
                focusMode
                  ? "Exit focus mode — release keyboard capture and leave fullscreen"
                  : "Focus mode — fullscreen + capture browser shortcuts (Ctrl-W/T/N) so they reach the TUI. Chromium only; exit with this button (Esc is captured)."
              }
              className={
                "rounded-2 px-2 py-0.5 text-[11px] focus:outline-none " +
                (focusMode
                  ? "bg-accent/20 text-accent hover:bg-accent/30"
                  : "text-fg-3 hover:bg-white/10 hover:text-fg-1")
              }
            >
              {focusMode ? "⤢ Exit focus" : "⤢ Focus mode"}
            </button>
          )}
          {onAddToGrid && (
            <button
              type="button"
              onClick={onAddToGrid}
              title="Add to grid — dock this terminal on the Terminals workspace. The session keeps running."
              className="rounded-2 px-2 py-0.5 text-[11px] text-fg-3 hover:bg-white/10 hover:text-fg-1 focus:outline-none"
            >
              ⊞ Add to grid
            </button>
          )}
          <button
            type="button"
            onClick={onMinimize}
            title="Minimize — keeps the session running; restore it from the dock"
            className="rounded-2 px-2 py-0.5 text-[11px] text-fg-3 hover:bg-white/10 hover:text-fg-1 focus:outline-none"
          >
            ▾ Minimize
          </button>
          <button
            type="button"
            onClick={requestClose}
            title={
              status === "open"
                ? "Stop the running process and close"
                : "Close"
            }
            className="rounded-2 px-2 py-0.5 text-[11px] text-fg-3 hover:bg-white/10 hover:text-fg-1 focus:outline-none"
          >
            {status === "open" ? "✕ Stop & close" : "✕ Close"}
          </button>
        </span>
      </div>
      {isRemote && (
        <RemoteControlBar
          control={control}
          showAcquire={showAcquire}
          onToggleAcquire={() => {
            setShowAcquire((v) => !v);
            setAcqErr(null);
          }}
          cap={capInput}
          confirm={confirmInput}
          onCap={setCapInput}
          onConfirm={setConfirmInput}
          onAcquire={acquireControl}
          acqErr={acqErr}
          standingStored={standingStored}
          standingInput={standingInput}
          rememberStanding={rememberStanding}
          onStandingInput={setStandingInput}
          onRememberStanding={setRememberStanding}
          onAcquireStanding={acquireStanding}
          onForgetStanding={forgetStandingDevice}
        />
      )}
      {/* Feature B: local seat whose lease the native terminal reclaimed. */}
      {!isRemote && control === "revoked" && (
        <button
          type="button"
          onClick={() => reacquireLocalRef.current()}
          className="flex w-full items-center gap-2 border-b border-white/10 bg-warn/10 px-3 py-1.5 text-left text-[11px] text-warn hover:bg-warn/15"
        >
          <span className="rounded-full bg-warn/20 px-1.5 py-0.5 text-[9.5px] font-medium uppercase tracking-[0.05em]">
            read-only
          </span>
          <span>
            Control returned to the native terminal — click to take back.
          </span>
        </button>
      )}
      {!isRemote && control === "requesting" && (
        <div className="border-b border-white/10 bg-bg-1 px-3 py-1.5 text-[11px] text-fg-3">
          Taking back control…
        </div>
      )}
      <div
        ref={hostRef}
        className="min-h-0 flex-1 p-2"
        onMouseDown={() => {
          // Click-to-focus for the padding/letterbox area a docked grid tile's
          // xterm canvas doesn't cover (Feature C5); also the click half of the
          // local take-back path (Feature B).
          if (!isRemote && controlRef.current === "revoked") {
            reacquireLocalRef.current();
          }
          termRef.current?.focus();
        }}
      />
    </div>
  );
}

// SizeModeControl is the Feature-A size-mode toggle shown in the terminal
// chrome. In "fit" it offers "↺ Original size" (honest-disabled when the native
// geometry is unknown); in "original" it shows an active "⤢ Fit" to return to
// container-tracking. Writer-gated by its caller.
function SizeModeControl({
  mode,
  hasOriginal,
  onOriginal,
  onFit,
}: {
  mode: "fit" | "original";
  hasOriginal: boolean;
  onOriginal: () => void;
  onFit: () => void;
}) {
  if (mode === "original") {
    return (
      <button
        type="button"
        onClick={onFit}
        title="Pinned to the native terminal's original size — click to refit to the panel."
        className="rounded-2 bg-accent/20 px-2 py-0.5 text-[11px] text-accent hover:bg-accent/30 focus:outline-none"
      >
        ⤢ Fit
      </button>
    );
  }
  if (!hasOriginal) {
    return (
      <button
        type="button"
        disabled
        title="Original size unknown for this session"
        className="cursor-not-allowed rounded-2 px-2 py-0.5 text-[11px] text-fg-4 opacity-60"
      >
        ↺ Original size
      </button>
    );
  }
  return (
    <button
      type="button"
      onClick={onOriginal}
      title="Restore the native terminal's original size — fixes a TUI that garbled after a resize. Stops auto-refit until you switch back to Fit."
      className="rounded-2 px-2 py-0.5 text-[11px] text-fg-3 hover:bg-white/10 hover:text-fg-1 focus:outline-none"
    >
      ↺ Original size
    </button>
  );
}

// RemoteControlBar is the remote-device terminal-control strip (§4, deliverable
// 2). It renders ONLY on a remote-paired device: input is read-only until the
// user pastes the owner-conveyed capability + confirm and acquires control over
// the websocket. It reacts to control_granted / control_denied / control_revoked
// surfaced through the `control` prop.
function RemoteControlBar({
  control,
  showAcquire,
  onToggleAcquire,
  cap,
  confirm,
  onCap,
  onConfirm,
  onAcquire,
  acqErr,
  standingStored,
  standingInput,
  rememberStanding,
  onStandingInput,
  onRememberStanding,
  onAcquireStanding,
  onForgetStanding,
}: {
  control: Control;
  showAcquire: boolean;
  onToggleAcquire: () => void;
  cap: string;
  confirm: string;
  onCap: (v: string) => void;
  onConfirm: (v: string) => void;
  onAcquire: () => void;
  acqErr: string | null;
  standingStored: boolean;
  standingInput: string;
  rememberStanding: boolean;
  onStandingInput: (v: string) => void;
  onRememberStanding: (v: boolean) => void;
  onAcquireStanding: () => void;
  onForgetStanding: () => void;
}) {
  const writer = control === "writer";
  const requesting = control === "requesting";
  const label =
    control === "writer"
      ? "You have control"
      : control === "requesting"
        ? "Requesting control…"
        : control === "revoked"
          ? "Control ended — you are viewing read-only"
          : control === "denied"
            ? "Read-only — control was denied"
            : "Read-only view";
  const pillCls = writer
    ? "bg-ok/20 text-ok"
    : requesting
      ? "bg-white/10 text-fg-3"
      : "bg-warn/20 text-warn";
  return (
    <div className="border-b border-white/10 bg-[#101017] px-3 py-1.5 text-[11px]">
      <div className="flex items-center justify-between gap-2">
        <span className="flex items-center gap-2">
          <span
            className={`rounded-full px-1.5 py-0.5 text-[9.5px] font-medium uppercase tracking-[0.05em] ${pillCls}`}
          >
            {label}
          </span>
          {!writer && !requesting && (
            <span className="text-fg-3">
              Ask the owner to “Approve remote control”, then paste what they send you.
            </span>
          )}
        </span>
        {!writer && (
          <button
            type="button"
            onClick={onToggleAcquire}
            className="rounded-2 border border-accent/50 bg-accent/15 px-2 py-0.5 text-[11px] font-medium text-accent hover:bg-accent/25"
          >
            {showAcquire ? "Cancel" : control === "revoked" ? "Re-acquire control" : "Acquire control"}
          </button>
        )}
      </div>
      {standingStored && (
        <div className="mt-1.5 flex items-center justify-between gap-2 rounded-2 border border-line-2 bg-bg-1 px-2 py-1">
          <span className="text-[10.5px] text-fg-3">
            Standing access is saved on this device — control is re-acquired automatically on refresh.
          </span>
          <button
            type="button"
            onClick={onForgetStanding}
            title="Remove the saved standing secret from this browser (stops auto-acquiring on refresh)"
            className="shrink-0 rounded-2 border border-danger/40 bg-danger/10 px-2 py-0.5 text-[10.5px] text-danger hover:bg-danger/20"
          >
            Forget standing secret on this device
          </button>
        </div>
      )}
      {showAcquire && !writer && (
        <div className="mt-2 space-y-2 rounded-2 border border-line-2 bg-bg-1 p-2">
          <label className="block">
            <span className="mb-0.5 block text-[10px] uppercase tracking-wide text-fg-3">
              capability
            </span>
            <input
              value={cap}
              onChange={(e) => onCap(e.target.value)}
              autoComplete="off"
              spellCheck={false}
              placeholder="paste the capability the owner sent"
              className="w-full rounded-2 border border-line-2 bg-bg-0 px-2 py-1 font-mono text-[11px] text-fg-1 outline-none focus:border-accent"
            />
          </label>
          <label className="block">
            <span className="mb-0.5 block text-[10px] uppercase tracking-wide text-fg-3">
              confirm code
            </span>
            <input
              value={confirm}
              onChange={(e) => onConfirm(e.target.value)}
              autoComplete="off"
              spellCheck={false}
              placeholder="paste the confirm code"
              className="w-full rounded-2 border border-line-2 bg-bg-0 px-2 py-1 font-mono text-[11px] text-fg-1 outline-none focus:border-accent"
              onKeyDown={(e) => {
                if (e.key === "Enter") onAcquire();
              }}
            />
          </label>
          {acqErr && <div className="text-[11px] text-danger">{acqErr}</div>}
          <div className="flex items-center gap-2">
            <button
              type="button"
              onClick={onAcquire}
              disabled={requesting}
              className="rounded-2 border border-accent/50 bg-accent/15 px-3 py-1 text-[11px] font-medium text-accent hover:bg-accent/25 disabled:opacity-50"
            >
              {requesting ? "requesting…" : "Take control"}
            </button>
            <span className="text-[10px] text-fg-3">
              Sent over the encrypted session only — never stored or put in the URL.
            </span>
          </div>

          {/* Standing-secret alternative (§B, advanced). A single reusable
              secret that grants control of every terminal and can be remembered
              on this device so control survives refreshes. Off unless the user
              pastes one — the one-time capability above stays the default. */}
          <div className="mt-1 border-t border-line-2 pt-2">
            <div className="mb-1 text-[10px] uppercase tracking-wide text-fg-3">
              or use a standing secret (advanced)
            </div>
            <input
              value={standingInput}
              onChange={(e) => onStandingInput(e.target.value)}
              autoComplete="off"
              spellCheck={false}
              placeholder="paste the standing secret (starts with standing.)"
              className="w-full rounded-2 border border-line-2 bg-bg-0 px-2 py-1 font-mono text-[11px] text-fg-1 outline-none focus:border-accent"
              onKeyDown={(e) => {
                if (e.key === "Enter") onAcquireStanding();
              }}
            />
            <label className="mt-1.5 flex items-start gap-1.5">
              <input
                type="checkbox"
                checked={rememberStanding}
                onChange={(e) => onRememberStanding(e.target.checked)}
                className="mt-0.5"
              />
              <span className="text-[10px] text-fg-3">
                <span className="font-medium text-fg-2">Remember on this device (localStorage). </span>
                {STANDING_REMEMBER_RISK}
              </span>
            </label>
            <button
              type="button"
              onClick={onAcquireStanding}
              className="mt-1.5 rounded-2 border border-warn/50 bg-warn/10 px-3 py-1 text-[11px] font-medium text-warn hover:bg-warn/20"
            >
              Use standing secret
            </button>
          </div>
        </div>
      )}
      {acqErr && !showAcquire && <div className="mt-1 text-[11px] text-danger">{acqErr}</div>}
    </div>
  );
}

function StatusPill({
  status,
  exitCode,
}: {
  status: Status;
  exitCode: number | null;
}) {
  const map: Record<Status, { label: string; cls: string }> = {
    connecting: { label: "connecting…", cls: "bg-white/10 text-fg-3" },
    open: { label: "live", cls: "bg-ok/20 text-ok" },
    exited: {
      label: exitCode === null ? "exited" : `exited (${exitCode})`,
      cls: "bg-white/10 text-fg-3",
    },
    error: { label: "error", cls: "bg-danger/20 text-danger" },
  };
  const { label, cls } = map[status];
  return (
    <span
      className={`rounded-full px-1.5 py-0.5 text-[9.5px] font-medium uppercase tracking-[0.05em] ${cls}`}
    >
      {label}
    </span>
  );
}
