import { useEffect, useMemo, useState } from "react";
import { fetchJSON } from "@/lib/api";
import type { ProjectRow, ProjectsResponse, ToolPreflight } from "@/lib/types";
import {
  PROJECT_ROOT_DENIED_MSG,
  REMOTE_TERMINAL_OFF_MSG,
  isProjectRootDeniedError,
  isTerminalCapabilityError,
  useRemoteTerminalGate,
} from "@/lib/remoteTerminal";
import { Tooltip, TooltipSpan } from "@/components/primitives";

// Sentinel <select> value for the "type a path by hand" escape hatch. A NUL
// byte can never be a real project root, so it can't collide with one. The NUL
// is an ESCAPE (\u0000), never a literal byte — a literal NUL makes git treat
// this whole file as binary, hiding every change from diff review.
const CUSTOM_ROOT = "\u0000custom";

// shortenPath renders an absolute root as ".../parent/leaf" for the option
// label (the full path rides along as the option title). Mirrors the
// FilterBar project-picker convention so the two surfaces read the same.
function shortenPath(p: string): string {
  if (!p) return "—";
  const parts = p.split("/").filter(Boolean);
  if (parts.length <= 2) return p;
  return ".../" + parts.slice(-2).join("/");
}

// isUnderOrEqual mirrors internal/termsvc's server-side isUnderOrEqual: child is
// permitted when it equals parent or is a descendant of it. Both operands are
// the SERVER's canonical strings (allowed_project_roots is canonicalized daemon-
// side; a known-project root is compared verbatim — this is a UX hint, the POST
// re-runs the authoritative check). Trailing slashes are trimmed so "/a/" and
// "/a" match. The embedded terminal only runs where the daemon hosts a PTY
// (never native Windows), so POSIX separators are safe.
function isUnderOrEqual(child: string, parent: string): boolean {
  const c = child.replace(/\/+$/, "");
  const p = parent.replace(/\/+$/, "");
  return c === p || c.startsWith(p + "/");
}

// isPermittedRoot reports whether a project root would pass the server's
// allowed_project_roots check, using the canonical allow-list verbatim. An empty
// allow-list permits nothing but the agent's own default cwd (deny-all).
//
// KNOWN COSMETIC MISMATCH (documented, not a gate): this check is LEXICAL while
// the server canonicalizes the requested root through EvalSymlinks at launch
// time, and known-project rows arrive as the tool-reported (unresolved) paths.
// A symlinked project path can therefore render as permitted here yet be
// rejected by the POST (surfaced via PROJECT_ROOT_DENIED_MSG), or render
// disabled here yet be acceptable via "Custom path…" with the resolved target.
// The server's canonical check remains the ONLY authority — this partition is a
// UX hint and must never be loosened into (or mistaken for) authorization.
function isPermittedRoot(path: string, allowedRoots: string[]): boolean {
  return allowedRoots.some((r) => isUnderOrEqual(path, r));
}

// NewTerminalDialog is the F1 "New terminal" affordance: start a FRESH agent
// (no --continue-from) in the embedded web terminal. The tool picker is
// populated from GET /api/terminal/sessions (launchable_tools — the capability
// registry), and the optional project root is validated + canonicalized
// server-side against the operator's [terminal.launch].allowed_project_roots.
//
// Fresh launch is a default-OFF opt-in ([terminal.launch].allow_fresh_agent):
// when the operator hasn't enabled it the POST returns 403 and we surface the
// honest reason rather than pretending it worked.

type Props = {
  onClose: () => void;
  /**
   * Called with the minted handle + tool once a fresh launch succeeds. The
   * third arg (review finding 8) reports whether the launch was given a project
   * root, so the dock enables Files/Git without a reload. Absence ≡ false.
   */
  onLaunched: (handle: string, tool: string, hasProjectRoot?: boolean) => void;
};

type FreshLaunchResponse = { token: string; tool: string; subcommand: string; has_project_root?: boolean };

export function NewTerminalDialog({ onClose, onLaunched }: Props) {
  const [tools, setTools] = useState<string[]>([]);
  const [tool, setTool] = useState("");
  // Known project roots (GET /api/projects, ordered most-recent-first) power
  // the working-directory dropdown; the user can still pick "Custom path…" to
  // type an arbitrary one. `rootSel` is the <select> value; `customRoot` holds
  // the hand-typed path only while CUSTOM_ROOT is selected.
  const [projects, setProjects] = useState<ProjectRow[]>([]);
  // The operator's canonicalized [terminal.launch].allowed_project_roots, straight
  // from GET /api/terminal/sessions. Used verbatim to mark which roots a fresh
  // launch will actually accept (empty = deny-all: only the agent's default cwd).
  const [allowedRoots, setAllowedRoots] = useState<string[]>([]);
  const [rootSel, setRootSel] = useState("");
  const [customRoot, setCustomRoot] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  // Pre-launch binary-resolution verdict for the selected tool (tool-binary-
  // resolution arc). Null when unknown, the seam is disabled (501), or the fetch
  // failed — in every case the strip simply doesn't render (fail-silent), so an
  // older daemon without the preflight endpoint behaves exactly as before.
  const [preflight, setPreflight] = useState<ToolPreflight | null>(null);
  const [installBusy, setInstallBusy] = useState(false);
  // Remote-device launch gate: a paired device can only fresh-launch when the
  // owner has enabled [remote].allow_terminal. When it's off we say so up front
  // and disable Start, rather than letting the POST fail with a raw 403.
  const { blocked: remoteBlocked } = useRemoteTerminalGate();

  useEffect(() => {
    let cancelled = false;
    fetchJSON<{
      launchable_tools?: string[];
      allowed_project_roots?: string[];
    }>("/api/terminal/sessions")
      .then((d) => {
        if (cancelled) return;
        const list = d.launchable_tools ?? [];
        setTools(list);
        if (list.length > 0) setTool(list[0]);
        setAllowedRoots(d.allowed_project_roots ?? []);
      })
      .catch(() => {
        /* seam disabled — the submit will surface the honest error */
      });
    // Suggest the projects Observer already knows about. We deliberately do NOT
    // auto-select the most-recent one: it may not be allow-listed, which would
    // dead-end the launch with a 400. The default stays "Agent's default
    // directory" (always permitted); permitted projects are clearly grouped so
    // the user can pick one that will actually launch.
    fetchJSON<ProjectsResponse>("/api/projects")
      .then((d) => {
        if (cancelled) return;
        setProjects(d.rows ?? []);
      })
      .catch(() => {
        /* no project index yet — leave the launcher-default selection */
      });
    return () => {
      cancelled = true;
    };
  }, []);

  // Fetch the pre-launch binary-resolution verdict whenever the selected tool
  // changes. A 501 (seam disabled on an older daemon) or any other error clears
  // the strip silently — the launch itself stays the authority.
  useEffect(() => {
    if (!tool) {
      setPreflight(null);
      return;
    }
    let cancelled = false;
    fetchJSON<ToolPreflight>("/api/terminal/launch/preflight", { tool })
      .then((d) => {
        if (!cancelled) setPreflight(d);
      })
      .catch(() => {
        if (!cancelled) setPreflight(null);
      });
    return () => {
      cancelled = true;
    };
  }, [tool]);

  // installTool spawns the tool's grounded install command in a fresh embedded
  // terminal (POST /api/terminal/install), the guided fix for a not_found /
  // foreign_only verdict. It is a Local + confirm-token-gated EXECUTE, so it
  // reads the double-submit token from /api/remote/config exactly like the
  // Remote page's privileged POSTs, then opens the returned handle in the dock
  // like any launch (labelled "<tool> install"). The server owns the argv — the
  // request carries only the tool name.
  async function installTool() {
    if (!tool || installBusy) return;
    setInstallBusy(true);
    setErr(null);
    try {
      const cfg = await fetchJSON<{ confirm_token?: string }>("/api/remote/config");
      const ctok = cfg.confirm_token ?? "";
      if (!ctok) {
        setErr("No confirm token — reload the page.");
        return;
      }
      const r = await fetchJSON<{ handle: string; tool: string; command: string }>(
        "/api/terminal/install",
        undefined,
        {
          method: "POST",
          headers: { "Content-Type": "application/json", "X-Observer-Confirm": ctok },
          body: JSON.stringify({ tool }),
        },
      );
      onLaunched(r.handle, `${tool} install`, false);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setInstallBusy(false);
    }
  }

  // The path actually sent to the launch API: the hand-typed value when the
  // Custom escape hatch is chosen, otherwise the selected project root ("" ==
  // let the launcher use the agent's own default cwd). Validation is unchanged
  // — the server still canonicalizes it against [terminal.launch].allowed_project_roots.
  const effectiveRoot = useMemo(
    () => (rootSel === CUSTOM_ROOT ? customRoot.trim() : rootSel.trim()),
    [rootSel, customRoot],
  );

  // Partition the working-directory options against the server's allow-list.
  // Permitted = every canonical allowed root PLUS any known project under one
  // (deduped); a permitted known project shows its canonical entry when it IS an
  // allowed root, else its own path. Not permitted = known projects outside every
  // allowed root — shown disabled with an honest reason. Configured roots that
  // aren't known projects still surface (they're launch-ready).
  const { permittedRoots, blockedProjects } = useMemo(() => {
    const knownPaths = new Set(projects.map((p) => p.root_path));
    const permitted: string[] = [];
    const seen = new Set<string>();
    const add = (path: string) => {
      if (!seen.has(path)) {
        seen.add(path);
        permitted.push(path);
      }
    };
    // Configured roots that aren't themselves a known project row.
    for (const r of allowedRoots) {
      if (!knownPaths.has(r)) add(r);
    }
    const blocked: ProjectRow[] = [];
    for (const p of projects) {
      if (isPermittedRoot(p.root_path, allowedRoots)) add(p.root_path);
      else blocked.push(p);
    }
    return { permittedRoots: permitted, blockedProjects: blocked };
  }, [projects, allowedRoots]);

  // Verdict-derived UI state (tool-binary-resolution arc). A foreign_only /
  // not_found tool cannot be launched by the daemon, so Start is disabled — the
  // server stays the authority (it re-resolves at launch), this is the honest
  // up-front signal. The strip below renders the full guidance.
  const verdict = preflight?.verdict ?? "";
  const launchBlockedByVerdict = verdict === "foreign_only" || verdict === "not_found";

  const noAllowList = allowedRoots.length === 0;
  // Honest reason placed on every disabled option so hovering explains the block.
  const blockedTitle = noAllowList
    ? "No project roots are allow-listed — add one in [terminal.launch].allowed_project_roots (Terminals page → launch policy)"
    : "Not in [terminal.launch].allowed_project_roots — add it in the Terminals page → launch policy";

  async function submit() {
    if (!tool) {
      setErr("choose a tool");
      return;
    }
    if (remoteBlocked) {
      setErr(REMOTE_TERMINAL_OFF_MSG);
      return;
    }
    setBusy(true);
    setErr(null);
    try {
      const r = await fetchJSON<FreshLaunchResponse>(
        "/api/terminal/launch",
        undefined,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            tool,
            project_root: effectiveRoot || undefined,
          }),
        },
      );
      onLaunched(r.token, r.tool || tool, r.has_project_root);
    } catch (e) {
      // Swap the raw server body for actionable guidance on the two known
      // policy gates — the remote allow_terminal 403 and the allowed_project_roots
      // 400 — and keep the verbatim message for every other failure so it stays
      // diagnosable.
      setErr(
        isTerminalCapabilityError(e)
          ? REMOTE_TERMINAL_OFF_MSG
          : isProjectRootDeniedError(e)
            ? PROJECT_ROOT_DENIED_MSG
            : e instanceof Error
              ? e.message
              : String(e),
      );
    } finally {
      setBusy(false);
    }
  }

  return (
    <div
      className="fixed inset-0 z-[85] flex items-center justify-center bg-black/50 p-6"
      role="dialog"
      aria-modal="true"
      aria-label="New terminal"
      onClick={onClose}
    >
      <div
        className="w-[440px] max-w-[95vw] rounded-2 border bg-bg-1 p-4 shadow-lg"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="mb-3 flex items-center justify-between">
          <h2 className="text-sm font-semibold text-fg-1">New terminal</h2>
          <button
            type="button"
            onClick={onClose}
            className="rounded-2 px-2 py-0.5 text-[11px] text-fg-3 hover:bg-white/10 hover:text-fg-1"
          >
            ✕
          </button>
        </div>
        <p className="mb-3 text-[11px] leading-relaxed text-fg-3">
          Start a fresh agent in the embedded terminal. The operator must enable
          this in <code className="font-mono">[terminal.launch]</code> and
          allow-list the tool and project root — otherwise the launch is
          refused.
        </p>

        {remoteBlocked && (
          <div className="mb-3 rounded-2 border border-warn/40 bg-warn/10 px-2 py-1.5 text-[11px] text-warn">
            {REMOTE_TERMINAL_OFF_MSG}
          </div>
        )}

        <label className="mb-1 block text-[11px] font-medium text-fg-2">
          Tool
        </label>
        <select
          value={tool}
          onChange={(e) => setTool(e.target.value)}
          className="mb-3 w-full rounded-2 border bg-bg-0 px-2 py-1.5 text-[12px] text-fg-1"
        >
          {tools.length === 0 && <option value="">no launchable tools</option>}
          {tools.map((t) => (
            <option key={t} value={t}>
              {t}
            </option>
          ))}
        </select>

        <label
          htmlFor="new-terminal-root"
          className="mb-1 block text-[11px] font-medium text-fg-2"
        >
          Project root <span className="text-fg-3">(optional)</span>
        </label>
        <select
          id="new-terminal-root"
          value={rootSel}
          onChange={(e) => setRootSel(e.target.value)}
          className="w-full rounded-2 border bg-bg-0 px-2 py-1.5 text-[12px] text-fg-1"
        >
          {/* Native <option> can't host a React tooltip (the browser owns
              the select popup rendering) — keep the native title= here. */}
          <option
            value=""
            title="No project root: the fresh agent runs in the SuperBased daemon's own working directory (where observer start / observer dashboard was launched from)."
          >
            Agent's default directory (where SuperBased runs)
          </option>
          {permittedRoots.length > 0 && (
            <optgroup label="Permitted">
              {permittedRoots.map((r) => (
                // Native <option> — title= stays (React tooltip can't render
                // inside the browser-owned select popup).
                <option key={r} value={r} title={r}>
                  {shortenPath(r)}
                </option>
              ))}
            </optgroup>
          )}
          {blockedProjects.length > 0 && (
            <optgroup label="Not permitted (configure in Terminals → launch policy)">
              {blockedProjects.map((p) => (
                // Native <option> — title= stays (React tooltip can't render
                // inside the browser-owned select popup).
                <option
                  key={p.root_path}
                  value={p.root_path}
                  disabled
                  title={`${p.root_path} — ${blockedTitle}`}
                >
                  {shortenPath(p.root_path)}
                </option>
              ))}
            </optgroup>
          )}
          <option value={CUSTOM_ROOT}>Custom path…</option>
        </select>
        {noAllowList && projects.length > 0 && (
          <p className="mt-1 text-[10.5px] leading-relaxed text-fg-3">
            No project roots are allow-listed, so only the agent's default
            directory can launch — that's the SuperBased daemon's own working
            directory (where <code className="font-mono">observer start</code> ran).
            Add roots under{" "}
            <code className="font-mono">[terminal.launch].allowed_project_roots</code>{" "}
            on the Terminals page (launch policy) to enable them.
          </p>
        )}
        {rootSel === CUSTOM_ROOT ? (
          <input
            type="text"
            value={customRoot}
            onChange={(e) => setCustomRoot(e.target.value)}
            autoFocus
            placeholder="/abs/path/to/project (must be allow-listed)"
            className="mt-2 w-full rounded-2 border bg-bg-0 px-2 py-1.5 font-mono text-[12px] text-fg-1"
          />
        ) : rootSel ? (
          <Tooltip content={rootSel}>
            <div className="mt-1 break-all font-mono text-[10.5px] text-fg-3">
              {rootSel}
            </div>
          </Tooltip>
        ) : null}
        {rootSel === CUSTOM_ROOT && (
          <p className="mt-1 text-[10.5px] leading-relaxed text-fg-3">
            Windows paths (<code className="font-mono">C:\Users\…</code>) are
            accepted and translated to their WSL{" "}
            <code className="font-mono">/mnt/c/…</code> form.
          </p>
        )}
        <div className="mb-3" />

        {preflight && verdict !== "ok" && (
          <div className="mb-3">
            {(verdict === "ok_off_path" || verdict === "shadowed") &&
              preflight.notes &&
              preflight.notes.length > 0 && (
                <div className="rounded-2 border border-fg-3/30 bg-white/5 px-2 py-1.5 text-[11px] leading-relaxed text-fg-3">
                  {/* Render ALL notes (cap 4), each on its own line: when the
                      login-capture note is prepended, a shadowed verdict's shim
                      explanation is in notes[1+] and must stay visible. */}
                  {preflight.notes.slice(0, 4).map((n, i) => (
                    <div key={i} className={i > 0 ? "mt-1" : undefined}>
                      {n}
                    </div>
                  ))}
                </div>
              )}
            {(verdict === "foreign_only" || verdict === "not_found") && (
              <div className="rounded-2 border border-warn/40 bg-warn/10 px-2 py-1.5 text-[11px] leading-relaxed text-warn">
                {verdict === "foreign_only" ? (
                  <span>
                    {tool} is installed on Windows, not in WSL — the daemon can't
                    launch it.
                  </span>
                ) : (
                  <span>{tool} is not installed.</span>
                )}
                {preflight.install_command && (
                  <>
                    <div className="mt-1.5 text-fg-3">Install it with:</div>
                    <code className="mt-1 block break-all rounded-2 bg-bg-0 px-2 py-1 font-mono text-[10.5px] text-fg-1">
                      {preflight.install_command}
                    </code>
                  </>
                )}
                {preflight.can_install && (
                  <button
                    type="button"
                    disabled={installBusy}
                    onClick={installTool}
                    className="mt-2 rounded-2 bg-accent px-3 py-1 text-[11px] font-medium text-white disabled:opacity-50"
                  >
                    {installBusy ? "Starting install…" : "Install in terminal"}
                  </button>
                )}
              </div>
            )}
          </div>
        )}

        {err && (
          <div className="mb-3 rounded-2 border border-danger/30 bg-danger/10 px-2 py-1.5 text-[11px] text-danger">
            {err}
          </div>
        )}

        <div className="flex justify-end gap-2">
          <button
            type="button"
            onClick={onClose}
            className="rounded-2 px-3 py-1.5 text-[12px] text-fg-2 hover:bg-white/10"
          >
            Cancel
          </button>
          {(() => {
            const tip = remoteBlocked
              ? REMOTE_TERMINAL_OFF_MSG
              : launchBlockedByVerdict
                ? verdict === "foreign_only"
                  ? `${tool} is installed on Windows, not in WSL — the daemon can't launch it`
                  : `${tool} is not installed`
                : null;
            const startBtn = (
              <button
                type="button"
                disabled={busy || !tool || remoteBlocked || launchBlockedByVerdict}
                onClick={submit}
                className="rounded-2 bg-accent px-3 py-1.5 text-[12px] font-medium text-white disabled:opacity-50"
              >
                {busy ? "Starting…" : "Start"}
              </button>
            );
            // The tip only shows while the button is disabled (blocked), and a
            // disabled <button> swallows pointer events — TooltipSpan gives it a
            // hoverable span reference. No tip → bare button (no extra tab stop).
            return tip ? (
              <TooltipSpan content={tip}>{startBtn}</TooltipSpan>
            ) : (
              startBtn
            );
          })()}
        </div>
      </div>
    </div>
  );
}
