package statusline

import (
	"encoding/json"
	"fmt"
)

// Input is the parsed shape of Claude Code's statusLine stdin-JSON
// contract (plan §1.1, §6.1 "Critical files": session_id,
// transcript_path, cwd, model.{id,display_name},
// workspace.{current_dir,project_dir},
// cost.{total_cost_usd,total_duration_ms,total_lines_added,
// total_lines_removed}, output_style, version).
//
// Every field is a pointer, and every nested group is a pointer to its
// own struct, so an ABSENT field is representable as nil rather than
// coerced into a fabricated zero value (a float64 cost of 0 and an absent
// cost are different facts; only ParseInput's caller — the segment
// renderers in segments.go — gets to decide what an absent datum means
// for the rendered line, and they decide "omit the segment," never
// "print zero"). The json tags below are the wire contract itself, so
// this struct is decoded directly by ParseInput with no intermediate
// shadow type.
type Input struct {
	// SessionID is Claude Code's session identifier for this statusLine
	// invocation, when present.
	SessionID *string `json:"session_id"`
	// TranscriptPath is the path to the session's transcript file, when
	// present. Never used to source a segment today, but kept because
	// the documented contract carries it.
	TranscriptPath *string `json:"transcript_path"`
	// CWD is the top-level "cwd" field some Claude Code versions carry
	// alongside (or instead of) Workspace.CurrentDir.
	CWD *string `json:"cwd"`
	// Model carries the current model's id/display name, when the host
	// supplied a "model" object at all.
	Model *Model `json:"model"`
	// Workspace carries the current/project directory, when the host
	// supplied a "workspace" object at all.
	Workspace *Workspace `json:"workspace"`
	// Cost carries the session's accumulated cost/duration/line-count
	// figures, when the host supplied a "cost" object at all.
	Cost *Cost `json:"cost"`
	// OutputStyle is Claude Code's active output-style name, when
	// present.
	OutputStyle *string `json:"output_style"`
	// Version is the reporting Claude Code version string, when present.
	Version *string `json:"version"`
}

// Model is the "model" object of the statusLine stdin-JSON payload.
type Model struct {
	// ID is the model's raw identifier (e.g. "claude-opus-4-5-20260101").
	ID *string `json:"id"`
	// DisplayName is the model's human-friendly name (e.g.
	// "claude-opus-4-5"), preferred over ID when both are present
	// (segments.go).
	DisplayName *string `json:"display_name"`
}

// Workspace is the "workspace" object of the statusLine stdin-JSON
// payload.
type Workspace struct {
	// CurrentDir is the directory Claude Code is currently running in.
	CurrentDir *string `json:"current_dir"`
	// ProjectDir is the resolved project root, when Claude Code was able
	// to determine one.
	ProjectDir *string `json:"project_dir"`
}

// Cost is the "cost" object of the statusLine stdin-JSON payload —
// figures Claude Code has already computed server-side for this session,
// no lookup required to render them (plan §1.1, §4.1).
type Cost struct {
	// TotalCostUSD is the session's accumulated observed spend, in US
	// dollars.
	TotalCostUSD *float64 `json:"total_cost_usd"`
	// TotalDurationMS is the session's accumulated wall-clock duration,
	// in milliseconds.
	TotalDurationMS *int64 `json:"total_duration_ms"`
	// TotalLinesAdded is the session's accumulated added-line count.
	TotalLinesAdded *int64 `json:"total_lines_added"`
	// TotalLinesRemoved is the session's accumulated removed-line count.
	TotalLinesRemoved *int64 `json:"total_lines_removed"`
}

// ParseInput decodes a Claude Code statusLine stdin-JSON payload.
//
// It is deliberately tolerant: unknown top-level or nested fields are
// silently ignored (encoding/json's default behavior — no explicit
// DisallowUnknownFields), and any field the payload DOES supply is kept
// even when others are missing (a payload with only "session_id" yields
// an Input with SessionID set and every other field nil).
//
// When data is not valid JSON at all (malformed, truncated, or piped from
// something that isn't Claude Code), ParseInput returns a non-nil error
// AND a usable zero-value Input — the caller is never forced to treat a
// parse failure as fatal; it can render the degraded wordmark-only line
// and move on (plan §2.3's fail-open contract extends to this parse
// step, not just the daemon HTTP path).
func ParseInput(data []byte) (Input, error) {
	var in Input
	if err := json.Unmarshal(data, &in); err != nil {
		return Input{}, fmt.Errorf("statusline.ParseInput: %w", err)
	}
	return in, nil
}
