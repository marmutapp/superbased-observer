import { useEffect, useRef, useState } from "react";

// useTerminalStatuses subscribes ONCE to the multiplexed agent-status stream
// (GET /ws/terminal/status, F4) and returns a map of PTY handle → fused status.
// The status is HEURISTIC + fused (PTY activity + OSC hints + lifecycle) — the
// UI surfaces it with its confidence and never as certainty. A dashboard
// without the status seam (WS closes immediately) simply yields an empty map.

export type AgentStatusInfo = {
  handle: string;
  run_id?: string;
  status:
    | "working"
    | "waiting-for-input"
    | "blocked"
    | "idle"
    | "exited"
    | "unknown";
  evidence: string;
  confidence: "trusted" | "hint" | "none";
  age_seconds: number;
};

export function useTerminalStatuses(): Record<string, AgentStatusInfo> {
  const [statuses, setStatuses] = useState<Record<string, AgentStatusInfo>>({});
  const wsRef = useRef<WebSocket | null>(null);

  useEffect(() => {
    let disposed = false;
    let retry: ReturnType<typeof setTimeout> | null = null;

    const connect = () => {
      if (disposed) return;
      const proto = window.location.protocol === "https:" ? "wss" : "ws";
      const ws = new WebSocket(
        `${proto}://${window.location.host}/ws/terminal/status`,
      );
      wsRef.current = ws;
      ws.onmessage = (ev: MessageEvent) => {
        try {
          const s = JSON.parse(ev.data as string) as AgentStatusInfo;
          if (!s.handle) return;
          setStatuses((prev) => ({ ...prev, [s.handle]: s }));
        } catch {
          /* ignore malformed frame */
        }
      };
      ws.onclose = () => {
        wsRef.current = null;
        // Reconnect with a gentle backoff (the seam may be briefly down on
        // dashboard restart); a permanently-disabled seam just keeps retrying
        // harmlessly at a low rate.
        if (!disposed) retry = setTimeout(connect, 5000);
      };
      ws.onerror = () => {
        try {
          ws.close();
        } catch {
          /* ignore */
        }
      };
    };
    connect();

    return () => {
      disposed = true;
      if (retry) clearTimeout(retry);
      try {
        wsRef.current?.close();
      } catch {
        /* ignore */
      }
    };
  }, []);

  return statuses;
}

// AgentStatusBadge renders one fused status compactly. Low-confidence and
// "unknown" states are visually muted so a hint never reads as a fact.
export function AgentStatusBadge({ info }: { info?: AgentStatusInfo }) {
  if (!info || info.status === "exited") return null;
  const map: Record<
    AgentStatusInfo["status"],
    { label: string; cls: string }
  > = {
    working: { label: "working", cls: "bg-ok/20 text-ok" },
    "waiting-for-input": { label: "waiting", cls: "bg-warn/20 text-warn" },
    blocked: { label: "blocked", cls: "bg-danger/20 text-danger" },
    idle: { label: "idle", cls: "bg-white/10 text-fg-3" },
    exited: { label: "exited", cls: "bg-white/10 text-fg-3" },
    unknown: { label: "unknown", cls: "bg-white/5 text-fg-3" },
  };
  const { label, cls } = map[info.status];
  const muted = info.confidence !== "trusted" ? " opacity-80" : "";
  const title =
    info.evidence +
    (info.confidence !== "trusted" ? ` (${info.confidence})` : "");
  return (
    <span
      title={title}
      className={`rounded-full px-1.5 py-0.5 text-[9px] font-medium uppercase tracking-[0.04em] ${cls}${muted}`}
    >
      {label}
    </span>
  );
}
