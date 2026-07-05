package export

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Event is one guard audit event in export-facing form. The cmd layer
// maps store guard_events rows onto this type at the boundary; the
// JSON tags are the JSONL wire shape. All content here is local audit
// data the operator already owns — the store bounded reason/excerpt
// at insert time (its seam), so no re-truncation happens here.
type Event struct {
	// ID is the local guard_events row id (export-side correlation
	// key back into the audit DB).
	ID int64 `json:"id"`
	// TS is the event time (the action's timestamp, not write time).
	TS time.Time `json:"ts"`
	// SessionID anchors the event to a captured session.
	SessionID string `json:"session_id,omitempty"`
	// ActionID / APITurnID anchor to the originating watcher action /
	// proxy turn; nil on the hook path.
	ActionID  *int64 `json:"action_id,omitempty"`
	APITurnID *int64 `json:"api_turn_id,omitempty"`
	// Tool is the AI client adapter name.
	Tool string `json:"tool,omitempty"`
	// EventKind is the policy event vocabulary (shell_exec, ...).
	EventKind string `json:"event_kind,omitempty"`
	// RuleID is the catalog / user rule that produced the verdict.
	RuleID string `json:"rule_id"`
	// Category is the rule category (destructive, boundary, ...).
	Category string `json:"category,omitempty"`
	// Severity is the policy severity label (info|warn|high|critical;
	// "medium" is reserved spec vocabulary).
	Severity string `json:"severity,omitempty"`
	// Decision is the recorded outcome (allow|flag|ask|deny|mask).
	Decision string `json:"decision,omitempty"`
	// DegradedFrom records a downgrade (e.g. "approved" exceptions).
	DegradedFrom string `json:"degraded_from,omitempty"`
	// Enforced reports whether the decision actually blocked/asked.
	Enforced bool `json:"enforced"`
	// Source attributes the verdict (builtin|user|project|org|llm_judge).
	Source string `json:"source,omitempty"`
	// Reason is the human rationale (advice appended by the store).
	Reason string `json:"reason,omitempty"`
	// TargetHash is sha256 hex of the full target (joinable to
	// actions.target_hash).
	TargetHash string `json:"target_hash,omitempty"`
	// TargetExcerpt is the bounded target excerpt.
	TargetExcerpt string `json:"target_excerpt,omitempty"`
	// TaintOrigin names the taint source for T-5xx verdicts.
	TaintOrigin string `json:"taint_origin,omitempty"`
	// ChainPrev / ChainHash are the §10.4 tamper-evidence links.
	ChainPrev string `json:"chain_prev,omitempty"`
	ChainHash string `json:"chain_hash,omitempty"`
}

// JSONLLine renders one event as a single JSON line (no trailing
// newline — the writer owns separators). Timestamps normalize to UTC
// so exports are byte-stable across host timezones.
func JSONLLine(ev Event) ([]byte, error) {
	ev.TS = ev.TS.UTC()
	b, err := json.Marshal(ev)
	if err != nil {
		return nil, fmt.Errorf("export.JSONLLine: %w", err)
	}
	return b, nil
}

// cefSeverityTable is the spec §11.4 severity map, walked in order
// (module rule 5: decision tables, not ladders). "medium" is reserved
// vocabulary — the live policy enum emits info/warn/high/critical.
var cefSeverityTable = []struct {
	Label string
	Score int
}{
	{"info", 2},
	{"warn", 4},
	{"medium", 5},
	{"high", 8},
	{"critical", 10},
}

// cefSeverityUnknown is the score for severities outside the table:
// mid-scale (visible, not paging), with the raw label preserved in
// the sboSeverity extension.
const cefSeverityUnknown = 5

// CEFSeverityScore maps a policy severity label onto the CEF 0–10
// scale. ok=false means the label is outside the §11.4 table and the
// mid-scale default was returned.
func CEFSeverityScore(severity string) (score int, ok bool) {
	for _, row := range cefSeverityTable {
		if row.Label == severity {
			return row.Score, true
		}
	}
	return cefSeverityUnknown, false
}

// SeverityLabels returns the §11.4 severity vocabulary in ascending
// order — the CLI validates --min-severity against this.
func SeverityLabels() []string {
	out := make([]string, len(cefSeverityTable))
	for i, row := range cefSeverityTable {
		out[i] = row.Label
	}
	return out
}

// escapeCEFHeader escapes a CEF header (prefix) field: backslash and
// pipe per the ArcSight spec. Header fields are single-line by
// definition, so CR/LF collapse to a space rather than corrupting
// the record framing.
func escapeCEFHeader(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `|`, `\|`)
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// escapeCEFExtension escapes a CEF extension value: backslash and
// equals, with CR/LF emitted as the literal \r and \n sequences the
// spec prescribes for multi-line values. Pipes are deliberately NOT
// escaped here — the asymmetry is the CEF spec's.
func escapeCEFExtension(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `=`, `\=`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

// CEFLine renders one event as a CEF:0 record (guard spec §11.4):
//
//	CEF:0|SuperBased|ObserverGuard|<version>|<rule_id>|<name>|<sev>|ext
//
// The event name is "<category> <decision>" — compact and stable for
// SIEM grouping; the long-form rationale travels in msg. Extension
// order is FIXED so exports diff cleanly: rt, cat, act, outcome,
// cs1 sessionId, cs2 tool, cs3 eventKind, cs4 targetHash, cs5 source,
// cs6 target (excerpt), then the flat sbo* custom keys (sboEventId,
// sboChainHash, plus sboSeverity / sboTaintOrigin / sboDegradedFrom
// when set), with msg last. Empty fields are omitted per CEF
// convention; rt is epoch milliseconds UTC.
func CEFLine(ev Event, version string) string {
	score, known := CEFSeverityScore(ev.Severity)
	name := strings.TrimSpace(ev.Category + " " + ev.Decision)
	if name == "" {
		name = ev.EventKind
	}

	var b strings.Builder
	b.WriteString("CEF:0|SuperBased|ObserverGuard|")
	b.WriteString(escapeCEFHeader(version))
	b.WriteString("|")
	b.WriteString(escapeCEFHeader(ev.RuleID))
	b.WriteString("|")
	b.WriteString(escapeCEFHeader(name))
	b.WriteString("|")
	b.WriteString(strconv.Itoa(score))
	b.WriteString("|")

	ext := make([]string, 0, 16)
	add := func(key, value string) {
		if value != "" {
			ext = append(ext, key+"="+escapeCEFExtension(value))
		}
	}
	addLabeled := func(slot, label, value string) {
		if value != "" {
			ext = append(ext, slot+"Label="+label, slot+"="+escapeCEFExtension(value))
		}
	}
	if !ev.TS.IsZero() {
		add("rt", strconv.FormatInt(ev.TS.UTC().UnixMilli(), 10))
	}
	add("cat", ev.Category)
	add("act", ev.Decision)
	outcome := "observed"
	if ev.Enforced {
		outcome = "enforced"
	}
	add("outcome", outcome)
	addLabeled("cs1", "sessionId", ev.SessionID)
	addLabeled("cs2", "tool", ev.Tool)
	addLabeled("cs3", "eventKind", ev.EventKind)
	addLabeled("cs4", "targetHash", ev.TargetHash)
	addLabeled("cs5", "source", ev.Source)
	addLabeled("cs6", "target", ev.TargetExcerpt)
	add("sboEventId", strconv.FormatInt(ev.ID, 10))
	add("sboChainHash", ev.ChainHash)
	if !known {
		add("sboSeverity", ev.Severity)
	}
	add("sboTaintOrigin", ev.TaintOrigin)
	add("sboDegradedFrom", ev.DegradedFrom)
	add("msg", ev.Reason)

	b.WriteString(strings.Join(ext, " "))
	return b.String()
}

// Filter is the export window/severity gate (§11.4 CLI flags). Zero
// times mean unbounded; an empty MinSeverity admits everything.
type Filter struct {
	// Since admits events with TS >= Since (inclusive).
	Since time.Time
	// Until admits events with TS < Until (exclusive — half-open
	// windows compose without overlap).
	Until time.Time
	// MinSeverity admits events whose severity score is at least this
	// label's score. Validate against SeverityLabels() before use —
	// Match treats an unknown threshold as "no threshold" rather than
	// silently excluding everything.
	MinSeverity string
}

// Match reports whether an event passes the filter. An event whose
// OWN severity is outside the table always passes a severity
// threshold — an audit export fails toward inclusion (the G15
// fail-toward-visibility direction), never toward a silent gap.
func (f Filter) Match(ev Event) bool {
	if !f.Since.IsZero() && ev.TS.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && !ev.TS.Before(f.Until) {
		return false
	}
	if f.MinSeverity != "" {
		threshold, knownMin := CEFSeverityScore(f.MinSeverity)
		score, knownEv := CEFSeverityScore(ev.Severity)
		if knownMin && knownEv && score < threshold {
			return false
		}
	}
	return true
}
