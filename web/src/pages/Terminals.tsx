import { useEffect, useMemo, useState } from "react";
import { useLocation } from "react-router-dom";
import { ChartShell, Pill } from "@/components/primitives";
import { useApi } from "@/lib/useApi";
import { fetchJSON } from "@/lib/api";
import type { ProjectRow, ProjectsResponse } from "@/lib/types";
import { markRestartPending } from "@/lib/restartPending";
import {
  useTerminalStatuses,
  AgentStatusBadge,
} from "@/components/useTerminalStatuses";
import { WorkspaceGrid } from "@/components/workspace/WorkspaceGrid";

async function postConfirmJSON<T>(
  path: string,
  confirmToken: string,
  body: unknown,
): Promise<T> {
  return fetchJSON<T>(path, undefined, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "X-Observer-Confirm": confirmToken,
    },
    body: JSON.stringify(body ?? {}),
  });
}

// Terminals page (dashboard-management-surface plan §E). The settings sibling
// the cockpit F5 grid will later link into (this is the "Terminals" nav entry —
// deliberately not painted out). It surfaces three shipped things:
//   1. the [terminal.launch] launch-policy editor (allow_fresh_agent /
//      allowed_tools / allowed_project_roots) — a CapabilityLocal write, so it
//      only works from the local dashboard;
//   2. the live F4 agent-status stream (GET /ws/terminal/status);
//   3. the terminal_run history (GET /api/terminal/runs, metadata only).
// The launch policy EXPANDS execution authority, so the panel states that
// plainly and everything defaults off.

type TerminalPolicy = {
  confirm_token: string;
  config_writable: boolean;
  terminal_enabled: boolean;
  allow_fresh_agent: boolean;
  allowed_tools: string[];
  allowed_project_roots: string[];
  launchable_tools: string[];
  // allow_shell is a SEPARATE opt-in from allow_fresh_agent — a plain shell is
  // a strictly larger execution-authority expansion than a launchable AI tool
  // (arbitrary commands vs. one of the capability-registry's known
  // launchers), so it needs its own conscious toggle. See
  // internal/config's TerminalLaunchConfig.AllowShell.
  allow_shell: boolean;
  restart_required_on_save: boolean;
  // Runtime bounds — live-applied by POST /api/terminal/limits (no restart),
  // read-only on this GET.
  max_concurrent: number;
  idle_timeout: string;
};

type RunRow = {
  run_id: string;
  tool: string;
  kind: string;
  launched_at: string;
  ended_at?: string;
  exit_code?: number;
  running: boolean;
  best_session_id?: string;
  best_confidence?: number;
  command_count: number;
};

// shortenPath renders an absolute root as ".../parent/leaf" for compact display
// (the full path rides along as a title tooltip). Mirrors the NewTerminalDialog
// project-picker convention so the two surfaces read the same.
function shortenPath(p: string): string {
  if (!p) return "—";
  const parts = p.split("/").filter(Boolean);
  if (parts.length <= 2) return p;
  return ".../" + parts.slice(-2).join("/");
}

// rootSeenHint renders a compact "N sessions · last seen …" suffix for a
// suggested project root. Purely cosmetic — the value is a hint, never a gate.
function rootSeenHint(p: ProjectRow): string {
  const sess = `${p.session_count} session${p.session_count === 1 ? "" : "s"}`;
  if (!p.last_seen) return sess;
  const d = new Date(p.last_seen);
  if (Number.isNaN(d.getTime())) return sess;
  return `${sess} · last seen ${d.toLocaleDateString()}`;
}

async function putPolicy(
  confirmToken: string,
  body: unknown,
): Promise<{ saved: boolean; restart_required: boolean }> {
  return fetchJSON("/api/terminal/policy", undefined, {
    method: "PUT",
    headers: {
      "Content-Type": "application/json",
      "X-Observer-Confirm": confirmToken,
    },
    body: JSON.stringify(body),
  });
}

export function TerminalsPage() {
  const policy = useApi<TerminalPolicy>("/api/terminal/policy");
  const runs = useApi<{ runs: RunRow[] }>("/api/terminal/runs", undefined, [], {
    refreshMs: 15_000,
  });
  const statuses = useTerminalStatuses();

  const p = policy.data;
  const confirmToken = p?.confirm_token ?? "";

  // Workspace vs Settings tab (dock-grid design D1): the grid IS the page;
  // the policy/remote/standing/status/history content moves behind Settings.
  const [tab, setTab] = useState<"workspace" | "settings">("workspace");
  // Deep-link: /terminals?tab=workspace|settings (the provider's requestDock
  // navigates here so a queued "Add to grid" always lands on the grid, even
  // if this page was left on the Settings tab).
  const location = useLocation();
  useEffect(() => {
    const want = new URLSearchParams(location.search).get("tab");
    if (want === "workspace" || want === "settings") setTab(want);
  }, [location.search]);

  // Local editable copy of the policy, seeded from the server.
  const [allowFresh, setAllowFresh] = useState(false);
  const [allowShell, setAllowShell] = useState(false);
  const [tools, setTools] = useState<string[]>([]);
  const [roots, setRoots] = useState<string[]>([]);
  const [newRoot, setNewRoot] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  // Known project roots (GET /api/projects) power the add-able suggestion list
  // below the free-text input. These are OBSERVED, tool-reported roots — pure
  // SUGGESTIONS; the operator explicitly allow-lists one by adding + saving,
  // and the server re-validates every entry on save (a stale/foreign/symlink-
  // mismatched root is rejected there, not here).
  const [projects, setProjects] = useState<ProjectRow[]>([]);

  // Runtime-bounds editor state (POST /api/terminal/limits — live-applied).
  // max_concurrent is kept as a string so the input can be transiently empty;
  // idle_timeout is a free-text Go duration ("0" = never).
  const [maxConcurrent, setMaxConcurrent] = useState("");
  const [idleTimeout, setIdleTimeout] = useState("");
  const [limitsBusy, setLimitsBusy] = useState(false);
  const [limitsErr, setLimitsErr] = useState<string | null>(null);
  const [limitsMsg, setLimitsMsg] = useState<string | null>(null);

  useEffect(() => {
    if (p) {
      setAllowFresh(p.allow_fresh_agent);
      setAllowShell(p.allow_shell);
      setTools(p.allowed_tools ?? []);
      setRoots(p.allowed_project_roots ?? []);
      setMaxConcurrent(String(p.max_concurrent ?? 0));
      setIdleTimeout(p.idle_timeout ?? "");
    }
  }, [p]);

  useEffect(() => {
    let cancelled = false;
    fetchJSON<ProjectsResponse>("/api/projects")
      .then((d) => {
        if (!cancelled) setProjects(d.rows ?? []);
      })
      .catch(() => {
        /* no project index yet — the free-text input is the fallback */
      });
    return () => {
      cancelled = true;
    };
  }, []);

  // Suggestions = observed project roots, POSIX-absolute ones first (a foreign/
  // Windows root will be rejected server-side, so it's de-prioritized cosmetically
  // but still offered). Already-added roots stay in the list, shown as added.
  const rootSuggestions = useMemo(() => {
    return [...projects].sort((a, b) => {
      const af = a.root_path.startsWith("/") ? 0 : 1;
      const bf = b.root_path.startsWith("/") ? 0 : 1;
      return af - bf;
    });
  }, [projects]);

  const statusList = useMemo(() => Object.values(statuses), [statuses]);
  const liveStatuses = statusList.filter((s) => s.status !== "exited");

  function toggleTool(tool: string) {
    setSaved(false);
    setTools((cur) =>
      cur.includes(tool) ? cur.filter((t) => t !== tool) : [...cur, tool],
    );
  }

  function addRootValue(raw: string) {
    const r = raw.trim();
    if (!r) return;
    setSaved(false);
    setRoots((cur) => (cur.includes(r) ? cur : [...cur, r]));
  }

  function addRoot() {
    addRootValue(newRoot);
    setNewRoot("");
  }

  async function save() {
    if (!confirmToken) {
      setErr("No confirm token — reload the page.");
      return;
    }
    setBusy(true);
    setErr(null);
    setSaved(false);
    try {
      const res = await putPolicy(confirmToken, {
        allow_fresh_agent: allowFresh,
        allowed_tools: tools,
        allowed_project_roots: roots,
        allow_shell: allowShell,
      });
      if (res.restart_required) markRestartPending("terminal-policy");
      setSaved(true);
      policy.reload();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  async function saveLimits() {
    if (!confirmToken) {
      setLimitsErr("No confirm token — reload the page.");
      return;
    }
    // Client-side sanity only: a non-negative integer. The server owns
    // duration validation and surfaces its own message.
    const trimmed = maxConcurrent.trim();
    const n = Number(trimmed);
    if (trimmed === "" || !Number.isInteger(n) || n < 0) {
      setLimitsErr("Max concurrent must be a non-negative whole number.");
      return;
    }
    setLimitsBusy(true);
    setLimitsErr(null);
    setLimitsMsg(null);
    // Send only the fields that changed from the loaded policy.
    const body: { max_concurrent?: number; idle_timeout?: string } = {};
    if (n !== (p?.max_concurrent ?? 0)) body.max_concurrent = n;
    if (idleTimeout.trim() !== (p?.idle_timeout ?? "")) {
      body.idle_timeout = idleTimeout.trim();
    }
    if (body.max_concurrent === undefined && body.idle_timeout === undefined) {
      setLimitsMsg("No changes.");
      setLimitsBusy(false);
      return;
    }
    try {
      const res = await postConfirmJSON<{ restart_required: boolean }>(
        "/api/terminal/limits",
        confirmToken,
        body,
      );
      setLimitsMsg(
        res.restart_required ? "Saved — restart required." : "Applied live.",
      );
      policy.reload();
    } catch (e) {
      setLimitsErr(e instanceof Error ? e.message : String(e));
    } finally {
      setLimitsBusy(false);
    }
  }

  return (
    <div className="space-y-4 p-5">
      <div className="flex flex-wrap items-end justify-between gap-2">
        <div>
          <h1 className="text-[15px] font-semibold text-fg-1">Terminals</h1>
          <p className="mt-0.5 text-[12px] text-fg-3">
            {tab === "workspace"
              ? "Your terminal workspace — run and arrange multiple live terminals on one grid."
              : "Launch policy, live agent status, and run history for the embedded terminal. Launch policy is an owner-local setting — it only saves from this machine."}
          </p>
        </div>
        <div className="flex gap-1 rounded-2 border border-line-2 bg-bg-1 p-0.5 text-[12px]">
          <button
            type="button"
            onClick={() => setTab("workspace")}
            className={
              tab === "workspace"
                ? "rounded-[6px] bg-bg-3 px-3 py-1 text-fg-1"
                : "rounded-[6px] px-3 py-1 text-fg-3 hover:text-fg-1"
            }
          >
            Workspace
          </button>
          <button
            type="button"
            onClick={() => setTab("settings")}
            className={
              tab === "settings"
                ? "rounded-[6px] bg-bg-3 px-3 py-1 text-fg-1"
                : "rounded-[6px] px-3 py-1 text-fg-3 hover:text-fg-1"
            }
          >
            Settings
          </button>
        </div>
      </div>

      {tab === "workspace" && (
        <WorkspaceGrid
          policyHint={
            p && (!p.terminal_enabled || !p.allow_fresh_agent)
              ? "Fresh launch is off — enable it in Settings → Fresh-agent launch policy."
              : ""
          }
          onOpenSettings={() => setTab("settings")}
        />
      )}

      {tab === "settings" && (
        <>

      {/* Launch policy editor (§E). */}
      <ChartShell
        title="Fresh-agent launch policy"
        sub="Controls whether the dashboard may start a NEW agent (not just continue an existing session) in the embedded terminal. This expands execution authority — everything defaults off."
        right={
          <Pill variant={p?.allow_fresh_agent ? "warn" : "neutral"}>
            {p?.allow_fresh_agent ? "fresh launch on" : "fresh launch off"}
          </Pill>
        }
      >
        <div className="space-y-4 p-1 text-[12px]">
          {!p?.config_writable && (
            <div className="rounded-2 border border-line-2 bg-bg-2 px-3 py-2 text-fg-3">
              This dashboard was started without a writable config path, so launch policy can't be edited
              here. Edit <code className="text-fg-2">[terminal.launch]</code> in your config.toml instead.
            </div>
          )}
          {p?.terminal_enabled === false && (
            <div className="rounded-2 border border-warn/40 bg-warn/10 px-3 py-2 text-warn">
              The terminal surface is disabled ([terminal].enabled = false). Fresh launch needs both it and
              the master toggle below.
            </div>
          )}

          <label className="flex items-start gap-2">
            <input
              type="checkbox"
              checked={allowFresh}
              disabled={!p?.config_writable}
              onChange={(e) => {
                setSaved(false);
                setAllowFresh(e.target.checked);
              }}
              className="mt-0.5"
            />
            <span>
              <span className="font-medium text-fg-1">Allow fresh-agent launches</span>
              <span className="block text-[11px] text-fg-3">
                When on, the dashboard can spawn a new agent from the launchable set below, in one of the
                allowed project roots. When off, every fresh-launch request is refused (session continuation
                is unaffected).
              </span>
            </span>
          </label>

          <label className="flex items-start gap-2">
            <input
              type="checkbox"
              checked={allowShell}
              disabled={!p?.config_writable}
              onChange={(e) => {
                setSaved(false);
                setAllowShell(e.target.checked);
              }}
              className="mt-0.5"
            />
            <span>
              <span className="font-medium text-fg-1">Allow plain shell terminals</span>
              <span className="block text-[11px] text-fg-3">
                A SEPARATE opt-in from fresh-agent launches above — it starts your own shell ($SHELL, or
                bash/sh as a fallback) instead of an AI tool, so it is a strictly larger execution-authority
                expansion (any command, not one of the launchable tools). Turning on fresh-agent launches
                does not also grant this. Config key: <code className="text-fg-2">[terminal.launch].allow_shell</code>.
              </span>
            </span>
          </label>

          <div>
            <div className="mb-1 text-[11px] text-fg-3">
              allowed tools (from the launchable set — a tool must be launchable in the capability registry)
            </div>
            <div className="flex flex-wrap gap-1.5">
              {(p?.launchable_tools ?? []).map((tool) => {
                const on = tools.includes(tool);
                return (
                  <button
                    key={tool}
                    type="button"
                    disabled={!p?.config_writable}
                    onClick={() => toggleTool(tool)}
                    className={`rounded-2 border px-2 py-0.5 text-[11px] disabled:opacity-40 ${
                      on
                        ? "border-accent/50 bg-accent/15 text-accent"
                        : "border-line-2 bg-bg-2 text-fg-3 hover:bg-bg-3"
                    }`}
                  >
                    {tool}
                  </button>
                );
              })}
            </div>
            {tools.length === 0 && (
              <div className="mt-1 text-[11px] text-fg-3">
                None selected — no tool may fresh-launch (deny-all).
              </div>
            )}
          </div>

          <div>
            <div className="mb-1 text-[11px] text-fg-3">
              allowed project roots (each is canonicalized + symlink-checked on save; must be an existing
              absolute directory)
            </div>
            <div className="space-y-1">
              {roots.map((r) => (
                <div key={r} className="flex items-center gap-2">
                  <code className="flex-1 break-all rounded-2 border border-line-2 bg-bg-1 px-2 py-1 font-mono text-[11px] text-fg-2">
                    {r}
                  </code>
                  <button
                    type="button"
                    disabled={!p?.config_writable}
                    onClick={() => {
                      setSaved(false);
                      setRoots((cur) => cur.filter((x) => x !== r));
                    }}
                    className="rounded-2 border border-line-2 bg-bg-2 px-2 py-1 text-[11px] text-fg-3 hover:bg-bg-3 disabled:opacity-40"
                  >
                    remove
                  </button>
                </div>
              ))}
              {roots.length === 0 && (
                <div className="text-[11px] text-fg-3">
                  None — no project_root may be set. A fresh launch with no project root runs in the SuperBased
                  daemon's own working directory (the directory <code className="text-fg-2">observer start</code> /{" "}
                  <code className="text-fg-2">observer dashboard</code> was launched from).
                </div>
              )}
              <div className="flex items-center gap-2 pt-1">
                <input
                  value={newRoot}
                  disabled={!p?.config_writable}
                  onChange={(e) => setNewRoot(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter") addRoot();
                  }}
                  placeholder="/absolute/path/to/project"
                  className="flex-1 rounded-2 border border-line-2 bg-bg-1 px-2 py-1 font-mono text-[11px] text-fg-1 outline-none focus:border-accent disabled:opacity-40"
                />
                <button
                  type="button"
                  disabled={!p?.config_writable || newRoot.trim() === ""}
                  onClick={addRoot}
                  className="rounded-2 border border-line-2 bg-bg-2 px-2 py-1 text-[11px] text-fg-2 hover:bg-bg-3 disabled:opacity-40"
                >
                  add
                </button>
              </div>
            </div>

            {/* Add-able suggestions from observed projects (GET /api/projects).
                These are pure SUGGESTIONS — the operator allow-lists a root by
                adding it and saving; the server re-validates every entry on save
                and rejects stale/foreign/symlink-mismatched ones with a reason
                (surfaced inline near Save below). */}
            {rootSuggestions.length > 0 && (
              <div className="mt-3 rounded-2 border border-line-1 bg-bg-1 p-2">
                <div className="mb-1.5 text-[11px] text-fg-3">
                  Suggested from observed projects — add one to allow-list it. Roots are validated on save; a
                  foreign (Windows/UNC), stale, or symlink-mismatched root is rejected there, not here.
                </div>
                <div className="max-h-[220px] space-y-1 overflow-auto">
                  {rootSuggestions.map((proj) => {
                    const added = roots.includes(proj.root_path);
                    const foreign = !proj.root_path.startsWith("/");
                    return (
                      <div key={proj.root_path} className="flex items-center gap-2">
                        <code
                          title={proj.root_path}
                          className={`min-w-0 flex-1 truncate rounded-2 border border-line-2 bg-bg-0 px-2 py-1 font-mono text-[11px] ${
                            foreign ? "text-fg-3" : "text-fg-2"
                          }`}
                        >
                          {shortenPath(proj.root_path)}
                        </code>
                        <span className="hidden whitespace-nowrap text-[10px] text-fg-3 sm:inline">
                          {rootSeenHint(proj)}
                        </span>
                        <button
                          type="button"
                          disabled={!p?.config_writable || added}
                          title={
                            foreign
                              ? `${proj.root_path} — looks like a foreign path; it will likely be rejected on save`
                              : proj.root_path
                          }
                          onClick={() => addRootValue(proj.root_path)}
                          className={`shrink-0 rounded-2 border px-2 py-1 text-[11px] disabled:opacity-40 ${
                            added
                              ? "border-line-2 bg-bg-2 text-fg-3"
                              : "border-accent/50 bg-accent/15 text-accent hover:bg-accent/25"
                          }`}
                        >
                          {added ? "added" : "add"}
                        </button>
                      </div>
                    );
                  })}
                </div>
              </div>
            )}
          </div>

          {/* Save error surfaced inline in the editor — the server rejects the
              WHOLE write on any bad entry with a "project root %q rejected: …"
              message that names which root, so the operator can fix it here. */}
          {err && (
            <div className="rounded-2 border border-danger/40 bg-danger/10 px-3 py-2 text-[12px] text-danger">
              {err}
            </div>
          )}

          <div className="flex items-center gap-3 pt-1">
            <button
              type="button"
              disabled={busy || !p?.config_writable}
              onClick={save}
              className="rounded-2 border border-accent/50 bg-accent/15 px-3 py-1 text-[12px] text-accent hover:bg-accent/25 disabled:opacity-50"
            >
              {busy ? "saving…" : "Save launch policy"}
            </button>
            {saved && <span className="text-[11px] text-ok">Saved.</span>}
            {p?.restart_required_on_save && (
              <span className="text-[11px] text-fg-3">
                Takes effect on the next daemon restart (the policy is read at start-up).
              </span>
            )}
          </div>
        </div>
      </ChartShell>

      {/* Terminal limits — the two runtime bounds live-applied to the PTY
          manager (POST /api/terminal/limits). Unlike the launch policy above,
          these bind immediately (no restart). Owner-local write. */}
      <ChartShell
        title="Terminal limits"
        sub="How many terminals can run at once, and when to reap idle ones. Applied live — no restart."
      >
        <div className="space-y-3">
          <div className="flex flex-col gap-1">
            <label className="text-[12px] font-medium text-fg-1">
              Max concurrent terminals
            </label>
            <input
              type="number"
              min={0}
              step={1}
              value={maxConcurrent}
              disabled={!p?.config_writable}
              onChange={(e) => setMaxConcurrent(e.target.value)}
              className="w-32 rounded-2 border border-line-2 bg-bg-1 px-2 py-1 text-[12px] text-fg-1 disabled:opacity-50"
            />
            <p className="text-[11px] text-fg-3">
              How many embedded/attach terminals can run at once. The Workspace
              grid is sized for 9.
            </p>
          </div>

          <div className="flex flex-col gap-1">
            <label className="text-[12px] font-medium text-fg-1">
              Idle timeout
            </label>
            <input
              type="text"
              value={idleTimeout}
              placeholder="0"
              disabled={!p?.config_writable}
              onChange={(e) => setIdleTimeout(e.target.value)}
              className="w-32 rounded-2 border border-line-2 bg-bg-1 px-2 py-1 text-[12px] text-fg-1 disabled:opacity-50"
            />
            <p className="text-[11px] text-fg-3">
              0 = never (default): live sessions stay until the agent exits. Set
              a Go duration (e.g. 30m, 2h) to reap terminals with no I/O for that
              long.
            </p>
          </div>

          {limitsErr && (
            <div className="rounded-2 border border-danger/40 bg-danger/10 px-3 py-2 text-[12px] text-danger">
              {limitsErr}
            </div>
          )}

          <div className="flex items-center gap-3 pt-1">
            <button
              type="button"
              disabled={limitsBusy || !p?.config_writable}
              onClick={saveLimits}
              className="rounded-2 border border-accent/50 bg-accent/15 px-3 py-1 text-[12px] text-accent hover:bg-accent/25 disabled:opacity-50"
            >
              {limitsBusy ? "saving…" : "Save limits"}
            </button>
            {limitsMsg && <span className="text-[11px] text-ok">{limitsMsg}</span>}
          </div>
        </div>
      </ChartShell>

      {/* Live F4 agent status. */}
      <ChartShell
        title="Live agent status"
        sub="Fused from PTY activity, OSC hints, and launcher lifecycle (F4). Low-confidence hints are shown muted — a hint is never presented as certainty."
      >
        <div className="p-1 text-[12px]">
          {liveStatuses.length === 0 ? (
            <div className="text-fg-3">No live terminal agents.</div>
          ) : (
            <table className="w-full text-left">
              <thead className="text-[11px] text-fg-3">
                <tr>
                  <th className="py-1">handle</th>
                  <th className="py-1">status</th>
                  <th className="py-1">evidence</th>
                  <th className="py-1">age</th>
                </tr>
              </thead>
              <tbody className="font-mono text-fg-2">
                {liveStatuses.map((st) => (
                  <tr key={st.handle} className="border-t border-line-1">
                    <td className="py-1">{st.handle.slice(0, 12)}…</td>
                    <td className="py-1">
                      <AgentStatusBadge info={st} />
                    </td>
                    <td className="py-1 font-sans text-fg-3">{st.evidence}</td>
                    <td className="py-1">{Math.round(st.age_seconds)}s</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </ChartShell>

      {/* Run history. */}
      <ChartShell
        title="Run history"
        sub="Every dashboard terminal launch (metadata only — project roots and correlation tokens are stored hashed, never in the clear). Correlated session appears once the launch produces one."
      >
        <div className="max-h-[320px] overflow-auto p-1 text-[11px]">
          {(runs.data?.runs.length ?? 0) === 0 ? (
            <div className="text-fg-3">No terminal runs yet.</div>
          ) : (
            <table className="w-full text-left font-mono">
              <thead className="text-fg-3">
                <tr>
                  <th className="py-1">launched</th>
                  <th className="py-1">tool</th>
                  <th className="py-1">kind</th>
                  <th className="py-1">state</th>
                  <th className="py-1">session</th>
                  <th className="py-1">cmds</th>
                </tr>
              </thead>
              <tbody className="text-fg-2">
                {runs.data?.runs.map((r) => (
                  <tr key={r.run_id} className="border-t border-line-1">
                    <td className="py-1 pr-2">{new Date(r.launched_at).toLocaleString()}</td>
                    <td className="py-1 pr-2">{r.tool}</td>
                    <td className="py-1 pr-2">{r.kind}</td>
                    <td className="py-1 pr-2">
                      {r.running ? (
                        <Pill variant="success">running</Pill>
                      ) : r.exit_code === 0 || r.exit_code === undefined ? (
                        <span className="text-fg-3">exited</span>
                      ) : (
                        <Pill variant="danger">exit {r.exit_code}</Pill>
                      )}
                    </td>
                    <td className="py-1 pr-2">
                      {r.best_session_id ? (
                        <span title={`confidence ${(r.best_confidence ?? 0).toFixed(2)}`}>
                          {r.best_session_id.slice(0, 10)}…
                        </span>
                      ) : (
                        <span className="text-fg-3">—</span>
                      )}
                    </td>
                    <td className="py-1">{r.command_count}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </ChartShell>
        </>
      )}
    </div>
  );
}
