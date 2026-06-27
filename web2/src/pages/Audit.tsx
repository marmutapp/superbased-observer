import { useMemo, useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { api, type AuditEntry } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { dateTime } from "@/lib/format";
import { Button, Card, Empty, ErrorState, PageHeader, Spinner } from "@/components/ui";
import { Pill } from "@/components/primitives";
import { DataTable } from "@/components/DataTable";

const PAGE = 50;

const ACTION_LABEL: Record<string, string> = {
  view_team_developers: "Viewed developers",
  view_org_developers: "Viewed people",
  view_org_sessions: "Viewed sessions",
  view_session_messages: "Viewed messages",
  drill_down_developers: "Drill-down",
  revoke_bearer: "Revoked bearer",
  set_team_role: "Changed role",
  view_guard_agents: "Viewed agent chains",
  publish_policy_bundle: "Published policy bundle",
};

export function AuditPage() {
  const [offset, setOffset] = useState(0);
  const { data, error, loading, reload } = useApi(() => api.audit(PAGE, offset), [offset]);

  // Server returns a newest-first page; client-side sort is disabled so the
  // table reflects the true server order across the Newer/Older pager.
  const columns = useMemo<ColumnDef<AuditEntry, any>[]>(
    () => [
      {
        accessorKey: "timestamp",
        header: "When",
        enableSorting: false,
        cell: (c) => (
          <span className="whitespace-nowrap text-fg-3">{dateTime(c.row.original.timestamp)}</span>
        ),
      },
      {
        id: "actor",
        header: "Actor",
        enableSorting: false,
        cell: (c) => (
          <span className="text-fg-1">{c.row.original.actor_email || c.row.original.actor_user_id}</span>
        ),
      },
      {
        id: "action",
        header: "Action",
        enableSorting: false,
        cell: (c) => (
          <Pill variant="accent">{ACTION_LABEL[c.row.original.action] ?? c.row.original.action}</Pill>
        ),
      },
      {
        id: "target",
        header: "Target",
        enableSorting: false,
        cell: (c) => {
          const e = c.row.original;
          return (
            <span className="text-fg-2">
              {e.target_team_id || "—"}
              {e.target_detail ? ` · ${e.target_detail}` : ""}
            </span>
          );
        },
      },
      {
        accessorKey: "source_ip",
        header: "Source IP",
        enableSorting: false,
        cell: (c) => c.row.original.source_ip || "—",
        meta: { mono: true },
      },
    ],
    [],
  );

  return (
    <>
      <PageHeader
        title="Audit log"
        subtitle="Every developer drill-down and admin action. Append-only, newest first."
      />
      {error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : loading || !data ? (
        <Spinner />
      ) : data.entries.length === 0 ? (
        <Empty message="No audit entries yet." />
      ) : (
        <>
          <Card className="p-3">
            <DataTable
              data={data.entries}
              columns={columns}
              rowKey={(e) => String(e.id)}
              zebra
              minWidth={720}
            />
          </Card>
          <div className="mt-3 flex items-center justify-between text-xs text-fg-3">
            <Button onClick={() => setOffset(Math.max(0, offset - PAGE))} disabled={offset === 0}>
              ← Newer
            </Button>
            <span>
              Showing {offset + 1}–{offset + data.entries.length}
            </span>
            <Button onClick={() => setOffset(data.next_offset)} disabled={!data.has_more}>
              Older →
            </Button>
          </div>
        </>
      )}
    </>
  );
}
