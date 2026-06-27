import clsx from "clsx";
import { Link } from "react-router-dom";
import { ChartShell, Pill, Tooltip } from "@/components/primitives";
import { ChartState } from "@/components/ChartState";
import { useApi } from "@/lib/useApi";
import type { DoctorReport, HealthFailuresResponse } from "@/lib/types";

// HealthSection — the `observer doctor` checks in the dashboard
// (usability arc P4.8 / review row D1). Read-only; the Details lines
// carry each check's remediation hint, exactly as the CLI prints
// them. Runs on open and on Re-run only (the DB integrity check is
// not free on a large observer.db).
export function HealthSection() {
  return (
    <>
      <DoctorCard />
      <div className="mt-4">
        <FailuresCard />
      </div>
    </>
  );
}

function DoctorCard() {
  const report = useApi<DoctorReport>("/api/health/doctor");
  const d = report.data;
  return (
    <ChartShell
      title="Health"
      sub="The `observer doctor` checks: database integrity, hook checksums and binary paths, MCP registrations, pidbridge, concurrent daemons, codex hook trust, org enrolment."
    >
      <ChartState
        loading={report.loading}
        error={report.error}
        empty={!report.loading && (d?.checks ?? []).length === 0}
        emptyHint="No checks returned."
      >
        <div className="space-y-1.5">
          {(d?.checks ?? []).map((c) => (
            <div
              key={c.name}
              className="rounded-2 border border-line-1 bg-bg-2 px-3 py-2"
            >
              <div className="flex items-baseline gap-2">
                <span
                  className={clsx(
                    "w-4 text-center text-[12px] font-bold",
                    c.status === "ok"
                      ? "text-success"
                      : c.status === "warn"
                        ? "text-warn"
                        : "text-danger",
                  )}
                >
                  {c.status === "ok" ? "✓" : c.status === "warn" ? "⚠" : "✗"}
                </span>
                <span className="font-mono text-[11.5px] text-fg-1">
                  {c.name}
                </span>
                <span className="flex-1 text-[11.5px] text-fg-2">
                  {c.message}
                </span>
              </div>
              {c.details && c.details.length > 0 && (
                <ul className="ml-6 mt-1 list-disc space-y-0.5 text-[11px] text-fg-3">
                  {c.details.map((line, i) => (
                    <li key={i} className="whitespace-pre-wrap">
                      {line}
                    </li>
                  ))}
                </ul>
              )}
            </div>
          ))}
        </div>
        <div className="mt-3 flex flex-wrap items-center gap-3 border-t border-line-1 pt-3">
          <button
            type="button"
            onClick={report.reload}
            className="rounded-2 border border-line-2 bg-bg-2 px-2.5 py-1 text-[11px] text-fg-2 hover:bg-bg-3"
          >
            Re-run checks
          </button>
          {d && (
            <span className="text-[11.5px] text-fg-3">
              {d.ok} ok · {d.warn} warn · {d.fail} fail
              {d.all_ok && (
                <span className="ml-2 text-success">all checks passed ★</span>
              )}
            </span>
          )}
        </div>
      </ChartState>
    </ChartShell>
  );
}

// FailuresCard — the failure_context table surfaced (P4.11): recent
// failed commands grouped, recovered-vs-not, deep-link to the session
// where the latest failure happened. Closes the quality loop the data
// has supported since v1 with zero UI.
function FailuresCard() {
  const failures = useApi<HealthFailuresResponse>("/api/health/failures");
  const f = failures.data;
  return (
    <ChartShell
      title="Recent failures"
      sub="Failed commands from the last 7 days, grouped. Recovered = a later attempt of the same command succeeded; unrecovered groups are the time sinks worth a root-cause fix."
    >
      <ChartState
        loading={failures.loading}
        error={failures.error}
        empty={!failures.loading && (f?.failures ?? []).length === 0}
        emptyHint="No failures captured in the last 7 days."
      >
        <table className="w-full border-collapse text-[11.5px]">
          <thead>
            <tr className="text-left text-[10px] uppercase tracking-[0.06em] text-fg-3">
              <th className="pb-1.5 pr-3 font-semibold">Command</th>
              <th className="pb-1.5 pr-3 font-semibold">Fails</th>
              <th className="pb-1.5 pr-3 font-semibold">Outcome</th>
              <th className="pb-1.5 pr-3 font-semibold">Project</th>
              <th className="pb-1.5 font-semibold">Last session</th>
            </tr>
          </thead>
          <tbody>
            {(f?.failures ?? []).map((g) => (
              <tr key={g.command + g.last_at} className="border-t border-line-1">
                <td className="max-w-[280px] py-1.5 pr-3">
                  <Tooltip
                    content={
                      <span className="whitespace-pre-wrap break-all">
                        {g.error_message
                          ? `${g.command}\n\n${g.error_category || "error"}: ${g.error_message}`
                          : g.command}
                      </span>
                    }
                    maxWidth={460}
                  >
                    <code className="block cursor-help truncate font-mono text-fg-1">
                      {g.command}
                    </code>
                  </Tooltip>
                </td>
                <td className="py-1.5 pr-3 text-fg-2">
                  {g.fails}
                  {g.retries > 0 && (
                    <span className="text-fg-4"> (+{g.retries} retries)</span>
                  )}
                </td>
                <td className="py-1.5 pr-3">
                  {g.recovered ? (
                    <Pill variant="success">recovered</Pill>
                  ) : (
                    <Pill variant="warn">unrecovered</Pill>
                  )}
                </td>
                <td className="py-1.5 pr-3 text-fg-3">{g.project || "—"}</td>
                <td className="py-1.5">
                  <Link
                    to={`/sessions?session=${encodeURIComponent(g.session_id)}`}
                    className="text-accent hover:underline"
                  >
                    open
                  </Link>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {f && f.total > f.failures.length && (
          <p className="mt-2 text-[11px] text-fg-3">
            {f.total} failure events total in the window; showing the{" "}
            {f.failures.length} most recent command groups.
          </p>
        )}
      </ChartState>
    </ChartShell>
  );
}
