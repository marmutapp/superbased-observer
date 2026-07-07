import { useState } from "react";
import { api, type NewObsAlert } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { dateTime, pct1, usd } from "@/lib/format";
import { Card, ErrorState, PageHeader } from "@/components/ui";
import { Pill, StatStripSkeleton } from "@/components/primitives";

const METRICS = [
  { value: "error_rate", label: "Error rate", unit: "%" },
  { value: "cost_usd", label: "Cost (window)", unit: "$" },
  { value: "latency_p95_ms", label: "P95 latency", unit: "ms" },
] as const;

// Org observability alerting (obs-org-tier OP6b). Admin-authored threshold
// rules over the obs_summaries aggregate (error rate / cost / p95 latency) that
// fire a webhook on a crossing. Distinct from the api_turns budget caps.
export function ObsAlertsPage() {
  const { data, error, loading, reload } = useApi(() => api.obsAlerts(), []);
  const [busy, setBusy] = useState(false);
  const [formErr, setFormErr] = useState<string | null>(null);

  async function create(rule: NewObsAlert) {
    setBusy(true);
    setFormErr(null);
    try {
      await api.createObsAlert(rule);
      reload();
    } catch (e) {
      setFormErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }
  async function remove(id: string) {
    setBusy(true);
    try {
      await api.deleteObsAlert(id);
      reload();
    } finally {
      setBusy(false);
    }
  }

  return (
    <>
      <PageHeader
        title="Trajectory alerts"
        subtitle="Threshold rules over the shared trajectory aggregate (error rate / cost / P95 latency) that fire a webhook on a crossing. Distinct from the api_turns budget caps."
      />
      {error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : loading || !data ? (
        <StatStripSkeleton count={3} />
      ) : (
        <div className="space-y-5">
          <CreateForm onCreate={create} busy={busy} err={formErr} />
          <Card className="p-3">
            <div className="mb-2 px-1 text-sm font-medium text-fg-1">Rules</div>
            {data.rules.length === 0 ? (
              <p className="px-1 py-4 text-[13px] text-fg-3">No alert rules yet. Create one above.</p>
            ) : (
              <table className="w-full text-[12px]">
                <thead>
                  <tr className="text-left text-fg-3">
                    <th className="py-1 font-medium">Name</th>
                    <th className="py-1 font-medium">Condition</th>
                    <th className="py-1 text-right font-medium">Current</th>
                    <th className="py-1 font-medium">Status</th>
                    <th className="py-1 font-medium">Last fired</th>
                    <th className="py-1"></th>
                  </tr>
                </thead>
                <tbody>
                  {data.rules.map((r) => (
                    <tr key={r.id} className="border-t border-line-2">
                      <td className="py-1.5 text-fg-1">{r.name || "(unnamed)"}</td>
                      <td className="py-1.5 font-mono text-fg-2">
                        {metricLabel(r.metric)} {r.comparator === "gte" ? "≥" : ">"} {fmtMetric(r.metric, r.threshold)}
                        <span className="text-fg-3"> · {r.window_days}d</span>
                      </td>
                      <td className="py-1.5 text-right text-fg-1">{fmtMetric(r.metric, r.last_value)}</td>
                      <td className="py-1.5">
                        {r.breaching ? <Pill variant="danger">breaching</Pill> : <Pill variant="success">ok</Pill>}
                      </td>
                      <td className="py-1.5 text-fg-3">{r.last_fired_at ? dateTime(r.last_fired_at) : "—"}</td>
                      <td className="py-1.5 text-right">
                        <button onClick={() => remove(r.id)} disabled={busy} className="text-[11px] text-danger hover:underline disabled:opacity-50">
                          delete
                        </button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </Card>

          {data.events.length > 0 && (
            <Card className="p-3">
              <div className="mb-2 px-1 text-sm font-medium text-fg-1">Recent fires</div>
              <table className="w-full text-[12px]">
                <thead>
                  <tr className="text-left text-fg-3">
                    <th className="py-1 font-medium">When</th>
                    <th className="py-1 font-medium">Metric</th>
                    <th className="py-1 text-right font-medium">Value / threshold</th>
                    <th className="py-1 font-medium">Webhook</th>
                  </tr>
                </thead>
                <tbody>
                  {data.events.map((ev, i) => (
                    <tr key={i} className="border-t border-line-2">
                      <td className="py-1 text-fg-2">{dateTime(ev.fired_at)}</td>
                      <td className="py-1 font-mono text-fg-2">{metricLabel(ev.metric)}</td>
                      <td className="py-1 text-right text-fg-1">
                        {fmtMetric(ev.metric, ev.value)} / {fmtMetric(ev.metric, ev.threshold)}
                      </td>
                      <td className="py-1">
                        {ev.delivered ? <Pill variant="success">delivered</Pill> : <Pill variant="warn">failed</Pill>}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </Card>
          )}
        </div>
      )}
    </>
  );
}

function CreateForm({ onCreate, busy, err }: { onCreate: (r: NewObsAlert) => void; busy: boolean; err: string | null }) {
  const [name, setName] = useState("");
  const [metric, setMetric] = useState("error_rate");
  const [threshold, setThreshold] = useState("");
  const [webhook, setWebhook] = useState("");
  const unit = METRICS.find((m) => m.value === metric)?.unit ?? "";

  return (
    <Card className="p-4">
      <div className="mb-3 text-sm font-medium text-fg-1">New alert rule</div>
      <div className="flex flex-wrap items-end gap-3">
        <Field label="Name">
          <input value={name} onChange={(e) => setName(e.target.value)} placeholder="High error rate" className={inputCls} />
        </Field>
        <Field label="Metric">
          <select value={metric} onChange={(e) => setMetric(e.target.value)} className={inputCls}>
            {METRICS.map((m) => (
              <option key={m.value} value={m.value}>{m.label}</option>
            ))}
          </select>
        </Field>
        <Field label={`Threshold (${unit === "%" ? "0–1" : unit})`}>
          <input value={threshold} onChange={(e) => setThreshold(e.target.value)} placeholder={metric === "error_rate" ? "0.05" : metric === "cost_usd" ? "100" : "5000"} className={`${inputCls} w-28`} />
        </Field>
        <Field label="Webhook URL (optional)">
          <input value={webhook} onChange={(e) => setWebhook(e.target.value)} placeholder="https://hooks.slack.com/…" className={`${inputCls} w-72`} />
        </Field>
        <button
          onClick={() => {
            const t = parseFloat(threshold);
            if (!isFinite(t)) return;
            onCreate({ name, metric, threshold: t, webhook_url: webhook || undefined });
          }}
          disabled={busy || threshold === ""}
          className="rounded-md bg-accent px-3 py-1.5 text-[13px] font-medium text-white hover:opacity-90 disabled:opacity-50"
        >
          Create rule
        </button>
      </div>
      {err && <p className="mt-2 text-[12px] text-danger">{err}</p>}
    </Card>
  );
}

const inputCls = "rounded-md border border-line-2 bg-bg-1 px-2 py-1.5 text-[13px] text-fg-1 outline-none focus:border-accent";

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <label className="flex flex-col gap-1">
      <span className="text-[11px] text-fg-3">{label}</span>
      {children}
    </label>
  );
}

function metricLabel(m: string): string {
  return METRICS.find((x) => x.value === m)?.label ?? m;
}

function fmtMetric(metric: string, v: number): string {
  if (metric === "error_rate") return pct1(v);
  if (metric === "cost_usd") return usd(v);
  return `${Math.round(v)}ms`;
}
