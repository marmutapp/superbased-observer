package policy

import "time"

// EventKind classifies the surface a policy Event originated from.
// Rules declare which kinds they apply to (Rule.AppliesTo); the engine
// never inspects the originating client, only the kind plus
// capability flags (spec §3.3).
type EventKind string

// EventKind values (spec §3.4). G1 ships rules for KindShellExec and
// KindFileAccess; the remaining kinds are defined now so boundary code
// written in later commits classifies against a stable vocabulary.
const (
	// KindToolCall is a generic (non-shell, non-file) tool
	// invocation observed at a hook or in a transcript.
	KindToolCall EventKind = "tool_call"
	// KindFileAccess is a file read/write/edit; Target is the path
	// and ActionType carries the models-taxonomy verb.
	KindFileAccess EventKind = "file_access"
	// KindShellExec is a shell command execution; Target is the raw
	// command string (or Args the pre-split argv).
	KindShellExec EventKind = "shell_exec"
	// KindMCPCall is an MCP tool invocation.
	KindMCPCall EventKind = "mcp_call"
	// KindAPIRequest is an outbound LLM API request (proxy seam).
	KindAPIRequest EventKind = "api_request"
	// KindAPIResponse is an LLM API response (proxy seam).
	KindAPIResponse EventKind = "api_response"
	// KindConfigChange is a change to a watched client/MCP/guard
	// configuration file.
	KindConfigChange EventKind = "config_change"
	// KindSessionMeta is session-level metadata (posture signals
	// such as yolo flags or sandbox state).
	KindSessionMeta EventKind = "session_meta"
)

// Dialect identifies the shell dialect of a command string so the
// parser can apply the right tokenization and alias rules. It is
// resolved AT THE BOUNDARY per client+OS — capability-style data, not
// tool identity (approved deviation 1, guard-g1-design-note §4).
// Regardless of the hint, nested `powershell -Command` / `cmd /c`
// invocations are detected mid-parse and switch dialect for their
// payloads.
type Dialect string

// Dialect values. An empty Dialect means DialectPosix.
const (
	// DialectPosix covers sh/bash/zsh-style command lines (also the
	// Git-Bash-on-Windows hook path).
	DialectPosix Dialect = "posix"
	// DialectPowerShell covers Windows PowerShell / pwsh command
	// lines.
	DialectPowerShell Dialect = "powershell"
	// DialectCmd covers cmd.exe command lines.
	DialectCmd Dialect = "cmd"
)

// Capabilities describes what the originating channel can do, resolved
// at the boundary (hook receiver / adapter / proxy) per client+OS.
// Rules and emission logic branch on these flags, never on tool
// identity (spec §3.3). The per-client values live in the conformance
// matrix (G6) — a data table, not code.
type Capabilities struct {
	// PreExecution is true when the event arrives BEFORE the action
	// executes (hook, proxy) and false on post-hoc surfaces
	// (watcher ingest).
	PreExecution bool
	// CanBlock is true when the channel can actually stop the
	// action (documented deny semantics — spec §0.1 Q4: probed per
	// client, never assumed).
	CanBlock bool
	// CanAsk is true when the client renders a native ask/permission
	// prompt. Without it, ask degrades per spec §6.2 — never to a
	// silent allow.
	CanAsk bool
	// HasNativeDeny is true when the client has its own deny-rule
	// dialect that observer compiles policy into (spec §13.2).
	HasNativeDeny bool
	// Sandboxed is true when the session runs inside an OS sandbox
	// (posture signal, spec §13.1).
	Sandboxed bool
	// ProxyRouted is true when the client's API traffic flows
	// through the observer proxy.
	ProxyRouted bool
}

// TaintMark records one untrusted-source observation in a session
// (spec §4.5): a web fetch result, an MCP result from an unpinned
// server, a read of an externally-modified file, and so on.
type TaintMark struct {
	// Source is the source class (the TaintSourceXxx constants in
	// rules_taint.go: "web_fetch", "mcp_unpinned", "external_file",
	// "attachment", "secrets_read").
	Source string
	// Origin is a bounded human-readable description of where the
	// taint came from (a URL host, a server name — never content).
	Origin string
	// Imperative is set when the source CONTENT carried
	// imperative-instruction patterns (the §8.4 injection
	// heuristics, proxy-side — G9). Watcher-path marks never set it:
	// content is not inspected post-hoc. T-501 consumes it.
	Imperative bool
	// Turn is the session turn index at which the mark was set;
	// taint decays after [guard.taint].decay_turns.
	Turn int
	// At is when the mark was set.
	At time.Time
}

// TaintState is the per-session set of active taint marks. The state
// is OWNED by the guard composition layer (G3) — it tracks
// cross-event session state and decay; this package only ever reads
// the snapshot it receives on the Event (spec §17.4 one-owner rule).
// In G1 the type exists so the Event shape is complete; the T-5xx
// rules that consume it land with the guard layer.
type TaintState struct {
	// Marks are the active (not yet decayed) taint marks.
	Marks []TaintMark
}

// Tainted reports whether any taint mark is active.
func (t TaintState) Tainted() bool { return len(t.Marks) > 0 }

// HasSource reports whether any active mark carries the given source
// class. Used by rules and by the user-rule taint_source matcher.
func (t TaintState) HasSource(source string) bool {
	for _, m := range t.Marks {
		if m.Source == source {
			return true
		}
	}
	return false
}

// Event is one normalized agent action presented for evaluation — the
// only input type that crosses the seam (spec §3.4). Boundaries (hook
// receiver, proxy, ingest) build an Event and call Engine.Evaluate;
// nothing in this package reaches back out for more context.
type Event struct {
	// Kind classifies the originating surface; rules filter on it.
	Kind EventKind
	// ActionType carries the models-taxonomy verb ("read_file",
	// "write_file", "edit_file", "run_command", ...) when the event
	// is sourced from a normalized Action. The string values mirror
	// internal/models; they are not imported to keep this package
	// dependency-free (see actionIsWrite in rules_boundary.go).
	ActionType string
	// Tool is the client name — for REPORTING only. No logic in
	// this package may branch on it (spec §17.3, grep-enforced).
	Tool string
	// Target is the path / URL / command string the action operates
	// on.
	Target string
	// Args is the pre-split argv when the boundary already has one
	// (some hook payloads deliver argv rather than a command
	// string). When empty, shell events are parsed from Target.
	Args []string
	// Dialect is the shell dialect hint for Target/Args (approved
	// deviation 1). Empty means DialectPosix.
	Dialect Dialect
	// Cwd is the working directory the action runs in, when the
	// boundary knows it (hook payloads carry it). Relative path
	// targets resolve against Cwd, falling back to ProjectRoot.
	// Additive field discovered during G1 implementation: without
	// it, relative targets ("rm -rf ./src", "../other") cannot be
	// resolved at all.
	Cwd string
	// ProjectRoot is the resolved project root (internal/git
	// resolution, done by the caller — never here).
	ProjectRoot string
	// SessionID identifies the agent session for reporting and
	// session-scoped state lookups.
	SessionID string
	// Caps are the channel capabilities resolved at the boundary.
	Caps Capabilities
	// Taint is the session's current taint snapshot (owned by the
	// guard layer; G1 rules do not yet consume it).
	Taint TaintState
	// Raw is a bounded (≤4 KiB by convention at the boundary) slice
	// of the raw payload for matcher access. The proxy seam (G9) is
	// the documented exception to the bound: KindAPIRequest events
	// built for the §8.4 injection heuristics carry one full
	// tool-result/web-content segment (already segment-capped at the
	// boundary) because injected instructions can sit anywhere in the
	// segment. Raw is in-memory only — it never persists.
	Raw []byte
	// Secrets are typed secret-detector findings stamped by the
	// boundary that built the event (the proxy egress seam, §8.2 —
	// the same compute-at-the-owner pattern as Taint). The R-172
	// api_request row consumes them; this package never runs
	// detectors over message content itself.
	Secrets []SecretFinding
	// MCPFindings are MCP config-layer security findings stamped by
	// the guard composition layer (the §9.2/§9.3 pin-diff and
	// poisoning results computed in guard/mcpsec — the same
	// compute-at-the-owner pattern as Taint and Secrets). The
	// R-301/302/303/305 rows consume them; this package never reads
	// configs or pin state itself.
	MCPFindings []MCPFinding
	// PostureFindings are client-posture findings stamped by the
	// guard composition layer (the §13.2 native-dialect drift check —
	// the same compute-at-the-owner pattern as MCPFindings). The
	// R-204 row consumes them; this package never reads native
	// client configs itself.
	PostureFindings []PostureFinding
	// SessionCostUSD / DailyCostUSD are the spend-so-far stamps for
	// the §12.1 budget rules and the §4.4 session_cost_usd_gt user
	// matcher, stamped by the guard layer's TTL-cached budget lookup
	// (the compute-at-the-owner pattern; this package never queries
	// cost data). 0 means unknown/unstamped — budget rules treat it
	// as no-match, never as "free". Hook-path events are never
	// stamped (the lookup lives in the daemon).
	SessionCostUSD float64
	DailyCostUSD   float64
	// RepeatCount is the consecutive-identical-action run length
	// INCLUDING this event, stamped by the guard layer's repeat
	// tracker on the watcher ingest path (the A-610 stuck-loop
	// signal and the §4.4 repeat_count_gt user matcher). 0 means
	// untracked (hook path, proxy path).
	RepeatCount int
	// Now is the evaluation timestamp, injected for determinism.
	Now time.Time
}

// SecretFinding is one typed secret detection stamped onto an Event
// by the boundary (internal/scrub's typed detectors, injected — this
// package imports zero observer packages). It deliberately carries NO
// value/span: rules only ever see what KIND of secret appeared.
type SecretFinding struct {
	// Type is the stable detector name ("github_pat", "entropy", ...).
	Type string
	// Certain reports a pattern-certain detection; the entropy
	// heuristic reports false (spec §8.2 gates masking on certainty).
	Certain bool
}
