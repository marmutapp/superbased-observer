import { useMemo, useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { Eye, ShieldAlert } from "lucide-react";
import { api, type PeopleResult, type PersonRollup } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { useFilters } from "@/lib/filters";
import { compact, dateTime, num, usd } from "@/lib/format";
import { Button, Card, Empty, ErrorState, PageHeader, Spinner } from "@/components/ui";
import { Sparkline, StatCard, ToolBadge } from "@/components/primitives";
import { DataTable } from "@/components/DataTable";

export function PeoplePage() {
  const { days } = useFilters();
  // Aggregate header — content-free org-wide totals, NOT a per-developer
  // disclosure, so it loads by default (no audit).
  const overview = useApi(() => api.overview(days), [days]);

  // The named leaderboard is the SAME audited disclosure class as the per-team
  // developer drill-down: loading it writes a view_org_developers audit row, so
  // it loads only on an explicit click — never as a page-load side effect.
  const [people, setPeople] = useState<PeopleResult | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  async function reveal() {
    setBusy(true);
    setErr(null);
    try {
      setPeople(await api.people(days)); // AUDITED on the server
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  const ov = overview.data;

  return (
    <>
      <PageHeader
        title="People"
        subtitle="Org-wide per-developer activity. Works without SCIM team groups. The named leaderboard is an audited disclosure."
      />

      {/* Aggregate context (no audit). */}
      <div className="mb-5 grid grid-cols-2 gap-3 sm:grid-cols-4">
        <StatCard
          label="Active developers"
          value={ov ? num(ov.active_developers) : "—"}
          loading={overview.loading}
          helpId="tile.active_developers"
        />
        <StatCard label="Total spend" value={ov ? usd(ov.total_cost_usd) : "—"} sub={ov ? `last ${ov.window_days}d` : undefined} loading={overview.loading} accent helpId="tile.total_spend" />
        <StatCard
          label="Avg / developer"
          value={ov && ov.active_developers > 0 ? usd(ov.total_cost_usd / ov.active_developers) : "—"}
          loading={overview.loading}
        />
        <StatCard label="Sessions" value={ov ? num(ov.total_sessions) : "—"} loading={overview.loading} />
      </div>

      {/* Audited per-developer leaderboard. */}
      <Card>
        <div className="flex items-center justify-between">
          <div className="flex items-center gap-2 text-sm font-medium text-fg-1">
            <ShieldAlert className="h-4 w-4 text-warn" />
            Developer leaderboard
          </div>
          {!people && (
            <Button variant="primary" onClick={reveal} disabled={busy} title="This action will be recorded in the audit log">
              <span className="inline-flex items-center gap-1.5">
                <Eye className="h-3.5 w-3.5" />
                {busy ? "Recording…" : "Load developer leaderboard (audited)"}
              </span>
            </Button>
          )}
        </div>
        {!people && !err && (
          <p className="mt-2 text-xs text-fg-3">
            Per-developer rows are a privacy-sensitive disclosure — loading this
            leaderboard writes a <code>view_org_developers</code> entry to the
            audit log, the same rule as the per-team cost drill-down. Scoped to
            your role (admins see everyone; leads see their teams).
          </p>
        )}
        {err && <p className="mt-3 text-sm text-bad">{err}</p>}
        {busy && !people && <Spinner label="Loading leaderboard…" />}
        {people && <Leaderboard data={people} />}
      </Card>

      {overview.error && !ov && (
        <div className="mt-3">
          <ErrorState message={overview.error} onRetry={overview.reload} />
        </div>
      )}
    </>
  );
}

function Leaderboard({ data }: { data: PeopleResult }) {
  const columns = useMemo<ColumnDef<PersonRollup, any>[]>(
    () => [
      {
        id: "rank",
        header: "#",
        enableSorting: false,
        cell: (c) => <span className="font-mono text-fg-3">{c.row.index + 1}</span>,
      },
      {
        id: "developer",
        accessorFn: (p) => p.display_name || p.email,
        header: "Developer",
        cell: (c) => {
          const p = c.row.original;
          return (
            <div>
              <div className="text-fg-1">{p.display_name || p.email}</div>
              {p.display_name && <div className="text-[11px] text-fg-3">{p.email}</div>}
            </div>
          );
        },
      },
      {
        accessorKey: "cost_usd",
        header: "Spend",
        cell: (c) => usd(c.row.original.cost_usd),
        meta: { align: "right", mono: true },
      },
      {
        id: "spark",
        header: "7d",
        enableSorting: false,
        cell: (c) => {
          const s = c.row.original.spark;
          return s && s.length >= 2 ? (
            <Sparkline data={s} width={64} height={20} />
          ) : (
            <span className="text-fg-4">—</span>
          );
        },
      },
      {
        accessorKey: "session_count",
        header: "Sessions",
        cell: (c) => num(c.row.original.session_count),
        meta: { align: "right" },
      },
      {
        accessorKey: "action_count",
        header: "Actions",
        cell: (c) => num(c.row.original.action_count),
        meta: { align: "right" },
      },
      {
        accessorKey: "tokens",
        header: "Tokens",
        cell: (c) => compact(c.row.original.tokens),
        meta: { align: "right", mono: true },
      },
      {
        accessorKey: "top_tool",
        header: "Top tool",
        cell: (c) =>
          c.row.original.top_tool ? (
            <ToolBadge tool={c.row.original.top_tool} />
          ) : (
            <span className="text-fg-4">—</span>
          ),
      },
      {
        accessorKey: "top_model",
        header: "Top model",
        cell: (c) => (
          <span className="truncate text-fg-2" title={c.row.original.top_model}>
            {c.row.original.top_model || "—"}
          </span>
        ),
      },
      {
        accessorKey: "last_active",
        header: "Last active",
        cell: (c) => (
          <span className="text-fg-3">
            {c.row.original.last_active ? dateTime(c.row.original.last_active) : "—"}
          </span>
        ),
        meta: { align: "right" },
      },
    ],
    [],
  );

  if (data.people.length === 0) {
    return <Empty message="No developer activity in this window." />;
  }
  return (
    <div className="mt-3">
      <DataTable
        data={data.people}
        columns={columns}
        rowKey={(p) => p.user_id}
        initialSort={[{ id: "cost_usd", desc: true }]}
        zebra
        minWidth={900}
      />
    </div>
  );
}
