import { useState } from "react";
import { Printer } from "lucide-react";
import { api, type KeyCost } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { compact, num, usd } from "@/lib/format";
import { Card, Empty, ErrorState, PageHeader, Spinner } from "@/components/ui";

export function ReportPage() {
  const [month, setMonth] = useState(""); // empty → current UTC month
  const { data, error, loading, reload } = useApi(() => api.report(month || undefined), [month]);

  const controls = (
    <div className="flex items-center gap-2 text-[12px]">
      <input
        type="month"
        value={month}
        onChange={(e) => setMonth(e.target.value)}
        className="rounded-2 border border-line-2 bg-bg-2 px-2 py-1 text-fg-1 focus:border-accent focus:outline-none"
      />
      <button
        type="button"
        onClick={() => window.print()}
        className="inline-flex items-center gap-1.5 rounded-2 border border-line-2 bg-bg-2 px-2.5 py-1 text-fg-2 hover:bg-bg-3 hover:text-fg-0"
      >
        <Printer size={13} /> Print
      </button>
    </div>
  );

  return (
    <>
      <PageHeader
        title="Cost statement"
        subtitle="Print-friendly monthly spend by model, tool, and project, plus the most expensive sessions. File → Print for a PDF."
        right={controls}
      />
      {error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : loading || !data ? (
        <Card className="p-4">
          <Spinner label="Building statement…" />
        </Card>
      ) : (
        <div className="space-y-5">
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-3">
            <StatBox label="Month" value={data.month} />
            <StatBox label="Total spend" value={usd(data.total_usd)} accent />
            <StatBox label="Models" value={num(data.by_model.length)} />
          </div>

          <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
            <CostTable title="By model" rows={data.by_model} />
            <CostTable title="By tool" rows={data.by_tool} />
            <CostTable title="By project" rows={data.by_project} mono />
          </div>

          <Card className="p-3">
            <div className="mb-2 px-1 text-sm font-medium text-fg-1">Top sessions</div>
            {data.top_sessions.length === 0 ? (
              <Empty message="No sessions this month." />
            ) : (
              <table className="w-full text-left text-[11.5px]">
                <thead className="text-[10px] uppercase tracking-[0.06em] text-fg-3">
                  <tr className="border-b border-line-2">
                    <th className="bg-bg-2 px-2 py-2 font-medium">Session</th>
                    <th className="bg-bg-2 px-2 py-2 font-medium">Developer</th>
                    <th className="bg-bg-2 px-2 py-2 font-medium">Tool</th>
                    <th className="bg-bg-2 px-2 py-2 text-right font-medium">Cost</th>
                  </tr>
                </thead>
                <tbody>
                  {data.top_sessions.map((s) => (
                    <tr key={s.session_id} className="border-b border-line-1 last:border-b-0">
                      <td className="px-2 py-1.5 font-mono text-fg-3">{s.session_id.slice(0, 12)}</td>
                      <td className="px-2 py-1.5 text-fg-2">{s.email || "—"}</td>
                      <td className="px-2 py-1.5 text-fg-2">{s.tool || "—"}</td>
                      <td className="whitespace-nowrap px-2 py-1.5 text-right font-mono tabular-nums text-fg-1">{usd(s.cost_usd)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </Card>
        </div>
      )}
    </>
  );
}

function CostTable({ title, rows, mono }: { title: string; rows: KeyCost[]; mono?: boolean }) {
  const total = rows.reduce((a, r) => a + r.cost_usd, 0);
  return (
    <Card className="p-3">
      <div className="mb-2 px-1 text-sm font-medium text-fg-1">{title}</div>
      {rows.length === 0 ? (
        <Empty message="No spend." />
      ) : (
        <ul className="space-y-1">
          {rows.map((r) => (
            <li key={r.key} className="flex items-center justify-between gap-2 text-[12px]">
              <span className={`min-w-0 flex-1 truncate ${mono ? "font-mono" : ""} text-fg-2`} title={r.key}>
                {r.key}
              </span>
              <span className="shrink-0 text-[10px] text-fg-4">{compact(r.tokens)}</span>
              <span className="w-16 shrink-0 text-right font-mono tabular-nums text-fg-1">{usd(r.cost_usd)}</span>
            </li>
          ))}
          <li className="mt-1 flex items-center justify-between border-t border-line-1 pt-1 text-[12px] font-medium">
            <span className="text-fg-2">Total</span>
            <span className="font-mono tabular-nums text-fg-0">{usd(total)}</span>
          </li>
        </ul>
      )}
    </Card>
  );
}

function StatBox({ label, value, accent }: { label: string; value: string; accent?: boolean }) {
  return (
    <div className={accent ? "rounded-3 border border-accent/40 bg-bg-2 px-4 py-3" : "rounded-3 border border-line-2 bg-bg-2 px-4 py-3"}>
      <div className="text-[10px] uppercase tracking-wide text-fg-3">{label}</div>
      <div className="mt-1 font-mono text-[20px] leading-tight text-fg-0">{value}</div>
    </div>
  );
}
