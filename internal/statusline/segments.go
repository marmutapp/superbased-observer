package statusline

import (
	"fmt"
	"strconv"
)

// DaemonTile is the small subset of the lean `GET /api/statusline`
// daemon response (plan §2.2) that this package needs to render the
// "today" segment. Every field is a pointer so an unreachable, timed-out,
// or never-started daemon (plan §2.3's fail-open ladder) is representable
// as a fully zero-value DaemonTile — every segment sourced from it simply
// omits itself, never renders a fabricated "$0.00" or "0%".
type DaemonTile struct {
	// TodayUSD is today's total observed spend across every captured
	// tool, priced through the cost engine (not a raw-column SUM — see
	// the plan's WP0 status block correction: the raw
	// estimated_cost_usd column is unpopulated for major sources).
	TodayUSD *float64
	// SessionUSD is the daemon's own view of the current session's
	// spend, when the caller supplied a session_id and the daemon found
	// a match. The "session" segment prefers Input.Cost.TotalCostUSD
	// (Claude Code's own figure, no lookup needed) when present, and
	// only falls back to this field otherwise.
	SessionUSD *float64
	// CacheReadShare is the fraction [0,1] of today's input-side tokens
	// served from prefix cache. Not rendered by any default segment
	// today; carried for a future segment kind without requiring a
	// DaemonTile shape change (CLAUDE.md #6, additive).
	CacheReadShare *float64
}

// The segment-kind vocabulary accepted by RenderOptions.Segments and
// the [statusline].segments config key (plan §1.2, §6.3). Unknown
// strings in a Segments list are skipped silently — a typo in a config
// file degrades the line, it never panics or errors the command (plan
// §2.3's "never regress" fail-open discipline extended to config
// parsing).
const (
	// SegmentWordmark renders Wordmark. Unlike every other segment kind,
	// its datum (the constant itself) is never absent, AND (as of the F12
	// fix) Render forces it present exactly once at the front of every
	// composition regardless of whether, where, or how many times the
	// caller's Segments list mentions it — there is no way for a caller
	// to produce a wordmark-less or wordmark-reordered line. See
	// Render's doc comment / normalizeSegments for the enforcement.
	SegmentWordmark = "wordmark"
	// SegmentSession renders the current session's observed spend,
	// sourced from Input.Cost.TotalCostUSD (Claude Code's own figure)
	// when present, DaemonTile.SessionUSD otherwise.
	SegmentSession = "session"
	// SegmentToday renders DaemonTile.TodayUSD.
	SegmentToday = "today"
	// SegmentModel renders the current model's display name (falling
	// back to its raw id), sourced from Input.Model.
	SegmentModel = "model"
)

// DefaultSegments is the default ordered segment list (plan §1.2 "wordmark,
// session, today, model" / §4.1 / the WP0 status block's provisional
// decision lock for §9.2/§9.3: the full set including "today"). Render
// falls back to this list whenever RenderOptions.Segments is empty, so a
// caller need not repeat the default just to get it.
var DefaultSegments = []string{SegmentWordmark, SegmentSession, SegmentToday, SegmentModel}

// maxModelWidth is the longest a model-name segment is allowed to display
// before truncation with an ellipsis — long enough for the model names
// this product actually prices (e.g. "claude-opus-4-5-20260101" at 24
// runes), short enough that one long id can't dominate a one-line status
// bar the way it's harmless to dominate a multi-row table column.
const maxModelWidth = 24

// segment is one already-rendered piece of the output line, plus whether
// it's the wordmark. joinSegments (render.go) special-cases the wordmark:
// it joins to its neighbor with a plain space instead of " · " only when
// Wordmark's own text already ends in the separator glyph (a capability
// check, not a hardcoded assumption about any one wordmark's shape).
type segment struct {
	text       string
	isWordmark bool
}

// renderSessionSegment renders the "session" segment kind: "<$X.XX>
// session". It prefers the stdin-JSON session cost (Claude Code already
// computed this figure server-side; no lookup, no honesty caveat beyond
// the product's existing list-price framing — plan §4.1) and falls back
// to the daemon's own session-scoped figure when stdin didn't supply one.
// Returns ok=false (segment omitted) when neither source has a value.
func renderSessionSegment(in Input, tile DaemonTile) (string, bool) {
	if in.Cost != nil && in.Cost.TotalCostUSD != nil {
		return formatUSD(*in.Cost.TotalCostUSD) + " session", true
	}
	if tile.SessionUSD != nil {
		return formatUSD(*tile.SessionUSD) + " session", true
	}
	return "", false
}

// renderTodaySegment renders the "today" segment kind: "<$X.XX> today",
// sourced from the daemon tile. Returns ok=false (segment omitted) when
// the daemon path never produced a figure (plan §2.3 cases 1/2/3/4).
func renderTodaySegment(tile DaemonTile) (string, bool) {
	if tile.TodayUSD == nil {
		return "", false
	}
	return formatUSD(*tile.TodayUSD) + " today", true
}

// renderModelSegment renders the "model" segment kind: the model's
// display name (preferred) or raw id, truncated per maxModelWidth.
// Returns ok=false (segment omitted) when no model information at all
// was supplied.
func renderModelSegment(in Input) (string, bool) {
	name := modelName(in)
	if name == "" {
		return "", false
	}
	return truncateModel(name), true
}

// modelName resolves the best available model label: DisplayName when
// present and non-empty (after sanitization — see below), ID otherwise,
// "" when neither is available.
//
// This is the ONE place externally-sourced text (Input.Model's fields,
// sourced from Claude Code's stdin JSON) enters this package's rendered
// output, so it is also the sanitization boundary (F1): both fields are
// passed through sanitizeControls before being considered, so a
// DisplayName of "safe\nERROR" resolves to the single-line "safe ERROR"
// rather than reaching the caller — and never with control characters
// still embedded — regardless of what a future caller does with the
// return value. A DisplayName that sanitizes down to the empty string
// (e.g. it was control characters only) falls through to ID exactly like
// an originally-empty DisplayName does.
func modelName(in Input) string {
	if in.Model == nil {
		return ""
	}
	if in.Model.DisplayName != nil {
		if name := sanitizeControls(*in.Model.DisplayName); name != "" {
			return name
		}
	}
	if in.Model.ID != nil {
		return sanitizeControls(*in.Model.ID)
	}
	return ""
}

// truncateModel shortens s to at most maxModelWidth runes, replacing the
// tail with a single "…" when it doesn't fit — matching the
// truncate-with-ellipsis convention internal/oneshot uses for its MODEL
// table column (a fresh, independent implementation per plan §6.2's
// "small independent copy is fine" call, not a shared import).
func truncateModel(s string) string {
	r := []rune(s)
	if len(r) <= maxModelWidth {
		return s
	}
	return string(r[:maxModelWidth-1]) + "…"
}

// formatUSD renders a dollar amount for the narrow statusline format:
// two decimal places below $100 ("$3.42" — cent-level precision where it
// matters most, the small figures a status bar shows most often), whole
// dollars at or above $100 ("$1235" — keeping the segment short once a
// session's or a day's spend grows large enough that cents stop being
// the interesting digit). Values are rounded to the nearest cent/dollar,
// never truncated (unlike internal/oneshot's token-count formatter — a
// dollar total is already a sum of rounded per-turn costs, not a place
// where truncation avoids a spurious unit-boundary crossing).
func formatUSD(v float64) string {
	abs := v
	if abs < 0 {
		abs = -abs
	}
	if abs < 100 {
		return "$" + strconv.FormatFloat(v, 'f', 2, 64)
	}
	return fmt.Sprintf("$%.0f", v)
}
