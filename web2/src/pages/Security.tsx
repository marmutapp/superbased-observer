import { useState } from "react";
import { Bar, BarChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { api, type GuardAgentsResult, type GuardTrendPoint } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { useFilters } from "@/lib/filters";
import { dateTime, num, pct, shortDate } from "@/lib/format";
import { Button, Card, Empty, ErrorState, PageHeader, Spinner } from "@/components/ui";
import { ChartShell, Pill, StatCard } from "@/components/primitives";
import { CHART_AXIS, CHART_GRID } from "@/components/charts/common";
import { ChartTooltip } from "@/components/charts/ChartTooltip";

type PillVariant = "neutral" | "success" | "warn" | "danger" | "info" | "accent";

// Decision → pill variant (matches the agent dashboard's verdict palette).
const DECISION_VARIANT: Record<string, PillVariant> = {
  deny: "danger",
  ask: "warn",
  flag: "warn",
  mask: "accent",
};

const SEVERITY_VARIANT: Record<string, PillVariant> = {
  critical: "danger",
  high: "danger",
  medium: "warn",
  warn: "warn",
};

// GuardTrendChart renders the per-day verdict mix as stacked bars.
// Content-free (counts only).
function GuardTrendChart({ data }: { data: GuardTrendPoint[] }) {
  if (!data.length) return <Empty message="No guard events in this window." />;
  return (
    <ResponsiveContainer width="100%" height={220}>
      <BarChart data={data} margin={{ top: 8, right: 8, bottom: 0, left: 0 }}>
        <CartesianGrid {...CHART_GRID} />
        <XAxis dataKey="date" tickFormatter={shortDate} {...CHART_AXIS} minTickGap={24} />
        <YAxis {...CHART_AXIS} width={36} />
        <Tooltip
          content={<ChartTooltip labelFormatter={shortDate} />}
          cursor={{ fill: "var(--bg-3)", opacity: 0.4 }}
        />
        <Bar dataKey="deny" stackId="v" fill="var(--danger)" name="Deny" />
        <Bar dataKey="ask" stackId="v" fill="var(--warn)" name="Ask" />
        <Bar dataKey="flag" stackId="v" fill="var(--tok-write)" name="Flag" />
        <Bar dataKey="mask" stackId="v" fill="var(--tool-cline)" name="Mask" />
        <Bar dataKey="other" stackId="v" fill="var(--fg-4)" name="Other" />
      </BarChart>
    </ResponsiveContainer>
  );
}

// AgentChains is the audited per-developer disclosure: the server writes an
// audit_log row on every fetch, so it loads only on an explicit click —
// never as a page-load side effect (the developers-drill-down convention).
function AgentChains() {
  const [data, setData] = useState<GuardAgentsResult | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const load = () => {
    setBusy(true);
    setError(null);
    api
      .guardAgents()
      .then(setData)
      .catch((e: unknown) => setError(String(e)))
      .finally(() => setBusy(false));
  };

  return (
    <Card>
      <div className="mb-2 flex items-center justify-between">
        <div className="text-sm font-medium text-fg-1">Audit-chain continuity</div>
        <Button onClick={load} disabled={busy} title="This disclosure is recorded in the audit log.">
          {busy ? "Loading…" : data ? "Refresh (audited)" : "Load agent report (audited)"}
        </Button>
      </div>
      {error ? (
        <div className="py-2 text-sm text-danger">{error}</div>
      ) : !data ? (
        <div className="py-4 text-sm text-fg-3">
          Per-agent rows are a privacy-sensitive disclosure — loading this report writes an audit-log
          entry, the same rule as the per-developer cost drill-down.
        </div>
      ) : data.agents.length === 0 ? (
        <Empty message="No guard-active agents yet." />
      ) : (
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b border-line-1 text-left text-[11px] uppercase tracking-wide text-fg-3">
              <th className="px-2 py-2 font-medium">Agent</th>
              <th className="px-2 py-2 font-medium">Chain</th>
              <th className="px-2 py-2 text-right font-medium">Events</th>
              <th className="px-2 py-2 text-right font-medium">Segments</th>
              <th className="px-2 py-2 font-medium">First seen</th>
              <th className="px-2 py-2 font-medium">Last seen</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-line-1">
            {data.agents.map((a) => (
              <tr key={a.user_id}>
                <td className="px-2 py-2 text-fg-1">{a.display_name || a.email || a.user_id}</td>
                <td className="px-2 py-2">
                  {a.broken ? <Pill variant="danger">discontinuous</Pill> : <Pill variant="success">continuous</Pill>}
                </td>
                <td className="px-2 py-2 text-right font-mono text-fg-2">{num(a.events)}</td>
                <td className="px-2 py-2 text-right font-mono text-fg-2">{num(a.segments)}</td>
                <td className="px-2 py-2 text-fg-3">{dateTime(a.first_seen ?? "")}</td>
                <td className="px-2 py-2 text-fg-3">{dateTime(a.last_seen ?? "")}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </Card>
  );
}

export function SecurityPage() {
  const { days } = useFilters();
  const overview = useApi(() => api.guardOverview(days), [days]);
  const teams = useApi(() => api.guardTeams(days), [days]);
  const rules = useApi(() => api.guardRules(days), [days]);

  return (
    <>
      <PageHeader
        title="Security"
        subtitle="Guard-layer fleet visibility: verdict trends, rule hits, per-team posture, audit-chain health. Counts and hashes only — never command or file content."
      />
      {overview.error ? (
        <ErrorState message={overview.error} onRetry={overview.reload} />
      ) : overview.loading || !overview.data ? (
        <Spinner />
      ) : (
        <div className="space-y-5">
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
            <StatCard
              label="Guard events"
              value={num(overview.data.total_events)}
              sub={`last ${overview.data.window_days}d`}
            />
            <StatCard
              label="Denied"
              value={num(overview.data.deny_count)}
              sub={`${num(overview.data.ask_count)} asked · ${num(overview.data.flag_count)} flagged · ${num(overview.data.mask_count)} masked`}
            />
            <StatCard label="Guard-active agents" value={num(overview.data.active_agents)} />
            <StatCard
              label="Broken audit chains"
              value={num(overview.data.broken_chain_agents)}
              warn={overview.data.broken_chain_agents > 0}
              sub={overview.data.broken_chain_agents > 0 ? "needs attention" : "all continuous"}
            />
          </div>

          <ChartShell title="Verdicts over time">
            <GuardTrendChart data={overview.data.trend_by_day} />
          </ChartShell>

          <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
            <Card>
              <div className="mb-2 text-sm font-medium text-fg-1">Rule hits</div>
              {rules.error ? (
                <div className="py-2 text-sm text-danger">{rules.error}</div>
              ) : rules.loading || !rules.data ? (
                <Spinner label="Loading rules…" />
              ) : rules.data.rules.length === 0 ? (
                <Empty message="No rule hits in this window." />
              ) : (
                <table className="w-full text-sm">
                  <thead>
                    <tr className="border-b border-line-1 text-left text-[11px] uppercase tracking-wide text-fg-3">
                      <th className="px-2 py-2 font-medium">Rule</th>
                      <th className="px-2 py-2 font-medium">Severity</th>
                      <th className="px-2 py-2 text-right font-medium">Hits</th>
                      <th className="px-2 py-2 text-right font-medium">Agents</th>
                      <th className="px-2 py-2 text-right font-medium">Denies</th>
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-line-1">
                    {rules.data.rules.map((r) => (
                      <tr key={r.rule_id}>
                        <td className="px-2 py-2">
                          <span className="font-mono text-fg-1">{r.rule_id}</span>
                          <span className="ml-2 text-xs text-fg-3">{r.category}</span>
                        </td>
                        <td className="px-2 py-2">
                          <Pill variant={SEVERITY_VARIANT[r.severity] ?? "neutral"}>{r.severity || "—"}</Pill>
                        </td>
                        <td className="px-2 py-2 text-right font-mono text-fg-2">{num(r.hits)}</td>
                        <td className="px-2 py-2 text-right font-mono text-fg-2">{num(r.agents)}</td>
                        <td className="px-2 py-2 text-right font-mono text-fg-2">{num(r.deny_count)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </Card>

            <Card>
              <div className="mb-2 text-sm font-medium text-fg-1">Team posture</div>
              {teams.error ? (
                <div className="py-2 text-sm text-danger">{teams.error}</div>
              ) : teams.loading || !teams.data ? (
                <Spinner label="Loading teams…" />
              ) : teams.data.teams.length === 0 ? (
                <Empty message="No teams in scope." />
              ) : (
                <table className="w-full text-sm">
                  <thead>
                    <tr className="border-b border-line-1 text-left text-[11px] uppercase tracking-wide text-fg-3">
                      <th className="px-2 py-2 font-medium">Team</th>
                      <th className="px-2 py-2 text-right font-medium">Active</th>
                      <th className="px-2 py-2 text-right font-medium">Events</th>
                      <th className="px-2 py-2 text-right font-medium">Enforced</th>
                      <th className="px-2 py-2 text-right font-medium">Chains</th>
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-line-1">
                    {teams.data.teams.map((tm) => (
                      <tr key={tm.team_id}>
                        <td className="px-2 py-2 text-fg-1">{tm.display_name}</td>
                        <td className="px-2 py-2 text-right font-mono text-fg-2">
                          {num(tm.active_agents)}/{num(tm.member_count)}
                        </td>
                        <td className="px-2 py-2 text-right font-mono text-fg-2">{num(tm.events)}</td>
                        <td className="px-2 py-2 text-right font-mono text-fg-2">
                          {tm.events > 0 ? pct(tm.enforced_share) : "—"}
                        </td>
                        <td className="px-2 py-2 text-right">
                          {tm.broken_chain_agents > 0 ? (
                            <Pill variant="danger">{num(tm.broken_chain_agents)} broken</Pill>
                          ) : (
                            <Pill variant="success">ok</Pill>
                          )}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </Card>
          </div>

          {overview.data.top_rules.length > 0 && (
            <div className="flex flex-wrap items-center gap-2 text-xs text-fg-3">
              <span>Decisions:</span>
              {(["deny", "ask", "flag", "mask"] as const).map((d) => (
                <Pill key={d} variant={DECISION_VARIANT[d]}>{d}</Pill>
              ))}
              <span>recorded from hook, proxy and watcher enforcement points.</span>
            </div>
          )}

          <AgentChains />
        </div>
      )}
    </>
  );
}
