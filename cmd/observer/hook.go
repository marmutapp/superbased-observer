package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/adapter/hermes"
	"github.com/marmutapp/superbased-observer/internal/compression/indexing"
	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/contentcap"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/guard"
	"github.com/marmutapp/superbased-observer/internal/guard/notify"
	"github.com/marmutapp/superbased-observer/internal/hook"
	"github.com/marmutapp/superbased-observer/internal/intelligence/compaction"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/pidbridge"
	"github.com/marmutapp/superbased-observer/internal/scrub"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// defaultHookMaxRuntime is the wall-clock budget for a single `observer
// hook` invocation. Hooks that exceed it exit promptly via os.Exit(0)
// (fail-open — a timeout must never block the host tool). The
// 30 s default is generous vs. the typical hook (sub-second to ~3 s
// including db.Open + a single write) but tight enough that a hung hook
// can't pin the host AI tool for minutes. Override with `--max-runtime`
// or `OBSERVER_HOOK_MAX_RUNTIME`. See V3-1 in
// docs/observer-platform-issues-v3.md: pre-fix, a hook subprocess could
// block 20+ minutes waiting on stuck DB / syscall paths because the
// "MUST NEVER block the host" invariant was documented but unenforced.
const defaultHookMaxRuntime = 30 * time.Second

// resolveHookMaxRuntime picks the watchdog budget. Precedence: explicit
// --max-runtime flag (when non-zero) > OBSERVER_HOOK_MAX_RUNTIME env >
// defaultHookMaxRuntime. A non-positive value (after parsing) disables
// the watchdog entirely — escape hatch for diagnostics, not the
// production path.
func resolveHookMaxRuntime(flag time.Duration, env string) time.Duration {
	if flag != 0 {
		return flag
	}
	if env != "" {
		if d, err := time.ParseDuration(env); err == nil {
			return d
		}
	}
	return defaultHookMaxRuntime
}

// installHookWatchdog arms a time.AfterFunc that fires exitFn(0) when
// maxRuntime elapses. Returns a stop func to disarm on a clean exit.
// exitFn is injected so unit tests can verify firing without actually
// terminating the test process. In production, callers wire it to
// os.Exit.
//
// **Fail-OPEN on timeout (exit 0, never 2).** A non-zero exit from a
// PreToolUse hook BLOCKS the host tool — so a watchdog that exits 2 turns
// a transient capture stall (e.g. the DB momentarily locked by a daemon
// startup prune/checkpoint on a large DB) into a hard block on the user's
// work, cascading across the session. A watchdog timeout is NOT a deny
// decision; the hook simply didn't finish in time, so it must allow the
// tool through. This upholds the command's "exit 0 on every path"
// invariant (see newHookCmd). The process still terminates promptly, so
// the host tool is never pinned waiting.
func installHookWatchdog(maxRuntime time.Duration, exitFn func(int), stderr io.Writer) func() {
	if maxRuntime <= 0 {
		return func() {}
	}
	timer := time.AfterFunc(maxRuntime, func() {
		fmt.Fprintf(stderr, "observer-hook: max-runtime %v exceeded; allowing the tool (fail-open, exit 0; see V3-1)\n", maxRuntime)
		exitFn(0)
	})
	return func() { timer.Stop() }
}

// newHookCmd implements `observer hook <tool> <event>`. The host AI tool
// invokes this on every fired event with a JSON payload on stdin.
//
// For Claude Code (and any unknown tool) we approve immediately and rely on
// the JSONL watcher for capture. For Cursor — which has no native session
// log — we additionally insert the event into the observer DB before exiting.
//
// The command is intentionally flat rather than a subcommand tree so that
// settings.json / hooks.json can hard-code predictable command strings.
//
// Spec P1: this command MUST exit 0 on every path. Errors are written to
// stderr but never propagate as a non-zero exit. A process-level watchdog
// (`--max-runtime`, default 30 s) is the defensive belt-and-suspenders:
// if any code path blocks (stuck DB write, blocked syscall, anything) the
// AfterFunc calls os.Exit(0) — fail-OPEN, so a transient capture stall
// (e.g. the DB momentarily locked by a daemon startup prune/checkpoint)
// can never block the host tool. The process still terminates promptly.
//
// `--config <path>` propagates the proxy's config to the hook handler so
// the DB write lands on the same observer.db the proxy is using. Without
// it, the hook always reads ~/.observer/config.toml regardless of which
// proxy daemon fired it.
func newHookCmd() *cobra.Command {
	var (
		configPath string
		maxRuntime time.Duration
	)
	cmd := &cobra.Command{
		Use:   "hook <tool> <event>",
		Short: "Handle an AI-tool hook event on stdin and reply on stdout",
		Long: "Invoked by the host AI tool via its hook configuration. Reads a\n" +
			"JSON payload from stdin and replies on stdout. For Cursor, also\n" +
			"records the event in the observer DB. MUST NEVER block the host\n" +
			"tool — errors are logged to stderr and swallowed. A process\n" +
			"watchdog (--max-runtime, default 30s) enforces the invariant.",
		Args: cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			budget := resolveHookMaxRuntime(maxRuntime, os.Getenv("OBSERVER_HOOK_MAX_RUNTIME"))
			stopWatchdog := installHookWatchdog(budget, os.Exit, os.Stderr)
			defer stopWatchdog()

			tool := args[0]
			event := ""
			if len(args) >= 2 {
				event = args[1]
			}
			switch tool {
			case "cursor":
				handleCursorHook(cmd.Context(), event, configPath)
			case "claude-code":
				handleClaudeCodeHook(cmd.Context(), event, configPath)
			case "codex":
				handleCodexHook(cmd.Context(), event, configPath)
			case "hermes":
				handleHermesHook(cmd.Context(), event, configPath)
			default:
				label := tool
				if event != "" {
					label = tool + ":" + event
				}
				hook.HandleApprove(label, os.Stdin, os.Stdout, os.Stderr)
			}
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to observer config.toml — when set, hook DB writes land on the config's observer.db (matches the proxy / MCP server invocations)")
	cmd.Flags().DurationVar(&maxRuntime, "max-runtime", 0, "Hard wall-clock cap on a single hook invocation; on exceedance the process exits(2) so the host AI tool isn't pinned. 0 = use OBSERVER_HOOK_MAX_RUNTIME or the 30s default. See docs/observer-platform-issues-v3.md V3-1.")
	return cmd
}

// handleClaudeCodeHook dispatches Claude Code hook events. For PreCompact
// and PostCompact we write to compaction_events; for PreToolUse we may
// rewrite a Bash command to funnel through `observer run`; other events
// fall through to HandleApprove (the JSONL watcher captures them
// out-of-band). Always replies with an approval on stdout — must never
// block the host.
//
// `configPath`, when non-empty, is forwarded to all `config.Load` calls
// so the hook handler reads the same config (and therefore writes to
// the same DB) as whichever proxy daemon launched it.
func handleClaudeCodeHook(ctx context.Context, event, configPath string) {
	label := "claude-code"
	if event != "" {
		label = "claude-code:" + event
	}
	if event == "pre-tool" {
		handleClaudeCodePreTool(os.Stdin, os.Stdout, os.Stderr, label, configPath)
		return
	}
	if event == "post-tool" {
		handleClaudeCodePostTool(os.Stdin, os.Stdout, os.Stderr, label, configPath)
		return
	}
	if event == "session-start" {
		writer := makePidbridgeWriter(configPath)
		handleClaudeCodeSessionStart(ctx, os.Getppid(), defaultAncestors, os.Stdin, os.Stdout, os.Stderr, label, writer)
		return
	}
	// Tier 1 expansion (2026-05): each of these events maps to one row
	// in the actions table via a per-event builder. Shared helper handles
	// stdin → reply → DB open → insert.
	switch event {
	case "session-end":
		handleClaudeCodeActionEvent(ctx, label, configPath, buildClaudeSessionEndEvent)
		return
	case "user-prompt-submit":
		handleClaudeCodeActionEvent(ctx, label, configPath, buildClaudeUserPromptSubmitEvent)
		return
	case "post-tool-failure":
		handleClaudeCodeActionEvent(ctx, label, configPath, buildClaudePostToolFailureEvent)
		return
	case "stop-failure":
		handleClaudeCodeActionEvent(ctx, label, configPath, buildClaudeStopFailureEvent)
		return
	case "subagent-start":
		handleClaudeCodeActionEvent(ctx, label, configPath, buildClaudeSubagentStartEvent)
		return
	case "subagent-stop":
		handleClaudeCodeActionEvent(ctx, label, configPath, buildClaudeSubagentStopEvent)
		return
	case "stop":
		handleClaudeCodeActionEvent(ctx, label, configPath, buildClaudeStopEvent)
		return
	case "notification":
		handleClaudeCodeActionEvent(ctx, label, configPath, buildClaudeNotificationEvent)
		return
	case "cwd-changed":
		handleClaudeCodeActionEvent(ctx, label, configPath, buildClaudeCwdChangedEvent)
		return
	case "setup":
		handleClaudeCodeActionEvent(ctx, label, configPath, buildClaudeSetupEvent)
		return
	case "user-prompt-expansion":
		handleClaudeCodeActionEvent(ctx, label, configPath, buildClaudeUserPromptExpansionEvent)
		return
	case "post-tool-batch":
		handleClaudeCodeActionEvent(ctx, label, configPath, buildClaudePostToolBatchEvent)
		return
	case "permission-request":
		handleClaudeCodeActionEvent(ctx, label, configPath, buildClaudePermissionRequestEvent)
		return
	case "permission-denied":
		handleClaudeCodeActionEvent(ctx, label, configPath, buildClaudePermissionDeniedEvent)
		return
	case "instructions-loaded":
		handleClaudeCodeActionEvent(ctx, label, configPath, buildClaudeInstructionsLoadedEvent)
		return
	case "config-change":
		handleClaudeCodeActionEvent(ctx, label, configPath, buildClaudeConfigChangeEvent)
		return
	case "worktree-remove":
		handleClaudeCodeActionEvent(ctx, label, configPath, buildClaudeWorktreeRemoveEvent)
		return
	case "worktree-create":
		// Special path: blocking hook that must write the chosen
		// worktree path on stdout (per docs.claude.com/docs/en/hooks
		// reply matrix) — any non-zero exit or empty stdout fails
		// the Agent spawn. NOT registered by default; user opts in
		// per docs/claude-worktree-hook.md.
		handleClaudeCodeWorktreeCreate(ctx, label, configPath, os.Stdin, os.Stdout, os.Stderr)
		return
	}
	if event != "pre-compact" && event != "post-compact" {
		hook.HandleApprove(label, os.Stdin, os.Stdout, os.Stderr)
		return
	}
	// Read stdin first — we need it for both the approval reply (which is
	// stateless) and the compaction handler.
	body, _ := io.ReadAll(io.LimitReader(os.Stdin, 2*1024*1024))
	// Reply immediately; the DB write is best-effort.
	_ = json.NewEncoder(os.Stdout).Encode(hook.Decision{Decision: "approve"})

	cfg, err := config.Load(config.LoadOptions{GlobalPath: configPath})
	if err != nil {
		fmt.Fprintf(os.Stderr, "observer-hook: %s config: %v\n", label, err)
		return
	}
	database, err := db.Open(ctx, db.Options{Path: cfg.Observer.DBPath, SkipIntegrityCheck: true})
	if err != nil {
		fmt.Fprintf(os.Stderr, "observer-hook: %s db: %v\n", label, err)
		return
	}
	defer database.Close()

	var payload struct {
		SessionID string `json:"session_id"`
		Cwd       string `json:"cwd"`
		Trigger   string `json:"trigger"`
	}
	_ = json.Unmarshal(body, &payload)
	if payload.SessionID == "" {
		fmt.Fprintf(os.Stderr, "observer-hook: %s no session_id in payload\n", label)
		return
	}

	rec := compaction.New(database)
	switch event {
	case "pre-compact":
		if _, err := rec.Capture(ctx, compaction.CaptureOptions{
			SessionID:   payload.SessionID,
			ProjectRoot: payload.Cwd,
			Tool:        models.ToolClaudeCode,
			Timestamp:   time.Now().UTC(),
			Trigger:     payload.Trigger,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "observer-hook: %s capture: %v\n", label, err)
		}
	case "post-compact":
		if err := rec.Reconcile(ctx, compaction.ReconcileOptions{
			SessionID: payload.SessionID,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "observer-hook: %s reconcile: %v\n", label, err)
		}
	}
}

// pidbridgeWriter is the injected DB-write side of the SessionStart hook,
// split out so tests can substitute an in-memory writer without touching
// the user's config.
type pidbridgeWriter func(ctx context.Context, e pidbridge.Entry) error

// defaultPidbridgeWriter opens the configured observer DB, writes one
// pidbridge entry, and closes the DB. Safe to call concurrently — each
// invocation opens its own *sql.DB.
func defaultPidbridgeWriter(ctx context.Context, e pidbridge.Entry) error {
	return pidbridgeWriterWithConfig(ctx, e, "")
}

// makePidbridgeWriter returns a writer bound to the given configPath.
// Empty configPath produces the default writer (legacy behaviour).
func makePidbridgeWriter(configPath string) pidbridgeWriter {
	if configPath == "" {
		return defaultPidbridgeWriter
	}
	return func(ctx context.Context, e pidbridge.Entry) error {
		return pidbridgeWriterWithConfig(ctx, e, configPath)
	}
}

func pidbridgeWriterWithConfig(ctx context.Context, e pidbridge.Entry, configPath string) error {
	cfg, err := config.Load(config.LoadOptions{GlobalPath: configPath})
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	database, err := db.Open(ctx, db.Options{Path: cfg.Observer.DBPath, SkipIntegrityCheck: true})
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer database.Close()
	return pidbridge.New(database).Write(ctx, e)
}

// ancestorsFunc returns the list of PIDs to register starting from
// parentPID and walking up the process tree. Injected so tests don't
// need to touch /proc.
type ancestorsFunc func(parentPID int) []int

// defaultAncestors is the production resolver: walks /proc from
// parentPID via PPid, returning each non-shell ancestor up to a cap.
func defaultAncestors(parentPID int) []int {
	return collectClaudeCodeAncestors(parentPID, "/proc", 10)
}

// handleClaudeCodeSessionStart writes pidbridge rows linking every
// non-shell ancestor of the hook process to the session_id in the
// payload, so the proxy can attribute incoming TCP requests even if
// the immediate parent (a short-lived Node worker) has exited by the
// time the API call fires. Always replies approve; DB writes are
// best-effort (spec P1).
//
// We register the whole chain (hook's parent → its parent → ...) up
// to the shell because Claude Code routes hook invocations through
// transient Node workers whose PIDs disappear quickly. Registering
// only os.Getppid() led to unresolvable api_turns.session_id values.
//
// parentPID, ancestors and writer are injected for tests; production
// calls via the dispatcher pass os.Getppid(), defaultAncestors, and
// defaultPidbridgeWriter.
func handleClaudeCodeSessionStart(
	ctx context.Context,
	parentPID int,
	ancestors ancestorsFunc,
	stdin io.Reader,
	stdout, stderr io.Writer,
	label string,
	writer pidbridgeWriter,
) {
	body, _ := io.ReadAll(io.LimitReader(stdin, 2*1024*1024))
	// Reply approve, optionally carrying the advisor digest as
	// additionalContext (plan §15.7 Phase 3; gated by [advisor]
	// session_digest, default OFF). The digest is one point-read of the
	// daemon-refreshed advisor_digest snapshot — the hook never computes
	// (P1), and any failure degrades to a plain approve (P6).
	if digest := advisorSessionDigest(ctx); digest != "" {
		_ = json.NewEncoder(stdout).Encode(map[string]any{
			"decision": "approve",
			"hookSpecificOutput": map[string]any{
				"hookEventName":     "SessionStart",
				"additionalContext": digest,
			},
		})
	} else {
		_ = json.NewEncoder(stdout).Encode(hook.Decision{Decision: "approve"})
	}

	var payload struct {
		SessionID     string `json:"session_id"`
		Cwd           string `json:"cwd"`
		HookEventName string `json:"hook_event_name"`
		Source        string `json:"source"`
	}
	_ = json.Unmarshal(body, &payload)
	if payload.SessionID == "" {
		fmt.Fprintf(stderr, "observer-hook: %s no session_id in payload\n", label)
		return
	}
	if parentPID <= 1 {
		fmt.Fprintf(stderr, "observer-hook: %s refusing to register parent pid %d\n", label, parentPID)
		return
	}

	pids := ancestors(parentPID)
	if len(pids) == 0 {
		fmt.Fprintf(stderr, "observer-hook: %s no ancestor pids collected (parent=%d %s)\n",
			label, parentPID, describePID(parentPID))
		return
	}
	fmt.Fprintf(stderr, "observer-hook: %s registering %d pid(s) for session=%s: %v\n",
		label, len(pids), payload.SessionID, pids)
	for _, pid := range pids {
		entry := pidbridge.Entry{
			PID:       pid,
			SessionID: payload.SessionID,
			Tool:      models.ToolClaudeCode,
			CWD:       payload.Cwd,
		}
		if err := writer(ctx, entry); err != nil {
			fmt.Fprintf(stderr, "observer-hook: %s pidbridge pid=%d: %v\n", label, pid, err)
		}
	}
}

// describePID returns a compact diagnostic tag "comm=<comm> cmdline=<args>"
// for a PID so stderr logs make failed walks easier to triage. Best-effort —
// missing/unreadable proc entries return "(gone)".
func describePID(pid int) string {
	comm, ok := readComm(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
	if !ok {
		return "(gone)"
	}
	cmdline, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	args := strings.ReplaceAll(strings.TrimRight(string(cmdline), "\x00"), "\x00", " ")
	if len(args) > 140 {
		args = args[:137] + "..."
	}
	return fmt.Sprintf("comm=%s cmdline=%q", comm, args)
}

// shellComms is the allowlist of /proc/<pid>/comm values that mark the
// boundary between Claude Code's process tree and the user's
// interactive shell. The walker stops at an interactive shell but
// crosses command-shells (bash -c '...' wrappers), because Claude
// Code invokes hooks via `/bin/bash -c` — if we stopped at any bash
// we'd never reach the long-lived main claude process behind it.
var shellComms = map[string]bool{
	"bash": true, "zsh": true, "sh": true, "fish": true,
	"dash": true, "ash": true, "ksh": true,
}

// initComms marks init-class processes: the boundary between user
// session trees and system space. The walker STOPS at one without
// registering it. This matters for wsl.exe-BRIDGED hooks (Windows AI
// tool + WSL daemon): their WSL-side ancestry is relay "init"
// processes up to the distro init at pid 2, none of which correspond
// to the AI tool — registering them poisons the bridge so that every
// later hookless connection (codex exec, third-party clients)
// ancestor-walks into the stale entry and inherits the wrong
// tool/cwd (D17, found during the A3 soak bring-up).
var initComms = map[string]bool{
	"init": true, "systemd": true,
}

// collectClaudeCodeAncestors walks /proc/<pid>/status:PPid from
// startPID upward, returning each non-shell PID encountered. The walk
// handles two kinds of shells differently:
//
//   - Command-shells (`bash -c '...'` and friends) are SKIPPED but do
//     not terminate the walk. Claude Code wraps hook invocations in
//     `/bin/bash -c` so the hook's immediate parent is almost always a
//     transient command-shell; we need to step past it to find the
//     long-lived main claude process.
//   - Interactive shells (no `-c` in cmdline) TERMINATE the walk.
//     This is the user's session shell and we don't want to attribute
//     future unrelated traffic to it.
//
// Caps at maxDepth to guard against cycles or pathological trees. A
// missing /proc/<pid>/comm entry registers cur as a best-effort floor
// and stops; a missing /proc/<pid>/status terminates further walking.
//
// Typical return for a Claude Code SessionStart hook:
//   - [claude-main]                  (hook spawned via `bash -c` → skipped)
//   - [node-worker, claude-main]     (hook spawned directly)
func collectClaudeCodeAncestors(startPID int, procDir string, maxDepth int) []int {
	if startPID <= 1 || maxDepth <= 0 {
		return nil
	}
	var pids []int
	seen := map[int]bool{}
	cur := startPID
	for i := 0; i < maxDepth && cur > 1; i++ {
		if seen[cur] {
			break
		}
		seen[cur] = true

		comm, commOK := readComm(filepath.Join(procDir, strconv.Itoa(cur), "comm"))
		if cur <= 2 || (commOK && initComms[comm]) {
			// Init-class (pid 1/2 or an init/systemd comm anywhere in
			// the chain — WSL relay processes report comm "init" at
			// arbitrary pids). System space: stop without registering.
			break
		}
		if commOK && shellComms[comm] {
			if !isCommandShell(cur, procDir) {
				// Interactive shell — user's terminal. Stop.
				break
			}
			// Command-shell wrapper (`bash -c ...`). Skip it and keep
			// walking to find what spawned it.
			ppid, err := readPPidFromStatus(filepath.Join(procDir, strconv.Itoa(cur), "status"))
			if err != nil || ppid <= 1 {
				break
			}
			cur = ppid
			continue
		}
		if !commOK {
			// /proc entry vanished between exec and our read. Only
			// best-effort register when this is the starting PID — we
			// need *something* in the bridge to preserve the pre-fix
			// floor behaviour. For a dead mid-walk ancestor there's no
			// PPid link to follow anyway, so we just stop.
			if i == 0 {
				pids = append(pids, cur)
			}
			break
		}
		pids = append(pids, cur)

		ppid, err := readPPidFromStatus(filepath.Join(procDir, strconv.Itoa(cur), "status"))
		if err != nil || ppid <= 1 {
			break
		}
		cur = ppid
	}
	return pids
}

// isCommandShell reports whether pid's /proc/<pid>/cmdline contains
// the `-c` flag. A shell with `-c` is a one-shot command wrapper; a
// shell without `-c` is a user's interactive session.
func isCommandShell(pid int, procDir string) bool {
	b, err := os.ReadFile(filepath.Join(procDir, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	// /proc/<pid>/cmdline is NUL-separated argv. Skip argv[0] (the
	// shell path itself) — `-c` in arg[0] would be pathological.
	parts := strings.Split(string(b), "\x00")
	for i, p := range parts {
		if i == 0 {
			continue
		}
		if p == "-c" {
			return true
		}
	}
	return false
}

// readComm returns the first line of /proc/<pid>/comm, trimmed. The
// bool reports whether the file could be read — a false return
// terminates the ancestor walk.
func readComm(path string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

// readPPidFromStatus parses /proc/<pid>/status for the PPid: line.
// Duplicate of internal/pidbridge.readPPid; kept local so cmd/observer
// doesn't leak an internal helper and the ancestor collector can be
// tested with a fake /proc.
func readPPidFromStatus(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "PPid:") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "PPid:"))
		return strconv.Atoi(rest)
	}
	return 0, errors.New("no PPid line")
}

// preToolPayload is the slice of Claude Code's PreToolUse payload that we
// care about. Unknown fields are ignored.
type preToolPayload struct {
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		Command string `json:"command"`
	} `json:"tool_input"`
	// Cwd is the host shell's working directory. Its path style (a
	// Windows drive-letter / backslash vs a POSIX slash) reveals which OS
	// the shell runs on — used by the cross-OS rewrite guard below.
	Cwd string `json:"cwd"`
}

// claudecodeEffortPayload is the slice of any tool-context Claude Code
// hook (PreToolUse / PostToolUse / Stop / SubagentStop) that we need to
// persist the per-turn effort level. tool_use_id ties the row back to
// the matching tool-use action emitted by the JSONL adapter
// (actions.source_event_id is set to the Anthropic `toolu_xxx` block
// ID by internal/adapter/claudecode/adapter.go::buildToolUseEvent).
type claudecodeEffortPayload struct {
	SessionID     string `json:"session_id"`
	ToolUseID     string `json:"tool_use_id"`
	HookEventName string `json:"hook_event_name"`
	Effort        struct {
		Level string `json:"level"`
	} `json:"effort"`
}

// preToolReply is the approval reply with an optional updatedInput payload.
// Claude Code ≥0.2 honors hookSpecificOutput.updatedInput to mutate the Bash
// command argv before the tool call fires; older versions ignore the block
// and run the tool as originally requested (safe-by-default degradation).
type preToolReply struct {
	Decision           string             `json:"decision"`
	Continue           bool               `json:"continue"`
	HookSpecificOutput *preToolRewriteOut `json:"hookSpecificOutput,omitempty"`
}

type preToolRewriteOut struct {
	HookEventName      string         `json:"hookEventName"`
	PermissionDecision string         `json:"permissionDecision"`
	UpdatedInput       map[string]any `json:"updatedInput,omitempty"`
}

// handleClaudeCodePreTool reads a PreToolUse payload and emits an approval
// reply that optionally rewrites a Bash command to funnel through
// `observer run`. Any failure falls through to plain approval — this hook
// MUST NEVER block the host tool.
func handleClaudeCodePreTool(stdin io.Reader, stdout, stderr io.Writer, label, configPath string) {
	body, _ := io.ReadAll(io.LimitReader(stdin, 2*1024*1024))
	fmt.Fprintf(stderr, "observer-hook: event=%s received bytes=%d at=%s\n",
		label, len(body), time.Now().UTC().Format(time.RFC3339))

	cfg, cfgErr := config.Load(config.LoadOptions{GlobalPath: configPath})

	// Guard seam (guard spec §3.2 seam 1, G4): evaluate BEFORE the
	// rewrite decision. A deny/ask emission replaces the reply
	// entirely (HandleGuarded wrote it); an allow/flag emission falls
	// through to the normal approve/rewrite path with the verdict
	// record deferred until after that reply (§6.4 reply-first).
	// Every guard failure path is fail-open: nil guard → unguarded
	// pre-tool, exactly the pre-G4 behavior.
	var recordAfterReply func()
	if cfgErr == nil && cfg.Guard.Enabled && cfg.Guard.Mode != "off" {
		if g := buildHookGuard(cfg, stderr); g != nil {
			var blocked bool
			blocked, recordAfterReply = hook.HandleGuarded(
				label, body, g, makeGuardPersist(cfg, g, label, stderr), stdout, stderr,
			)
			if blocked {
				// Still capture effort — the turn happened even though
				// the tool call was blocked/deferred.
				recordClaudecodeEffort(body, "PreToolUse", label, configPath, stderr)
				return
			}
		}
	}

	binary, binErr := absoluteBinaryPath()
	rewritten, newCmd, reason := decidePreToolRewrite(body, cfg, cfgErr, binary, binErr, runtime.GOOS)

	reply := preToolReply{Decision: "approve", Continue: true}
	if rewritten {
		reply.HookSpecificOutput = &preToolRewriteOut{
			HookEventName:      "PreToolUse",
			PermissionDecision: "allow",
			UpdatedInput:       map[string]any{"command": newCmd},
		}
		fmt.Fprintf(stderr, "observer-hook: %s rewrote bash command (%s)\n", label, reason)
	} else if reason != "" {
		fmt.Fprintf(stderr, "observer-hook: %s bash passthrough (%s)\n", label, reason)
	}
	_ = json.NewEncoder(stdout).Encode(reply)
	if recordAfterReply != nil {
		recordAfterReply()
	}

	// After replying — capture the per-turn effort.level so the
	// dashboard's per-action Effort column can render it for
	// claude-code rows. The JSONL transcript never carries effort
	// (verified against code.claude.com/docs/en/hooks: effort is only
	// emitted to tool-context hooks), so this is the only per-turn
	// capture surface.
	recordClaudecodeEffort(body, "PreToolUse", label, configPath, stderr)
}

// buildHookGuard constructs the guard composition layer for a hook
// invocation. Hook processes are short-lived: the guard (and its user
// policy parse) rebuilds per invocation — construction is table
// validation + one small file read, comfortably inside the §6.4
// latency budget. KnownProjectRoots is deliberately EMPTY here (it
// would need a DB read before the reply); R-151 cross-project bleed
// is therefore watcher-path-only, documented in BuildClaudeCodeEvent's
// boundary notes alongside the ProjectRoot limitation. Returns nil on
// any failure (fail-open at composition — never break the host tool).
func buildHookGuard(cfg config.Config, stderr io.Writer) *guard.Guard {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	g, err := guard.New(guard.Options{Config: cfg.Guard, Home: home, Notifier: notify.NewDesktop()})
	if err != nil {
		fmt.Fprintf(stderr, "observer-hook: guard construction failed (running unguarded): %v\n", err)
		return nil
	}
	for _, issue := range g.LoadIssues() {
		fmt.Fprintf(stderr, "observer-hook: guard policy issue: %s\n", issue)
	}
	// §6.3 approvals on the hook path: the lookup opens the DB
	// LAZILY and only runs for verdicts that would block (ask/deny —
	// rare by construction), so the common approve path never pays a
	// DB open before its reply. Open failures report false:
	// fail-safe toward enforcement.
	g.SetApprovalLookup(func(ruleID, sessionID, rootHash string) bool {
		database, err := db.Open(context.Background(), db.Options{Path: cfg.Observer.DBPath, SkipIntegrityCheck: true})
		if err != nil {
			return false
		}
		defer database.Close()
		lctx, cancel := context.WithTimeout(context.Background(), cfg.Observer.Hooks.HookTimeout())
		defer cancel()
		return store.New(database).ApprovalActiveFor(lctx, ruleID, sessionID, rootHash, time.Now().UTC())
	})
	return g
}

// makeGuardPersist returns the lazy persist callback HandleGuarded
// invokes AFTER the reply is on stdout: open the daemon DB
// (SkipIntegrityCheck — the daemon already probes at startup), write
// one guard_events row through the one-owner store helper, close,
// then fire the [guard.alerts] desktop notification if the verdict
// meets the threshold (also post-reply, so a slow notification
// helper can't delay the host tool). Best-effort end to end; the
// forensics JSONL already has the verdict if this fails.
func makeGuardPersist(cfg config.Config, g *guard.Guard, label string, stderr io.Writer) func(guard.ActionVerdict) {
	return func(v guard.ActionVerdict) {
		database, err := db.Open(context.Background(), db.Options{Path: cfg.Observer.DBPath, SkipIntegrityCheck: true})
		if err != nil {
			fmt.Fprintf(stderr, "observer-hook: %s guard persist db: %v\n", label, err)
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), cfg.Observer.Hooks.HookTimeout())
			if _, perr := store.New(database).PersistGuardVerdicts(ctx, []guard.ActionVerdict{v}); perr != nil {
				fmt.Fprintf(stderr, "observer-hook: %s guard persist: %v\n", label, perr)
			}
			cancel()
			database.Close()
		}
		g.MaybeAlert(v)
	}
}

// handleClaudeCodePostTool captures effort.level + tool_use_id from a
// PostToolUse payload into the claudecode_effort sidecar table. Replies
// approve fast and never blocks the host (spec P1).
//
// Unlike PreToolUse this handler has no rewrite responsibility; its
// sole job is the effort capture. PostToolUse fires after the tool
// finishes; on rare hook-before-JSONL orderings this gives us a second
// chance to capture effort even if the PreToolUse fire was lost.
func handleClaudeCodePostTool(stdin io.Reader, stdout, stderr io.Writer, label, configPath string) {
	body, _ := io.ReadAll(io.LimitReader(stdin, 2*1024*1024))
	body = bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF})
	_ = json.NewEncoder(stdout).Encode(hook.Decision{Decision: "approve"})

	recordClaudecodeEffort(body, "PostToolUse", label, configPath, stderr)
}

// recordClaudecodeEffort parses (session_id, tool_use_id, effort.level)
// from a tool-context Claude Code hook payload and upserts it into the
// claudecode_effort sidecar. Two no-op cases:
//
//   - effort.level is empty (the current model doesn't support effort,
//     per the docs: "Present ... when the current model supports the
//     effort parameter"). Nothing to persist.
//   - tool_use_id is empty (malformed payload, or a future event class
//     that lost the field). Nothing to key on.
//
// Errors log to stderr; never propagate.
func recordClaudecodeEffort(body []byte, eventName, label, configPath string, stderr io.Writer) {
	var p claudecodeEffortPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return
	}
	if p.Effort.Level == "" || p.SessionID == "" || p.ToolUseID == "" {
		return
	}
	cfg, err := config.Load(config.LoadOptions{GlobalPath: configPath})
	if err != nil {
		fmt.Fprintf(stderr, "observer-hook: %s effort config: %v\n", label, err)
		return
	}
	// db.Open runs a quick_check integrity probe that competes with the
	// running daemon's WAL holder. Don't fence it with HookTimeout; that
	// budget is for the write side. Mirrors handleCursorHook /
	// handleCodexHook which both pass an unbounded context to db.Open.
	//
	// Cross-OS note: when Claude Code runs in a different OS-context than
	// the daemon (the Windows CC + WSL daemon straddle), the hook is
	// registered as a wsl.exe bridge (see registerClaudeCodeWindows) so it
	// executes INSIDE the daemon's context — this db.Open therefore resolves
	// the daemon's own DB natively. No cross-OS SQLite open is attempted.
	database, err := db.Open(context.Background(), db.Options{Path: cfg.Observer.DBPath, SkipIntegrityCheck: true})
	if err != nil {
		fmt.Fprintf(stderr, "observer-hook: %s effort db: %v\n", label, err)
		return
	}
	defer database.Close()
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Observer.Hooks.HookTimeout())
	defer cancel()
	if err := store.New(database).UpsertClaudecodeEffort(ctx, p.SessionID, p.ToolUseID, p.Effort.Level, eventName); err != nil {
		fmt.Fprintf(stderr, "observer-hook: %s effort upsert: %v\n", label, err)
	}
}

// decidePreToolRewrite is the pure decision function used by
// handleClaudeCodePreTool. Reason "" signals "not a Bash call, nothing to
// consider"; any other reason is a short tag for diagnostic logging.
func decidePreToolRewrite(body []byte, cfg config.Config, cfgErr error, binary string, binErr error, hostOS string) (rewrite bool, newCommand, reason string) {
	var p preToolPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return false, "", ""
	}
	if p.ToolName != "Bash" || p.ToolInput.Command == "" {
		return false, "", ""
	}
	if cfgErr != nil {
		return false, "", "config-error"
	}
	if !cfg.Compression.Shell.Enabled {
		return false, "", "shell-disabled"
	}
	// Cross-OS shell guard (the Windows-CC + WSL-daemon straddle). When
	// Claude Code runs on Windows but the daemon runs in WSL, the hook is
	// registered as a wsl.exe bridge (registerClaudeCodeWindows), so it
	// executes on Linux while the rewritten command runs back in the
	// Windows shell (Git Bash / MSYS). RewriteBash embeds THIS process's
	// own observer path — a `/mnt/...` WSL path that shell can't exec — so
	// the rewritten command would exit 127. The payload cwd reveals the
	// host shell's OS: a Windows-style cwd (drive-letter / backslash) seen
	// by a non-Windows hook binary means the shell is on the other side of
	// the OS boundary. Skip the rewrite — shell-output capture is simply
	// unavailable for that topology. Effort/action capture is unaffected
	// (those hooks don't rewrite the command).
	if hostOS != "windows" && looksWindowsPath(p.Cwd) {
		return false, "", "cross-os-shell"
	}
	if binErr != nil || binary == "" {
		return false, "", "binary-lookup-error"
	}
	newCmd, changed := hook.RewriteBash(binary, p.ToolInput.Command, cfg.Compression.Shell.ExcludeCommands)
	if !changed {
		return false, "", "not-rewritable"
	}
	return true, newCmd, "ok"
}

// looksWindowsPath reports whether p is a Windows-style absolute path — a
// drive-letter prefix (`C:\` or `C:/`) or any backslash separator. Used to
// infer that the host shell which produced the hook payload runs on
// Windows, so a non-Windows (bridged) hook process must not rewrite the
// command with its own filesystem-local observer path.
func looksWindowsPath(p string) bool {
	if len(p) >= 2 && p[1] == ':' &&
		((p[0] >= 'A' && p[0] <= 'Z') || (p[0] >= 'a' && p[0] <= 'z')) {
		return true
	}
	return strings.Contains(p, `\`)
}

// handleCursorHook opens the observer DB and dispatches the cursor hook.
// Replies on stdout immediately (handled inside HandleCursorEvent) so the
// host doesn't wait on the DB insert.
func handleCursorHook(ctx context.Context, event, configPath string) {
	if event == "" {
		event = "unknown"
	}
	cfg, err := config.Load(config.LoadOptions{GlobalPath: configPath})
	if err != nil {
		// Fall back to approve-only — never block the host.
		fmt.Fprintf(os.Stderr, "observer-hook: cursor config: %v\n", err)
		hook.HandleApprove("cursor:"+event, os.Stdin, os.Stdout, os.Stderr)
		return
	}
	database, err := db.Open(ctx, db.Options{Path: cfg.Observer.DBPath, SkipIntegrityCheck: true})
	if err != nil {
		fmt.Fprintf(os.Stderr, "observer-hook: cursor db: %v\n", err)
		hook.HandleApprove("cursor:"+event, os.Stdin, os.Stdout, os.Stderr)
		return
	}
	defer database.Close()

	sc := scrub.New()
	// v1.6.23 audit F3/F4: wire the FTS5 indexer so postToolUse
	// tool_output bodies and beforeReadFile file content land in
	// action_excerpts the same way the daemon's watcher-attached
	// indexer would. Use the configured cap (or DefaultMaxExcerptBytes
	// when zero) — matches main.go's daemon Indexer setup.
	idx := indexing.New(database, cfg.Compression.Indexing.MaxExcerptBytes)

	// Guard seam (G6): cursor's before-events carry documented
	// permission JSON, so the guarded receiver can deny/ask. Nil
	// guard (disabled, mode off, construction failure) degrades to
	// the unguarded receiver — exactly the pre-G6 behavior. The
	// persist callback reuses THIS process's already-open DB handle
	// (unlike claude-code's lazy open, the cursor hook holds one for
	// capture anyway).
	var g *guard.Guard
	if cfg.Guard.Enabled && cfg.Guard.Mode != "off" {
		g = buildHookGuard(cfg, os.Stderr)
	}
	if g != nil {
		st := store.New(database).WithIndexer(idx)
		persist := func(v guard.ActionVerdict) {
			pctx, cancel := context.WithTimeout(context.Background(), cfg.Observer.Hooks.HookTimeout())
			defer cancel()
			if _, err := st.PersistGuardVerdicts(pctx, []guard.ActionVerdict{v}); err != nil {
				fmt.Fprintf(os.Stderr, "observer-hook: cursor guard persist: %v\n", err)
			}
			g.MaybeAlert(v)
		}
		hook.HandleCursorEventGuarded(event, g, persist, st, sc, os.Stdin, os.Stdout, os.Stderr, cfg.Observer.Hooks.HookTimeout())
		return
	}
	hook.HandleCursorEvent(event, store.New(database).WithIndexer(idx), sc, os.Stdin, os.Stdout, os.Stderr, cfg.Observer.Hooks.HookTimeout())
}

// handleCodexHook opens the observer DB and dispatches a Codex hook.
// Replies on stdout immediately (inside HandleCodexEvent) so the host
// doesn't wait on the DB insert. Codex's hook event names are
// CamelCase identical to Claude Code's; we forward them as-is to the
// adapter (no kebab-case translation as observer's CLI does for
// claude-code).
func handleCodexHook(ctx context.Context, event, configPath string) {
	if event == "" {
		event = "unknown"
	}
	cfg, err := config.Load(config.LoadOptions{GlobalPath: configPath})
	if err != nil {
		fmt.Fprintf(os.Stderr, "observer-hook: codex config: %v\n", err)
		// Bare ack via empty JSON object — codex hooks accept this as
		// "no action" across all event classes.
		_ = json.NewEncoder(os.Stdout).Encode(struct{}{})
		return
	}
	database, err := db.Open(ctx, db.Options{Path: cfg.Observer.DBPath, SkipIntegrityCheck: true})
	if err != nil {
		fmt.Fprintf(os.Stderr, "observer-hook: codex db: %v\n", err)
		_ = json.NewEncoder(os.Stdout).Encode(struct{}{})
		return
	}
	defer database.Close()

	sc := scrub.New()
	hook.HandleCodexEvent(event, store.New(database), sc, os.Stdin, os.Stdout, os.Stderr, cfg.Observer.Hooks.HookTimeout())
}

// handleHermesHook dispatches a Hermes Agent hook event delivered by
// the Python plugin bridge at ~/.hermes/plugins/superbased-observer/.
//
// Wire shape per docs/hermes-adapter-plan.md §11.1 + §17.1.F:
// structured JSON on stdin carrying event=tool_call|session_start|
// session_end|api_request|subagent_stop plus tool-specific fields
// (tool_name, args, result, cwd, model, usage, …). Per
// hermes_cli/plugins.py the bridge's subprocess.run wrapper has a
// 0.5 s timeout and swallows exceptions — observer being down or
// slow MUST never propagate into the host. We reply approve on
// stdout immediately and do all DB work after.
//
// Both tool-event (ActionFoo rows) and token-event (post_api_request
// usage) paths flow through here; the per-event dispatch lives in
// internal/adapter/hermes/hook.go::BuildToolEvent / BuildTokenEvent
// so the parser is shared with the SQLite backfill path.
func handleHermesHook(ctx context.Context, event, configPath string) {
	label := "hermes"
	if event != "" {
		label = "hermes:" + event
	}

	body, _ := io.ReadAll(io.LimitReader(os.Stdin, 2*1024*1024))
	body = bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF})
	// Reply approve immediately — host must never wait on the DB
	// path. Bridge accepts "approve" identically to the Claude Code
	// shape; on parse failure it falls back to no-op.
	_ = json.NewEncoder(os.Stdout).Encode(hook.Decision{Decision: "approve"})

	cfg, err := config.Load(config.LoadOptions{GlobalPath: configPath})
	if err != nil {
		fmt.Fprintf(os.Stderr, "observer-hook: %s config: %v\n", label, err)
		return
	}
	database, err := db.Open(ctx, db.Options{Path: cfg.Observer.DBPath, SkipIntegrityCheck: true})
	if err != nil {
		fmt.Fprintf(os.Stderr, "observer-hook: %s db: %v\n", label, err)
		return
	}
	defer database.Close()

	sc := scrub.New()

	var toolEvents []models.ToolEvent
	var tokenEvents []models.TokenEvent

	if ev, ok, buildErr := hermes.BuildToolEvent(event, body, sc); buildErr != nil {
		fmt.Fprintf(os.Stderr, "observer-hook: %s build tool event: %v\n", label, buildErr)
	} else if ok {
		toolEvents = append(toolEvents, ev)
	}

	if tok, ok, buildErr := hermes.BuildTokenEvent(event, body); buildErr != nil {
		fmt.Fprintf(os.Stderr, "observer-hook: %s build token event: %v\n", label, buildErr)
	} else if ok {
		tokenEvents = append(tokenEvents, tok)
	}

	if len(toolEvents) == 0 && len(tokenEvents) == 0 {
		return
	}

	insertCtx, cancel := context.WithTimeout(ctx, cfg.Observer.Hooks.HookTimeout())
	defer cancel()
	if _, err := store.New(database).Ingest(insertCtx, toolEvents, tokenEvents, store.IngestOptions{}); err != nil {
		fmt.Fprintf(os.Stderr, "observer-hook: %s insert: %v\n", label, err)
	}
}

// claudeActionBuilder takes a raw Claude Code hook payload and returns a
// ToolEvent ready for insertion. ok=false means "skip this fire" (e.g.
// missing required field). Builders never block — they parse, validate,
// and return.
type claudeActionBuilder func(body []byte) (models.ToolEvent, bool)

// handleClaudeCodeActionEvent is the shared dispatch for Tier 1 hook
// events that ingest one row each. Reads stdin, replies approve, opens
// the configured DB, calls build(body), and inserts the resulting
// ToolEvent. All errors log to stderr and never block the host (spec P1).
func handleClaudeCodeActionEvent(ctx context.Context, label, configPath string, build claudeActionBuilder) {
	body, _ := io.ReadAll(io.LimitReader(os.Stdin, 2*1024*1024))
	body = bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF})
	_ = json.NewEncoder(os.Stdout).Encode(hook.Decision{Decision: "approve"})

	ev, ok := build(body)
	if !ok {
		return
	}
	if ev.SessionID == "" {
		fmt.Fprintf(os.Stderr, "observer-hook: %s no session_id in payload\n", label)
		return
	}

	cfg, err := config.Load(config.LoadOptions{GlobalPath: configPath})
	if err != nil {
		fmt.Fprintf(os.Stderr, "observer-hook: %s config: %v\n", label, err)
		return
	}
	database, err := db.Open(ctx, db.Options{Path: cfg.Observer.DBPath, SkipIntegrityCheck: true})
	if err != nil {
		fmt.Fprintf(os.Stderr, "observer-hook: %s db: %v\n", label, err)
		return
	}
	defer database.Close()

	insertCtx, cancel := context.WithTimeout(ctx, cfg.Observer.Hooks.HookTimeout())
	defer cancel()
	if _, err := store.New(database).Ingest(insertCtx, []models.ToolEvent{ev}, nil, store.IngestOptions{}); err != nil {
		fmt.Fprintf(os.Stderr, "observer-hook: %s insert: %v\n", label, err)
	}
}

// claudeBaseEnvelope captures the universal envelope every Claude Code
// hook payload carries (per docs https://code.claude.com/docs/en/hooks).
// Builders embed this and add event-specific fields. The PermissionMode
// + Effort fields land on every fire (verified via v1.4.45 capture); we
// surface them through ActionMetadata so dashboards can filter by mode.
type claudeBaseEnvelope struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	PermissionMode string `json:"permission_mode"`
	Effort         struct {
		Level string `json:"level"`
	} `json:"effort"`
}

// envelopeMetadata builds an ActionMetadata from the envelope's
// universal fields. Returns nil when no fields apply so callers can
// pass it directly to ev.Metadata without an extra IsZero check.
func envelopeMetadata(env claudeBaseEnvelope) *models.ActionMetadata {
	m := models.ActionMetadata{
		PermissionMode: env.PermissionMode,
		EffortLevel:    env.Effort.Level,
	}
	if m.IsZero() {
		return nil
	}
	return &m
}

// baseToolEvent fills the fields common to every claude-code hook row.
// Sets SourceFile = "claude-code:hook" and routes ProjectRoot from cwd.
// Populates Metadata from the envelope's permission_mode + effort.level
// so per-event builders inherit those fields automatically.
func baseToolEvent(env claudeBaseEnvelope, action, eventName string) models.ToolEvent {
	return models.ToolEvent{
		SourceFile:    "claude-code:hook",
		SourceEventID: env.SessionID + ":" + eventName,
		SessionID:     env.SessionID,
		ProjectRoot:   env.Cwd,
		Timestamp:     time.Now().UTC(),
		Tool:          models.ToolClaudeCode,
		ActionType:    action,
		Success:       true,
		Metadata:      envelopeMetadata(env),
	}
}

func buildClaudeSessionEndEvent(body []byte) (models.ToolEvent, bool) {
	var p struct{ claudeBaseEnvelope }
	if err := json.Unmarshal(body, &p); err != nil {
		return models.ToolEvent{}, false
	}
	if p.SessionID == "" {
		return models.ToolEvent{}, false
	}
	ev := baseToolEvent(p.claudeBaseEnvelope, models.ActionSessionEnd, "session_end")
	ev.Target = "session_ended"
	return ev, true
}

func buildClaudeUserPromptSubmitEvent(body []byte) (models.ToolEvent, bool) {
	var p struct {
		claudeBaseEnvelope
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return models.ToolEvent{}, false
	}
	if p.SessionID == "" {
		return models.ToolEvent{}, false
	}
	ev := baseToolEvent(p.claudeBaseEnvelope, models.ActionUserPrompt, "user_prompt_submit")
	text := scrub.New().String(p.Prompt)
	ev.RawToolInput = text
	ev.Target = previewLine(text, 120)
	return ev, true
}

func buildClaudePostToolFailureEvent(body []byte) (models.ToolEvent, bool) {
	var p struct {
		claudeBaseEnvelope
		ToolName    string          `json:"tool_name"`
		ToolInput   json.RawMessage `json:"tool_input"`
		ToolUseID   string          `json:"tool_use_id"`
		Error       string          `json:"error"`
		IsInterrupt bool            `json:"is_interrupt"`
		DurationMs  int64           `json:"duration_ms"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return models.ToolEvent{}, false
	}
	if p.SessionID == "" {
		return models.ToolEvent{}, false
	}
	ev := baseToolEvent(p.claudeBaseEnvelope, models.ActionToolFailure, "post_tool_failure")
	ev.SourceEventID = p.ToolUseID + ":post_tool_failure"
	ev.Target = p.ToolName
	ev.RawToolName = p.ToolName
	if len(p.ToolInput) > 0 {
		ev.RawToolInput = scrub.New().String(string(p.ToolInput))
	}
	ev.ErrorMessage = p.Error
	ev.DurationMs = p.DurationMs
	ev.Success = false
	if p.IsInterrupt {
		// User-cancelled vs genuine failure surfaces on
		// metadata.is_interrupt (migration 017). Pre-fix this was
		// lossy-encoded as a "[interrupt] " ErrorMessage prefix that
		// dashboards had to string-match on.
		if ev.Metadata == nil {
			ev.Metadata = &models.ActionMetadata{}
		}
		ev.Metadata.IsInterrupt = true
	}
	return ev, true
}

func buildClaudeStopFailureEvent(body []byte) (models.ToolEvent, bool) {
	var p struct {
		claudeBaseEnvelope
		ErrorType            string `json:"error_type"`
		ErrorMessage         string `json:"error_message"`
		Error                string `json:"error"`
		LastAssistantMessage string `json:"last_assistant_message"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return models.ToolEvent{}, false
	}
	if p.SessionID == "" {
		return models.ToolEvent{}, false
	}
	// StopFailure carries the typed error class via either error_type
	// (per docs) or error (observed in real captures); prefer the more
	// specific one.
	cls := p.ErrorType
	if cls == "" {
		cls = p.Error
	}
	msg := p.ErrorMessage
	if msg == "" {
		msg = p.LastAssistantMessage
	}
	ev := baseToolEvent(p.claudeBaseEnvelope, models.ActionAPIError, "stop_failure")
	ev.Target = cls
	ev.RawToolName = cls
	ev.ErrorMessage = msg
	ev.Success = false
	return ev, true
}

func buildClaudeSubagentStartEvent(body []byte) (models.ToolEvent, bool) {
	var p struct {
		claudeBaseEnvelope
		AgentID   string `json:"agent_id"`
		AgentType string `json:"agent_type"`
		Prompt    string `json:"prompt"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return models.ToolEvent{}, false
	}
	if p.SessionID == "" {
		return models.ToolEvent{}, false
	}
	ev := baseToolEvent(p.claudeBaseEnvelope, models.ActionSubagentStart, "subagent_start")
	if p.AgentID != "" {
		ev.SourceEventID = p.AgentID + ":subagent_start"
	}
	ev.Target = p.AgentType
	ev.RawToolName = p.AgentID
	if p.Prompt != "" {
		ev.RawToolInput = scrub.New().String(p.Prompt)
	}
	ev.IsSidechain = true
	return ev, true
}

func buildClaudeSubagentStopEvent(body []byte) (models.ToolEvent, bool) {
	var p struct {
		claudeBaseEnvelope
		AgentID              string `json:"agent_id"`
		AgentType            string `json:"agent_type"`
		AgentTranscriptPath  string `json:"agent_transcript_path"`
		LastAssistantMessage string `json:"last_assistant_message"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return models.ToolEvent{}, false
	}
	if p.SessionID == "" {
		return models.ToolEvent{}, false
	}
	// Empty-shell suppression: claude-code fires SubagentStop with
	// only agent_id + envelope sometimes (lifecycle marker without
	// payload detail). Verified live: 11 of 12 historical rows on
	// the user's DB had empty target / raw_tool_input — they
	// rendered as blank rows in the dashboard. Suppress when there's
	// nothing the dashboard can show; the loss is just the
	// lifecycle marker, which the JSONL watcher's sidechain capture
	// already covers.
	if p.AgentType == "" && p.LastAssistantMessage == "" && p.AgentTranscriptPath == "" {
		return models.ToolEvent{}, false
	}
	ev := baseToolEvent(p.claudeBaseEnvelope, models.ActionSubagentStop, "subagent_stop")
	if p.AgentID != "" {
		ev.SourceEventID = p.AgentID + ":subagent_stop"
	}
	// Target shows in dashboard listings. Prefer the categorical
	// agent_type ("Explore", "general-purpose", …); fall back to a
	// short preview of the assistant's final message so the row
	// always carries SOMETHING the user can see at a glance.
	switch {
	case p.AgentType != "":
		ev.Target = p.AgentType
	case p.LastAssistantMessage != "":
		ev.Target = previewLine(p.LastAssistantMessage, 120)
	default:
		// Has agent_transcript_path only — surface the basename so
		// the row points at where the data lives.
		ev.Target = filepath.Base(p.AgentTranscriptPath)
	}
	ev.RawToolName = p.AgentID
	if p.LastAssistantMessage != "" {
		// Land the assistant's final message in BOTH
		// raw_tool_input (the dashboard-rendered body) AND
		// ToolOutput (the FTS5 index). Pre-fix the message only
		// went to FTS5, so the dashboard space rendered blank.
		scrubbed := scrub.New().String(p.LastAssistantMessage)
		ev.RawToolInput = scrubbed
		ev.ToolOutput = scrubbed
	}
	ev.IsSidechain = true
	return ev, true
}

// buildClaudeStopEvent handles Claude Code's `Stop` hook, which fires when
// a top-level turn ends (mirroring SubagentStop's role for subagent turns).
// Pre-v1.4.49 the dispatch fell through to the generic approve-reply path,
// so the assistant's final-message text was never landed in a row. This
// builder emits a `claudecode.assistant_text` row carrying the assistant's
// closing utterance, keeping it consistent with the cross-adapter
// convention (Codex/Cline/Roo/Antigravity all emit assistant-text rows
// with ActionTaskComplete + `<source>.assistant_text` RawToolName).
//
// Empty-message suppression: Stop fires on every turn-end, including
// interruptions where there's no model output to record. Returning (zero,
// false) for empty LastAssistantMessage keeps the table free of marker-
// only rows. The lifecycle marker is still observable via the JSONL
// adapter's per-turn task_complete row.
//
// SourceEventID embeds a content hash of the envelope body so replayed
// captures dedupe via the (source_file, source_event_id) UPSERT path —
// observer's hooks share SourceFile="claude-code:hook" across events, so
// the session-id + event + content-hash combo is the dedup key.
//
// No token/cost fields are set even if the envelope grows usage data in
// future Claude Code versions — pricing is attributed via the API proxy
// path, not via the JSONL/hook assistant-text rows.
func buildClaudeStopEvent(body []byte) (models.ToolEvent, bool) {
	var p struct {
		claudeBaseEnvelope
		StopHookActive       bool   `json:"stop_hook_active"`
		LastAssistantMessage string `json:"last_assistant_message"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return models.ToolEvent{}, false
	}
	if p.SessionID == "" {
		return models.ToolEvent{}, false
	}
	if strings.TrimSpace(p.LastAssistantMessage) == "" {
		return models.ToolEvent{}, false
	}
	ev := baseToolEvent(p.claudeBaseEnvelope, models.ActionTaskComplete, "stop")
	ev.SourceEventID = p.SessionID + ":stop:" + claudeContentHash(body)
	preview := previewLine(p.LastAssistantMessage, 200)
	ev.Target = preview
	ev.PrecedingReasoning = preview
	ev.RawToolName = "claudecode.assistant_text"
	scrubbed := scrub.New().String(p.LastAssistantMessage)
	ev.ToolOutput = contentcap.Cap(scrubbed, contentcap.DefaultMaxBytes)
	return ev, true
}

func buildClaudeNotificationEvent(body []byte) (models.ToolEvent, bool) {
	var p struct {
		claudeBaseEnvelope
		NotificationType string `json:"notification_type"`
		Message          string `json:"message"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return models.ToolEvent{}, false
	}
	if p.SessionID == "" {
		return models.ToolEvent{}, false
	}
	ev := baseToolEvent(p.claudeBaseEnvelope, models.ActionNotification, "notification")
	ev.Target = p.NotificationType
	ev.ErrorMessage = p.Message
	return ev, true
}

func buildClaudeCwdChangedEvent(body []byte) (models.ToolEvent, bool) {
	// Captured payload uses old_cwd / new_cwd (not previous_cwd as the
	// docs claim). Accept both for forward-compat with future Claude
	// Code releases.
	var p struct {
		claudeBaseEnvelope
		OldCwd      string `json:"old_cwd"`
		NewCwd      string `json:"new_cwd"`
		PreviousCwd string `json:"previous_cwd"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return models.ToolEvent{}, false
	}
	if p.SessionID == "" {
		return models.ToolEvent{}, false
	}
	previous := p.OldCwd
	if previous == "" {
		previous = p.PreviousCwd
	}
	ev := baseToolEvent(p.claudeBaseEnvelope, models.ActionCwdChange, "cwd_changed")
	ev.Target = p.NewCwd
	if ev.Target == "" {
		ev.Target = p.Cwd
	}
	ev.PrecedingReasoning = previous
	return ev, true
}

// claudeContentHash returns a short stable hex hash of body suitable
// for disambiguating SourceEventIDs when multiple Tier 2/3 hook fires
// per session share an event name (UserPromptExpansion, PostToolBatch,
// PermissionRequest). 8 hex chars from fnv-32a; collision probability
// over a single session is negligible. Deterministic across re-ingest
// so replayed captures dedupe via ON CONFLICT instead of duplicating.
func claudeContentHash(body []byte) string {
	h := fnv.New32a()
	_, _ = h.Write(body)
	return strconv.FormatUint(uint64(h.Sum32()), 16)
}

// buildClaudeSetupEvent handles Claude Code's `Setup` event — fires only
// on `--init-only`, `-p --init`, or `-p --maintenance` runs (not every
// session launch). Payload shape verified against
// docs.claude.com/docs/en/hooks: top-level `trigger` field carries
// "init" | "maintenance".
func buildClaudeSetupEvent(body []byte) (models.ToolEvent, bool) {
	var p struct {
		claudeBaseEnvelope
		Trigger string `json:"trigger"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return models.ToolEvent{}, false
	}
	if p.SessionID == "" {
		return models.ToolEvent{}, false
	}
	ev := baseToolEvent(p.claudeBaseEnvelope, models.ActionSetup, "setup")
	ev.Target = p.Trigger
	if p.Trigger != "" {
		// Trigger discriminates `init` vs `maintenance` if both fire
		// in the same session (rare — they're mutually-exclusive
		// launch modes — but defensive).
		ev.SourceEventID = p.SessionID + ":setup:" + p.Trigger
	}
	return ev, true
}

// buildClaudeUserPromptExpansionEvent handles `UserPromptExpansion` —
// fires AFTER UserPromptSubmit when the input matched a registered
// slash-command or mcp-prompt name. Distinct from UserPromptSubmit's
// free-text capture: this row records the resolved command_name +
// expansion_type, so dashboards can chart slash-command usage. Fields
// verified per docs.
func buildClaudeUserPromptExpansionEvent(body []byte) (models.ToolEvent, bool) {
	var p struct {
		claudeBaseEnvelope
		ExpansionType string `json:"expansion_type"`
		CommandName   string `json:"command_name"`
		CommandArgs   string `json:"command_args"`
		CommandSource string `json:"command_source"`
		Prompt        string `json:"prompt"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return models.ToolEvent{}, false
	}
	if p.SessionID == "" {
		return models.ToolEvent{}, false
	}
	ev := baseToolEvent(p.claudeBaseEnvelope, models.ActionUserPromptExpansion, "user_prompt_expansion")
	ev.SourceEventID = p.SessionID + ":user_prompt_expansion:" + claudeContentHash(body)
	ev.Target = p.CommandName
	ev.RawToolName = p.ExpansionType
	if p.Prompt != "" {
		ev.RawToolInput = scrub.New().String(p.Prompt)
	}
	// command_source + command_args land on PrecedingReasoning as a
	// small JSON blob so analysts can filter on plugin vs user vs
	// project skill provenance without re-scanning the raw prompt.
	if p.CommandSource != "" || p.CommandArgs != "" {
		meta := map[string]string{}
		if p.CommandSource != "" {
			meta["command_source"] = p.CommandSource
		}
		if p.CommandArgs != "" {
			meta["command_args"] = scrub.New().String(p.CommandArgs)
		}
		if b, err := json.Marshal(meta); err == nil {
			ev.PrecedingReasoning = string(b)
		}
	}
	return ev, true
}

// buildClaudePostToolBatchEvent handles `PostToolBatch` — the end-of-
// batch summary after a run of consecutive tool calls. Carries the
// list of tool_calls (their names + serialized tool_responses) so
// analysts can inspect batch composition without joining per-call
// rows. tool_response in this event is the serialized text the model
// SEES (not the structured Output object PostToolUse carries) — for
// Read that means line-number-prefixed text.
func buildClaudePostToolBatchEvent(body []byte) (models.ToolEvent, bool) {
	var p struct {
		claudeBaseEnvelope
		ToolCalls []struct {
			ToolName     string          `json:"tool_name"`
			ToolUseID    string          `json:"tool_use_id"`
			ToolInput    json.RawMessage `json:"tool_input"`
			ToolResponse json.RawMessage `json:"tool_response"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return models.ToolEvent{}, false
	}
	if p.SessionID == "" {
		return models.ToolEvent{}, false
	}
	if len(p.ToolCalls) == 0 {
		// Empty batch is a documented no-op — skip rather than
		// emit a placeholder. Symmetric to the SubagentStop empty-
		// shell suppression in v1.4.47.
		return models.ToolEvent{}, false
	}
	ev := baseToolEvent(p.claudeBaseEnvelope, models.ActionPostToolBatch, "post_tool_batch")
	ev.SourceEventID = p.SessionID + ":post_tool_batch:" + claudeContentHash(body)
	ev.Target = fmt.Sprintf("%d tool call(s)", len(p.ToolCalls))
	ev.RawToolName = p.ToolCalls[0].ToolName
	// Full tool_calls array lands in RawToolInput as a scrubbed JSON
	// blob. Dashboards render it as the row's "what was in the batch"
	// detail; analysts can json_extract individual entries for
	// per-tool drilldown.
	if raw, err := json.Marshal(p.ToolCalls); err == nil {
		ev.RawToolInput = scrub.New().String(string(raw))
	}
	return ev, true
}

// buildClaudePermissionRequestEvent handles `PermissionRequest` — fires
// when the host asks the user (or auto-mode classifier) to authorize
// a tool call. Distinct from PermissionDenied: the request itself is
// just the prompt; the outcome lands as either continued tool
// execution (PostToolUse) or an ActionPermissionDenied row. Per docs
// PermissionRequest has NO tool_use_id, so we hash the body for
// SourceEventID uniqueness.
func buildClaudePermissionRequestEvent(body []byte) (models.ToolEvent, bool) {
	var p struct {
		claudeBaseEnvelope
		ToolName              string          `json:"tool_name"`
		ToolInput             json.RawMessage `json:"tool_input"`
		PermissionSuggestions json.RawMessage `json:"permission_suggestions"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return models.ToolEvent{}, false
	}
	if p.SessionID == "" {
		return models.ToolEvent{}, false
	}
	ev := baseToolEvent(p.claudeBaseEnvelope, models.ActionPermissionRequest, "permission_request")
	ev.SourceEventID = p.SessionID + ":permission_request:" + claudeContentHash(body)
	ev.Target = p.ToolName
	ev.RawToolName = p.ToolName
	if len(p.ToolInput) > 0 {
		ev.RawToolInput = scrub.New().String(string(p.ToolInput))
	}
	// permission_suggestions (addRules / setMode / etc.) lands on
	// PrecedingReasoning so analysts can see WHAT the host proposed
	// as the resolution path (e.g. "add this rule to localSettings").
	if len(p.PermissionSuggestions) > 0 {
		ev.PrecedingReasoning = string(p.PermissionSuggestions)
	}
	return ev, true
}

// buildClaudePermissionDeniedEvent handles `PermissionDenied` — fires
// only in auto-mode classifier denials. Distinct from ActionToolFailure:
// ToolFailure is the tool itself failing; PermissionDenied is the
// permission layer refusing to dispatch the tool in the first place.
// Has tool_use_id (unlike PermissionRequest) so we key on it
// symmetrically with PostToolUseFailure.
func buildClaudePermissionDeniedEvent(body []byte) (models.ToolEvent, bool) {
	var p struct {
		claudeBaseEnvelope
		ToolName  string          `json:"tool_name"`
		ToolInput json.RawMessage `json:"tool_input"`
		ToolUseID string          `json:"tool_use_id"`
		Reason    string          `json:"reason"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return models.ToolEvent{}, false
	}
	if p.SessionID == "" {
		return models.ToolEvent{}, false
	}
	ev := baseToolEvent(p.claudeBaseEnvelope, models.ActionPermissionDenied, "permission_denied")
	if p.ToolUseID != "" {
		ev.SourceEventID = p.ToolUseID + ":permission_denied"
	}
	ev.Target = p.ToolName
	ev.RawToolName = p.ToolName
	if len(p.ToolInput) > 0 {
		ev.RawToolInput = scrub.New().String(string(p.ToolInput))
	}
	ev.ErrorMessage = p.Reason
	ev.Success = false
	return ev, true
}

// buildClaudeInstructionsLoadedEvent handles `InstructionsLoaded` —
// fires when a CLAUDE.md / instructions file lands in context. Carries
// which file, what memory_type ("User" | "Project" | "Local" |
// "Managed"), why ("session_start" | "nested_traversal" |
// "path_glob_match" | "include" | "compact"), and optional glob /
// trigger / parent path fields for lazy / glob-matched / include loads.
func buildClaudeInstructionsLoadedEvent(body []byte) (models.ToolEvent, bool) {
	var p struct {
		claudeBaseEnvelope
		FilePath        string   `json:"file_path"`
		MemoryType      string   `json:"memory_type"`
		LoadReason      string   `json:"load_reason"`
		Globs           []string `json:"globs"`
		TriggerFilePath string   `json:"trigger_file_path"`
		ParentFilePath  string   `json:"parent_file_path"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return models.ToolEvent{}, false
	}
	if p.SessionID == "" {
		return models.ToolEvent{}, false
	}
	ev := baseToolEvent(p.claudeBaseEnvelope, models.ActionInstructionsLoaded, "instructions_loaded")
	// File_path is the natural per-session discriminator: a session
	// loads each instruction file at most once per load_reason. Same
	// file re-loaded under a different reason updates in place via
	// ON CONFLICT.
	if p.FilePath != "" {
		ev.SourceEventID = p.SessionID + ":instructions_loaded:" + p.FilePath
	}
	ev.Target = p.FilePath
	ev.RawToolName = p.MemoryType
	// load_reason + optional fields land on RawToolInput as a JSON
	// blob so dashboards can render "why this file loaded" without
	// re-parsing the full envelope.
	meta := map[string]any{}
	if p.LoadReason != "" {
		meta["load_reason"] = p.LoadReason
	}
	if len(p.Globs) > 0 {
		meta["globs"] = p.Globs
	}
	if p.TriggerFilePath != "" {
		meta["trigger_file_path"] = p.TriggerFilePath
	}
	if p.ParentFilePath != "" {
		meta["parent_file_path"] = p.ParentFilePath
	}
	if len(meta) > 0 {
		if b, err := json.Marshal(meta); err == nil {
			ev.RawToolInput = string(b)
		}
	}
	return ev, true
}

// buildClaudeConfigChangeEvent handles `ConfigChange` — fires when a
// settings.json mutation is observed by the host. Source discriminates
// the scope (user / project / local / policy / skills). file_path is
// optional (absent for the `skills` source).
func buildClaudeConfigChangeEvent(body []byte) (models.ToolEvent, bool) {
	var p struct {
		claudeBaseEnvelope
		Source   string `json:"source"`
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return models.ToolEvent{}, false
	}
	if p.SessionID == "" {
		return models.ToolEvent{}, false
	}
	ev := baseToolEvent(p.claudeBaseEnvelope, models.ActionConfigChange, "config_change")
	// {source}:{file_path} disambiguates one file modified twice in
	// different scopes (e.g. project_settings + local_settings both
	// pointing at .claude/settings.json — rare but documented).
	ev.SourceEventID = p.SessionID + ":config_change:" + p.Source + ":" + p.FilePath
	if p.FilePath != "" {
		ev.Target = p.FilePath
	} else {
		ev.Target = p.Source
	}
	ev.RawToolName = p.Source
	return ev, true
}

// buildClaudeWorktreeRemoveEvent handles `WorktreeRemove` — fires when
// a Claude Code Agent worktree is cleaned up. Non-blocking (per the
// docs reply matrix); the handler just records the event. Target
// carries the worktree_path so dashboards can correlate create / remove
// pairs by path.
func buildClaudeWorktreeRemoveEvent(body []byte) (models.ToolEvent, bool) {
	var p struct {
		claudeBaseEnvelope
		WorktreePath string `json:"worktree_path"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return models.ToolEvent{}, false
	}
	if p.SessionID == "" {
		return models.ToolEvent{}, false
	}
	ev := baseToolEvent(p.claudeBaseEnvelope, models.ActionWorktreeRemove, "worktree_remove")
	// Worktree path is the natural per-session discriminator —
	// removing the same path twice in one session is a no-op for
	// dedup purposes.
	if p.WorktreePath != "" {
		ev.SourceEventID = p.SessionID + ":worktree_remove:" + p.WorktreePath
	}
	ev.Target = p.WorktreePath
	return ev, true
}

// handleClaudeCodeWorktreeCreate is the dedicated handler for the
// `WorktreeCreate` blocking hook. Unlike the standard
// handleClaudeCodeActionEvent flow which always replies
// `{"decision":"approve"}` on stdout, WorktreeCreate must reply with
// the chosen worktree PATH so Claude Code's Agent spawn can proceed.
// The reply shape (per docs.claude.com/docs/en/hooks reply matrix):
//
//	{"hookSpecificOutput": {"worktreePath": "<absolute-path>"}}
//
// Any non-zero exit or absent worktreePath fails the spawn — so this
// handler must always succeed end-to-end. If the payload is malformed
// or missing fields, we fall back to a synthesized name + the
// canonical default location (`~/.claude/worktrees/<name>`) and emit
// the reply anyway. The action-row write is best-effort (spec P1) —
// DB errors log to stderr but the reply still goes out.
//
// NOT registered by default. See docs/claude-worktree-hook.md for the
// opt-in procedure + capture-first verification. stdin/stdout/stderr
// are injected for testability — production callers pass os.Stdin /
// os.Stdout / os.Stderr.
func handleClaudeCodeWorktreeCreate(ctx context.Context, label, configPath string, stdin io.Reader, stdout, stderr io.Writer) {
	body, _ := io.ReadAll(io.LimitReader(stdin, 2*1024*1024))
	body = bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF})

	worktreePath, ev, hasSession := buildClaudeWorktreeCreateReply(body)

	// Reply on stdout FIRST so Claude Code never blocks on the DB
	// write. Matches the spec P1 reply-first-then-record pattern used
	// by handleClaudeCodeActionEvent.
	reply := map[string]any{
		"hookSpecificOutput": map[string]string{
			"worktreePath": worktreePath,
		},
	}
	_ = json.NewEncoder(stdout).Encode(reply)

	if !hasSession {
		return
	}

	cfg, err := config.Load(config.LoadOptions{GlobalPath: configPath})
	if err != nil {
		fmt.Fprintf(stderr, "observer-hook: %s config: %v\n", label, err)
		return
	}
	database, err := db.Open(ctx, db.Options{Path: cfg.Observer.DBPath, SkipIntegrityCheck: true})
	if err != nil {
		fmt.Fprintf(stderr, "observer-hook: %s db: %v\n", label, err)
		return
	}
	defer database.Close()
	insertCtx, cancel := context.WithTimeout(ctx, cfg.Observer.Hooks.HookTimeout())
	defer cancel()
	if _, err := store.New(database).Ingest(insertCtx, []models.ToolEvent{ev}, nil, store.IngestOptions{}); err != nil {
		fmt.Fprintf(stderr, "observer-hook: %s insert: %v\n", label, err)
	}
}

// buildClaudeWorktreeCreateReply computes the WorktreeCreate reply
// path and the action row for a parsed payload. Pure (no I/O, no
// config) so it's unit-testable. The handler wires it to stdin /
// stdout / DB.
//
// Path resolution rules (in priority order):
//  1. $OBSERVER_CLAUDE_WORKTREE_ROOT/<name> — advanced users with a
//     non-default worktree filesystem.
//  2. ~/.claude/worktrees/<name> — Claude Code's documented default
//     location.
//  3. /tmp/.claude/worktrees/<name> — last-ditch when UserHomeDir
//     fails (the spawn will likely fail to mkdir this on most hosts,
//     but at least we replied with SOMETHING rather than empty stdout
//     which fails 100%).
//
// `name` synthesis: when payload.name is empty (e.g. malformed
// payload, missing field), we synthesize "worktree-<base36 nanos>".
// Better to echo SOMETHING than an empty path.
//
// hasSession reflects whether the payload carried a session_id —
// when false, the action-row write is skipped (matches the standard
// helper's guard).
func buildClaudeWorktreeCreateReply(body []byte) (worktreePath string, ev models.ToolEvent, hasSession bool) {
	var p struct {
		claudeBaseEnvelope
		Name string `json:"name"`
	}
	_ = json.Unmarshal(body, &p)

	name := p.Name
	if name == "" {
		name = "worktree-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	root := os.Getenv("OBSERVER_CLAUDE_WORKTREE_ROOT")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			home = "/tmp"
		}
		root = filepath.Join(home, ".claude", "worktrees")
	}
	worktreePath = filepath.Join(root, name)

	if p.SessionID == "" {
		return worktreePath, models.ToolEvent{}, false
	}
	ev = baseToolEvent(p.claudeBaseEnvelope, models.ActionWorktreeCreate, "worktree_create")
	ev.SourceEventID = p.SessionID + ":worktree_create:" + name
	ev.Target = name
	ev.RawToolName = name
	ev.RawToolInput = worktreePath
	return worktreePath, ev, true
}

func previewLine(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
