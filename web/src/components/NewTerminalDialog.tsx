import { useEffect, useMemo, useRef, useState } from "react";
import { fetchJSON } from "@/lib/api";
import type {
  ProjectRow,
  ProjectsResponse,
  SandboxAvailability,
  ToolModels,
  ToolPreflight,
} from "@/lib/types";
import {
  PROJECT_ROOT_DENIED_MSG,
  REMOTE_TERMINAL_OFF_MSG,
  isProjectRootDeniedError,
  isTerminalCapabilityError,
  useRemoteTerminalGate,
} from "@/lib/remoteTerminal";
import { ComboChip, Tooltip, TooltipSpan, type ComboOption } from "@/components/primitives";

// Sentinel <select> value for the "type a path by hand" escape hatch. A NUL
// byte can never be a real project root, so it can't collide with one. The NUL
// is an ESCAPE (\u0000), never a literal byte — a literal NUL makes git treat
// this whole file as binary, hiding every change from diff review.
const CUSTOM_ROOT = "\u0000custom";

// SHELL_TOOL is the reserved pseudo-tool sentinel for a fresh PLAIN SHELL
// launch (must match internal/termsvc.ShellTool = "shell" verbatim). It is
// deliberately never a member of launchable_tools (the capability registry) —
// this dialog adds it client-side as its own option, gated by the SEPARATE
// [terminal.launch].allow_shell opt-in (shell_enabled from GET
// /api/terminal/sessions), not the AI-tool allow-list.
const SHELL_TOOL = "shell";

// SANDBOX_SOURCE_LABELS maps the server's closed workspace-source vocabulary
// (SandboxAvailability.sources[].id, mirrored 1:1 from internal/workspace's
// Source constants) to the dialog's display labels — the ONE place that
// vocabulary is spelled out for humans. Falls back to the raw id for any
// future source the dialog hasn't been taught about yet, rather than hiding
// it.
const SANDBOX_SOURCE_LABELS: Record<string, string> = {
  live: "Live project directory",
  "clone-local": "Copy of the project (clone)",
  "clone-remote": "Clone a remote URL",
  worktree: "Worktree",
};

// sandboxDisabledReason computes the "Run in sandbox" checkbox's disabled
// copy, honoring the honest-copy convention (CLAUDE.md): every disabled
// control names the exact blocker verbatim from the server, never a generic
// "unavailable". Priority: no tool picked > the probe hasn't resolved yet (or
// failed — B5's fail-silent pattern: a broken probe disables the control
// rather than erroring the whole dialog) > the daemon-wide verdict
// (SandboxAvailability.reason, quoted verbatim) > the shell pseudo-tool
// (never in the capability registry the probe's per-tool map is built from)
// > the selected tool's own SandboxToolAvail.reason (v1 grounds only
// claude-code; every other launchable tool carries an honest reason instead
// of silently omitting itself from the map). Returns null when the checkbox
// should be enabled.
function sandboxDisabledReason(
  tool: string,
  probe: SandboxAvailability | null,
): string | null {
  if (!tool) return "Choose a tool first.";
  if (!probe) {
    return "Sandbox status unknown — this daemon may not support sandboxed terminals, or the probe request failed.";
  }
  if (!probe.available) {
    return `Sandbox unavailable — ${probe.reason || "sandboxing is unavailable on this daemon"}`;
  }
  if (tool === SHELL_TOOL) {
    return "A plain shell has no AI-tool state dirs to sandbox — not sandbox-launchable.";
  }
  const t = probe.tools?.[tool];
  if (!t || !t.available) {
    return t?.reason || "No grounded sandbox profile for this tool — not sandbox-launchable.";
  }
  return null;
}

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

export type NewTerminalDraft = {
  tool: string;
  rootSel: string;
  customRoot: string;
  modelSel: string;
  sandboxOn: boolean;
  workspaceSource: string;
  workspaceRemote: string;
  workspaceBranch: string;
};

type Props = {
  onClose: () => void;
  initialDraft?: NewTerminalDraft;
  resumedAfterInstall?: boolean;
  /**
   * Called with the minted handle + tool once a fresh launch succeeds. The
   * third arg (review finding 8) reports whether the launch was given a project
   * root, so the dock enables Files/Git without a reload. The fourth arg is
   * present only for a guided installer: the dock restores that exact launch
   * draft when the installer terminal exits. Absence ≡ false/no resume.
   */
  onLaunched: (
    handle: string,
    tool: string,
    hasProjectRoot?: boolean,
    resumeDraft?: NewTerminalDraft,
  ) => void;
};

type FreshLaunchResponse = { token: string; tool: string; subcommand: string; has_project_root?: boolean };

export function NewTerminalDialog({
  onClose,
  onLaunched,
  initialDraft,
  resumedAfterInstall = false,
}: Props) {
  const [tools, setTools] = useState<string[]>([]);
  const [tool, setTool] = useState(initialDraft?.tool ?? "");
  // Known project roots (GET /api/projects, ordered most-recent-first) power
  // the working-directory dropdown; the user can still pick "Custom path…" to
  // type an arbitrary one. `rootSel` is the <select> value; `customRoot` holds
  // the hand-typed path only while CUSTOM_ROOT is selected.
  const [projects, setProjects] = useState<ProjectRow[]>([]);
  // The operator's canonicalized [terminal.launch].allowed_project_roots, straight
  // from GET /api/terminal/sessions. Used verbatim to mark which roots a fresh
  // launch will actually accept (empty = deny-all: only the agent's default cwd).
  const [allowedRoots, setAllowedRoots] = useState<string[]>([]);
  // shell_enabled from GET /api/terminal/sessions — the SEPARATE
  // [terminal.launch].allow_shell opt-in that gates the Shell option below,
  // independent of allow_fresh_agent / allowed_tools.
  const [shellEnabled, setShellEnabled] = useState(false);
  const [rootSel, setRootSel] = useState(initialDraft?.rootSel ?? "");
  const [customRoot, setCustomRoot] = useState(initialDraft?.customRoot ?? "");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  // Pre-launch binary-resolution verdict for the selected tool (tool-binary-
  // resolution arc). Null when unknown, the seam is disabled (501), or the fetch
  // failed — in every case the strip simply doesn't render (fail-silent), so an
  // older daemon without the preflight endpoint behaves exactly as before.
  const [preflight, setPreflight] = useState<ToolPreflight | null>(null);
  const [installBusy, setInstallBusy] = useState(false);
  // Model-suggestion list for the selected tool (B5 model picker), from GET
  // /api/terminal/launch/models. Null when unknown, the tool doesn't support
  // model selection, the seam is disabled (404/501 on an older daemon), or the
  // fetch failed — in every case the picker simply doesn't render, mirroring
  // the preflight strip's fail-silent degrade.
  const [toolModels, setToolModels] = useState<ToolModels | null>(null);
  // The user's explicit model choice; "" means "use the tool's own default"
  // and is never sent on the launch POST.
  const [modelSel, setModelSel] = useState(initialDraft?.modelSel ?? "");
  const previousTool = useRef(tool);

  // Preserve an installer-resumed model on the first render, then clear it on
  // every real adapter transition — including a server-driven fallback when a
  // resumed adapter is no longer launchable. A dropdown-only reset misses that
  // fallback and can send one adapter's model to another.
  useEffect(() => {
    if (previousTool.current === tool) return;
    previousTool.current = tool;
    setModelSel("");
  }, [tool]);
  // Sandbox probe result (B9 U7), from GET /api/terminal/sandbox. Null when
  // unfetched, the seam is disabled (an older daemon has no such route), or
  // the fetch failed — mirrors the preflight/model-picker fail-silent
  // degrade: the checkbox below simply renders disabled, never an error.
  // Fetched once on dialog open (the response already carries a per-tool
  // map covering every launchable tool, so a tool-change never needs its own
  // refetch — see sandboxDisabledReason).
  const [sandboxProbe, setSandboxProbe] = useState<SandboxAvailability | null>(null);
  // Whether the user has opted into a sandboxed launch. Seeded from the
  // server's [terminal.sandbox].default_on signal once the probe resolves;
  // the checkbox remains an explicit per-launch override.
  const [sandboxOn, setSandboxOn] = useState(initialDraft?.sandboxOn ?? false);
  const [workspaceSource, setWorkspaceSource] = useState(
    initialDraft?.workspaceSource ?? "live",
  );
  const [workspaceRemote, setWorkspaceRemote] = useState(
    initialDraft?.workspaceRemote ?? "",
  );
  const [workspaceBranch, setWorkspaceBranch] = useState(
    initialDraft?.workspaceBranch ?? "",
  );
  // Remote-device launch gate: a paired device can only fresh-launch when the
  // owner has enabled [remote].allow_terminal. When it's off we say so up front
  // and disable Start, rather than letting the POST fail with a raw 403.
  const { blocked: remoteBlocked } = useRemoteTerminalGate();

  useEffect(() => {
    let cancelled = false;
    fetchJSON<{
      launchable_tools?: string[];
      allowed_project_roots?: string[];
      shell_enabled?: boolean;
    }>("/api/terminal/sessions")
      .then((d) => {
        if (cancelled) return;
        const list = d.launchable_tools ?? [];
        setTools(list);
        setTool((current) => {
          if (current === SHELL_TOOL && d.shell_enabled) return current;
          if (current && list.includes(current)) return current;
          return list[0] ?? "";
        });
        setAllowedRoots(d.allowed_project_roots ?? []);
        setShellEnabled(d.shell_enabled ?? false);
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
    // The shell pseudo-tool is never in the capability registry the preflight
    // seam resolves against — skip the fetch and clear any stale AI-tool
    // verdict rather than let it 404/error into a confusing strip.
    if (!tool || tool === SHELL_TOOL) {
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

  // Fetch the model-suggestion list for the selected tool (B5). Mirrors the
  // preflight effect exactly: Shell is skipped (it has no model concept), a
  // 404/501 or any other error clears the state silently, and a stale response
  // from a since-abandoned tool is ignored via the same cancellation flag. The
  // selected model is reset whenever the tool changes, whether or not the
  // fetch succeeds — a model picked for one tool must never leak into another.
  useEffect(() => {
    if (!tool || tool === SHELL_TOOL) {
      setToolModels(null);
      return;
    }
    let cancelled = false;
    fetchJSON<ToolModels>("/api/terminal/launch/models", { tool })
      .then((d) => {
        if (!cancelled) setToolModels(d);
      })
      .catch(() => {
        if (!cancelled) setToolModels(null);
      });
    return () => {
      cancelled = true;
    };
  }, [tool]);

  // Fetch the sandbox probe (B9 U7) once on dialog open. Unlike preflight/
  // toolModels above this does NOT depend on `tool` — the response's `tools`
  // map already covers every launchable tool in one shot, so re-selecting the
  // tool dropdown re-evaluates sandboxDisabledReason from the SAME fetched
  // probe rather than re-fetching. A 404/501 (older daemon) or any other
  // error leaves sandboxProbe null, which sandboxDisabledReason renders as a
  // disabled checkbox with an honest "status unknown" reason — mirrors the
  // preflight/model-picker fail-silent degrade.
  useEffect(() => {
    let cancelled = false;
    fetchJSON<SandboxAvailability>("/api/terminal/sandbox")
      .then((d) => {
        if (!cancelled) {
          setSandboxProbe(d);
          if (!initialDraft) setSandboxOn(d.default_on ?? false);
        }
      })
      .catch(() => {
        if (!cancelled) setSandboxProbe(null);
      });
    return () => {
      cancelled = true;
    };
  }, []);

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
      onLaunched(r.handle, `${tool} install`, false, {
        tool,
        rootSel,
        customRoot,
        modelSel,
        sandboxOn,
        workspaceSource,
        workspaceRemote,
        workspaceBranch,
      });
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

  // The searchable folder-selector's option list (ComboChip): the agent's
  // default directory, then every permitted root, then every blocked known
  // project (shown but disabled, honest-reason tooltip — same partition the
  // native <select> used, now with type-ahead search over BOTH groups so a
  // large project index stays navigable). There is no filesystem-browse/
  // listdir daemon endpoint (as of this writing) to power a true directory-
  // tree picker, so this searchable list over the operator's allow-list +
  // observed-project index is the selector — not a live filesystem browser.
  // A "Custom path…" escape hatch below covers any allow-listed folder that
  // isn't yet a known project.
  const rootOptions = useMemo<ComboOption[]>(() => {
    const opts: ComboOption[] = [
      {
        value: "",
        label: "Agent's default directory (where SuperBased runs)",
        searchable: "agent's default directory where superbased runs",
        title:
          "No project root: the fresh agent runs in the SuperBased daemon's own working directory (where observer start / observer dashboard was launched from).",
      },
    ];
    for (const r of permittedRoots) {
      opts.push({
        value: r,
        label: <span className="font-mono text-[11px]">{shortenPath(r)}</span>,
        searchable: r.toLowerCase(),
        title: r,
        groupLabel: "Permitted",
      });
    }
    for (const p of blockedProjects) {
      opts.push({
        value: p.root_path,
        label: <span className="font-mono text-[11px]">{shortenPath(p.root_path)}</span>,
        searchable: p.root_path.toLowerCase(),
        title: `${p.root_path} — ${blockedTitle}`,
        disabled: true,
        groupLabel: "Not permitted",
      });
    }
    opts.push({
      value: CUSTOM_ROOT,
      label: "Custom path…",
      searchable: "custom path type manually",
      title: "Type an absolute path by hand — it must be allow-listed to launch.",
    });
    return opts;
  }, [permittedRoots, blockedProjects, blockedTitle]);

  // Sandbox toggle (B9 U7): null when the checkbox should be enabled, else the
  // honest disabled-copy string (see sandboxDisabledReason above).
  const sandboxReason = useMemo(
    () => sandboxDisabledReason(tool, sandboxProbe),
    [tool, sandboxProbe],
  );
  const sandboxCheckboxDisabled = sandboxReason !== null;
  // A source the user has currently picked but that isn't actually available
  // (e.g. left on "clone-remote" after switching to a probe/tool where it's
  // gated off) blocks the client-side hint the same way an empty remote URL
  // does — the server re-validates regardless, this only prevents an
  // obviously-doomed submit.
  const selectedSourceAvail = sandboxProbe?.sources?.find((s) => s.id === workspaceSource);
  const sandboxRemoteURLMissing =
    sandboxOn && !sandboxCheckboxDisabled && workspaceSource === "clone-remote" && workspaceRemote.trim() === "";
  const sandboxSourceUnavailable =
    sandboxOn && !sandboxCheckboxDisabled && selectedSourceAvail !== undefined && !selectedSourceAvail.available;
  const sandboxBlocksStart = sandboxOn && !sandboxCheckboxDisabled && (sandboxRemoteURLMissing || sandboxSourceUnavailable);

  // If the tool changes (or the probe resolves) into a state where the
  // sandbox checkbox becomes disabled, uncheck it rather than leaving a
  // stale "on" that the POST body would silently drop (sandbox fields are
  // only added when sandboxOn is true — see submit()).
  useEffect(() => {
    if (sandboxCheckboxDisabled && sandboxOn) setSandboxOn(false);
  }, [sandboxCheckboxDisabled, sandboxOn]);

  async function submit() {
    if (!tool) {
      setErr("choose a tool");
      return;
    }
    if (remoteBlocked) {
      setErr(REMOTE_TERMINAL_OFF_MSG);
      return;
    }
    if (sandboxBlocksStart) {
      setErr(
        sandboxRemoteURLMissing
          ? "Enter a remote URL to clone into the sandbox."
          : "The selected workspace source isn't available — pick another one.",
      );
      return;
    }
    setBusy(true);
    setErr(null);
    try {
      // Sandbox fields are added ONLY when the checkbox is on — off, the
      // request body is byte-identical to a pre-U7 launch (plan §5). The
      // server re-validates workspace_source membership + everything else
      // and fail-CLOSES; this is a UX hint, not authorization.
      const sandboxFields = sandboxOn
        ? {
            sandbox: true,
            workspace_source: workspaceSource || "live",
            ...(workspaceRemote.trim() ? { workspace_remote: workspaceRemote.trim() } : {}),
            ...(workspaceBranch.trim() ? { workspace_branch: workspaceBranch.trim() } : {}),
          }
        : {};
      const r = await fetchJSON<FreshLaunchResponse>(
        "/api/terminal/launch",
        undefined,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            tool,
            project_root: effectiveRoot || undefined,
            model: modelSel || undefined,
            ...sandboxFields,
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

        {resumedAfterInstall && (
          <div className="mb-3 rounded-2 border border-ok/40 bg-ok/10 px-2 py-1.5 text-[11px] text-ok">
            Installer terminal finished. Your adapter, project, model, and sandbox choices were preserved;
            the availability check below has been refreshed.
          </div>
        )}

        <label className="mb-1 block text-[11px] font-medium text-fg-2">
          Tool
        </label>
        <select
          value={tool}
          onChange={(e) => {
            setTool(e.target.value);
          }}
          className="mb-3 w-full rounded-2 border bg-bg-0 px-2 py-1.5 text-[12px] text-fg-1"
        >
          {tools.length === 0 && <option value="">no launchable tools</option>}
          {tools.map((t) => (
            <option key={t} value={t}>
              {t}
            </option>
          ))}
          {/* Native <option> — title= stays (React tooltip can't render inside
              the browser-owned select popup). Shell is a reserved pseudo-tool,
              never a member of `tools` (the capability registry) — gated by
              its own SEPARATE [terminal.launch].allow_shell opt-in instead of
              the AI-tool allow-list. */}
          <option
            value={SHELL_TOOL}
            disabled={!shellEnabled}
            title={
              shellEnabled
                ? "Start a plain shell ($SHELL, or bash/sh as a fallback) — no AI tool involved."
                : "Not enabled — turn on [terminal.launch].allow_shell in Terminals → launch policy"
            }
          >
            Shell {shellEnabled ? "" : "(disabled)"}
          </option>
        </select>

        {toolModels && toolModels.supported && toolModels.models.length > 0 && (
          <>
            <label
              htmlFor="new-terminal-model"
              className="mb-1 block text-[11px] font-medium text-fg-2"
            >
              Model <span className="text-fg-3">(optional)</span>
            </label>
            <select
              id="new-terminal-model"
              value={modelSel}
              onChange={(e) => setModelSel(e.target.value)}
              className="mb-3 w-full rounded-2 border bg-bg-0 px-2 py-1.5 text-[12px] text-fg-1"
            >
              <option value="" title={`Let ${tool} choose its own default model`}>
                Tool default
              </option>
              {toolModels.models.map((m) => (
                <option key={m.model} value={m.model} title={m.model}>
                  {m.model}
                  {m.source === "history" && m.count
                    ? ` (${m.count.toLocaleString()} uses)`
                    : ""}
                </option>
              ))}
            </select>
          </>
        )}

        <label className="mb-1 block text-[11px] font-medium text-fg-2">
          Project root <span className="text-fg-3">(optional)</span>
        </label>
        <ComboChip
          value={rootSel}
          onChange={setRootSel}
          options={rootOptions}
          label="Folder"
          fullWidth
          popoverWidth={396}
          placeholder="Search permitted folders…"
          emptyHint="No folders match. Try “Custom path…” or add a root in Terminals → Settings → Folder Selection."
          buttonValueRender={(sel) => (
            <span className="min-w-0 flex-1 truncate text-left font-semibold text-fg-0">
              {sel?.label ?? "Agent's default directory (where SuperBased runs)"}
            </span>
          )}
        />
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

        <label className="mb-1 flex items-start gap-2">
          <input
            type="checkbox"
            checked={sandboxOn}
            disabled={sandboxCheckboxDisabled}
            title={sandboxReason ?? undefined}
            onChange={(e) => setSandboxOn(e.target.checked)}
            className="mt-0.5"
          />
          <span>
            <span className="font-medium text-fg-1">Run in sandbox</span>
            <span className="block text-[11px] text-fg-3">
              {sandboxReason ??
                "Launch inside a bubblewrap sandbox with an isolated $HOME — the agent can't read or write your real home directory or other projects."}
            </span>
          </span>
        </label>

        {sandboxOn && !sandboxCheckboxDisabled && (
          <div className="mb-3 mt-2 rounded-2 border border-line-2 bg-bg-2 p-2">
            <label
              htmlFor="new-terminal-sandbox-source"
              className="mb-1 block text-[11px] font-medium text-fg-2"
            >
              Workspace
            </label>
            <select
              id="new-terminal-sandbox-source"
              value={workspaceSource}
              onChange={(e) => setWorkspaceSource(e.target.value)}
              className="w-full rounded-2 border bg-bg-0 px-2 py-1.5 text-[12px] text-fg-1"
            >
              {(sandboxProbe?.sources ?? []).map((s) => (
                // Native <option> — title= stays (React tooltip can't render
                // inside the browser-owned select popup), same convention as
                // the project-root optgroups above.
                <option
                  key={s.id}
                  value={s.id}
                  disabled={!s.available}
                  title={
                    s.available
                      ? SANDBOX_SOURCE_LABELS[s.id] ?? s.id
                      : s.reason || "not available"
                  }
                >
                  {SANDBOX_SOURCE_LABELS[s.id] ?? s.id}
                  {s.available ? "" : " (unavailable)"}
                </option>
              ))}
            </select>
            {sandboxSourceUnavailable && (
              <p className="mt-1 text-[10.5px] leading-relaxed text-warn">
                {selectedSourceAvail?.reason || "This workspace source isn't available."}
              </p>
            )}
            {workspaceSource === "clone-remote" && (
              <>
                <label
                  htmlFor="new-terminal-sandbox-remote"
                  className="mb-1 mt-2 block text-[11px] font-medium text-fg-2"
                >
                  Remote URL
                </label>
                <input
                  id="new-terminal-sandbox-remote"
                  type="text"
                  value={workspaceRemote}
                  onChange={(e) => setWorkspaceRemote(e.target.value)}
                  placeholder="https://github.com/org/repo.git"
                  className="w-full rounded-2 border bg-bg-0 px-2 py-1.5 font-mono text-[12px] text-fg-1"
                />
                <label
                  htmlFor="new-terminal-sandbox-branch"
                  className="mb-1 mt-2 block text-[11px] font-medium text-fg-2"
                >
                  Branch <span className="text-fg-3">(optional)</span>
                </label>
                <input
                  id="new-terminal-sandbox-branch"
                  type="text"
                  value={workspaceBranch}
                  onChange={(e) => setWorkspaceBranch(e.target.value)}
                  placeholder="main"
                  className="w-full rounded-2 border bg-bg-0 px-2 py-1.5 font-mono text-[12px] text-fg-1"
                />
                {sandboxRemoteURLMissing && (
                  <p className="mt-1 text-[10.5px] leading-relaxed text-warn">
                    Enter a remote URL — the daemon will run `git clone` with your
                    ambient auth into a managed workspace.
                  </p>
                )}
              </>
            )}
          </div>
        )}

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
                : sandboxBlocksStart
                  ? sandboxRemoteURLMissing
                    ? "Enter a remote URL to clone into the sandbox, or choose a different workspace"
                    : "The selected workspace source isn't available — choose a different one"
                  : null;
            const startBtn = (
              <button
                type="button"
                disabled={
                  busy || !tool || remoteBlocked || launchBlockedByVerdict || sandboxBlocksStart
                }
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
