import { useApi } from "@/lib/useApi";
import { fmtInt, fmtUSD } from "@/lib/format";
import { HelpInd } from "@/components/HelpInd";
import type { CacheStatusResponse, CacheWindowStatus } from "@/lib/types";

// CacheExpiryCard — the cache-expiry warning surface (Part A of
// docs/plans/cache-expiry-warning-and-keepwarm-plan-2026-06-25.md). Reads
// /api/cache/status and lists the live prompt caches with a live countdown
// to expiry, the dollars at risk if each goes cold, and — when keep-warm
// is in advise/enforce mode — the cheapest content-free lever to keep it
// warm.
//
// Shows warm caches too (with their countdown) so an active session has a
// visible "expires in 3:12" timer, not just at-risk ones. Long-dead cold
// caches are dropped at the boundary; only recently-cold ones surface.
// Hidden entirely when no caches are live. Optional sessionId scopes it to
// one session (used by the SessionDetailPanel).
export function CacheExpiryCard({ sessionId }: { sessionId?: string }) {
  const status = useApi<CacheStatusResponse>(
    "/api/cache/status",
    sessionId ? { session: sessionId } : {},
    [sessionId ?? ""],
    { refreshMs: 5000 },
  );

  const data = status.data;
  if (!data || !data.enabled) return null;

  const windows = data.windows ?? [];
  if (windows.length === 0) return null;

  const atRisk = windows.filter((w) => w.severity !== "ok").length;
  const warm = windows.length - atRisk;
  const showAdvice = data.keepwarm_mode !== "off";

  return (
    <div className="rounded-3 border border-fg-2/12 bg-bg-2/40 px-4 py-3">
      <div className="mb-2 flex items-center justify-between gap-2">
        <span className="flex items-center gap-1.5 text-[12px] font-semibold text-fg-1">
          Cache expiry
          <HelpInd id="card.cache_expiry" />
        </span>
        <span className="text-[10px] uppercase tracking-wide text-fg-3">
          {atRisk > 0 && <span className="text-warn">{atRisk} at risk</span>}
          {atRisk > 0 && warm > 0 && " · "}
          {warm > 0 && <span>{warm} warm</span>}
          {" · keep-warm: "}
          {data.keepwarm_mode}
        </span>
      </div>
      <div className="space-y-1">
        {windows.slice(0, 12).map((w, i) => (
          <CacheExpiryRow key={`${w.window.scope}-${i}`} w={w} showAdvice={showAdvice} />
        ))}
      </div>
    </div>
  );
}

function CacheExpiryRow({
  w,
  showAdvice,
}: {
  w: CacheWindowStatus;
  showAdvice: boolean;
}) {
  const adviceShown = showAdvice && w.recommendation.action !== "none";
  return (
    <div className="rounded-2 px-2 py-1 hover:bg-fg-2/5">
      <div className="flex items-center gap-2 text-[11px]">
        <span className="w-20 shrink-0 font-mono tabular-nums text-fg-1">
          {timeLabel(w)}
        </span>
        <SeverityDot severity={w.severity} />
        <span className="min-w-0 flex-1 truncate text-fg-2">{w.window.model}</span>
        <span className="shrink-0 text-fg-3">{fmtInt(w.window.prefix_tokens)} tok</span>
        <span
          className={`shrink-0 font-medium ${
            w.severity === "ok" ? "text-fg-2" : "text-warn"
          }`}
        >
          {fmtUSD(w.value_at_risk_usd)}
        </span>
      </div>
      {adviceShown && (
        <div className="mt-0.5 pl-[5.5rem] text-[10px] text-info">
          💡 {w.recommendation.rationale}
        </div>
      )}
    </div>
  );
}

// timeLabel renders the countdown / relative-expiry for a window. Warm and
// soon/critical show a live countdown ("3:12"); cold shows how long ago it
// expired ("2m ago"). A leading "~" marks an estimated (non-authoritative)
// expiry.
function timeLabel(w: CacheWindowStatus): string {
  const secs = w.seconds_to_expiry;
  const prefix = w.estimated ? "~" : "";
  if (w.severity === "cold" || secs <= 0) {
    return `${ago(-secs)} ago`;
  }
  return `${prefix}${clock(secs)}`;
}

function clock(secs: number): string {
  if (secs >= 60) {
    const m = Math.floor(secs / 60);
    const s = secs % 60;
    return `${m}:${s.toString().padStart(2, "0")}`;
  }
  return `${secs}s`;
}

function ago(secs: number): string {
  if (secs >= 60) return `${Math.floor(secs / 60)}m`;
  return `${secs}s`;
}

function SeverityDot({ severity }: { severity: string }) {
  const map: Record<string, string> = {
    cold: "bg-danger",
    critical: "bg-warn",
    soon: "bg-info",
    ok: "bg-success",
  };
  const cls = map[severity] ?? "bg-fg-3";
  return <span className={`size-1.5 shrink-0 rounded-full ${cls}`} title={severity} />;
}
