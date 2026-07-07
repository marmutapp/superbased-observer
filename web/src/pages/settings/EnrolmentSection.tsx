import { useState } from "react";
import { ChartShell, SlideOver, StatCard, Tooltip } from "@/components/primitives";
import { ChartState } from "@/components/ChartState";
import { useApi } from "@/lib/useApi";
import { fmtBytes, fmtInt } from "@/lib/format";
import type { EnrolmentStatus } from "@/lib/types";

// EnrolmentSection is the Settings → Enrolment page: it shows whether this
// agent is enrolled in a Teams org, what the last push shared, lets the
// developer inspect the exact bytes sent, and lets them unenrol. It is purely
// a view over /api/enrolment/* — on a solo-local install (org mode off) the
// status reports not-enrolled and the page shows how to join.
export function EnrolmentSection() {
  const status = useApi<EnrolmentStatus>("/api/enrolment/status", undefined, [], {
    refreshMs: 15000,
  });
  const [payloadOpen, setPayloadOpen] = useState(false);
  const [unenroll, setUnenroll] = useState<{
    state: "idle" | "confirm" | "working" | "err";
    message?: string;
  }>({ state: "idle" });

  const data = status.data;
  const enrolled = !!data?.enrolled;

  async function doUnenroll() {
    setUnenroll({ state: "working" });
    try {
      const res = await fetch("/api/enrolment/unenroll", { method: "POST" });
      if (!res.ok) throw new Error(`server returned ${res.status}`);
      setUnenroll({ state: "idle" });
      status.reload();
    } catch (e) {
      setUnenroll({ state: "err", message: e instanceof Error ? e.message : String(e) });
    }
  }

  return (
    <ChartShell
      title="Organisation enrolment"
      sub="Teams visibility: when enrolled, this agent shares content-free activity rollups (counts, costs, timings, paths — never prompt text or tool output) with your organisation's Observer server. Enrol with `observer enroll <org-url> <token>`; unenrol any time below."
      right={
        enrolled ? (
          <div className="flex items-center gap-2 text-[11px]">
            <button
              type="button"
              onClick={() => setPayloadOpen(true)}
              className="rounded-2 border border-line-2 bg-bg-2 px-2.5 py-1 text-fg-1 hover:bg-bg-3"
            >
              View raw push payload
            </button>
            {unenroll.state === "confirm" ? (
              <span className="flex items-center gap-1.5">
                <span className="text-fg-2">Unenrol?</span>
                <button
                  type="button"
                  onClick={doUnenroll}
                  className="rounded-2 border border-danger/40 bg-danger-soft px-2.5 py-1 font-medium text-danger hover:bg-danger/20"
                >
                  Confirm
                </button>
                <button
                  type="button"
                  onClick={() => setUnenroll({ state: "idle" })}
                  className="rounded-2 border border-line-2 bg-bg-2 px-2.5 py-1 text-fg-2 hover:bg-bg-3"
                >
                  Cancel
                </button>
              </span>
            ) : (
              <button
                type="button"
                onClick={() => setUnenroll({ state: "confirm" })}
                disabled={unenroll.state === "working"}
                className="rounded-2 border border-danger/40 bg-danger-soft px-3 py-1 font-medium text-danger disabled:opacity-40"
              >
                {unenroll.state === "working" ? "Unenrolling…" : "Unenrol"}
              </button>
            )}
          </div>
        ) : null
      }
    >
      <ChartState
        loading={status.loading && !data}
        error={status.error}
        empty={false}
        height={160}
      >
        {unenroll.state === "err" && (
          <div className="mb-3 rounded-2 border border-danger/40 bg-danger-soft px-3 py-2 text-[11.5px] text-danger">
            Unenrol failed: {unenroll.message}
          </div>
        )}

        {!enrolled ? (
          <div className="rounded-2 border border-dashed border-line-2 bg-bg-3/40 px-4 py-3 text-[12px] text-fg-2">
            <div className="mb-1 font-medium text-fg-1">Not enrolled</div>
            This agent is not enrolled in an organisation — nothing is shared.
            To join, ask an admin for a one-time token and run{" "}
            <code className="font-mono text-fg-1">observer enroll &lt;org-url&gt; &lt;token&gt;</code>,
            then set <code className="font-mono text-fg-1">[org_client] enabled = true</code> and
            restart <code className="font-mono text-fg-1">observer start</code>.
          </div>
        ) : (
          <div className="space-y-4">
            <div className="grid grid-cols-1 gap-3 md:grid-cols-2 lg:grid-cols-4">
              <StatCard label="Organisation" value={data?.org_name || "—"} sub={data?.org_id} />
              <StatCard label="You" value={data?.user_email || "—"} />
              <StatCard label="Server" value={<Mono>{data?.org_server_url || "—"}</Mono>} />
              <StatCard
                label="Credential store"
                value={data?.credential_store || "—"}
                sub={data?.enrolled_at ? `enrolled ${data.enrolled_at}` : undefined}
              />
            </div>
            <LastPush push={data?.last_push} />
          </div>
        )}
      </ChartState>

      <RawPayloadDrawer open={payloadOpen} onClose={() => setPayloadOpen(false)} />
    </ChartShell>
  );
}

function LastPush({ push }: { push?: EnrolmentStatus["last_push"] }) {
  if (!push) {
    return (
      <div className="rounded-2 border border-line-1 bg-bg-2 px-3 py-2 text-[11.5px] text-fg-3">
        No push yet — the first content-free rollup is shared on the next push interval.
      </div>
    );
  }
  const ok = push.status === "ok";
  return (
    <div className="rounded-2 border border-line-1 bg-bg-2 px-3 py-2 text-[11.5px]">
      <div className="mb-1 text-[10px] font-semibold uppercase tracking-[0.06em] text-fg-3">
        Last push
      </div>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-fg-2">
        <span>
          status{" "}
          <b className={ok ? "text-success" : "text-danger"}>{push.status}</b>
        </span>
        <span>{push.pushed_at}</span>
        <span>{fmtInt(push.row_count)} rows</span>
        <span>{fmtBytes(push.bytes)}</span>
        {push.error && <span className="text-danger">{push.error}</span>}
      </div>
    </div>
  );
}

// RawPayloadDrawer fetches /api/enrolment/last-payload and shows it verbatim —
// this is the exact content-free JSON last shared with the org, so a developer
// can audit precisely what was sent.
function RawPayloadDrawer({ open, onClose }: { open: boolean; onClose: () => void }) {
  const [text, setText] = useState<string>("");
  const [loading, setLoading] = useState(false);

  async function load() {
    setLoading(true);
    try {
      const res = await fetch("/api/enrolment/last-payload");
      const raw = await res.text();
      try {
        setText(JSON.stringify(JSON.parse(raw), null, 2));
      } catch {
        setText(raw);
      }
    } catch (e) {
      setText(`failed to load: ${e instanceof Error ? e.message : String(e)}`);
    } finally {
      setLoading(false);
    }
  }

  // Lazy-load when the drawer opens.
  if (open && text === "" && !loading) void load();

  return (
    <SlideOver
      open={open}
      onClose={onClose}
      title="Last shared payload"
      subtitle="The exact content-free rollup last pushed to your org — byte-for-byte. No prompt text or tool output is ever included."
      width={760}
    >
      <div className="p-4">
        {loading ? (
          <div className="text-[12px] text-fg-3">loading…</div>
        ) : text === "null" || text === "" ? (
          <div className="rounded-2 border border-dashed border-line-2 px-4 py-6 text-center text-[12px] text-fg-3">
            Nothing pushed yet.
          </div>
        ) : (
          <pre className="m-0 max-h-[70vh] overflow-auto whitespace-pre-wrap break-all rounded-2 border border-line-1 bg-bg-1 px-3 py-2 font-mono text-[11.5px] text-fg-2">
            {text}
          </pre>
        )}
      </div>
    </SlideOver>
  );
}

function Mono({ children }: { children: React.ReactNode }) {
  return (
    <Tooltip content={<span className="break-all font-mono">{children}</span>} maxWidth={420}>
      <span tabIndex={0} className="block cursor-help truncate font-mono text-[13px] focus:outline-none">
        {children}
      </span>
    </Tooltip>
  );
}
