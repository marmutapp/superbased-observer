package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/hook"
	"github.com/marmutapp/superbased-observer/internal/intelligence/compaction"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/pidbridge"
	"github.com/marmutapp/superbased-observer/internal/scrub"
	"github.com/marmutapp/superbased-observer/internal/store"
)

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
// stderr but never propagate as a non-zero exit.
func newHookCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hook <tool> <event>",
		Short: "Handle an AI-tool hook event on stdin and reply on stdout",
		Long: "Invoked by the host AI tool via its hook configuration. Reads a\n" +
			"JSON payload from stdin and replies on stdout. For Cursor, also\n" +
			"records the event in the observer DB. MUST NEVER block the host\n" +
			"tool — errors are logged to stderr and swallowed.",
		Args: cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			tool := args[0]
			event := ""
			if len(args) >= 2 {
				event = args[1]
			}
			switch tool {
			case "cursor":
				handleCursorHook(cmd.Context(), event)
			case "claude-code":
				handleClaudeCodeHook(cmd.Context(), event)
			default:
				label := tool
				if event != "" {
					label = tool + ":" + event
				}
				hook.HandleApprove(label, os.Stdin, os.Stdout, os.Stderr)
			}
		},
	}
	return cmd
}

// handleClaudeCodeHook dispatches Claude Code hook events. For PreCompact
// and PostCompact we write to compaction_events; for PreToolUse we may
// rewrite a Bash command to funnel through `observer run`; other events
// fall through to HandleApprove (the JSONL watcher captures them
// out-of-band). Always replies with an approval on stdout — must never
// block the host.
func handleClaudeCodeHook(ctx context.Context, event string) {
	label := "claude-code"
	if event != "" {
		label = "claude-code:" + event
	}
	if event == "pre-tool" {
		handleClaudeCodePreTool(os.Stdin, os.Stdout, os.Stderr, label)
		return
	}
	if event == "session-start" {
		handleClaudeCodeSessionStart(ctx, os.Getppid(), defaultAncestors, os.Stdin, os.Stdout, os.Stderr, label, defaultPidbridgeWriter)
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

	cfg, err := config.Load(config.LoadOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "observer-hook: %s config: %v\n", label, err)
		return
	}
	database, err := db.Open(ctx, db.Options{Path: cfg.Observer.DBPath})
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
	cfg, err := config.Load(config.LoadOptions{})
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	database, err := db.Open(ctx, db.Options{Path: cfg.Observer.DBPath})
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
	_ = json.NewEncoder(stdout).Encode(hook.Decision{Decision: "approve"})

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
func handleClaudeCodePreTool(stdin io.Reader, stdout, stderr io.Writer, label string) {
	body, _ := io.ReadAll(io.LimitReader(stdin, 2*1024*1024))
	fmt.Fprintf(stderr, "observer-hook: event=%s received bytes=%d at=%s\n",
		label, len(body), time.Now().UTC().Format(time.RFC3339))

	cfg, cfgErr := config.Load(config.LoadOptions{})
	binary, binErr := absoluteBinaryPath()
	rewritten, newCmd, reason := decidePreToolRewrite(body, cfg, cfgErr, binary, binErr)

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
}

// decidePreToolRewrite is the pure decision function used by
// handleClaudeCodePreTool. Reason "" signals "not a Bash call, nothing to
// consider"; any other reason is a short tag for diagnostic logging.
func decidePreToolRewrite(body []byte, cfg config.Config, cfgErr error, binary string, binErr error) (rewrite bool, newCommand, reason string) {
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
	if binErr != nil || binary == "" {
		return false, "", "binary-lookup-error"
	}
	newCmd, changed := hook.RewriteBash(binary, p.ToolInput.Command, cfg.Compression.Shell.ExcludeCommands)
	if !changed {
		return false, "", "not-rewritable"
	}
	return true, newCmd, "ok"
}

// handleCursorHook opens the observer DB and dispatches the cursor hook.
// Replies on stdout immediately (handled inside HandleCursorEvent) so the
// host doesn't wait on the DB insert.
func handleCursorHook(ctx context.Context, event string) {
	if event == "" {
		event = "unknown"
	}
	cfg, err := config.Load(config.LoadOptions{})
	if err != nil {
		// Fall back to approve-only — never block the host.
		fmt.Fprintf(os.Stderr, "observer-hook: cursor config: %v\n", err)
		hook.HandleApprove("cursor:"+event, os.Stdin, os.Stdout, os.Stderr)
		return
	}
	database, err := db.Open(ctx, db.Options{Path: cfg.Observer.DBPath})
	if err != nil {
		fmt.Fprintf(os.Stderr, "observer-hook: cursor db: %v\n", err)
		hook.HandleApprove("cursor:"+event, os.Stdin, os.Stdout, os.Stderr)
		return
	}
	defer database.Close()

	sc := scrub.New()
	hook.HandleCursorEvent(event, store.New(database), sc, os.Stdin, os.Stdout, os.Stderr, cfg.Observer.Hooks.HookTimeout())
}
