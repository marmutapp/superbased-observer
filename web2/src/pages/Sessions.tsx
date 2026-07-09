import { useMemo, useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import {
  api,
  type MessageEntry,
  type MessagesResult,
  type SessionDetailResult,
  type SessionRow,
} from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { useFilters } from "@/lib/filters";
import { compact, dateTime, num, usd } from "@/lib/format";
import { Card, Empty, ErrorState, PageHeader, Spinner } from "@/components/ui";
import { Pill, SlideOver, ToolBadge } from "@/components/primitives";
import { DataTable, Pagination } from "@/components/DataTable";

const PAGE = 50;

// durationStr renders a compact session duration from ISO start/end.
function durationStr(start?: string, end?: string): string {
  if (!start || !end) return "—";
  const ms = new Date(end).getTime() - new Date(start).getTime();
  if (!Number.isFinite(ms) || ms <= 0) return "—";
  const min = Math.round(ms / 60000);
  if (min < 60) return `${min}m`;
  const h = Math.floor(min / 60);
  return `${h}h ${min % 60}m`;
}

export function SessionsPage() {
  const { days } = useFilters();
  const [offset, setOffset] = useState(0);
  const [tool, setTool] = useState("");
  const [model, setModel] = useState("");
  const [selected, setSelected] = useState<string | null>(null);

  const { data, error, loading, reload } = useApi(
    () => api.sessions({ days, limit: PAGE, offset, tool: tool || undefined, model: model || undefined }),
    [days, offset, tool, model],
  );
  // Option lists for the filter row, from the full org tool/model rollups.
  const tools = useApi(() => api.tools(days), [days]);
  const models = useApi(() => api.models(days), [days]);

  function changeFilter(set: (v: string) => void, v: string) {
    set(v);
    setOffset(0);
  }

  const columns = useMemo<ColumnDef<SessionRow, any>[]>(
    () => [
      {
        id: "developer",
        accessorFn: (s) => s.display_name || s.email || s.user_id,
        header: "Developer",
        cell: (c) => {
          const s = c.row.original;
          return (
            <div>
              <div className="text-fg-1">{s.display_name || s.email || s.user_id}</div>
              {s.display_name && s.email && <div className="text-[11px] text-fg-3">{s.email}</div>}
            </div>
          );
        },
      },
      {
        accessorKey: "tool",
        header: "Tool",
        cell: (c) => (c.row.original.tool ? <ToolBadge tool={c.row.original.tool} /> : <span className="text-fg-4">—</span>),
      },
      {
        accessorKey: "model",
        header: "Model",
        cell: (c) => (
          <span className="truncate font-mono text-[11px] text-fg-2" title={c.row.original.model}>
            {c.row.original.model || "—"}
          </span>
        ),
      },
      {
        id: "started",
        accessorKey: "started_at",
        header: "Started",
        cell: (c) => (
          <span className="whitespace-nowrap text-fg-3">
            {c.row.original.started_at ? dateTime(c.row.original.started_at) : "—"}
          </span>
        ),
      },
      {
        id: "duration",
        header: "Duration",
        enableSorting: false,
        cell: (c) => (
          <span className="text-fg-3">{durationStr(c.row.original.started_at, c.row.original.ended_at)}</span>
        ),
        meta: { align: "right" },
      },
      {
        accessorKey: "cost_usd",
        header: "Cost",
        cell: (c) => usd(c.row.original.cost_usd),
        meta: { align: "right", mono: true },
      },
      {
        accessorKey: "tokens",
        header: "Tokens",
        cell: (c) => compact(c.row.original.tokens),
        meta: { align: "right", mono: true },
      },
      {
        accessorKey: "action_count",
        header: "Actions",
        cell: (c) => num(c.row.original.action_count),
        meta: { align: "right" },
      },
    ],
    [],
  );

  return (
    <>
      <PageHeader
        title="Sessions"
        subtitle="Scoped session list. Per-developer identity is an audited disclosure (a view_org_sessions audit entry is written when this loads). Message content is shown only where a node opted to share it (audited separately)."
      />

      {/* Filter row. */}
      <div className="mb-4 flex flex-wrap items-center gap-2 text-[12px]">
        <FilterSelect
          label="Tool"
          value={tool}
          onChange={(v) => changeFilter(setTool, v)}
          options={(tools.data?.tools ?? []).map((t) => t.tool)}
        />
        <FilterSelect
          label="Model"
          value={model}
          onChange={(v) => changeFilter(setModel, v)}
          options={(models.data?.models ?? []).map((m) => m.model)}
        />
        {(tool || model) && (
          <button
            type="button"
            onClick={() => {
              setTool("");
              setModel("");
              setOffset(0);
            }}
            className="rounded-2 border border-line-2 bg-bg-2 px-2 py-1 text-fg-2 hover:bg-bg-3 hover:text-fg-0"
          >
            Clear
          </button>
        )}
      </div>

      {error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : !data ? (
        <Card className="p-4">
          <Spinner label="Loading sessions…" />
        </Card>
      ) : data.total === 0 ? (
        <Empty message={tool || model ? "No sessions match the filters." : "No sessions in scope for this window."} />
      ) : (
        <Card className="p-3">
          <DataTable
            data={data.sessions}
            columns={columns}
            rowKey={(s) => s.session_id}
            onRowClick={(s) => setSelected(s.session_id)}
            initialSort={[{ id: "started", desc: true }]}
            zebra
            minWidth={900}
            loading={loading}
          />
          <Pagination page={Math.floor(offset / PAGE) + 1} limit={PAGE} total={data.total} onPage={(p) => setOffset((p - 1) * PAGE)} loading={loading} />
        </Card>
      )}

      <SessionDetailDrawer id={selected} onClose={() => setSelected(null)} />
    </>
  );
}

function FilterSelect({
  label,
  value,
  onChange,
  options,
}: {
  label: string;
  value: string;
  onChange: (v: string) => void;
  options: string[];
}) {
  return (
    <label className="inline-flex items-center gap-1.5 text-fg-3">
      {label}
      <select
        value={value}
        onChange={(e) => onChange(e.target.value)}
        className="rounded-2 border border-line-2 bg-bg-2 px-2 py-1 text-[12px] text-fg-1 focus:border-accent focus:outline-none"
      >
        <option value="">All</option>
        {options.map((o) => (
          <option key={o} value={o}>
            {o}
          </option>
        ))}
      </select>
    </label>
  );
}

function SessionDetailDrawer({ id, onClose }: { id: string | null; onClose: () => void }) {
  const { data, error, loading } = useApi(
    () => (id ? api.sessionDetail(id) : Promise.resolve(null as SessionDetailResult | null)),
    [id],
  );
  const open = id !== null;
  return (
    <SlideOver
      open={open}
      onClose={onClose}
      title="Session detail"
      subtitle={id ? <span className="font-mono">{id}</span> : ""}
      width={720}
    >
      <div className="p-5">
        {error ? (
          <p className="text-sm text-bad">{error}</p>
        ) : loading || !data ? (
          <Spinner label="Loading session…" />
        ) : (
          <DetailBody d={data} />
        )}
      </div>
    </SlideOver>
  );
}

function DetailBody({ d }: { d: SessionDetailResult }) {
  const b = d.buckets;
  const maxAction = Math.max(1, ...d.action_types.map((a) => a.count));
  return (
    <div className="space-y-5">
      <div className="flex flex-wrap items-center gap-2">
        {d.tool && <ToolBadge tool={d.tool} />}
        {d.model && <span className="font-mono text-[11px] text-fg-2">{d.model}</span>}
        {d.project_id && <Pill variant="neutral">proj {d.project_id.slice(0, 8)}</Pill>}
      </div>
      <div className="text-[12px] text-fg-3">
        {d.display_name || d.email || d.user_id}
        {d.started_at ? ` · started ${dateTime(d.started_at)}` : ""}
        {d.ended_at ? ` · ended ${dateTime(d.ended_at)}` : ""}
      </div>

      {/* KPI band. */}
      <div className="grid grid-cols-2 gap-2.5 sm:grid-cols-4">
        <Kpi label="Cost" value={usd(d.cost_usd)} accent />
        <Kpi label="Tokens" value={compact(d.tokens)} />
        <Kpi label="Actions" value={num(d.action_count)} />
        <Kpi label="API turns" value={num(d.api_turn_count)} sub={d.api_turn_count === 0 ? "JSONL only" : "proxy"} />
      </div>

      {/* Token buckets. */}
      <div>
        <div className="mb-2 text-[10px] font-semibold uppercase tracking-wide text-fg-3">Token buckets</div>
        <div className="grid grid-cols-2 gap-2.5 sm:grid-cols-5">
          <Kpi label="Net input" value={compact(b.net_input)} />
          <Kpi label="Cache read" value={compact(b.cache_read)} />
          <Kpi label="Cache write" value={compact(b.cache_write)} />
          <Kpi label="Output" value={compact(b.output)} />
          <Kpi label="Reasoning" value={compact(b.reasoning)} />
        </div>
        {b.cache_read === 0 && b.cache_write === 0 && (
          <p className="mt-2 text-[11px] text-fg-3">
            No cache buckets — this session was not proxy-captured (cache read/write are proxy-only).
          </p>
        )}
      </div>

      {/* Action-type breakdown. */}
      <div>
        <div className="mb-2 text-[10px] font-semibold uppercase tracking-wide text-fg-3">Action types</div>
        {d.action_types.length === 0 ? (
          <Empty message="No actions recorded for this session." />
        ) : (
          <div className="space-y-1.5">
            {d.action_types.map((a) => (
              <div key={a.action_type} className="flex items-center gap-2 text-[12px]">
                <span className="w-32 shrink-0 truncate text-fg-2" title={a.action_type}>
                  {a.action_type}
                </span>
                <div className="h-2 flex-1 overflow-hidden rounded-pill bg-bg-3">
                  <div
                    className="h-full bg-accent"
                    style={{ width: `${(a.count / maxAction) * 100}%` }}
                  />
                </div>
                <span className="w-16 shrink-0 text-right font-mono text-fg-2">{num(a.count)}</span>
                <span className="w-14 shrink-0 text-right text-[11px] text-fg-3">
                  {a.count > 0 ? `${Math.round((a.success_count / a.count) * 100)}%` : "—"}
                </span>
              </div>
            ))}
          </div>
        )}
      </div>

      {/* Captured message content — a DEEPER, separately audited disclosure. */}
      <MessagesSection id={d.session_id} />
    </div>
  );
}

// MessagesSection lazily loads the captured OTel message bodies for a session.
// Nothing is fetched (and no view_session_messages audit row is written) until
// the admin actively expands it — reading the actual prose is its own recorded,
// deeper disclosure than the metadata above.
function MessagesSection({ id }: { id: string }) {
  const [expanded, setExpanded] = useState(false);
  const { data, error, loading } = useApi(
    () => (expanded ? api.sessionMessages(id) : Promise.resolve(null as MessagesResult | null)),
    [expanded, id],
  );
  return (
    <div className="border-t border-line-2 pt-4">
      <div className="mb-2 flex items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <span className="text-[10px] font-semibold uppercase tracking-wide text-fg-3">Messages</span>
          <Pill variant="warn">Audited disclosure</Pill>
        </div>
        {!expanded && (
          <button
            type="button"
            onClick={() => setExpanded(true)}
            className="rounded-2 border border-line-2 bg-bg-2 px-2.5 py-1 text-[12px] text-fg-1 hover:bg-bg-3 hover:text-fg-0"
          >
            Show captured messages
          </button>
        )}
      </div>

      {!expanded ? (
        <p className="text-[11px] text-fg-4">
          Message content is captured only on nodes running Claude Code with native-OTel logging
          enabled and content sharing on (full_content / admin_managed). Viewing it writes a
          separate <span className="font-mono">view_session_messages</span> audit entry.
        </p>
      ) : error ? (
        <p className="text-[12px] text-bad">{error}</p>
      ) : loading || !data ? (
        <Spinner label="Loading messages…" />
      ) : data.messages.length === 0 ? (
        <Empty message="No message content captured for this session. The developer's tool/posture produced none on the org wire." />
      ) : !data.content_available ? (
        <div className="space-y-2">
          <p className="text-[11px] text-fg-3">
            Hashes present, bodies not shared — this node shipped {data.messages.length} content
            hash{data.messages.length === 1 ? "" : "es"} for this session but did not enable
            full-content sharing, so no body is available.
          </p>
          <div className="space-y-1">
            {data.messages.map((m, i) => (
              <div key={i} className="flex items-center gap-2 text-[11px]">
                <Pill variant="neutral">{m.kind}</Pill>
                <span className="truncate font-mono text-fg-4" title={m.content_hash}>
                  {m.content_hash}
                </span>
              </div>
            ))}
          </div>
        </div>
      ) : (
        <div className="space-y-3">
          {data.messages.map((m, i) => (
            <MessageBody key={i} m={m} />
          ))}
        </div>
      )}
    </div>
  );
}

// MessageBody renders one captured body (kind + timestamp header, monospace
// scroll-capped content). A hash-only entry inside an otherwise-populated
// session shows its hash instead of a body.
function MessageBody({ m }: { m: MessageEntry }) {
  const kindLabel: Record<string, string> = {
    prompt: "Prompt",
    tool_input: "Tool input",
    tool_output: "Tool output",
    raw_body: "Raw body",
  };
  return (
    <div className="rounded-2 border border-line-2 bg-bg-2">
      <div className="flex items-center justify-between gap-2 border-b border-line-2 px-2.5 py-1.5">
        <Pill variant={m.kind === "prompt" ? "accent" : "neutral"}>{kindLabel[m.kind] ?? m.kind}</Pill>
        <span className="text-[11px] text-fg-3">{m.timestamp ? dateTime(m.timestamp) : ""}</span>
      </div>
      {m.content ? (
        <pre className="max-h-72 overflow-auto whitespace-pre-wrap break-words px-2.5 py-2 text-[12px] leading-snug text-fg-1">
          {m.content}
        </pre>
      ) : (
        <div className="px-2.5 py-2 text-[11px] text-fg-4">
          Body not shared · <span className="font-mono">{m.content_hash}</span>
        </div>
      )}
    </div>
  );
}

function Kpi({ label, value, sub, accent }: { label: string; value: string; sub?: string; accent?: boolean }) {
  return (
    <div className={accent ? "rounded-2 border border-accent/40 bg-bg-2 px-3 py-2.5" : "rounded-2 border border-line-2 bg-bg-2 px-3 py-2.5"}>
      <div className="text-[10px] uppercase tracking-wide text-fg-3">{label}</div>
      <div className="mt-1 font-mono text-[16px] leading-tight text-fg-0">{value}</div>
      {sub ? <div className="mt-0.5 text-[11px] text-fg-3">{sub}</div> : null}
    </div>
  );
}
