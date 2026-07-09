package processobs

import (
	"strings"
	"time"
)

// CorrelationWindow is the default time tolerance between a run_command
// action and the process it spawned: the AI tool logs the command, then the
// shell/binary starts within a few seconds. A process that starts much later
// is not considered part of that command's spawn.
const CorrelationWindow = 30 * time.Second

// correlationBackSkew allows a small negative delta (process start observed
// slightly before the action timestamp) for clock differences between the
// proxy/hook clock and the backend clock.
const correlationBackSkew = 2 * time.Second

// ProcRunRef is the minimal persisted process-run projection the action
// correlator reads. The store loads these; the correlator is pure.
type ProcRunRef struct {
	ProcessKey       string
	ParentProcessKey string
	StartedAt        time.Time
	ArgvPreview      string
	ExeBasename      string
	// Linked is true when the run already carries an action_id — it is left
	// untouched (idempotent: a second pass re-confirms, never re-links).
	Linked bool
}

// ActionRef is a run_command-class action a process subtree may be
// attributed to. Command is the action's target (the command string);
// TurnIndex is nil when the action row has none.
//
// Duration is the command's measured runtime when the source records it
// (codex logs run_command at exec_command_end — the command FINISH — and
// carries a duration). It widens the back-skew in bestAction so a process
// that started up to `Duration` before the action's (end) timestamp still
// links: without it, every command running longer than correlationBackSkew
// fails to link to the message that spawned it. Zero for sources that log
// at command START (the Claude Code PreToolUse hook), which keep the tight
// 2s back-skew.
type ActionRef struct {
	ActionID  int64
	TurnIndex *int
	Command   string
	Timestamp time.Time
	Duration  time.Duration
	// Success is the action's recorded outcome (actions.success). It is
	// consumed by DeriveCommandRuns (derive.go) to set the synthetic run's
	// coarse exit state; the correlator itself ignores it. The actions table
	// has no numeric exit_code column, so a derived run can only distinguish
	// success from failure, not the specific exit code.
	Success bool
}

// ActionLink assigns one process (by key) to the action that spawned it.
type ActionLink struct {
	ProcessKey string
	ActionID   int64
	TurnIndex  *int
}

// CorrelateActions implements the §9.2.4 deferred pass: it links each
// unlinked process to the run_command action that spawned it. A process
// whose leading executable or argv matches an action within `window` becomes
// an ANCHOR; the action then propagates DOWN that process's subtree (via
// parent_process_key) until another anchor is reached — a nested command owns
// its own subtree, so the nearest anchor ancestor always wins.
//
// Pure: the store loads runs+actions and applies the returned links. window
// <= 0 uses CorrelationWindow. Already-Linked runs are never reassigned.
func CorrelateActions(runs []ProcRunRef, actions []ActionRef, window time.Duration) []ActionLink {
	if window <= 0 {
		window = CorrelationWindow
	}

	byKey := make(map[string]*ProcRunRef, len(runs))
	children := make(map[string][]string)
	for i := range runs {
		r := &runs[i]
		byKey[r.ProcessKey] = r
		if r.ParentProcessKey != "" {
			children[r.ParentProcessKey] = append(children[r.ParentProcessKey], r.ProcessKey)
		}
	}

	// Anchors: each unlinked run that directly matches an action.
	anchors := make(map[string]ActionRef)
	for i := range runs {
		r := &runs[i]
		if r.Linked {
			continue
		}
		if a, ok := bestAction(r, actions, window); ok {
			anchors[r.ProcessKey] = a
		}
	}

	links := make([]ActionLink, 0, len(anchors))
	assigned := make(map[string]bool)
	for anchorKey, act := range anchors {
		// DFS the anchor's subtree, assigning the action to unlinked nodes and
		// stopping at any OTHER anchor (that subtree belongs to its own action).
		stack := []string{anchorKey}
		for len(stack) > 0 {
			k := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if assigned[k] {
				continue
			}
			if k != anchorKey {
				if _, isOtherAnchor := anchors[k]; isOtherAnchor {
					continue
				}
			}
			r := byKey[k]
			if r == nil || r.Linked {
				continue
			}
			assigned[k] = true
			links = append(links, ActionLink{ProcessKey: k, ActionID: act.ActionID, TurnIndex: act.TurnIndex})
			stack = append(stack, children[k]...)
		}
	}
	return links
}

// bestAction picks the action that best explains a process: strongest match
// (the process IS the command's leading binary > the command appears in the
// argv), tie-broken by closeness in time. Returns ok=false when nothing
// matches inside the window.
func bestAction(r *ProcRunRef, actions []ActionRef, window time.Duration) (ActionRef, bool) {
	var best ActionRef
	bestScore := 0
	var bestDelta time.Duration
	for _, a := range actions {
		delta := r.StartedAt.Sub(a.Timestamp)
		// The action timestamp is the command START for hook-logged sources
		// (delta ≈ +small) but the command END for codex (logged at
		// exec_command_end). For the latter the process started ~Duration
		// before the action, so widen the back-skew by the recorded duration;
		// the forward window stays put.
		backSkew := correlationBackSkew + a.Duration
		if delta < -backSkew || delta > window {
			continue
		}
		score := matchScore(r, a)
		if score == 0 {
			continue
		}
		abs := delta
		if abs < 0 {
			abs = -abs
		}
		if score > bestScore || (score == bestScore && abs < bestDelta) {
			best, bestScore, bestDelta = a, score, abs
		}
	}
	return best, bestScore > 0
}

// matchScore rates how well a process matches an action's command: 2 when the
// process's executable basename is the command's leading binary, 1 when the
// command string appears in the (scrubbed) argv preview (the `sh -c "<cmd>"`
// wrapper case), 0 otherwise.
func matchScore(r *ProcRunRef, a ActionRef) int {
	if lead := leadingBinary(a.Command); lead != "" && r.ExeBasename != "" && execMatchesLeadingBinary(r.ExeBasename, lead) {
		return 2
	}
	if r.ArgvPreview != "" && a.Command != "" {
		if strings.Contains(strings.ToLower(r.ArgvPreview), strings.ToLower(firstN(a.Command, 60))) {
			return 1
		}
	}
	return 0
}

// execMatchesLeadingBinary reports whether a captured process's exe basename is
// the command's leading binary, tolerating the two systematic skews between the
// two sources: the Windows `.exe` suffix (the command says `git`, the captured
// process is `git.exe`) and an interpreter VERSION tail (the command says
// `python3`/`node`, the process is `python3.8`/`node18`). Without this, every
// Windows command and every versioned-interpreter command scores 0 and the
// process never links to the action that spawned it. Case-insensitive.
func execMatchesLeadingBinary(exeBasename, cmdLead string) bool {
	e := strings.ToLower(strings.TrimSuffix(strings.ToLower(exeBasename), ".exe"))
	c := strings.ToLower(strings.TrimSuffix(strings.ToLower(cmdLead), ".exe"))
	if e == "" || c == "" {
		return false
	}
	if e == c {
		return true
	}
	// Version-tail tolerance: the longer name must extend the shorter at a
	// version boundary (a digit or dot), so `python3`↔`python3.8` and
	// `node`↔`node18` match but `git`↔`gitk` does not.
	longer, shorter := e, c
	if len(c) > len(e) {
		longer, shorter = c, e
	}
	if strings.HasPrefix(longer, shorter) {
		tail := longer[len(shorter):]
		if tail != "" && (tail[0] == '.' || (tail[0] >= '0' && tail[0] <= '9')) {
			return true
		}
	}
	return false
}

// leadingBinary returns the basename of the first real executable token in a
// command, skipping leading `VAR=val` env assignments and the common
// `sudo`/`env` prefixes. "" when the command is empty.
func leadingBinary(cmd string) string {
	for _, f := range strings.Fields(cmd) {
		if f == "sudo" || f == "env" {
			continue
		}
		if strings.Contains(f, "=") && !strings.HasPrefix(f, "-") && !strings.ContainsAny(f, "/\\") {
			continue // VAR=val
		}
		return basename(f)
	}
	return ""
}

// firstN returns the first n bytes of s (commands are ASCII-ish; a mid-rune
// cut only weakens a substring match, never corrupts state).
func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
