package compile

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"sort"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/policy"
)

// Dialect identifies one client's native permission-rule language.
type Dialect string

// Dialect values — one per spec §13.2 target. Only the implemented
// ones produce entries; the rest are declared in the target table
// with their deferral reason (Q4 honesty, recorded as data).
const (
	// DialectClaudeCode is Claude Code's permissions.deny/ask arrays
	// in ~/.claude/settings.json.
	DialectClaudeCode Dialect = "claude-code"
	// DialectOpenCode is OpenCode's permission.bash pattern map in
	// ~/.config/opencode/opencode.json (last-match-wins order).
	DialectOpenCode Dialect = "opencode"
	// DialectCursor is Cursor's permissions denylist (deferred).
	DialectCursor Dialect = "cursor"
	// DialectWindsurf is Windsurf's cascadeCommandsDenyList (deferred).
	DialectWindsurf Dialect = "windsurf"
	// DialectAmp is Amp's amp.permissions reject/delegate rules
	// (deferred).
	DialectAmp Dialect = "amp"
	// DialectGemini is Gemini CLI's policy-engine files (deferred).
	DialectGemini Dialect = "gemini"
	// DialectCodex is Codex's rules prefix forbids (deferred).
	DialectCodex Dialect = "codex"
)

// Entry is one native permission entry: an action plus the dialect's
// native entry text (Claude Code: a permission rule string such as
// "Bash(mkfs:*)"; OpenCode: a bash command pattern such as "mkfs*").
type Entry struct {
	// Action is "deny" or "ask" — normalized vocabulary; each
	// dialect's Apply maps it onto the native encoding.
	Action string
	// Value is the native entry text.
	Value string
}

// Entry action vocabulary.
const (
	// ActionDeny blocks natively.
	ActionDeny = "deny"
	// ActionAsk prompts natively.
	ActionAsk = "ask"
)

// Fidelity grades one rule→dialect translation row.
type Fidelity string

// Fidelity values.
const (
	// FidelityExact means the native entries match the rule trigger's
	// scope (or a precise subset arm of it).
	FidelityExact Fidelity = "exact"
	// FidelityApprox means the native entries differ from the rule's
	// scope — broader, narrower, or partial; the row Note documents
	// exactly how, and broadened entries are demoted to ask.
	FidelityApprox Fidelity = "approximation"
	// FidelityNone means the rule is not expressible in the dialect;
	// the row Note documents why.
	FidelityNone Fidelity = "none"
)

// Translation is one (rule, dialect) row of a compile table: the
// per-rule compilability record the spec demands as data.
type Translation struct {
	// RuleID is the built-in catalog rule the row translates.
	RuleID string
	// Fidelity grades the translation; Note is mandatory for
	// approximation and none rows (pinned by tests).
	Fidelity Fidelity
	// Note documents the approximation or the inexpressibility —
	// lossy translations are never silent.
	Note string
	// Entries are the native entries the row contributes when the
	// rule is active. Empty exactly when Fidelity is none.
	Entries []Entry
}

// ApplyResult reports one (would-be) apply pass over a native config.
type ApplyResult struct {
	// Updated is the full new file content, nil when the file already
	// matches the wanted entry set (nothing to write).
	Updated []byte
	// Added are entries missing from the file that the pass adds —
	// the R-204 drift signal when observed by a read-only check.
	Added []Entry
	// Removed are managed (universe-recognised) entries the current
	// policy no longer wants, which the pass removes. Stale-stricter
	// only — never an R-204 signal.
	Removed []Entry
}

// Target describes one native dialect: where its config lives, whose
// client it belongs to, and (when implemented) its translation table
// and config rewriter. The deferred spec §13.2 targets carry their
// deferral reason as data.
type Target struct {
	// Dialect is the target's identity (guard_pins name).
	Dialect Dialect
	// Client is the canonical client name (adapter / conformance
	// vocabulary) the dialect belongs to.
	Client string
	// RelPath is the native config file's path segments under the
	// user home directory. Empty for deferred targets.
	RelPath []string
	// Implemented reports whether the dialect compiles in this
	// version. Deferred targets carry Reason instead.
	Implemented bool
	// Reason documents WHY a target is deferred (Q4: semantics are
	// researched before they are promised). Empty when implemented.
	Reason string

	rows  func() []Translation
	apply func(existing []byte, want []Entry) (ApplyResult, error)
}

// Path returns the absolute native config path under home.
func (t Target) Path(home string) string {
	if len(t.RelPath) == 0 {
		return ""
	}
	return filepath.Join(append([]string{home}, t.RelPath...)...)
}

// Rows returns the dialect's translation table (nil for deferred
// targets) — one row per built-in catalog rule, in catalog order.
func (t Target) Rows() []Translation {
	if t.rows == nil {
		return nil
	}
	return t.rows()
}

// Apply computes the config rewrite installing want into existing
// (and retiring managed entries no longer wanted). Pure — the caller
// owns reading and writing the file. Errors mean the existing content
// could not be safely understood (e.g. malformed JSON); callers must
// surface the issue and NOT write.
func (t Target) Apply(existing []byte, want []Entry) (ApplyResult, error) {
	return t.apply(existing, want)
}

// Targets returns every spec §13.2 dialect target in stable order —
// implemented first, then the deferred ones with reasons.
func Targets() []Target {
	return []Target{
		{
			Dialect: DialectClaudeCode, Client: "claude-code",
			RelPath: []string{".claude", "settings.json"}, Implemented: true,
			rows: claudeCodeRows, apply: applyClaudeCode,
		},
		{
			Dialect: DialectOpenCode, Client: "opencode",
			// OpenCode keeps its config XDG-style on every OS (the
			// same deliberate choice its Kilo fork inherited) — NOT
			// %APPDATA% on Windows.
			RelPath: []string{".config", "opencode", "opencode.json"}, Implemented: true,
			rows: openCodeRows, apply: applyOpenCode,
		},
		{
			Dialect: DialectCursor, Client: "cursor",
			Reason: "Cursor's permissions denylist format/location varies across releases and is unverified against a live install; compiling unverified syntax could break the client's permission loading",
		},
		{
			Dialect: DialectWindsurf, Client: "windsurf",
			Reason: "Windsurf's cascadeCommandsDenyList lives in IDE settings storage; on-disk location and merge behavior unverified against a live install",
		},
		{
			Dialect: DialectAmp, Client: "amp",
			Reason: "Amp's amp.permissions reject/delegate entries are IDE settings keys; the delegate seam (spec §6.2) is the better integration and joins with an Amp hook receiver",
		},
		{
			Dialect: DialectGemini, Client: "gemini-cli",
			Reason: "Gemini CLI's policy-engine rule files are still experimental upstream; syntax unverified against a live install",
		},
		{
			Dialect: DialectCodex, Client: "codex",
			Reason: "Codex's rules prefix-forbid surface is not present in current stable releases; unverified",
		},
	}
}

// TargetFor returns the target for one dialect name, false when
// unknown.
func TargetFor(dialect string) (Target, bool) {
	for _, t := range Targets() {
		if string(t.Dialect) == dialect {
			return t, true
		}
	}
	return Target{}, false
}

// RowResult records what one translation row contributed to a compile
// pass — the per-rule evidence `observer guard compile --diff` and the
// table tests render.
type RowResult struct {
	// RuleID / Fidelity / Note mirror the translation row.
	RuleID   string
	Fidelity Fidelity
	Note     string
	// Emitted are the entries the row contributed (post decision-cap,
	// pre dedup).
	Emitted []Entry
	// Skipped is the non-empty reason when the row emitted nothing.
	Skipped string
}

// Compiled is one dialect's compile result.
type Compiled struct {
	// Target identifies the dialect compiled for.
	Target Target
	// Entries are the wanted native entries in stable emission order,
	// deduplicated.
	Entries []Entry
	// Rows are the per-rule translation decisions in table order.
	Rows []RowResult
	// Uncompiled lists effective rule IDs with no translation row —
	// user/project-authored rules, which have no per-rule table
	// knowledge (recorded so coverage gaps are visible, never silent).
	Uncompiled []string
}

// Compile translates the effective policy (the engine's RuleInfos —
// overrides applied, disabled rules removed) into one dialect's
// native entry set. Deferred targets compile to an empty entry set
// with their table absent.
//
// Gating per rule ID: absent from the effective set ⇒ row skipped
// ("disabled"); effective enforce decision below ask ⇒ row skipped
// (flag/allow have no native expression); deny entries are capped to
// ask when the effective decision is ask. Multi-row catalog IDs use
// their strictest row's enforce decision (the entries' own actions
// already encode the per-arm split, e.g. R-152 write=deny/read=ask).
func Compile(t Target, effective []policy.RuleInfo) Compiled {
	out := Compiled{Target: t}
	present := map[string]bool{}
	enforce := map[string]policy.Decision{}
	for _, r := range effective {
		if d, ok := enforce[r.ID]; !ok || r.Enforce > d {
			enforce[r.ID] = r.Enforce
		}
		present[r.ID] = true
	}
	covered := map[string]bool{}
	seen := map[Entry]bool{}
	for _, row := range t.Rows() {
		covered[row.RuleID] = true
		res := RowResult{RuleID: row.RuleID, Fidelity: row.Fidelity, Note: row.Note}
		switch {
		case row.Fidelity == FidelityNone:
			res.Skipped = "not expressible in this dialect"
		case !present[row.RuleID]:
			res.Skipped = "rule disabled in effective policy"
		case enforce[row.RuleID] < policy.DecisionAsk:
			res.Skipped = "effective enforce decision is " + enforce[row.RuleID].String() + " — no native expression"
		default:
			for _, e := range row.Entries {
				if e.Action == ActionDeny && enforce[row.RuleID] < policy.DecisionDeny {
					e.Action = ActionAsk
				}
				res.Emitted = append(res.Emitted, e)
				if !seen[e] {
					seen[e] = true
					out.Entries = append(out.Entries, e)
				}
			}
		}
		out.Rows = append(out.Rows, res)
	}
	for _, r := range effective {
		if !covered[r.ID] {
			covered[r.ID] = true // dedup multi-row IDs in the report
			out.Uncompiled = append(out.Uncompiled, r.ID)
		}
	}
	sort.Strings(out.Uncompiled)
	return out
}

// HashEntries returns the versioned pin hash of an entry set for
// guard_pins kind=native_dialect rows: order-insensitive (sorted
// canonical lines) so cosmetic emission-order changes never read as
// drift; action changes and entry changes always do.
func HashEntries(entries []Entry) string {
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		lines = append(lines, e.Action+"\x1f"+e.Value)
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return "v1 " + hex.EncodeToString(sum[:])
}
