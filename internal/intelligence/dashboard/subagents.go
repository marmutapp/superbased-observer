package dashboard

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// subagents.go — the session-detail sub-agents read model.
//
// The claude-code sub-agent model (migration 010, commit 54a51540) keeps
// every sub-agent's activity on the PARENT's session row flagged
// is_sidechain=1, bracketed by spawn_subagent / subagent_start /
// subagent_stop lifecycle actions. This file turns that flat material into
// per-sub-agent summaries for GET /api/session/<id>/subagents and the
// SessionDetailPanel's Sub-agents section.
//
// Since migration 087 token_usage rows carry the same flag, so the builder
// also accepts sidechain usage rows and folds their tokens + cost into the
// windows — closing commit ad46b05b's honest omission (activity without
// attribution of what it cost).
//
// Grouping rule (pure): a window OPENS on each spawn/start bracket and
// CLOSES on its stop bracket (or stays open — an unterminated sub-agent).
// Sidechain rows inside the open window belong to it.
//
// RETROSPECTIVE WINDOWS (C2 fix, operator-approved 2026-08-22): claude-code
// fires SubagentStop per sub-agent but has NO SubagentStart hook, so
// stop-without-open is the COMMON case, not an anomaly. Each such stop now
// claims the sidechain activity accumulated since the last bracket boundary
// as its own closed window, labeled by the stop's agent_id — instead of
// stranding ~2133/2996 actions of session 1e2f0aa0 in the unattributed
// bucket. A stop with no agent_id and no accumulated activity is still
// skipped (nothing to name, nothing to claim).
//
// Sidechain rows outside any window (before the first stop / after the last
// bracket of any kind) still land in ONE explicit unattributed bucket rather
// than being silently dropped. Label precedence: structured metadata agent_id
// → bracket Target (agent_type / persona name) → ordinal.
//
// Token rows never move the bracket state — only actions open/close windows.
// Each usage row is replayed against the same bracket timeline the action
// pass walked, applying every bracket at or before the row's timestamp;
// retroactive windows emit their open-mark at their computed onset so tokens
// in [onset, stop] bind to the labeled window. A row outside any window lands
// in the same unattributed bucket.

// SubagentSummary is one sub-agent's rolled-up window.
type SubagentSummary struct {
	// ID is the structured agent identity when capture stamped one (hook
	// agent_id / transcript agentName); "" for time-window-only grouping.
	ID string `json:"id,omitempty"`
	// Label is what the UI shows: the best identity available, falling back
	// to an ordinal ("sub-agent 2").
	Label string `json:"label"`
	// Type is the categorical agent type when known ("Explore",
	// "general-purpose", persona names).
	Type string `json:"type,omitempty"`
	// Start bounds the window; End is zero while still open.
	Start time.Time `json:"start"`
	End   time.Time `json:"end,omitempty"`
	// Open marks a window with no stop bracket yet — the sub-agent may
	// still be running (or its stop event was suppressed as empty-shell).
	Open bool `json:"open"`
	// ActionCount is the sidechain actions attributed to this window,
	// EXCLUDING the bracket rows themselves.
	ActionCount int `json:"action_count"`
	// ErrorCount is the attributed actions with success=false.
	ErrorCount int `json:"error_count"`
	// InputTokens / OutputTokens / CacheReadTokens sum the sidechain
	// token_usage rows attributed to this window (migration 087). Zero for
	// installs whose transcripts predate the flag until a re-ingest heals
	// them (`observer scan --force`).
	InputTokens     int64 `json:"input_tokens,omitempty"`
	OutputTokens    int64 `json:"output_tokens,omitempty"`
	CacheReadTokens int64 `json:"cache_read_tokens,omitempty"`
	// CostUSD sums estimated_cost_usd over the same attributed rows. The
	// JSON omits zero so pre-heal installs render exactly as before.
	CostUSD float64 `json:"cost_usd,omitempty"`
}

// SessionSubagentsResponse is the /api/session/<id>/subagents payload.
//
// Since migration 087 each summary also carries the token/cost rollup of
// the sidechain token_usage rows inside its window (input_tokens /
// output_tokens / cache_read_tokens / cost_usd, all omitempty — zero until
// a post-087 ingest or a `observer scan --force` re-parse heals the rows).
type SessionSubagentsResponse struct {
	SessionID string            `json:"session_id"`
	Total     int               `json:"total"`
	Subagents []SubagentSummary `json:"subagents"`
}

// bracketMark records one mutation of the open-window state during the
// action pass, so the token pass can replay the SAME timeline without
// re-walking the actions: open != nil means "window w opened at ts";
// open == nil means "the then-current window closed at ts".
type bracketMark struct {
	ts   time.Time
	open *subagentWindow
}

// subagentWindow is one accumulator: its summary plus the ordinal that
// labels time-window-only groupings ("sub-agent 2").
type subagentWindow struct {
	summary SubagentSummary
	ordinal int
}

// buildSubagentSummaries groups chronological sidechain material into
// per-sub-agent windows. Exported for tests; the handler is a thin wrapper.
// refs drive the window structure (brackets + activity counts); tokens are
// pure payload folded into whichever window is open at their timestamp.
func buildSubagentSummaries(refs []models.SubagentActionRef, tokens []models.SubagentTokenRef) []SubagentSummary {
	var windows []*subagentWindow
	var unattributed *subagentWindow // current retrospective-claimable segment
	var unattributedAggregate *subagentWindow
	var cur *subagentWindow
	var marks []bracketMark
	var lastBoundary time.Time // ts of the last bracket of any kind

	openWindow := func(ts time.Time) *subagentWindow {
		w := &subagentWindow{summary: SubagentSummary{Start: ts, Open: true}, ordinal: len(windows) + 1}
		windows = append(windows, w)
		marks = append(marks, bracketMark{ts: ts, open: w})
		lastBoundary = ts
		return w
	}
	labelFor := func(w *subagentWindow) {
		if w.summary.Label != "" {
			return
		}
		if w.summary.ID != "" {
			w.summary.Label = w.summary.ID
			return
		}
		if w.ordinal > 0 {
			w.summary.Label = fmt.Sprintf("sub-agent %d", w.ordinal)
		}
	}
	stashUnattributed := func() {
		if unattributed == nil {
			return
		}
		if unattributedAggregate == nil {
			unattributedAggregate = unattributed
		} else if unattributedAggregate != unattributed {
			dst, src := &unattributedAggregate.summary, &unattributed.summary
			if dst.Start.IsZero() || (!src.Start.IsZero() && src.Start.Before(dst.Start)) {
				dst.Start = src.Start
			}
			if dst.End.Before(src.End) {
				dst.End = src.End
			}
			dst.ActionCount += src.ActionCount
			dst.ErrorCount += src.ErrorCount
			// Leave the superseded window empty so the final payload filter
			// removes it; all unattributed segments render as one summary.
			src.ActionCount = 0
			src.ErrorCount = 0
		}
		unattributed = nil
	}

	for _, ref := range refs {
		switch ref.ActionType {
		case models.ActionSpawnSubagent, models.ActionSubagentStart:
			cur = openWindow(ref.Timestamp)
			if ref.Metadata != nil && ref.Metadata.AgentID != "" {
				cur.summary.ID = ref.Metadata.AgentID
			}
			if ref.Target != "" {
				cur.summary.Type = ref.Target
			}
			labelFor(cur)
		case models.ActionSubagentStop:
			if cur != nil && cur.summary.Open && cur != unattributed {
				// Forward path: a seen/open start bracket closes normally.
				if cur.summary.ID == "" && ref.Metadata != nil && ref.Metadata.AgentID != "" {
					cur.summary.ID = ref.Metadata.AgentID
				}
				if cur.summary.Type == "" && ref.Target != "" {
					cur.summary.Type = ref.Target
				}
				labelFor(cur)
				cur.summary.End = ref.Timestamp
				cur.summary.Open = false
				marks = append(marks, bracketMark{ts: ref.Timestamp})
				lastBoundary = ref.Timestamp
				cur = nil
				continue
			}
			// Retrospective path (C2): stop-without-open claims the
			// sidechain activity accumulated since the last boundary as
			// this agent's closed window. The pending unattributed bucket
			// IS that accumulation — convert it wholesale so its Start
			// (first row after the boundary) becomes the window onset.
			agentID := ""
			if ref.Metadata != nil {
				agentID = ref.Metadata.AgentID
			}
			// The bucket is only claimable when it holds POST-boundary
			// rows; a bucket predating the last boundary (e.g. activity
			// before a real start) stays an unattributed leftover and a
			// fresh bucket forms for whatever follows.
			claimable := unattributed != nil &&
				(lastBoundary.IsZero() || !unattributed.summary.Start.Before(lastBoundary))
			if !claimable {
				if unattributed != nil && !lastBoundary.IsZero() &&
					!unattributed.summary.Start.After(lastBoundary) {
					stashUnattributed() // detach the stale leftover
				}
				if agentID == "" {
					continue // nothing to claim and nothing to name
				}
				w := &subagentWindow{
					summary: SubagentSummary{Start: ref.Timestamp},
					ordinal: len(windows) + 1,
				}
				windows = append(windows, w)
				w.summary.ID = agentID
				labelFor(w)
				w.summary.End = ref.Timestamp
				w.summary.Open = false
				marks = append(marks,
					bracketMark{ts: w.summary.Start, open: w},
					bracketMark{ts: ref.Timestamp})
				lastBoundary = ref.Timestamp
				continue
			}
			var w *subagentWindow
			if unattributed != nil {
				w = unattributed
				unattributed = nil // next activity starts a fresh bucket
			} else {
				w = &subagentWindow{
					summary: SubagentSummary{Start: ref.Timestamp},
					ordinal: len(windows) + 1,
				}
				windows = append(windows, w)
			}
			cur = nil
			w.summary.ID = agentID
			if w.summary.Type == "" && ref.Target != "" {
				w.summary.Type = ref.Target
			}
			if w.ordinal == 0 {
				w.ordinal = len(windows)
			}
			// The bucket's Label must not survive the conversion — and it
			// must be cleared BEFORE labelFor, which early-returns on any
			// existing label.
			if strings.HasPrefix(w.summary.Label, "unattributed") {
				w.summary.Label = ""
			}
			labelFor(w)
			w.summary.End = ref.Timestamp
			w.summary.Open = false
			// Token replay: open at the claimed onset, close at the stop.
			marks = append(marks,
				bracketMark{ts: w.summary.Start, open: w},
				bracketMark{ts: ref.Timestamp})
			lastBoundary = ref.Timestamp
		default:
			if !ref.IsSidechain {
				continue
			}
			if cur == nil || !cur.summary.Open {
				// Outside any bracket: keep it visible in one explicit
				// unattributed bucket instead of dropping it. A bucket that
				// began at or before the most recent bracket belongs to the
				// earlier segment and must not absorb post-boundary activity.
				// Refs are ordered, so an activity row encountered after a
				// same-timestamp bracket is unambiguously on the new side.
				if unattributed != nil && !lastBoundary.IsZero() &&
					!unattributed.summary.Start.After(lastBoundary) {
					stashUnattributed()
				}
				if unattributed == nil {
					unattributed = &subagentWindow{summary: SubagentSummary{
						Label: "unattributed sub-agent activity",
						Start: ref.Timestamp,
						Open:  true,
					}}
					windows = append(windows, unattributed)
				}
				cur = unattributed
			}
			cur.summary.ActionCount++
			if !ref.Success {
				cur.summary.ErrorCount++
			}
			if cur.summary.End.Before(ref.Timestamp) {
				cur.summary.End = ref.Timestamp
			}
		}
	}
	// Any final unclaimed segment joins the one visible unattributed summary.
	// Keeping the claimable segment separate until now is what lets a trailing
	// stop name it without reaching back across an earlier bracket boundary.
	stashUnattributed()
	unattributed = unattributedAggregate

	// Token pass: replay the bracket timeline recorded above. Marks at or
	// before a row's timestamp have applied by the time it lands — the same
	// open-window rule the action pass uses (a stop sharing the row's exact
	// timestamp closes first, so such a row is outside — cross-table
	// timestamp ties carry no sub-row ordering to recover).
	mi := 0
	cur = nil
	for _, tok := range tokens {
		for mi < len(marks) && !marks[mi].ts.After(tok.Timestamp) {
			if marks[mi].open != nil {
				cur = marks[mi].open
			} else {
				// A close nulls cur directly; do NOT consult
				// cur.summary.Open here — the action pass above has
				// already flipped every closed window's flag to false,
				// so that check would misattribute tokens inside an
				// already-closed window to the unattributed bucket.
				cur = nil
			}
			mi++
		}
		if cur == nil {
			if unattributed == nil {
				unattributed = &subagentWindow{summary: SubagentSummary{
					Label: "unattributed sub-agent activity",
					Start: tok.Timestamp,
					Open:  true,
				}}
				windows = append(windows, unattributed)
			} else if unattributed.summary.Start.After(tok.Timestamp) {
				unattributed.summary.Start = tok.Timestamp
			}
			cur = unattributed
		}
		cur.summary.InputTokens += tok.InputTokens
		cur.summary.OutputTokens += tok.OutputTokens
		cur.summary.CacheReadTokens += tok.CacheReadTokens
		cur.summary.CostUSD += tok.EstimatedCostUSD
	}

	out := make([]SubagentSummary, 0, len(windows))
	for _, w := range windows {
		s := w.summary
		// An empty bracket pair carries no story — unless tokens landed in
		// it (a usage-only window, e.g. hook events suppressed but usage
		// rows captured, must stay visible).
		if s.ActionCount == 0 && s.InputTokens == 0 && s.OutputTokens == 0 &&
			s.CacheReadTokens == 0 && s.CostUSD == 0 && s.ID == "" && s.Type == "" {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// handleSessionSubagents serves GET /api/session/<id>/subagents.
func (s *Server) handleSessionSubagents(w http.ResponseWriter, r *http.Request, sessionID string) {
	if sessionID == "" {
		http.Error(w, "missing session id", http.StatusBadRequest)
		return
	}
	st := store.New(s.db())
	refs, err := st.SidechainActionsForSession(r.Context(), sessionID)
	if err != nil {
		http.Error(w, fmt.Sprintf("load sidechain actions: %v", err), http.StatusInternalServerError)
		return
	}
	tokens, err := st.SidechainTokenUsageForSession(r.Context(), sessionID)
	if err != nil {
		http.Error(w, fmt.Sprintf("load sidechain token usage: %v", err), http.StatusInternalServerError)
		return
	}
	subagents := buildSubagentSummaries(refs, tokens)
	if subagents == nil {
		subagents = []SubagentSummary{}
	}
	writeJSON(w, SessionSubagentsResponse{SessionID: sessionID, Total: len(subagents), Subagents: subagents})
}
