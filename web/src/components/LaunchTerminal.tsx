import { useEffect, useRef, useState } from "react";

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
};

export type Status = "connecting" | "open" | "exited" | "error";

export function LaunchTerminal({
  token,
  tool,
  onClose,
  onMinimize,
  onStatus,
  expanded,
}: Props) {
  const hostRef = useRef<HTMLDivElement | null>(null);
  // The outer panel element (header buttons + xterm host); the focus trap
  // treats anything inside it as a legitimate focus target.
  const rootRef = useRef<HTMLDivElement | null>(null);
  const termRef = useRef<import("@xterm/xterm").Terminal | null>(null);
  const [status, setStatus] = useState<Status>("connecting");
  const [exitCode, setExitCode] = useState<number | null>(null);
  const [errMsg, setErrMsg] = useState<string | null>(null);

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
      term.open(hostRef.current);
      termRef.current = term;
      try {
        fit.fit();
      } catch {
        /* container not laid out yet — the ResizeObserver will fit shortly */
      }

      const proto = window.location.protocol === "https:" ? "wss" : "ws";
      ws = new WebSocket(`${proto}://${window.location.host}/ws/launch/${token}`);
      ws.binaryType = "arraybuffer";

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
        term?.focus();
      };

      ws.onmessage = (ev: MessageEvent) => {
        if (!term) return;
        if (typeof ev.data === "string") {
          // Text frame = control message.
          try {
            const m = JSON.parse(ev.data) as { t?: string; code?: number };
            if (m.t === "exit") {
              setExitCode(typeof m.code === "number" ? m.code : 0);
              setStatus("exited");
            }
          } catch {
            /* ignore malformed control frame */
          }
          return;
        }
        term.write(new Uint8Array(ev.data as ArrayBuffer));
      };

      ws.onerror = () => {
        if (disposed) return;
        setErrMsg("connection error");
        setStatus((s) => (s === "exited" ? s : "error"));
      };

      ws.onclose = () => {
        if (disposed) return;
        setStatus((s) => (s === "exited" ? s : "exited"));
      };

      // Keystrokes → binary frames.
      term.onData((data) => {
        if (ws && ws.readyState === WebSocket.OPEN) {
          ws.send(new TextEncoder().encode(data));
        }
      });

      // Refit + notify the PTY on any container size change.
      ro = new ResizeObserver(() => {
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
    };
  }, [token]);

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

  return (
    <div
      ref={rootRef}
      className="flex h-[60vh] min-h-[360px] flex-col overflow-hidden rounded-2 border bg-[#0b0b0f]"
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
      <div ref={hostRef} className="min-h-0 flex-1 p-2" />
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
