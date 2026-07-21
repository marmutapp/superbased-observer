import { useEffect, useMemo, useRef, useState } from "react";
import { useLocation } from "react-router-dom";
import { ChartShell, Pill } from "@/components/primitives";
import { useApi } from "@/lib/useApi";
import { fetchJSON } from "@/lib/api";
import type { ProjectRow, ProjectsResponse } from "@/lib/types";
import { markRestartPending } from "@/lib/restartPending";
import { useLaunchDock, type DockSession } from "@/components/LaunchDock";
import {
  useTerminalStatuses,
  AgentStatusBadge,
} from "@/components/useTerminalStatuses";
import { WorkspaceGrid } from "@/components/workspace/WorkspaceGrid";

// LaunchInfoWire mirrors dashboard.LaunchInfo (GET /api/launch/sessions). The
// controller surface reads writer_holder ("local" | a remote device fingerprint
// | "") and the viewer count to show who currently drives each live terminal.
type LaunchInfoWire = {
  token: string;
  subcommand?: string;
  session_id?: string;
  viewers?: number;
  writer_holder?: string;
  exited?: boolean;
};

type RemoteSessionRow = { fingerprint: string };

// ApproveReveal holds a one-time approve-execute result. Like the pairing QR it
// is kept ONLY in memory, masked after a timeout, and never re-fetched.
type ApproveReveal = {
  handle: string;
  device: string;
  capability: string;
  confirm: string;
};

const APPROVE_REVEAL_MS = 60_000;

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
  restart_required_on_save: boolean;
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
  const launchSessions = useApi<{ sessions: LaunchInfoWire[] }>(
    "/api/launch/sessions",
    undefined,
    [],
    { refreshMs: 8_000 },
  );
  const remoteSessions = useApi<{ sessions: RemoteSessionRow[]; controller_live: boolean }>(
    "/api/remote/sessions",
    undefined,
    [],
    { refreshMs: 15_000 },
  );
  const dock = useLaunchDock();

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

  useEffect(() => {
    if (p) {
      setAllowFresh(p.allow_fresh_agent);
      setTools(p.allowed_tools ?? []);
      setRoots(p.allowed_project_roots ?? []);
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
                  None — no project_root may be set. A fresh launch with no project root runs in the Observer
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

      {/* Remote control of live terminals (§4, deliverable 1). Owner-local
          management surface: approve a remote device to drive a terminal
          (mints a one-time capability + confirm to convey), emergency-revoke a
          remote controller, or take control back locally. */}
      <RemoteTerminalControl
        sessions={(launchSessions.data?.sessions ?? []).filter((s) => s.token && !s.exited)}
        devices={remoteSessions.data?.sessions ?? []}
        controllerLive={remoteSessions.data?.controller_live ?? false}
        confirmToken={confirmToken}
        configWritable={p?.config_writable ?? false}
        onReload={() => {
          launchSessions.reload();
          remoteSessions.reload();
        }}
        onTakeOver={(handle, tool, sessionId) => {
          const arg = { tool, sessionId } as unknown as DockSession;
          arg.token = handle;
          dock.launch(arg);
        }}
      />

      {/* Standing terminal-control access (§B, deliverable 2) — opt-in, off by
          default, owner-local. Rendered below the single-use controller so the
          safer per-terminal flow reads as the primary path. */}
      <StandingTerminalAccess confirmToken={confirmToken} />

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

// RemoteTerminalControl is the owner-local controller surface (§4, deliverable
// 1). For each live terminal it shows the current controller (writer_holder) +
// viewer count and offers: Approve remote control (POST /api/remote/approve-
// execute → a one-time capability + confirm masked/copied to convey), emergency
// Revoke control (DELETE the remote controller's device session), and Take over
// (open the terminal locally, which the backend treats as a local takeover that
// demotes the remote writer). All routes are owner-loopback-only, so this panel
// works only from the local dashboard.
function RemoteTerminalControl({
  sessions,
  devices,
  controllerLive,
  confirmToken,
  configWritable,
  onReload,
  onTakeOver,
}: {
  sessions: LaunchInfoWire[];
  devices: RemoteSessionRow[];
  controllerLive: boolean;
  configWritable: boolean;
  confirmToken: string;
  onReload: () => void;
  onTakeOver: (handle: string, tool: string, sessionId: string) => void;
}) {
  const [pickDevice, setPickDevice] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [reveal, setReveal] = useState<ApproveReveal | null>(null);
  const [masked, setMasked] = useState(false);
  const maskTimer = useRef<number | null>(null);

  // Mask the one-time capability + confirm after a timeout, like the pairing
  // reveal. The value survives in memory so "Reveal" re-shows it with no
  // server round-trip.
  useEffect(() => {
    if (reveal) {
      setMasked(false);
      if (maskTimer.current) window.clearTimeout(maskTimer.current);
      maskTimer.current = window.setTimeout(() => setMasked(true), APPROVE_REVEAL_MS);
    }
    return () => {
      if (maskTimer.current) window.clearTimeout(maskTimer.current);
    };
  }, [reveal]);

  async function approve(handle: string) {
    const device = pickDevice[handle] || devices[0]?.fingerprint || "";
    if (!device) {
      setErr("No paired device to approve — pair one on the Remote page first.");
      return;
    }
    if (!confirmToken) {
      setErr("No confirm token — reload the page.");
      return;
    }
    setBusy("approve:" + handle);
    setErr(null);
    try {
      const res = await postConfirmJSON<{
        capability: string;
        confirm: string;
        device: string;
        handle: string;
      }>("/api/remote/approve-execute", confirmToken, { device, handle });
      setReveal({
        handle,
        device: res.device || device,
        capability: res.capability,
        confirm: res.confirm,
      });
      onReload();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(null);
    }
  }

  async function revoke(fingerprint: string) {
    if (!fingerprint || fingerprint === "local") return;
    if (
      !window.confirm(
        "Revoke this device's control now? It ends the remote writer immediately and unpairs the device (it must scan a new QR to reconnect).",
      )
    ) {
      return;
    }
    setBusy("revoke:" + fingerprint);
    setErr(null);
    try {
      await fetchJSON(`/api/remote/sessions/${fingerprint}`, undefined, { method: "DELETE" });
      onReload();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(null);
    }
  }

  function holderLabel(h?: string) {
    if (!h) return { text: "no controller", variant: "neutral" as const };
    if (h === "local") return { text: "you (local)", variant: "success" as const };
    return { text: `remote ${h}`, variant: "warn" as const };
  }

  return (
    <ChartShell
      title="Remote control of terminals"
      sub="Hand control of a live terminal to a paired remote device, revoke it, or take control back. Approving mints a one-time capability + confirm code you convey to that device — it is shown once and never stored. Owner-local only."
      right={
        <Pill variant={controllerLive ? "success" : "neutral"}>
          {controllerLive ? "remote live" : "remote off"}
        </Pill>
      }
    >
      <div className="space-y-3 p-1 text-[12px]">
        {err && (
          <div className="rounded-2 border border-danger/40 bg-danger/10 px-3 py-2 text-danger">
            {err}
          </div>
        )}

        {!configWritable && (
          <div className="rounded-2 border border-line-2 bg-bg-2 px-3 py-2 text-fg-3">
            This dashboard has no writable config path, so remote-control approval isn't available here.
          </div>
        )}
        {!controllerLive && (
          <div className="rounded-2 border border-warn/40 bg-warn/10 px-3 py-2 text-warn">
            Remote access isn't live. Arm it and pair a device on the <span className="font-medium">Remote</span> page
            before you can approve remote control.
          </div>
        )}

        {/* One-time capability + confirm reveal — masked after ~60s. */}
        {reveal && (
          <div className="space-y-2 rounded-2 border border-accent/40 bg-accent/10 p-3">
            <div className="text-[11px] text-fg-2">
              Convey BOTH values to device <span className="font-mono">{reveal.device}</span> — they pair the
              device to terminal <span className="font-mono">{reveal.handle.slice(0, 8)}…</span> once. Shown
              only now.
            </div>
            <CopyField label="capability" value={reveal.capability} masked={masked} />
            <CopyField label="confirm code" value={reveal.confirm} masked={masked} />
            <div className="flex gap-2">
              {masked ? (
                <button
                  type="button"
                  onClick={() => setMasked(false)}
                  className="rounded-2 border border-line-2 bg-bg-2 px-2 py-1 text-[11px] text-fg-2 hover:bg-bg-3"
                >
                  Reveal
                </button>
              ) : null}
              <button
                type="button"
                onClick={() => setReveal(null)}
                className="rounded-2 border border-line-2 bg-bg-2 px-2 py-1 text-[11px] text-fg-3 hover:bg-bg-3"
              >
                Done
              </button>
            </div>
          </div>
        )}

        {sessions.length === 0 ? (
          <div className="text-fg-3">No live terminals. Launch or continue a session to manage remote control.</div>
        ) : (
          <table className="w-full text-left">
            <thead className="text-[11px] text-fg-3">
              <tr>
                <th className="py-1">terminal</th>
                <th className="py-1">controller</th>
                <th className="py-1">viewers</th>
                <th className="py-1">approve device</th>
                <th className="py-1"></th>
              </tr>
            </thead>
            <tbody className="text-fg-2">
              {sessions.map((s) => {
                const holder = holderLabel(s.writer_holder);
                const remoteHeld = !!s.writer_holder && s.writer_holder !== "local";
                return (
                  <tr key={s.token} className="border-t border-line-1 align-top">
                    <td className="py-1.5">
                      <span className="font-mono text-fg-1">{s.subcommand || "terminal"}</span>
                      <span className="ml-1 font-mono text-[10.5px] text-fg-3">
                        {s.token.slice(0, 8)}…
                      </span>
                    </td>
                    <td className="py-1.5">
                      <Pill variant={holder.variant}>{holder.text}</Pill>
                    </td>
                    <td className="py-1.5">{s.viewers ?? 0}</td>
                    <td className="py-1.5">
                      <select
                        value={pickDevice[s.token] ?? ""}
                        disabled={!controllerLive || devices.length === 0}
                        onChange={(e) =>
                          setPickDevice((cur) => ({ ...cur, [s.token]: e.target.value }))
                        }
                        className="w-full rounded-2 border border-line-2 bg-bg-0 px-1.5 py-1 text-[11px] text-fg-1 disabled:opacity-40"
                      >
                        {devices.length === 0 && <option value="">no paired devices</option>}
                        {devices.length > 0 && <option value="">choose device…</option>}
                        {devices.map((d) => (
                          <option key={d.fingerprint} value={d.fingerprint}>
                            {d.fingerprint}
                          </option>
                        ))}
                      </select>
                    </td>
                    <td className="py-1.5">
                      <div className="flex flex-wrap justify-end gap-1.5">
                        <button
                          type="button"
                          disabled={
                            !controllerLive ||
                            !configWritable ||
                            devices.length === 0 ||
                            busy !== null
                          }
                          onClick={() => approve(s.token)}
                          className="rounded-2 border border-accent/50 bg-accent/15 px-2 py-0.5 text-[11px] font-medium text-accent hover:bg-accent/25 disabled:opacity-40"
                        >
                          {busy === "approve:" + s.token ? "approving…" : "Approve control"}
                        </button>
                        <button
                          type="button"
                          disabled={!remoteHeld || busy !== null}
                          title={
                            remoteHeld
                              ? "Revoke this device's control of this terminal now"
                              : "No remote device is currently controlling this terminal — writer control is single-use and ends when the device's socket closes (e.g. a phone refresh). To revoke a device entirely, use “Paired devices” below."
                          }
                          onClick={() => revoke(s.writer_holder as string)}
                          className="rounded-2 border border-danger/40 bg-danger/10 px-2 py-0.5 text-[11px] text-danger hover:bg-danger/20 disabled:opacity-40"
                        >
                          {busy === "revoke:" + s.writer_holder ? "revoking…" : "Revoke"}
                        </button>
                        <button
                          type="button"
                          onClick={() =>
                            onTakeOver(s.token, s.subcommand || "terminal", s.session_id || "")
                          }
                          title="Open this terminal locally — you take control back (demotes any remote writer)"
                          className="rounded-2 border border-line-2 bg-bg-2 px-2 py-0.5 text-[11px] text-fg-2 hover:bg-bg-3"
                        >
                          Take over
                        </button>
                      </div>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}

        {/* Paired devices (§4, deliverable A). A device is revocable whenever
            it is paired — NOT only while it happens to hold a live writer lease
            (writer leases are single-use and die on a phone refresh, so the
            per-terminal Revoke above is disabled most of the time). Revoking a
            device here ends any control it holds AND unpairs it. */}
        <div className="rounded-2 border border-line-2 bg-bg-1 p-3">
          <div className="mb-1 text-[11px] font-medium text-fg-2">Paired devices</div>
          <div className="mb-2 text-[11px] text-fg-3">
            Revoke a device to unpair it and immediately end any terminal control it holds. A revoked
            device must scan a new QR to reconnect.
          </div>
          {devices.length === 0 ? (
            <div className="text-[11px] text-fg-3">No paired devices.</div>
          ) : (
            <div className="space-y-1">
              {devices.map((d) => (
                <div key={d.fingerprint} className="flex items-center gap-2">
                  <code className="flex-1 rounded-2 border border-line-2 bg-bg-0 px-2 py-1 font-mono text-[11px] text-fg-2">
                    {d.fingerprint}
                  </code>
                  <button
                    type="button"
                    disabled={busy !== null}
                    onClick={() => revoke(d.fingerprint)}
                    title="Unpair this device and end any control it holds now"
                    className="rounded-2 border border-danger/40 bg-danger/10 px-2 py-1 text-[11px] text-danger hover:bg-danger/20 disabled:opacity-40"
                  >
                    {busy === "revoke:" + d.fingerprint ? "revoking…" : "Revoke device"}
                  </button>
                </div>
              ))}
            </div>
          )}
        </div>
      </div>
    </ChartShell>
  );
}

// StandingTerminalAccess is the OPT-IN standing terminal-control surface (§B):
// mint a single durable, hashed-at-rest secret that lets a paired device
// re-acquire writer control across websocket refreshes WITHOUT a fresh
// per-terminal owner approval each time. It is DEFAULT OFF and carries a firm
// security warning: standing access is a strict superset of risk over the
// single-use flow (anyone with the secret + a paired session controls EVERY
// terminal). Owner-local only. The raw secret is shown ONCE on mint and stored
// hashed at rest; revoke kills the secret AND every writer holding through it.
type StandingStatus = {
  enabled: boolean;
  secret_present: boolean;
  secret_fingerprint: string;
  allow_terminal: boolean;
  remote_enabled: boolean;
  config_writable: boolean;
  warning: string;
  revoke_on_takeover: boolean;
};

function StandingTerminalAccess({
  confirmToken,
}: {
  confirmToken: string;
}) {
  const status = useApi<StandingStatus>("/api/remote/standing-terminal", undefined, [], {
    refreshMs: 20_000,
  });
  const st = status.data;
  const [busy, setBusy] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [secret, setSecret] = useState<string | null>(null);
  const [masked, setMasked] = useState(false);
  const maskTimer = useRef<number | null>(null);

  // Mask the one-time secret after a timeout (like the pairing reveal). The
  // value survives in memory so "Reveal" re-shows it with no round-trip.
  useEffect(() => {
    if (secret) {
      setMasked(false);
      if (maskTimer.current) window.clearTimeout(maskTimer.current);
      maskTimer.current = window.setTimeout(() => setMasked(true), APPROVE_REVEAL_MS);
    }
    return () => {
      if (maskTimer.current) window.clearTimeout(maskTimer.current);
    };
  }, [secret]);

  const canManage = !!st?.config_writable;
  const prereqOK = !!st?.remote_enabled && !!st?.allow_terminal;
  // Honest disabled copy: name the exact missing dependency.
  const disabledReason = !canManage
    ? "This dashboard has no writable config path, so standing access can't be managed here."
    : !st?.remote_enabled
      ? "Remote access is off. Arm it on the Remote page first."
      : !st?.allow_terminal
        ? "Remote terminal control (allow_terminal) is off. Enable it on the Remote page first — standing access only grants what allow_terminal permits."
        : "";

  async function mint() {
    if (!confirmToken) {
      setErr("No confirm token — reload the page.");
      return;
    }
    setBusy("mint");
    setErr(null);
    try {
      const res = await postConfirmJSON<{ secret: string }>(
        "/api/remote/standing-terminal/mint",
        confirmToken,
        {},
      );
      setSecret(res.secret);
      status.reload();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(null);
    }
  }

  async function revoke() {
    if (!confirmToken) {
      setErr("No confirm token — reload the page.");
      return;
    }
    if (
      !window.confirm(
        "Revoke standing terminal-control access? This deletes the secret and immediately drops every remote writer that acquired control through it.",
      )
    ) {
      return;
    }
    setBusy("revoke");
    setErr(null);
    try {
      await postConfirmJSON("/api/remote/standing-terminal/revoke", confirmToken, {});
      setSecret(null);
      status.reload();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(null);
    }
  }

  // saveRevokeOnTakeover flips [remote].revoke_standing_on_takeover — the
  // opt-in hardening that makes a desktop takeover of a standing-secret writer
  // also revoke the standing secret. Default off (seamless). The takeover hook
  // reads the persisted value, so the flip is live immediately, no restart.
  async function saveRevokeOnTakeover(next: boolean) {
    if (!confirmToken) {
      setErr("No confirm token — reload the page.");
      return;
    }
    setBusy("revoke-on-takeover");
    setErr(null);
    try {
      await postConfirmJSON("/api/remote/standing-revoke-on-takeover", confirmToken, {
        revoke_standing_on_takeover: next,
      });
      status.reload();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(null);
    }
  }

  return (
    <ChartShell
      title="Standing terminal-control access (advanced)"
      sub="Opt-in: a single durable secret that lets a paired device keep terminal control across page refreshes, without re-approving each terminal. Off by default — per-terminal single-use approvals above are the safer path. Owner-local only."
      right={
        <Pill variant={st?.enabled ? "warn" : "neutral"}>
          {st?.enabled ? "standing access ON" : "standing access off"}
        </Pill>
      }
    >
      <div className="space-y-3 p-1 text-[12px]">
        {err && (
          <div className="rounded-2 border border-danger/40 bg-danger/10 px-3 py-2 text-danger">{err}</div>
        )}

        {/* The firm security warning — shown verbatim from the server. */}
        <div className="rounded-2 border border-warn/40 bg-warn/10 px-3 py-2 text-[11px] text-warn">
          <span className="font-medium">Security warning. </span>
          {st?.warning ??
            "Standing access means anyone who has this secret AND a paired remote session can control EVERY live terminal, across refreshes, until you revoke it. Per-terminal single-use approvals are safer."}
        </div>

        {disabledReason && (
          <div className="rounded-2 border border-line-2 bg-bg-2 px-3 py-2 text-fg-3">{disabledReason}</div>
        )}

        {/* One-time secret reveal — masked after ~60s. */}
        {secret && (
          <div className="space-y-2 rounded-2 border border-accent/40 bg-accent/10 p-3">
            <div className="text-[11px] text-fg-2">
              Standing secret — shown ONCE. Convey it to the device you want to grant standing control. A
              device may store it in its browser localStorage so control survives a refresh; that means the
              secret lives on that device — treat it like a password and revoke it if the device is lost.
            </div>
            <CopyField label="standing secret" value={secret} masked={masked} />
            <div className="flex gap-2">
              {masked && (
                <button
                  type="button"
                  onClick={() => setMasked(false)}
                  className="rounded-2 border border-line-2 bg-bg-2 px-2 py-1 text-[11px] text-fg-2 hover:bg-bg-3"
                >
                  Reveal
                </button>
              )}
              <button
                type="button"
                onClick={() => setSecret(null)}
                className="rounded-2 border border-line-2 bg-bg-2 px-2 py-1 text-[11px] text-fg-3 hover:bg-bg-3"
              >
                Done
              </button>
            </div>
          </div>
        )}

        {st?.enabled && st?.secret_present && (
          <div className="text-[11px] text-fg-3">
            A standing secret is provisioned (<span className="font-mono">{st.secret_fingerprint}</span>). The
            raw secret is never re-shown — rotate to issue a fresh one, or revoke to turn standing access off.
          </div>
        )}

        {st?.remote_enabled && (
          <label className="flex items-start gap-2 rounded-2 border border-line-2 bg-bg-2 px-3 py-2">
            <input
              type="checkbox"
              className="mt-[2px]"
              checked={!!st?.revoke_on_takeover}
              disabled={!canManage || busy !== null}
              onChange={(e) => void saveRevokeOnTakeover(e.target.checked)}
            />
            <span>
              <span className="font-medium text-fg-2">Revoke standing access when this desktop takes over</span>
              <span className="block text-[11px] text-fg-3">
                Off (default): taking over a remote writer only revokes its live control — the paired device
                can re-take control later (seamless). On: a desktop takeover of a writer that held control
                through the standing secret ALSO revokes the secret itself — the device must be granted a
                fresh secret to regain standing control. Live immediately; no restart.
              </span>
            </span>
          </label>
        )}

        <div className="flex flex-wrap items-center gap-2 pt-1">
          <button
            type="button"
            disabled={!prereqOK || !canManage || busy !== null}
            onClick={mint}
            className="rounded-2 border border-accent/50 bg-accent/15 px-3 py-1 text-[12px] text-accent hover:bg-accent/25 disabled:opacity-40"
          >
            {busy === "mint"
              ? st?.enabled
                ? "rotating…"
                : "enabling…"
              : st?.enabled
                ? "Rotate standing secret"
                : "Enable standing access"}
          </button>
          {st?.enabled && (
            <button
              type="button"
              disabled={!canManage || busy !== null}
              onClick={revoke}
              className="rounded-2 border border-danger/40 bg-danger/10 px-3 py-1 text-[12px] text-danger hover:bg-danger/20 disabled:opacity-40"
            >
              {busy === "revoke" ? "revoking…" : "Revoke standing access"}
            </button>
          )}
        </div>
      </div>
    </ChartShell>
  );
}

// CopyField renders a masked-or-shown secret with a copy button — the same
// convey-once discipline as the pairing reveal.
function CopyField({
  label,
  value,
  masked,
}: {
  label: string;
  value: string;
  masked: boolean;
}) {
  return (
    <div>
      <div className="mb-0.5 text-[10px] uppercase tracking-wide text-fg-3">{label}</div>
      <div className="flex items-center gap-2">
        <code className="flex-1 break-all rounded-2 border border-line-2 bg-bg-1 px-2 py-1 font-mono text-[11px] text-fg-2">
          {masked ? "•••••••••••••• (hidden — click Reveal)" : value}
        </code>
        <button
          type="button"
          onClick={() => navigator.clipboard?.writeText(value)}
          className="rounded-2 border border-line-2 bg-bg-2 px-2 py-1 text-[11px] text-fg-2 hover:bg-bg-3"
        >
          Copy
        </button>
      </div>
    </div>
  );
}
