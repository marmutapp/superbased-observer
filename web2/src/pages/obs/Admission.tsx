import { useMemo, useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import {
  api,
  type ObsAdmissionChain,
  type ObsAdmissionPolicyVersion,
  type ObsAdmissionReason,
  type ObsAdmissionUserBlocks,
  type ObsAdmissionVerdictRow,
  type ObsEndUserBucket,
} from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { useFilters } from "@/lib/filters";
import { dateTime, num, usd } from "@/lib/format";
import { Button, Card, Empty, ErrorState, PageHeader, Spinner } from "@/components/ui";
import { Pill, StatCard, StatStripSkeleton, TableSkeleton } from "@/components/primitives";
import { DataTable } from "@/components/DataTable";

// ObsAdmissionPage is the READ-ONLY org input-admission monitoring surface
// (Plane-A admission, gap-audit #1b). It renders the posture + verdict timeline
// + would-block/spend overlay + policy version history that enrolled nodes
// share under [org_client.share.obs].admission. Authoring is deliberately
// node-side ONLY (observer obs admission setup / config.toml) — there is NO
// remote policy write here, mirroring the "never server-forced" posture the
// rest of the org surfaces hold. Content-free by default: the request body is
// never shipped, and the human-readable REASON is a separate, server-audited
// lazy read. Admin-only.
export function ObsAdmissionPage() {
  const { days } = useFilters();
  const { data, error, loading, reload } = useApi(() => api.obsAdmission(days), [days]);

  return (
    <>
      <PageHeader
        title="Admission"
        subtitle="Input-admission verdicts + policy versions enrolled nodes shared — an admin gate over incoming app requests. Monitoring-only: authoring stays node-side, and the request body is never shipped."
      />
      {error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : loading || !data ? (
        <div className="space-y-5">
          <StatStripSkeleton count={4} />
          <Card className="p-4">
            <TableSkeleton rows={8} />
          </Card>
        </div>
      ) : !data.configured ? (
        <NotConfigured />
      ) : (
        <div className="space-y-5">
          <PostureRow
            mode={data.mode}
            judgeHosting={data.judge_hosting}
            criteriaCount={data.criteria_count}
            verdicts24h={data.verdicts_24h}
            chain={data.chain}
          />
          <VerdictTimeline verdicts={data.verdicts} days={days} />
          <BudgetCard days={days} wouldBlock={data.would_block_by_user} />
          <PolicyHistory policies={data.policies} />
        </div>
      )}
    </>
  );
}

// --- posture tiles ---------------------------------------------------------

function PostureRow({
  mode,
  judgeHosting,
  criteriaCount,
  verdicts24h,
  chain,
}: {
  mode: string;
  judgeHosting: string;
  criteriaCount: number;
  verdicts24h: { allow: number; flag: number; would_block: number };
  chain: ObsAdmissionChain;
}) {
  const totalVerdicts = verdicts24h.allow + verdicts24h.flag + verdicts24h.would_block;
  const chainDisplay = chainSummary(chain);
  return (
    <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
      <StatCard
        label="Mode"
        value={mode || "—"}
        warn={mode === "enforce"}
        sub={mode ? `${num(criteriaCount)} criteria` : "no policy shared"}
        helpId="tile.obs.admission_mode"
      />
      <StatCard
        label="Judge"
        value={judgeHosting || "—"}
        sub={judgeHosting ? "LLM judge" : "pre-filters only"}
        helpId="tile.obs.admission_judge"
      />
      <StatCard
        label="Verdicts 24h"
        value={num(totalVerdicts)}
        warn={verdicts24h.would_block > 0}
        sub={`allow ${num(verdicts24h.allow)} · flag ${num(verdicts24h.flag)} · would-block ${num(
          verdicts24h.would_block,
        )}`}
        helpId="tile.obs.admission_verdicts"
      />
      <StatCard
        label="Audit chain"
        value={chainDisplay.value}
        warn={!chain.ok}
        sub={chainDisplay.sub}
        helpId="tile.obs.admission_chain"
      />
    </div>
  );
}

// chainSummary renders the (possibly multi-node) hash-chain integrity state.
// One node → "intact" / "BROKEN"; multiple → "N/M nodes intact".
function chainSummary(chain: ObsAdmissionChain): { value: string; sub: string } {
  const nodes = chain.nodes ?? [];
  if (nodes.length <= 1) {
    const rows = nodes[0]?.rows ?? 0;
    return { value: chain.ok ? "intact" : "BROKEN", sub: `${num(rows)} rows` };
  }
  const intact = nodes.filter((n) => n.ok).length;
  const rows = nodes.reduce((a, n) => a + n.rows, 0);
  return {
    value: `${intact}/${nodes.length} nodes`,
    sub: chain.ok ? `intact · ${num(rows)} rows` : `${nodes.length - intact} broken · ${num(rows)} rows`,
  };
}

// --- verdict timeline + lazy reasons --------------------------------------

// decisionVariant maps an admission decision to a Pill variant.
function decisionVariant(d: string): "success" | "warn" | "info" | "danger" | "neutral" {
  switch (d) {
    case "allow":
      return "success";
    case "flag":
      return "warn";
    case "ask":
      return "info";
    case "would_block":
    case "deny":
      return "danger";
    default:
      return "neutral";
  }
}

function VerdictTimeline({ verdicts, days }: { verdicts: ObsAdmissionVerdictRow[]; days: number }) {
  const [reasonsOpen, setReasonsOpen] = useState(false);
  const [reasons, setReasons] = useState<ObsAdmissionReason[] | null>(null);
  const [reasonsLoading, setReasonsLoading] = useState(false);
  const [reasonsError, setReasonsError] = useState<string | null>(null);

  const loadReasons = () => {
    setReasonsOpen(true);
    if (reasons !== null || reasonsLoading) return; // load once
    setReasonsLoading(true);
    setReasonsError(null);
    api
      .obsAdmissionReasons(days)
      .then((r) => setReasons(r.reasons))
      .catch((e: unknown) => setReasonsError(String(e)))
      .finally(() => setReasonsLoading(false));
  };

  const columns = useMemo<ColumnDef<ObsAdmissionVerdictRow, any>[]>(
    () => [
      { accessorKey: "ts", header: "When", cell: (c) => <span className="text-fg-3">{dateTime(c.row.original.ts)}</span> },
      {
        accessorKey: "decision",
        header: "Decision",
        cell: (c) => <Pill variant={decisionVariant(c.row.original.decision)}>{c.row.original.decision || "—"}</Pill>,
      },
      { accessorKey: "severity", header: "Severity", cell: (c) => <span className="text-fg-2">{c.row.original.severity || "—"}</span> },
      {
        accessorKey: "criterion_id",
        header: "Criterion",
        cell: (c) => <span className="font-mono text-xs text-fg-2">{c.row.original.criterion_id || "—"}</span>,
      },
      {
        accessorKey: "judge_used",
        header: "Judge",
        cell: (c) =>
          c.row.original.judge_used ? (
            <Pill variant="accent">{c.row.original.judge_hosting || "judge"}</Pill>
          ) : (
            <span className="text-fg-3">pre-filter</span>
          ),
      },
      {
        accessorKey: "latency_ms",
        header: "Latency",
        cell: (c) => `${num(c.row.original.latency_ms)}ms`,
        meta: { align: "right", mono: true },
      },
      {
        accessorKey: "degraded",
        header: "Note",
        cell: (c) =>
          c.row.original.degraded ? <Pill variant="warn">{c.row.original.degraded}</Pill> : <span className="text-fg-3">—</span>,
      },
    ],
    [],
  );

  return (
    <Card className="p-3">
      <div className="mb-2 flex items-center justify-between gap-3 px-1">
        <div className="text-sm font-medium text-fg-1">
          Verdict timeline
          <span className="ml-2 text-[11px] font-normal text-fg-3">newest first · content-free</span>
        </div>
        <Button onClick={loadReasons} disabled={reasonsOpen && reasonsLoading}>
          {reasonsOpen ? "Reasons loaded" : "View reasons (audited)"}
        </Button>
      </div>
      {verdicts.length === 0 ? (
        <Empty message="No admission verdicts in this window." />
      ) : (
        <DataTable
          data={verdicts}
          columns={columns}
          rowKey={(r) => `${r.request_id}:${r.ts}:${r.criterion_id}`}
          initialSort={[{ id: "ts", desc: true }]}
          zebra
          minWidth={760}
        />
      )}
      {reasonsOpen && (
        <ReasonsPanel loading={reasonsLoading} error={reasonsError} reasons={reasons} />
      )}
    </Card>
  );
}

function ReasonsPanel({
  loading,
  error,
  reasons,
}: {
  loading: boolean;
  error: string | null;
  reasons: ObsAdmissionReason[] | null;
}) {
  return (
    <div className="mt-3 space-y-3 rounded-2 border border-line-2 bg-bg-1 p-3">
      <div className="rounded-2 border border-warn/30 bg-warn-soft px-3 py-2 text-[12px] text-warn">
        Reason excerpts are a deeper disclosure — this read is <b>audited server-side</b> (a row is
        written before the excerpts are returned). Excerpts arrive only from nodes that opted into
        full-content sharing; hash-only nodes contribute nothing here.
      </div>
      {loading ? (
        <Spinner label="Loading reasons…" />
      ) : error ? (
        <div className="text-[12px] text-danger">{error}</div>
      ) : !reasons || reasons.length === 0 ? (
        <Empty message="No reason excerpts shared for this window — nodes shipped verdict metadata only." />
      ) : (
        <ul className="space-y-2">
          {reasons.map((r, i) => (
            <li key={`${r.request_id}:${i}`} className="rounded-2 border border-line-2 bg-bg-2 p-2.5">
              <div className="mb-1 flex items-center gap-2 text-[11px] text-fg-3">
                <span>{dateTime(r.ts)}</span>
                <span className="font-mono text-fg-4">{r.request_id.slice(0, 12) || "—"}</span>
              </div>
              <div className="text-[12.5px] leading-relaxed text-fg-1">{r.reason_excerpt}</div>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

// --- budget / would-block overlay -----------------------------------------

type BudgetRow = {
  end_user: string;
  cost_usd: number | null;
  tokens: number | null;
  would_block: number;
  flag: number;
};

// BudgetCard reuses the existing per-end-user spend surface as the "top
// spenders" leaderboard and overlays the admission would-block/flag counts
// where an end-user matches. Both feeds are separately gated; the empty state
// names both gates honestly.
function BudgetCard({ days, wouldBlock }: { days: number; wouldBlock: ObsAdmissionUserBlocks[] }) {
  const { data, loading, error } = useApi(() => api.obsEndUserSpend(days), [days]);

  const spendConfigured = !!data?.configured;
  const spendUsers: ObsEndUserBucket[] = data?.users ?? [];
  const blocks = wouldBlock ?? [];

  const rows = useMemo<BudgetRow[]>(() => {
    const byUser = new Map<string, BudgetRow>();
    for (const u of spendUsers) {
      byUser.set(u.end_user, {
        end_user: u.end_user,
        cost_usd: u.cost_usd,
        tokens: u.total_tokens,
        would_block: 0,
        flag: 0,
      });
    }
    for (const b of blocks) {
      const existing = byUser.get(b.end_user);
      if (existing) {
        existing.would_block = b.would_block;
        existing.flag = b.flag;
      } else {
        byUser.set(b.end_user, {
          end_user: b.end_user,
          cost_usd: null,
          tokens: null,
          would_block: b.would_block,
          flag: b.flag,
        });
      }
    }
    return Array.from(byUser.values()).sort(
      (a, b) => (b.cost_usd ?? 0) - (a.cost_usd ?? 0) || b.would_block - a.would_block,
    );
  }, [spendUsers, blocks]);

  const columns = useMemo<ColumnDef<BudgetRow, any>[]>(
    () => [
      { accessorKey: "end_user", header: "End-user", cell: (c) => <span className="font-mono text-fg-1">{c.row.original.end_user}</span> },
      {
        accessorKey: "cost_usd",
        header: "Spend",
        cell: (c) => (c.row.original.cost_usd == null ? <span className="text-fg-3">—</span> : usd(c.row.original.cost_usd)),
        meta: { align: "right", mono: true },
      },
      {
        accessorKey: "would_block",
        header: "Would-block",
        cell: (c) =>
          c.row.original.would_block > 0 ? (
            <Pill variant="danger">{num(c.row.original.would_block)}</Pill>
          ) : (
            <span className="text-fg-3">0</span>
          ),
        meta: { align: "right" },
      },
      {
        accessorKey: "flag",
        header: "Flag",
        cell: (c) =>
          c.row.original.flag > 0 ? (
            <Pill variant="warn">{num(c.row.original.flag)}</Pill>
          ) : (
            <span className="text-fg-3">0</span>
          ),
        meta: { align: "right" },
      },
    ],
    [],
  );

  return (
    <Card className="p-3">
      <div className="mb-2 px-1 text-sm font-medium text-fg-1">
        End-user budget & would-block
        <span className="ml-2 text-[11px] font-normal text-fg-3">top spenders, overlaid with admission counts</span>
      </div>
      {loading && !data ? (
        <TableSkeleton rows={5} />
      ) : rows.length === 0 ? (
        <BudgetEmpty spendConfigured={spendConfigured} spendError={error} />
      ) : (
        <>
          {!spendConfigured && (
            <div className="mb-2 rounded-2 border border-line-2 bg-bg-1 px-3 py-2 text-[12px] text-fg-3">
              Spend is not shared for this window (needs{" "}
              <span className="font-mono text-fg-2">[org_client.share].obs_summary</span> +{" "}
              <span className="font-mono text-fg-2">full_content</span>); showing admission
              would-block counts only.
            </div>
          )}
          <DataTable
            data={rows}
            columns={columns}
            rowKey={(r) => r.end_user}
            initialSort={[{ id: "cost_usd", desc: true }]}
            zebra
            minWidth={520}
          />
        </>
      )}
    </Card>
  );
}

function BudgetEmpty({ spendConfigured, spendError }: { spendConfigured: boolean; spendError: string | null }) {
  return (
    <div className="px-1 py-6 text-sm leading-relaxed text-fg-2">
      <p className="max-w-2xl">
        No end-user budget or would-block data for this window. Two independent, node-side gates
        feed this card:
      </p>
      <ul className="mt-2 max-w-2xl list-inside list-disc space-y-1 text-fg-2">
        <li>
          <b className="text-fg-1">Would-block / flag counts</b> ride the admission opt-in —{" "}
          <span className="font-mono text-fg-2">[org_client.share.obs].admission = true</span>, and
          per-end-user counts appear only where the node also shares full content.
        </li>
        <li>
          <b className="text-fg-1">Spend</b> rides the observability summary plus raw-content
          sharing — <span className="font-mono text-fg-2">[org_client.share].obs_summary = true</span>{" "}
          and <span className="font-mono text-fg-2">full_content = true</span> (or{" "}
          <span className="font-mono text-fg-2">admin_managed = true</span> under a native-console
          deployment).
        </li>
      </ul>
      {!spendConfigured && !spendError && (
        <p className="mt-2 text-[12px] text-fg-3">Spend feed reports not-configured for this window.</p>
      )}
      {spendError && <p className="mt-2 text-[12px] text-fg-3">Spend feed: {spendError}</p>}
      <p className="mt-2 text-[12px] text-fg-3">There is no remote toggle for either gate.</p>
    </div>
  );
}

// --- policy version history (read-only) -----------------------------------

function PolicyHistory({ policies }: { policies: ObsAdmissionPolicyVersion[] }) {
  const [openHash, setOpenHash] = useState<string | null>(null);
  const list = policies ?? [];
  return (
    <Card className="p-0">
      <div className="flex items-center justify-between gap-3 border-b border-line-2 px-4 py-2.5">
        <span className="text-sm font-medium text-fg-1">Policy version history</span>
        <span className="text-[11px] text-fg-3">read-only · authoring is node-side</span>
      </div>
      <div className="border-b border-line-2 bg-bg-1 px-4 py-2 text-[12px] leading-relaxed text-fg-3">
        These are the admission policy versions enrolled nodes shared. Authoring happens on the node
        (<span className="font-mono text-fg-2">observer obs admission setup</span> or{" "}
        <span className="font-mono text-fg-2">config.toml</span>) — this surface is monitoring-only,
        there is deliberately no remote apply/save.
      </div>
      {list.length === 0 ? (
        <Empty message="No policy versions shared for this window." />
      ) : (
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b border-line-2 text-left text-[11px] uppercase tracking-wide text-fg-3">
              <th className="px-4 py-2.5 font-medium">Hash</th>
              <th className="px-4 py-2.5 font-medium">Author</th>
              <th className="px-4 py-2.5 font-medium">Mode</th>
              <th className="px-4 py-2.5 font-medium">Scope</th>
              <th className="px-4 py-2.5 text-right font-medium">Criteria</th>
              <th className="px-4 py-2.5 font-medium">Created</th>
              <th className="px-4 py-2.5"></th>
            </tr>
          </thead>
          <tbody className="divide-y divide-line-2">
            {list.map((p) => {
              const open = openHash === p.policy_hash;
              return (
                <PolicyRow key={p.policy_hash} p={p} open={open} onToggle={() => setOpenHash(open ? null : p.policy_hash)} />
              );
            })}
          </tbody>
        </table>
      )}
    </Card>
  );
}

function PolicyRow({
  p,
  open,
  onToggle,
}: {
  p: ObsAdmissionPolicyVersion;
  open: boolean;
  onToggle: () => void;
}) {
  return (
    <>
      <tr>
        <td className="px-4 py-2.5 font-mono text-fg-1">{p.policy_hash.slice(0, 12) || "—"}</td>
        <td className="px-4 py-2.5 text-fg-2">{p.user_email || "—"}</td>
        <td className="px-4 py-2.5">
          <Pill variant={p.mode === "enforce" ? "danger" : "neutral"}>{p.mode || "—"}</Pill>
        </td>
        <td className="px-4 py-2.5 text-fg-2">{p.scope || "—"}</td>
        <td className="px-4 py-2.5 text-right font-mono text-fg-2">{num(p.criteria_count)}</td>
        <td className="px-4 py-2.5 text-fg-2">{dateTime(p.created_at)}</td>
        <td className="px-4 py-2.5 text-right">
          <Button onClick={onToggle}>{open ? "Hide" : "View"}</Button>
        </td>
      </tr>
      {open && (
        <tr>
          <td colSpan={7} className="px-4 py-3">
            <PolicyBody body={p.body} />
          </td>
        </tr>
      )}
    </>
  );
}

// PolicyBody renders the policy body read-only with a client-side JSON lint:
// a valid JSON body is pretty-printed with a "valid JSON" badge; a parse error
// is surfaced inline and the raw body shown as-is (it may be a non-JSON native
// format). There is NO apply/save — monitoring-only, by design.
function PolicyBody({ body }: { body: string }) {
  const lint = useMemo(() => {
    if (!body.trim()) return { ok: false, error: "empty body", pretty: "" };
    try {
      const pretty = JSON.stringify(JSON.parse(body), null, 2);
      return { ok: true, error: "", pretty };
    } catch (e) {
      return { ok: false, error: (e as Error).message, pretty: "" };
    }
  }, [body]);

  return (
    <div className="space-y-2">
      <div className="flex items-center gap-2">
        {lint.ok ? (
          <Pill variant="success">valid JSON</Pill>
        ) : (
          <Pill variant="danger">not valid JSON</Pill>
        )}
        {!lint.ok && <span className="text-[12px] text-danger">{lint.error}</span>}
        {!lint.ok && body.trim() && (
          <span className="text-[11px] text-fg-3">(shown raw — may be a native/TOML policy format)</span>
        )}
      </div>
      <pre className="max-h-96 overflow-auto rounded-2 border border-line-2 bg-bg-3 p-3 font-mono text-xs text-fg-1">
        {lint.ok ? lint.pretty : body || "(empty)"}
      </pre>
    </div>
  );
}

// --- not configured --------------------------------------------------------

function NotConfigured() {
  return (
    <Card className="p-6">
      <h3 className="text-[15px] font-semibold text-fg-0">No admission data shared</h3>
      <p className="mt-2 max-w-2xl text-sm leading-relaxed text-fg-2">
        No enrolled node has shared input-admission verdicts for this window. Admission monitoring is
        a <b className="text-fg-1">node-side opt-in</b>: at least one enrolled node must set{" "}
        <span className="font-mono text-fg-2">[org_client.share.obs].admission = true</span> in its
        local <span className="font-mono text-fg-2">~/.observer/config.toml</span> for the
        content-free verdict metadata (decision, severity, criterion, latency — never the request
        body) and policy versions to reach this server.
      </p>
      <p className="mt-4 text-[12px] text-fg-3">
        There is <b className="text-fg-2">no remote toggle</b> the org admin can flip — the share is
        authored on the node, matching the "never server-forced" posture the rest of the org
        surfaces hold. Authoring the admission policy itself is likewise node-side (
        <span className="font-mono text-fg-2">observer obs admission setup</span>). See{" "}
        <span className="font-mono text-fg-2">docs/deployment-models.md</span>.
      </p>
    </Card>
  );
}
