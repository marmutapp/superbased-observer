import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Pill } from "@/components/primitives";
import { ChartState } from "@/components/ChartState";
import { useApi } from "@/lib/useApi";
import { fmtBytes, fmtClock, fmtDuration, fmtInt } from "@/lib/format";
import type {
  MetricSample,
  ProcessFinding,
  ProcessNode,
  SessionProcessResponse,
} from "@/lib/types";

// ProcessesSection — the Process Observability panel in the session-detail
// slide-over (docs/process-observability.md §13.1). The whole section is a
// disclosure: COLLAPSED by default (sticky in localStorage) and lazy-loaded —
// a closed section makes no request and skips the daemon's correlation passes.
// Open it for the OS-level process tree (attribution + the §9.2.4 spawning
// message link + per-process CPU/memory/disk metrics with sparklines) and the
// observe-only §14 findings. Tree or sortable Table view. Node-local; consumes
// GET /api/session/<id>/processes.

type PillVariant = "neutral" | "success" | "warn" | "danger" | "info" | "accent";

const SECTION_OPEN_KEY = "sb_proc_section_open";

const SEVERITY_VARIANT: Record<string, PillVariant> = {
  high: "danger",
  warn: "warn",
  info: "info",
};

function attributionVariant(source: string): PillVariant {
  switch (source) {
    case "env_token":
    case "bridge":
    case "adapter_pid":
      return "accent";
    case "inherited":
      return "neutral";
    case "none":
      return "warn";
    default:
      return "info";
  }
}

function runtimeLabel(n: ProcessNode): string {
  if (!n.exited) return "running";
  if (n.exit_signal && n.exit_signal > 0) return `sig ${n.exit_signal}`;
  return `exit ${n.exit_code}`;
}

function attributionLabel(n: ProcessNode): string {
  const conf =
    n.attribution_confidence && n.attribution_confidence !== "none"
      ? `/${n.attribution_confidence}`
      : "";
  return `${n.attribution_source}${conf}`;
}

// isDerived marks a row synthesized from the AI tool's own exec record (a
// run_command action) rather than observed at the OS level — the commands the
// poll backend missed (sub-interval, born-and-died-between-ticks). Such rows
// carry the deterministic message link but no pid / resource metrics / subtree.
// Keyed on attribution_source (action_correlation) + the pid 0 sentinel.
function isDerived(n: ProcessNode): boolean {
  return n.attribution_source === "action_correlation" && n.pid === 0;
}

// derivedRuntimeLabel reports the coarse outcome of a derived command. The
// actions table records only a success boolean (no numeric exit code), so a
// derived row can honestly say "ok" / "failed", not "exit 137".
function derivedRuntimeLabel(n: ProcessNode): string {
  return n.exit_code === 0 ? "ok" : "failed";
}

function hasMetrics(n: ProcessNode): boolean {
  return !!(
    (n.cpu_ms && n.cpu_ms > 0) ||
    (n.working_set_bytes && n.working_set_bytes > 0) ||
    (n.read_bytes && n.read_bytes > 0) ||
    (n.write_bytes && n.write_bytes > 0)
  );
}

function countDescendants(node: ProcessNode): number {
  let n = node.children.length;
  for (const c of node.children) n += countDescendants(c);
  return n;
}

function flatten(roots: ProcessNode[]): ProcessNode[] {
  const out: ProcessNode[] = [];
  const walk = (ns: ProcessNode[]) => {
    for (const n of ns) {
      out.push(n);
      walk(n.children);
    }
  };
  walk(roots);
  return out;
}

// Sparkline — a tiny SVG polyline of a metric series (default: working set).
function Sparkline({
  samples,
  pick,
  title,
}: {
  samples: MetricSample[];
  pick: (s: MetricSample) => number;
  title?: string;
}) {
  if (!samples || samples.length < 2) return null;
  const vals = samples.map(pick);
  const max = Math.max(1, ...vals);
  const w = 54;
  const h = 14;
  const pts = vals
    .map((v, i) => {
      const x = (i / (vals.length - 1)) * (w - 2) + 1;
      const y = h - 1 - (v / max) * (h - 2);
      return `${x.toFixed(1)},${y.toFixed(1)}`;
    })
    .join(" ");
  return (
    <svg
      width={w}
      height={h}
      viewBox={`0 0 ${w} ${h}`}
      className="shrink-0 text-accent"
      aria-hidden
    >
      {title ? <title>{title}</title> : null}
      <polyline points={pts} fill="none" stroke="currentColor" strokeWidth="1" />
    </svg>
  );
}

// MetricBadges — compact CPU / memory / disk readouts + a working-set
// sparkline. Network is intentionally absent (per-process bytes need ETW).
function MetricBadges({ node }: { node: ProcessNode }) {
  if (!hasMetrics(node)) return null;
  const ws = node.working_set_bytes ?? 0;
  const peak = node.peak_rss_bytes ?? 0;
  const rb = node.read_bytes ?? 0;
  const wb = node.write_bytes ?? 0;
  const memTitle =
    peak > ws ? `working set ${fmtBytes(ws)} · peak ${fmtBytes(peak)}` : `working set ${fmtBytes(ws)}`;
  return (
    <span className="flex flex-wrap items-center gap-x-2 gap-y-0.5 font-mono text-[10.5px] text-fg-3">
      {node.cpu_ms != null && node.cpu_ms > 0 && (
        <span title="cumulative CPU time (user+system)">⏱ {fmtDuration(node.cpu_ms)}</span>
      )}
      {ws > 0 && (
        <span title={memTitle}>
          ▦ {fmtBytes(ws)}
          {peak > ws ? <span className="text-fg-4"> /{fmtBytes(peak)}</span> : null}
        </span>
      )}
      {(rb > 0 || wb > 0) && (
        <span title={`disk read ${fmtBytes(rb)} · write ${fmtBytes(wb)}`}>
          ↓{fmtBytes(rb)} ↑{fmtBytes(wb)}
        </span>
      )}
      {node.thread_count != null && node.thread_count > 0 && (
        <span className="text-fg-4" title="threads">
          {node.thread_count}t
        </span>
      )}
      {node.metric_samples && node.metric_samples.length >= 2 && (
        <Sparkline
          samples={node.metric_samples}
          pick={(s) => s.ws}
          title="working-set trend"
        />
      )}
    </span>
  );
}

function MessageLink({
  node,
  onFocusMessage,
}: {
  node: ProcessNode;
  onFocusMessage?: (messageId: string) => void;
}) {
  if (!node.message_id) return null;
  if (!onFocusMessage) {
    return (
      <span className="font-mono text-[11px] text-accent">{node.message_id}</span>
    );
  }
  return (
    <button
      type="button"
      onClick={() => onFocusMessage(node.message_id!)}
      className="font-mono text-[11px] text-accent hover:underline focus:outline-none"
      title={`Jump to the message that spawned this process (${node.message_id})`}
    >
      {node.message_id}
    </button>
  );
}

function ProcessTreeNode({
  node,
  depth,
  collapsed,
  onToggle,
  onFocusMessage,
}: {
  node: ProcessNode;
  depth: number;
  collapsed: Set<string>;
  onToggle: (key: string) => void;
  onFocusMessage?: (messageId: string) => void;
}) {
  const hasChildren = node.children.length > 0;
  const isCollapsed = collapsed.has(node.process_key);
  return (
    <div>
      <div
        className="flex flex-wrap items-center gap-x-2 gap-y-0.5 border-b border-line-1/60 py-[3px] text-[12px] last:border-b-0"
        style={{ paddingLeft: depth * 14 }}
      >
        {hasChildren ? (
          <button
            type="button"
            onClick={() => onToggle(node.process_key)}
            className="w-[16px] select-none text-left text-fg-3 hover:text-fg-1"
            aria-label={isCollapsed ? "Expand" : "Collapse"}
            title={
              isCollapsed
                ? `Expand (${countDescendants(node)} descendant${countDescendants(node) === 1 ? "" : "s"})`
                : "Collapse"
            }
          >
            {isCollapsed ? "▸" : "▾"}
          </button>
        ) : (
          <span className="w-[16px] select-none text-fg-4">·</span>
        )}
        <span className="font-mono text-fg-1">{node.exe || "?"}</span>
        {isDerived(node) ? (
          <>
            <Pill
              variant="neutral"
              title="From the tool's own exec record (run_command action) — no OS process was captured for this command (it finished between poll ticks), so there is no pid, resource metrics, or subtree. The message link is exact."
            >
              from tool log
            </Pill>
            <span className={node.exit_code === 0 ? "text-fg-3" : "text-danger"}>
              {derivedRuntimeLabel(node)}
            </span>
          </>
        ) : (
          <>
            <span className="text-fg-3">pid {node.pid}</span>
            <Pill variant={attributionVariant(node.attribution_source)}>
              {attributionLabel(node)}
            </Pill>
            <span className={node.exited ? "text-fg-3" : "text-success"}>
              {runtimeLabel(node)}
            </span>
          </>
        )}
        {node.started_at && (
          <span className="text-fg-4" title={`started ${node.started_at}`}>
            {fmtClock(node.started_at)}
          </span>
        )}
        {isCollapsed && hasChildren && (
          <span className="text-fg-3" title="hidden descendants">
            +{countDescendants(node)}
          </span>
        )}
        <MetricBadges node={node} />
        {node.command && (
          <span className="truncate text-fg-3" title={node.command}>
            ↳ {node.command}
            {node.turn_index != null ? ` · turn ${node.turn_index}` : ""}
          </span>
        )}
        <MessageLink node={node} onFocusMessage={onFocusMessage} />
        {node.container_id && (
          <Pill variant="info" title="container id (cgroup-derived)">
            {node.container_id}
          </Pill>
        )}
      </div>
      {hasChildren &&
        !isCollapsed &&
        node.children.map((c) => (
          <ProcessTreeNode
            key={c.process_key}
            node={c}
            depth={depth + 1}
            collapsed={collapsed}
            onToggle={onToggle}
            onFocusMessage={onFocusMessage}
          />
        ))}
    </div>
  );
}

type SortKey = "cpu" | "mem" | "disk" | "start" | "exe";

function sortValue(n: ProcessNode, key: SortKey): number | string {
  switch (key) {
    case "cpu":
      return n.cpu_ms ?? 0;
    case "mem":
      return Math.max(n.working_set_bytes ?? 0, n.peak_rss_bytes ?? 0);
    case "disk":
      return (n.read_bytes ?? 0) + (n.write_bytes ?? 0);
    case "start":
      return n.pid; // stable proxy; the API already orders by start time
    case "exe":
      return (n.exe || "").toLowerCase();
  }
}

function ProcessTable({
  nodes,
  onFocusMessage,
}: {
  nodes: ProcessNode[];
  onFocusMessage?: (messageId: string) => void;
}) {
  const [sortKey, setSortKey] = useState<SortKey>("mem");
  const [desc, setDesc] = useState(true);
  const [showAll, setShowAll] = useState(false);
  const sorted = useMemo(() => {
    const arr = [...nodes];
    arr.sort((a, b) => {
      const av = sortValue(a, sortKey);
      const bv = sortValue(b, sortKey);
      let c: number;
      if (typeof av === "number" && typeof bv === "number") c = av - bv;
      else c = String(av).localeCompare(String(bv));
      return desc ? -c : c;
    });
    return arr;
  }, [nodes, sortKey, desc]);
  // Cap the rendered rows so a huge session doesn't build a 1,000-row table; the
  // sort puts the most relevant rows first, and "show all" lifts the cap (C1).
  const capped = sorted.length > TABLE_ROW_CAP && !showAll;
  const visible = capped ? sorted.slice(0, TABLE_ROW_CAP) : sorted;

  const Header = ({ k, label, right }: { k: SortKey; label: string; right?: boolean }) => (
    <th
      className={`cursor-pointer select-none py-1 font-medium hover:text-fg-1 ${right ? "text-right" : "text-left"}`}
      onClick={() => {
        if (sortKey === k) setDesc((v) => !v);
        else {
          setSortKey(k);
          setDesc(true);
        }
      }}
    >
      {label}
      {sortKey === k ? (desc ? " ↓" : " ↑") : ""}
    </th>
  );

  return (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[760px] text-left text-[11.5px]">
        <thead className="text-[10px] uppercase tracking-[0.06em] text-fg-3">
          <tr className="border-b border-line-2">
            <Header k="exe" label="Process" />
            <th className="py-1 font-medium">pid</th>
            <th className="py-1 font-medium">started</th>
            <th className="py-1 font-medium">state</th>
            <Header k="cpu" label="CPU" right />
            <Header k="mem" label="Memory" right />
            <Header k="disk" label="Disk R/W" right />
            <th className="py-1 pl-2 font-medium">message</th>
          </tr>
        </thead>
        <tbody>
          {visible.map((n) => (
            <tr key={n.process_key} className="border-b border-line-1/60 last:border-b-0">
              <td className="py-1 pr-2 font-mono text-fg-1">{n.exe || "?"}</td>
              <td
                className="py-1 pr-2 text-fg-3"
                title={isDerived(n) ? "from the tool's exec record — no OS process captured" : undefined}
              >
                {isDerived(n) ? <span className="text-fg-4">tool log</span> : n.pid}
              </td>
              <td className="py-1 pr-2 whitespace-nowrap tabular-nums text-fg-3" title={n.started_at || undefined}>
                {fmtClock(n.started_at)}
              </td>
              {isDerived(n) ? (
                <td className={`py-1 pr-2 ${n.exit_code === 0 ? "text-fg-3" : "text-danger"}`}>
                  {derivedRuntimeLabel(n)}
                </td>
              ) : (
                <td className={`py-1 pr-2 ${n.exited ? "text-fg-3" : "text-success"}`}>
                  {runtimeLabel(n)}
                </td>
              )}
              <td className="py-1 pr-2 text-right font-mono tabular-nums text-fg-2">
                {n.cpu_ms ? fmtDuration(n.cpu_ms) : "—"}
              </td>
              <td className="py-1 pr-2 text-right font-mono tabular-nums text-fg-2">
                {n.working_set_bytes ? fmtBytes(n.working_set_bytes) : "—"}
              </td>
              <td className="py-1 pr-2 text-right font-mono tabular-nums text-fg-3">
                {(n.read_bytes ?? 0) + (n.write_bytes ?? 0) > 0
                  ? `↓${fmtBytes(n.read_bytes ?? 0)} ↑${fmtBytes(n.write_bytes ?? 0)}`
                  : "—"}
              </td>
              <td className="py-1 pl-2">
                <MessageLink node={n} onFocusMessage={onFocusMessage} />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {sorted.length > TABLE_ROW_CAP && (
        <button
          type="button"
          onClick={() => setShowAll((v) => !v)}
          className="mt-1 text-[10.5px] text-fg-3 hover:text-fg-1"
        >
          {showAll ? `Show top ${TABLE_ROW_CAP}` : `Show all ${fmtInt(sorted.length)} (showing ${TABLE_ROW_CAP})`}
        </button>
      )}
    </div>
  );
}

function FindingRow({ f }: { f: ProcessFinding }) {
  return (
    <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-[11.5px]">
      <Pill variant={SEVERITY_VARIANT[f.severity] ?? "neutral"}>{f.severity}</Pill>
      <span className="font-mono text-fg-2">{f.rule_id.replace(/^process\./, "")}</span>
      {f.exe_basename && <span className="text-fg-1">{f.exe_basename}</span>}
      {f.detail && <span className="text-fg-3">{f.detail}</span>}
    </div>
  );
}

// PROC_REFRESH_MS: how often the open Processes panel re-polls while something
// is still running. Slower than the rest of the drawer (8s) — a process tree
// changes far less than the live conversation, and each poll can trigger a
// server-side correlation refresh. A fully-exited tree stops polling entirely.
const PROC_REFRESH_MS = 15000;
// LARGE_TREE_THRESHOLD: above this many captured processes the tree opens
// collapsed-to-roots, so a huge session (e.g. a long-running daemon subtree)
// doesn't render thousands of nodes + sparklines at once.
const LARGE_TREE_THRESHOLD = 150;
// TABLE_ROW_CAP: the Table view renders at most this many rows (the top ones by
// the active sort) with a "show all" escape hatch.
const TABLE_ROW_CAP = 100;

export function ProcessesSection({
  sessionId,
  onFocusMessage,
}: {
  sessionId: string | null;
  onFocusMessage?: (messageId: string) => void;
}) {
  const [open, setOpen] = useState<boolean>(() => {
    try {
      return localStorage.getItem(SECTION_OPEN_KEY) === "1";
    } catch {
      return false;
    }
  });
  const toggleOpen = useCallback(() => {
    setOpen((v) => {
      const next = !v;
      try {
        localStorage.setItem(SECTION_OPEN_KEY, next ? "1" : "0");
      } catch {
        /* ignore */
      }
      return next;
    });
  }, []);

  const [view, setView] = useState<"tree" | "table">("tree");
  // poll gates the auto-refresh: only re-poll while something is still running
  // (a fully-exited tree is static). Set by an effect once data has loaded, so
  // the first fetch is one-shot and polling starts only if needed.
  const [poll, setPoll] = useState(false);
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set());
  const toggle = useCallback((key: string) => {
    setCollapsed((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  }, []);

  // Lazy-load: only fetch (and trigger the daemon's correlation passes) once the
  // section is open. A closed section makes no request.
  const procs = useApi<SessionProcessResponse>(
    open && sessionId ? `/api/session/${sessionId}/processes` : null,
    undefined,
    [sessionId, open],
    open && poll ? { refreshMs: PROC_REFRESH_MS } : undefined,
  );
  const data = procs.data;
  const findings = data?.findings ?? [];

  const flat = useMemo(() => (data ? flatten(data.roots) : []), [data]);
  const runningCount = useMemo(() => flat.filter((n) => !n.exited).length, [flat]);
  const withMetrics = useMemo(() => flat.filter(hasMetrics).length, [flat]);

  // Auto-refresh only while open AND something is still running — a fully-exited
  // tree never changes, so one fetch is enough (A2). Derived after the fetch so
  // the first load is one-shot; polling then starts only if needed.
  useEffect(() => {
    setPoll(open && runningCount > 0);
  }, [open, runningCount]);

  const parentKeys = useMemo(() => {
    const keys: string[] = [];
    const walk = (nodes: ProcessNode[]) => {
      for (const n of nodes) {
        if (n.children.length > 0) {
          keys.push(n.process_key);
          walk(n.children);
        }
      }
    };
    if (data) walk(data.roots);
    return keys;
  }, [data]);
  const allCollapsed = parentKeys.length > 0 && parentKeys.every((k) => collapsed.has(k));

  // Large trees open collapsed-to-roots so a huge session doesn't render
  // thousands of nodes + sparklines at once (C1). Runs once per session, on the
  // first data load — leaves the operator's manual expand/collapse alone after.
  const collapseInitRef = useRef<string | null>(null);
  useEffect(() => {
    if (!data || !sessionId || collapseInitRef.current === sessionId) return;
    collapseInitRef.current = sessionId;
    if (data.total > LARGE_TREE_THRESHOLD) setCollapsed(new Set(parentKeys));
  }, [data, sessionId, parentKeys]);

  const summary = data
    ? `${fmtInt(data.total)} captured · ${fmtInt(runningCount)} running${
        withMetrics > 0 ? ` · ${fmtInt(withMetrics)} with metrics` : ""
      }${findings.length > 0 ? ` · ⚠ ${fmtInt(findings.length)}` : ""}`
    : open
      ? "Loading…"
      : "click to load OS-level process tree";

  return (
    <section className="space-y-2">
      <h3>
        <button
          type="button"
          onClick={toggleOpen}
          className="flex w-full items-center justify-between gap-2 text-left focus:outline-none"
          aria-expanded={open}
        >
          <span className="flex items-center gap-1.5 text-[11px] font-semibold uppercase tracking-[0.06em] text-fg-3">
            <span className="select-none text-fg-3">{open ? "▾" : "▸"}</span>
            Processes
          </span>
          <span className="text-[10.5px] text-fg-3">{summary}</span>
        </button>
      </h3>

      {open && (
        <ChartState
          loading={procs.loading && !data}
          error={procs.error}
          empty={!data || data.total === 0}
          emptyHint="No OS processes captured for this session. Enable [observer.process] and run observer where the AI tool spawns (process observability is opt-in)."
          height={120}
        >
          {data && (
            <div className="space-y-3">
              {findings.length > 0 && (
                <div className="space-y-1.5 rounded-3 border border-line-2 bg-bg-2 p-3">
                  <div className="text-[10px] font-semibold uppercase tracking-[0.08em] text-fg-3">
                    Findings · observe-only
                  </div>
                  {findings.map((f) => (
                    <FindingRow key={f.process_key + f.rule_id} f={f} />
                  ))}
                </div>
              )}

              <div className="flex items-center justify-between gap-2">
                <div className="flex overflow-hidden rounded-2 border border-line-2 text-[10.5px]">
                  {(["tree", "table"] as const).map((v) => (
                    <button
                      key={v}
                      type="button"
                      onClick={() => setView(v)}
                      className={
                        view === v
                          ? "bg-accent-soft px-2 py-0.5 text-accent"
                          : "px-2 py-0.5 text-fg-3 hover:text-fg-1"
                      }
                    >
                      {v === "tree" ? "Tree" : "Table"}
                    </button>
                  ))}
                </div>
                {view === "tree" && parentKeys.length > 0 && (
                  <button
                    type="button"
                    onClick={() => setCollapsed(allCollapsed ? new Set() : new Set(parentKeys))}
                    className="text-[10.5px] text-fg-3 hover:text-fg-1"
                  >
                    {allCollapsed ? "Expand all" : "Collapse all"}
                  </button>
                )}
              </div>

              <div className="rounded-3 border border-line-2 bg-bg-2 p-3">
                {view === "tree" ? (
                  data.roots.map((r) => (
                    <ProcessTreeNode
                      key={r.process_key}
                      node={r}
                      depth={0}
                      collapsed={collapsed}
                      onToggle={toggle}
                      onFocusMessage={onFocusMessage}
                    />
                  ))
                ) : (
                  <ProcessTable nodes={flat} onFocusMessage={onFocusMessage} />
                )}
              </div>
            </div>
          )}
        </ChartState>
      )}
    </section>
  );
}
