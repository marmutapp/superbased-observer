package termstatus

import "time"

// Status is the per-run agent status. "unknown" is a first-class value (codex
// P1 #13): a guess is never presented as a fact.
type Status string

const (
	// StatusWorking — the agent is actively producing (running a tool /
	// streaming output).
	StatusWorking Status = "working"
	// StatusWaitingForInput — the agent is at a prompt, waiting for the user.
	StatusWaitingForInput Status = "waiting-for-input"
	// StatusBlocked — the agent is stopped on an approval/permission prompt (a
	// stronger form of waiting; requires a trusted signal to assert).
	StatusBlocked Status = "blocked"
	// StatusIdle — no activity for a while, but the process is alive.
	StatusIdle Status = "idle"
	// StatusExited — the process has ended.
	StatusExited Status = "exited"
	// StatusUnknown — evidence is too weak/contradictory to label.
	StatusUnknown Status = "unknown"
)

// Confidence is the trust level of the strongest evidence a status rests on.
type Confidence string

const (
	// ConfTrusted — from a process/hook/OOB signal the child cannot forge.
	ConfTrusted Confidence = "trusted"
	// ConfHint — from the untrusted PTY stream (OSC marks, output recency).
	ConfHint Confidence = "hint"
	// ConfNone — no usable evidence.
	ConfNone Confidence = "none"
)

// PromptKind is the most recent OSC 133/633 prompt-mark seen (untrusted hint).
type PromptKind string

const (
	PromptNone            PromptKind = ""
	PromptStart           PromptKind = "prompt_start"     // OSC 133;A
	PromptCommandStart    PromptKind = "command_start"    // OSC 133;B
	PromptCommandExecuted PromptKind = "command_executed" // OSC 133;C
	PromptCommandFinished PromptKind = "command_finished" // OSC 133;D
)

// HookKind is the most recent trusted hook/OOB lifecycle signal, when a feed is
// wired. Zero (HookNone) when no trusted lifecycle signal is available.
type HookKind string

const (
	HookNone         HookKind = ""
	HookPreTool      HookKind = "pre_tool"     // a tool call started → working
	HookNotification HookKind = "notification" // the idle/notification hook → waiting
	HookStop         HookKind = "stop"         // a turn ended
	HookBlocked      HookKind = "blocked"      // an approval/permission prompt → blocked
)

// Signals is the fused evidence for one run at classification time. A zero
// timestamp means "no signal of that kind"; capability dispatch weights whatever
// is present (CLAUDE.md rule 3 — never branch on tool identity).
type Signals struct {
	// Now is the classification instant.
	Now time.Time
	// Exited + ExitCode: the trusted process-exit signal (termsession.Wait).
	Exited   bool
	ExitCode *int
	// LastOutput is when the PTY last produced output (any bytes).
	LastOutput time.Time
	// LastPromptKind/At: the most recent OSC prompt mark (untrusted hint).
	LastPromptKind PromptKind
	LastPromptAt   time.Time
	// LastBellAt: the most recent BEL / OSC 9 attention signal (untrusted).
	LastBellAt time.Time
	// LastHookKind/At: the most recent trusted lifecycle signal (hook/OOB).
	LastHookKind HookKind
	LastHookAt   time.Time
}

// Thresholds bound the time-based rules. Zero fields fall back to defaults.
type Thresholds struct {
	// IdleAfter: no output for this long (and no waiting signal) → idle.
	IdleAfter time.Duration
	// WaitingSilence: after a prompt-mark, this much PTY silence → waiting.
	WaitingSilence time.Duration
	// WorkingWindow: output within this window → working.
	WorkingWindow time.Duration
	// HookFresh: a trusted hook/OOB signal is authoritative within this window.
	HookFresh time.Duration
}

// Defaults for the thresholds.
const (
	defaultIdleAfter      = 5 * time.Minute
	defaultWaitingSilence = 2 * time.Second
	defaultWorkingWindow  = 3 * time.Second
	defaultHookFresh      = 90 * time.Second
)

func (t Thresholds) resolved() Thresholds {
	if t.IdleAfter <= 0 {
		t.IdleAfter = defaultIdleAfter
	}
	if t.WaitingSilence <= 0 {
		t.WaitingSilence = defaultWaitingSilence
	}
	if t.WorkingWindow <= 0 {
		t.WorkingWindow = defaultWorkingWindow
	}
	if t.HookFresh <= 0 {
		t.HookFresh = defaultHookFresh
	}
	return t
}

// Result is a classified status with its evidence basis. AgeSeconds is the age
// of the strongest evidence at Now, so the UI can show freshness.
type Result struct {
	Status     Status     `json:"status"`
	Evidence   string     `json:"evidence"`
	Confidence Confidence `json:"confidence"`
	AgeSeconds float64    `json:"age_seconds"`
}

// rule is one row of the ordered classifier table. It returns ok=true when it
// fires (with the Result), consulted top-down by confidence.
type rule struct {
	name string
	eval func(s Signals, th Thresholds) (Result, bool)
}

// rules is the ordered classifier table (CLAUDE.md rule 5). TRUSTED evidence
// (exit, hooks) is consulted before UNTRUSTED hints (prompt marks, output
// recency); the untrusted early-warning layer only fires when no trusted signal
// applies, and only produces waiting/working/idle — never blocked (which needs
// a trusted signal to assert).
var rules = []rule{
	{"exited", func(s Signals, _ Thresholds) (Result, bool) {
		if !s.Exited {
			return Result{}, false
		}
		ev := "process exited"
		if s.ExitCode != nil {
			ev = "process exited (code " + itoa(*s.ExitCode) + ")"
		}
		return Result{Status: StatusExited, Evidence: ev, Confidence: ConfTrusted}, true
	}},
	{"hook_trusted", func(s Signals, th Thresholds) (Result, bool) {
		if s.LastHookKind == HookNone || age(s.Now, s.LastHookAt) > th.HookFresh {
			return Result{}, false
		}
		st, ok := hookStatus(s.LastHookKind)
		if !ok {
			return Result{}, false
		}
		return Result{
			Status: st, Evidence: "hook: " + string(s.LastHookKind),
			Confidence: ConfTrusted, AgeSeconds: age(s.Now, s.LastHookAt).Seconds(),
		}, true
	}},
	{"prompt_waiting", func(s Signals, th Thresholds) (Result, bool) {
		// A prompt is being drawn / a command finished, AND the PTY has gone
		// quiet — the agent is waiting for input. This beats the 60s hook by
		// firing the moment a prompt boundary + silence coincide.
		if !isWaitingPrompt(s.LastPromptKind) {
			return Result{}, false
		}
		if age(s.Now, s.LastOutput) < th.WaitingSilence {
			return Result{}, false // still producing after the mark
		}
		return Result{
			Status: StatusWaitingForInput,
			Evidence: "prompt mark (" + string(s.LastPromptKind) + ") + " +
				dur(age(s.Now, s.LastOutput)) + " silent",
			Confidence: ConfHint, AgeSeconds: age(s.Now, s.LastPromptAt).Seconds(),
		}, true
	}},
	{"command_executing", func(s Signals, th Thresholds) (Result, bool) {
		// A command started executing and output is still flowing → working.
		if s.LastPromptKind != PromptCommandExecuted {
			return Result{}, false
		}
		if age(s.Now, s.LastOutput) > th.WorkingWindow {
			return Result{}, false
		}
		return Result{
			Status: StatusWorking, Evidence: "command executing, output flowing",
			Confidence: ConfHint, AgeSeconds: age(s.Now, s.LastOutput).Seconds(),
		}, true
	}},
	{"output_recent", func(s Signals, th Thresholds) (Result, bool) {
		if s.LastOutput.IsZero() || age(s.Now, s.LastOutput) > th.WorkingWindow {
			return Result{}, false
		}
		return Result{
			Status: StatusWorking, Evidence: "recent output",
			Confidence: ConfHint, AgeSeconds: age(s.Now, s.LastOutput).Seconds(),
		}, true
	}},
	{"idle", func(s Signals, th Thresholds) (Result, bool) {
		if s.LastOutput.IsZero() || age(s.Now, s.LastOutput) <= th.IdleAfter {
			return Result{}, false
		}
		return Result{
			Status: StatusIdle, Evidence: dur(age(s.Now, s.LastOutput)) + " since last output",
			Confidence: ConfHint, AgeSeconds: age(s.Now, s.LastOutput).Seconds(),
		}, true
	}},
}

// Classify fuses the signals into a status, walking the ordered rule table
// top-down and returning the first match. When nothing matches it returns
// StatusUnknown — the honest default, never a wrong label.
func Classify(s Signals, th Thresholds) Result {
	th = th.resolved()
	for _, r := range rules {
		if res, ok := r.eval(s, th); ok {
			return res
		}
	}
	return Result{Status: StatusUnknown, Evidence: "insufficient signal", Confidence: ConfNone}
}

// hookStatus maps a trusted hook kind to a status.
func hookStatus(k HookKind) (Status, bool) {
	switch k {
	case HookPreTool:
		return StatusWorking, true
	case HookNotification:
		return StatusWaitingForInput, true
	case HookBlocked:
		return StatusBlocked, true
	case HookStop:
		return StatusWaitingForInput, true
	default:
		return StatusUnknown, false
	}
}

// isWaitingPrompt reports whether a prompt mark indicates the shell/agent is
// waiting: a fresh prompt (A), the command-input region (B), or a finished
// command (D) all mean "no command is executing right now".
func isWaitingPrompt(k PromptKind) bool {
	switch k {
	case PromptStart, PromptCommandStart, PromptCommandFinished:
		return true
	default:
		return false
	}
}

func age(now, then time.Time) time.Duration {
	if then.IsZero() {
		return 1<<62 - 1 // effectively infinite: an absent signal is never "recent"
	}
	d := now.Sub(then)
	if d < 0 {
		return 0
	}
	return d
}
