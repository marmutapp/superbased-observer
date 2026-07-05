import { useState } from "react";
import { Link } from "react-router-dom";
import { ChartShell, PageHeader, Pill } from "@/components/primitives";
import { TitleWithHelp } from "@/components/HelpInd";
import { useApi } from "@/lib/useApi";
import { fetchJSON } from "@/lib/api";
import { fmtBytes, fmtInt } from "@/lib/format";
import type { ConfigResponse } from "@/lib/types";

// Privacy center (P6.5): one page answering "what does the observer
// capture, what never leaves this machine, and how do I verify it?".
// Composes existing surfaces (config flags, enrolment status, the
// last-payload transparency view) plus the live scrub tester.
//
// Working surface — calm register, zero delight elements (§9.4).
export function PrivacyPage() {
  const cfg = useApi<ConfigResponse>("/api/config");
  const secrets = (cfg.data?.config as Record<string, any> | undefined)
    ?.Observer?.Secrets;
  const retention = (cfg.data?.config as Record<string, any> | undefined)
    ?.Observer?.Retention;

  return (
    <div className="space-y-6 p-6">
      <PageHeader
        title="Privacy"
        sub="What the observer captures, what never leaves this machine, and the tools to verify both — the live scrub tester and the byte-for-byte view of anything shared with a Teams server."
        helpId="tab.privacy"
      />

      <ChartShell
        title={<TitleWithHelp text="What's captured" helpId="card.privacy_capture_map" />}
        sub="The capture contract, with this install's live settings"
      >
        <div className="grid grid-cols-1 gap-4 text-[12px] leading-relaxed xl:grid-cols-2">
          <div className="space-y-1.5">
            <p className="font-medium text-fg-1">Stored locally (observer.db)</p>
            <ul className="list-disc space-y-1 pl-4 text-fg-2">
              <li>Action metadata: tool, action type, timestamps, file paths, command lines</li>
              <li>Output excerpts, capped (FTS5 index; first/last bytes of long outputs)</li>
              <li>Token usage and API turn accounting (counts and costs, not content)</li>
              <li>Failure context: command summaries and error excerpts</li>
            </ul>
            <p className="font-medium text-fg-1">Never stored</p>
            <ul className="list-disc space-y-1 pl-4 text-fg-2">
              <li>Full file contents and full command outputs (paths and excerpts only)</li>
              <li>Credentials — secret-shaped strings are scrubbed before any row lands</li>
            </ul>
          </div>
          <div className="space-y-1.5">
            <p className="font-medium text-fg-1">Network posture</p>
            <ul className="list-disc space-y-1 pl-4 text-fg-2">
              <li>The observer and watcher make no network calls — everything is local</li>
              <li>The proxy only forwards your AI tool's own traffic to its provider</li>
              <li>
                Teams push happens only after explicit enrolment, hash-only by
                default; raw content requires the node-side{" "}
                <span className="font-mono text-[11px]">share.full_content</span>{" "}
                opt-in that no server can flip remotely
              </li>
            </ul>
            <p className="mt-2 font-medium text-fg-1">This install</p>
            <div className="flex flex-wrap gap-1.5 pt-1">
              <Pill variant={secrets?.EnableScrubbing ? "success" : "warn"}>
                scrubbing {secrets?.EnableScrubbing ? "on" : "off"}
              </Pill>
              <Pill>
                {fmtInt(secrets?.ExtraPatterns?.length ?? 0)} extra pattern
                {(secrets?.ExtraPatterns?.length ?? 0) === 1 ? "" : "s"}
              </Pill>
              <Pill>
                retention{" "}
                {retention?.Days ? `${retention.Days}d` : "unlimited"}
              </Pill>
            </div>
            <p className="pt-1 text-[11px] text-fg-3">
              Tune scrubbing in{" "}
              <Link to="/settings?section=secrets" className="font-medium text-accent hover:text-accent-strong">
                Settings → Secrets
              </Link>
              , retention (with a prune-now button) in{" "}
              <Link to="/settings?section=retention" className="font-medium text-accent hover:text-accent-strong">
                Settings → Retention
              </Link>
              .
            </p>
          </div>
        </div>
      </ChartShell>

      <ScrubTesterCard scrubbingEnabled={secrets?.EnableScrubbing ?? true} />
      <OrgPushCard />
    </div>
  );
}

function ScrubTesterCard({ scrubbingEnabled }: { scrubbingEnabled: boolean }) {
  const [input, setInput] = useState("");
  const [result, setResult] = useState<{
    enabled: boolean;
    scrubbed: string;
    changed: boolean;
  } | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const run = async () => {
    setBusy(true);
    setError(null);
    try {
      setResult(
        await fetchJSON("/api/privacy/scrub-test", undefined, {
          method: "POST",
          body: JSON.stringify({ text: input }),
        }),
      );
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <ChartShell
      title={<TitleWithHelp text="Scrub tester" helpId="card.privacy_scrub_tester" />}
      sub="Paste anything — see exactly what the scrubber would redact. Processed in memory only; nothing you paste here is logged or stored."
    >
      <div className="space-y-2">
        <textarea
          value={input}
          onChange={(e) => setInput(e.target.value)}
          rows={4}
          placeholder='Try a fake token: "export GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwx123456"'
          className="w-full rounded-2 border border-line-2 bg-bg-1 p-2.5 font-mono text-[11.5px] text-fg-1 outline-none placeholder:text-fg-3 focus:border-accent/60"
        />
        <div className="flex items-center gap-2">
          <button
            type="button"
            disabled={busy || input.trim() === ""}
            onClick={() => void run()}
            className="rounded-2 border border-accent/40 bg-accent-soft px-3 py-1 text-[11px] font-medium text-accent disabled:opacity-50"
          >
            {busy ? "scrubbing…" : "test scrub"}
          </button>
          {!scrubbingEnabled && (
            <Pill variant="warn">
              scrubbing is currently OFF — this shows what it would do once enabled
            </Pill>
          )}
          {result && (
            <Pill variant={result.changed ? "danger" : "success"}>
              {result.changed ? "secrets found and redacted" : "nothing to redact"}
            </Pill>
          )}
        </div>
        {error && <p className="text-[11px] text-danger">{error}</p>}
        {result && (
          <pre className="max-h-48 overflow-auto whitespace-pre-wrap break-words rounded-2 border border-line-1 bg-bg-1 p-2.5 font-mono text-[11px] leading-relaxed text-fg-2">
            {result.scrubbed}
          </pre>
        )}
      </div>
    </ChartShell>
  );
}

type EnrolStatus = {
  enrolled: boolean;
  org_name?: string;
  org_server_url?: string;
  user_email?: string;
  last_push?: {
    pushed_at: string;
    status: string;
    row_count: number;
    bytes: number;
    error?: string;
  };
};

function OrgPushCard() {
  const status = useApi<EnrolStatus>("/api/enrolment/status");
  const [payload, setPayload] = useState<string | null>(null);
  const [loadingPayload, setLoadingPayload] = useState(false);

  const showPayload = async () => {
    setLoadingPayload(true);
    try {
      const res = await fetch("/api/enrolment/last-payload");
      const text = await res.text();
      try {
        setPayload(JSON.stringify(JSON.parse(text), null, 2));
      } catch {
        setPayload(text);
      }
    } finally {
      setLoadingPayload(false);
    }
  };

  const st = status.data;
  return (
    <ChartShell
      title={<TitleWithHelp text="Teams sharing" helpId="card.privacy_org_push" />}
      sub="What (if anything) leaves this machine for an org server"
    >
      {!st?.enrolled ? (
        <p className="py-3 text-[12px] text-fg-2">
          Not enrolled in any org — nothing is shared, ever. Enrolment is an
          explicit <span className="font-mono text-[11px]">observer enroll</span>{" "}
          action; until then the observer has no outbound channel at all.
        </p>
      ) : (
        <div className="space-y-2 text-[12px]">
          <div className="flex flex-wrap items-center gap-1.5">
            <Pill variant="info">enrolled</Pill>
            <span className="text-fg-2">
              {st.org_name ?? "org"} · {st.org_server_url}
            </span>
            {st.last_push && (
              <span className="text-[11px] text-fg-3">
                last push {st.last_push.status} · {fmtInt(st.last_push.row_count)}{" "}
                rows · {fmtBytes(st.last_push.bytes)} · {st.last_push.pushed_at}
              </span>
            )}
          </div>
          <div className="flex items-center gap-2">
            <button
              type="button"
              disabled={loadingPayload}
              onClick={() => void showPayload()}
              className="rounded-2 border border-line-2 bg-bg-2 px-2 py-1 text-[11px] text-fg-2 hover:bg-bg-3"
            >
              {loadingPayload ? "loading…" : payload ? "refresh payload view" : "show exactly what was last sent"}
            </button>
            <Link
              to="/settings?section=org"
              className="text-[11px] font-medium text-accent hover:text-accent-strong"
            >
              Share-mode settings →
            </Link>
          </div>
          {payload && (
            <pre className="max-h-64 overflow-auto rounded-2 border border-line-1 bg-bg-1 p-2.5 font-mono text-[10.5px] leading-relaxed text-fg-2">
              {payload}
            </pre>
          )}
        </div>
      )}
    </ChartShell>
  );
}
