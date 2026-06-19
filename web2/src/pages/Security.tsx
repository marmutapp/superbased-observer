import { useState } from "react";
import { Bar, BarChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { api, type GuardAgentsResult, type GuardTrendPoint } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { dateTime, num, pct, shortDate } from "@/lib/format";
import { Badge, Button, Card, Empty, ErrorState, PageHeader, Spinner, Stat } from "@/components/ui";
import { WindowSelect } from "@/components/WindowSelect";

// Decision → badge tone (matches the agent dashboard's verdict palette).
const DECISION_TONE: Record<string, "bad" | "warn" | "accent" | "muted"> = {
  deny: "bad",
  ask: "warn",
  flag: "warn",
  mask: "accent",
};

const SEVERITY_TONE: Record<string, "bad" | "warn" | "muted"> = {
  critical: "bad",
  high: "bad",
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
        <XAxis
          dataKey="date"
          tickFormatter={shortDate}
          tick={{ fill: "#9aa3b2", fontSize: 11 }}
          axisLine={{ stroke: "#262b33" }}
          tickLine={false}
          minTickGap={24}
        />
        <YAxis tick={{ fill: "#9aa3b2", fontSize: 11 }} axisLine={false} tickLine={false} width={36} />
        <Tooltip
          contentStyle={{
            background: "#14171c",
            border: "1px solid #262b33",
            borderRadius: 8,
            color: "#e6e9ef",
            fontSize: 12,
          }}
          labelFormatter={(l: string) => shortDate(l)}
        />
        <Bar dataKey="deny" stackId="v" fill="#ef4444" name="Deny" />
        <Bar dataKey="ask" stackId="v" fill="#f59e0b" name="Ask" />
        <Bar dataKey="flag" stackId="v" fill="#eab308" name="Flag" />
        <Bar dataKey="mask" stackId="v" fill="#3b82f6" name="Mask" />
        <Bar dataKey="other" stackId="v" fill="#475569" name="Other" />
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
        <div className="text-sm font-medium text-fg">Audit-chain continuity</div>
        <Button onClick={load} disabled={busy} title="This disclosure is recorded in the audit log.">
          {busy ? "Loading…" : data ? "Refresh (audited)" : "Load agent report (audited)"}
        </Button>
      </div>
      {error ? (
        <div className="py-2 text-sm text-bad">{error}</div>
      ) : !data ? (
        <div className="py-4 text-sm text-faint">
          Per-agent rows are a privacy-sensitive disclosure — loading this report writes an audit-log
          entry, the same rule as the per-developer cost drill-down.
        </div>
      ) : data.agents.length === 0 ? (
        <Empty message="No guard-active agents yet." />
      ) : (
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b border-line text-left text-[11px] uppercase tracking-wide text-faint">
              <th className="px-2 py-2 font-medium">Agent</th>
              <th className="px-2 py-2 font-medium">Chain</th>
              <th className="px-2 py-2 text-right font-medium">Events</th>
              <th className="px-2 py-2 text-right font-medium">Segments</th>
              <th className="px-2 py-2 font-medium">First seen</th>
              <th className="px-2 py-2 font-medium">Last seen</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-line">
            {data.agents.map((a) => (
              <tr key={a.user_id}>
                <td className="px-2 py-2 text-fg">{a.display_name || a.email || a.user_id}</td>
                <td className="px-2 py-2">
                  {a.broken ? <Badge tone="bad">discontinuous</Badge> : <Badge tone="good">continuous</Badge>}
                </td>
                <td className="px-2 py-2 text-right font-mono text-muted">{num(a.events)}</td>
                <td className="px-2 py-2 text-right font-mono text-muted">{num(a.segments)}</td>
                <td className="px-2 py-2 text-muted">{dateTime(a.first_seen ?? "")}</td>
                <td className="px-2 py-2 text-muted">{dateTime(a.last_seen ?? "")}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </Card>
  );
}

export function SecurityPage() {
  const [days, setDays] = useState(30);
  const overview = useApi(() => api.guardOverview(days), [days]);
  const teams = useApi(() => api.guardTeams(days), [days]);
  const rules = useApi(() => api.guardRules(days), [days]);

  return (
    <>
      <PageHeader
        title="Security"
        subtitle="Guard-layer fleet visibility: verdict trends, rule hits, per-team posture, audit-chain health. Counts and hashes only — never command or file content."
        right={<WindowSelect value={days} onChange={setDays} />}
      />
      {overview.error ? (
        <ErrorState message={overview.error} onRetry={overview.reload} />
      ) : overview.loading || !overview.data ? (
        <Spinner />
      ) : (
        <div className="space-y-5">
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
            <Stat
              label="Guard events"
              value={num(overview.data.total_events)}
              sub={`last ${overview.data.window_days}d`}
            />
            <Stat label="Denied" value={num(overview.data.deny_count)} sub={`${num(overview.data.ask_count)} asked · ${num(overview.data.flag_count)} flagged · ${num(overview.data.mask_count)} masked`} />
            <Stat label="Guard-active agents" value={num(overview.data.active_agents)} />
            <Stat
              label="Broken audit chains"
              value={num(overview.data.broken_chain_agents)}
              sub={overview.data.broken_chain_agents > 0 ? "needs attention" : "all continuous"}
            />
          </div>

          <Card>
            <div className="mb-3 text-sm font-medium text-fg">Verdicts over time</div>
            <GuardTrendChart data={overview.data.trend_by_day} />
          </Card>

          <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
            <Card>
              <div className="mb-2 text-sm font-medium text-fg">Rule hits</div>
              {rules.error ? (
                <div className="py-2 text-sm text-bad">{rules.error}</div>
              ) : rules.loading || !rules.data ? (
                <Spinner label="Loading rules…" />
              ) : rules.data.rules.length === 0 ? (
                <Empty message="No rule hits in this window." />
              ) : (
                <table className="w-full text-sm">
                  <thead>
                    <tr className="border-b border-line text-left text-[11px] uppercase tracking-wide text-faint">
                      <th className="px-2 py-2 font-medium">Rule</th>
                      <th className="px-2 py-2 font-medium">Severity</th>
                      <th className="px-2 py-2 text-right font-medium">Hits</th>
                      <th className="px-2 py-2 text-right font-medium">Agents</th>
                      <th className="px-2 py-2 text-right font-medium">Denies</th>
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-line">
                    {rules.data.rules.map((r) => (
                      <tr key={r.rule_id}>
                        <td className="px-2 py-2">
                          <span className="font-mono text-fg">{r.rule_id}</span>
                          <span className="ml-2 text-xs text-faint">{r.category}</span>
                        </td>
                        <td className="px-2 py-2">
                          <Badge tone={SEVERITY_TONE[r.severity] ?? "muted"}>{r.severity || "—"}</Badge>
                        </td>
                        <td className="px-2 py-2 text-right font-mono text-muted">{num(r.hits)}</td>
                        <td className="px-2 py-2 text-right font-mono text-muted">{num(r.agents)}</td>
                        <td className="px-2 py-2 text-right font-mono text-muted">{num(r.deny_count)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </Card>

            <Card>
              <div className="mb-2 text-sm font-medium text-fg">Team posture</div>
              {teams.error ? (
                <div className="py-2 text-sm text-bad">{teams.error}</div>
              ) : teams.loading || !teams.data ? (
                <Spinner label="Loading teams…" />
              ) : teams.data.teams.length === 0 ? (
                <Empty message="No teams in scope." />
              ) : (
                <table className="w-full text-sm">
                  <thead>
                    <tr className="border-b border-line text-left text-[11px] uppercase tracking-wide text-faint">
                      <th className="px-2 py-2 font-medium">Team</th>
                      <th className="px-2 py-2 text-right font-medium">Active</th>
                      <th className="px-2 py-2 text-right font-medium">Events</th>
                      <th className="px-2 py-2 text-right font-medium">Enforced</th>
                      <th className="px-2 py-2 text-right font-medium">Chains</th>
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-line">
                    {teams.data.teams.map((tm) => (
                      <tr key={tm.team_id}>
                        <td className="px-2 py-2 text-fg">{tm.display_name}</td>
                        <td className="px-2 py-2 text-right font-mono text-muted">
                          {num(tm.active_agents)}/{num(tm.member_count)}
                        </td>
                        <td className="px-2 py-2 text-right font-mono text-muted">{num(tm.events)}</td>
                        <td className="px-2 py-2 text-right font-mono text-muted">
                          {tm.events > 0 ? pct(tm.enforced_share) : "—"}
                        </td>
                        <td className="px-2 py-2 text-right">
                          {tm.broken_chain_agents > 0 ? (
                            <Badge tone="bad">{num(tm.broken_chain_agents)} broken</Badge>
                          ) : (
                            <Badge tone="good">ok</Badge>
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
            <div className="text-xs text-faint">
              Decisions:{" "}
              {(["deny", "ask", "flag", "mask"] as const).map((d) => (
                <span key={d} className="mr-2">
                  <Badge tone={DECISION_TONE[d]}>{d}</Badge>
                </span>
              ))}
              recorded from hook, proxy and watcher enforcement points.
            </div>
          )}

          <AgentChains />
        </div>
      )}
    </>
  );
}
