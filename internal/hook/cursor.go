package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter/cursor"
	"github.com/marmutapp/superbased-observer/internal/guard"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/policy"
	"github.com/marmutapp/superbased-observer/internal/scrub"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// CursorSink is the subset of *store.Store methods needed to record a single
// hook event. Defined as an interface so tests can fake it without a real
// SQLite database.
type CursorSink interface {
	Ingest(ctx context.Context, events []models.ToolEvent, tokens []models.TokenEvent, opts store.IngestOptions) (store.IngestResult, error)
	UpdateActionOutcome(ctx context.Context, sourceFile, sourceEventID string, success bool, errorMessage string, durationMs int64, toolOutput, toolName, target string) (int64, error)
}

// HandleCursorEvent reads a Cursor hook payload from stdin, replies on
// stdout immediately so the host tool never waits, then synchronously
// inserts the event into the observer DB with a strict deadline.
//
// The reply goes out FIRST so even if the insert fails or hits the timeout
// the host tool is unblocked. The deadline (default 250ms) caps how long
// the insert can take; spec §14.1 budgets the whole hook at 500ms.
//
// Spec P1: never break the host tool. All error paths log to stderr and
// return without panicking.
func HandleCursorEvent(eventName string, sink CursorSink, sc *scrub.Scrubber, stdin io.Reader, stdout, stderr io.Writer, deadline time.Duration) {
	HandleCursorEventGuarded(eventName, nil, nil, sink, sc, stdin, stdout, stderr, deadline)
}

// HandleCursorEventGuarded is HandleCursorEvent with the guard seam
// (guard spec §3.2 seam 1, G6): for pre-execution events
// (before-shell / before-mcp / before-read) it evaluates the payload
// BEFORE the reply and emits Cursor's documented permission JSON
// ("allow" | "deny" | "ask" — spec §6.2) instead of the unconditional
// allow. Capture proceeds REGARDLESS of the verdict — a denied
// attempt is still an attempt worth recording — and verdict
// persistence runs last (reply → capture → persist; the reply always
// goes out first per the §6.4 budget; the forensics JSONL row carries
// the verdict either way).
//
// gd nil (or a non-pre-execution event) degrades to exactly the
// unguarded behavior. persist is the same nil-tolerant lazy-DB
// callback shape HandleGuarded takes.
func HandleCursorEventGuarded(eventName string, gd guard.Evaluator, persist func(guard.ActionVerdict), sink CursorSink, sc *scrub.Scrubber, stdin io.Reader, stdout, stderr io.Writer, deadline time.Duration) {
	body, _ := io.ReadAll(io.LimitReader(stdin, 2*1024*1024))
	body = bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF})

	permission := "allow"
	var verdict guard.ActionVerdict
	recordWorthy := false
	if gd != nil {
		if pe, ok := BuildCursorEvent(eventName, body, sc); ok {
			verdict, recordWorthy = gd.EvaluateHook(pe)
			em := guard.ResolveEmission(verdict.Verdict, pe.Caps)
			verdict.Enforced = em.Enforced
			if em.DegradedFrom != "" {
				// Preserve the §6.3 "approved" marker when no §6.2
				// capability degradation applied.
				verdict.DegradedFrom = em.DegradedFrom
			}
			permission = em.Permission
			if recordWorthy {
				action := "approve+flag"
				if permission != "allow" {
					action = "guard:" + permission
				}
				appendHookEventLog(hookEvent{
					Event:        "cursor:" + eventName,
					Bytes:        len(body),
					SessionID:    pe.SessionID,
					Action:       action,
					RuleID:       verdict.Verdict.RuleID,
					Decision:     verdict.Verdict.Decision.String(),
					Severity:     verdict.Verdict.Severity.String(),
					DegradedFrom: em.DegradedFrom,
				})
			}
			if permission != "allow" {
				fmt.Fprintf(stderr, "observer-hook: cursor %s guard %s (%s: %s)\n",
					eventName, permission, verdict.Verdict.RuleID, verdict.Verdict.Reason)
			}
		}
	}

	// Reply first — Cursor tolerates extra fields, so a single response
	// shape covers all event types.
	_ = json.NewEncoder(stdout).Encode(cursorReply{
		Permission: permission,
		Continue:   true,
	})

	processCursorEvent(eventName, body, sink, sc, stderr, deadline)

	if recordWorthy && persist != nil {
		persist(verdict)
	}
}

// processCursorEvent is the post-reply capture half of the cursor
// hook: stop-event token+transcript ingestion, after-event outcome
// enrichment, and before-event row insertion.
func processCursorEvent(eventName string, body []byte, sink CursorSink, sc *scrub.Scrubber, stderr io.Writer, deadline time.Duration) {
	if eventName == cursor.EventStop {
		tk, ok, err := cursor.BuildStopTokenEvent(body)
		if err != nil {
			fmt.Fprintf(stderr, "observer-hook: cursor build %s: %v\n", eventName, err)
			return
		}
		if !ok {
			return
		}
		if tk.ProjectRoot == "" || tk.SessionID == "" {
			fmt.Fprintf(stderr, "observer-hook: cursor %s missing project_root or session_id\n", eventName)
			return
		}
		// Auto-mode sessions report the placeholder model "default" in the
		// hook body; the real model (e.g. "composer-2.5") only lives in the
		// conversation's store.db turn blobs. Resolve it so token rows get
		// the concrete model the cost engine can price. Gated on the
		// sentinel, so explicit-model sessions pay no store.db I/O.
		if tk.Model == "" || strings.EqualFold(tk.Model, "default") || strings.EqualFold(tk.Model, "auto") {
			if home, err := os.UserHomeDir(); err == nil {
				if m := cursor.ResolveModelFromStore(home, tk.SessionID); m != "" {
					tk.Model = m
				}
			}
		}
		if deadline <= 0 {
			deadline = 250 * time.Millisecond
		}
		events, err := cursor.BuildStopTranscriptEvents(body, sc, tk.Timestamp)
		if err != nil {
			fmt.Fprintf(stderr, "observer-hook: cursor transcript %s: %v\n", eventName, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		defer cancel()
		if _, err := sink.Ingest(ctx, events, []models.TokenEvent{tk}, store.IngestOptions{}); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				fmt.Fprintf(stderr, "observer-hook: cursor %s insert deadline exceeded\n", eventName)
			} else {
				fmt.Fprintf(stderr, "observer-hook: cursor %s insert: %v\n", eventName, err)
			}
		}
		return
	}

	// After-events (afterShellExecution / afterMCPExecution / postToolUse)
	// don't insert a new row — they enrich the matching before-event row's
	// outcome fields in place via Store.UpdateActionOutcome. Dispatch
	// before falling through to BuildEvent so the after-event branch is
	// handled exactly once.
	if outcome, ok, err := cursor.BuildAfterOutcome(eventName, body); err != nil {
		fmt.Fprintf(stderr, "observer-hook: cursor build outcome %s: %v\n", eventName, err)
		return
	} else if ok {
		if deadline <= 0 {
			deadline = 250 * time.Millisecond
		}
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		defer cancel()
		n, err := sink.UpdateActionOutcome(
			ctx,
			outcome.SourceFile, outcome.SourceEventID,
			outcome.Success, outcome.ErrorMessage, outcome.DurationMs,
			outcome.Output, outcome.ToolName, outcome.Target,
		)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				fmt.Fprintf(stderr, "observer-hook: cursor %s outcome update deadline exceeded\n", eventName)
			} else {
				fmt.Fprintf(stderr, "observer-hook: cursor %s outcome update: %v\n", eventName, err)
			}
		} else if n == 0 {
			// Not necessarily an error — the before-row may not have
			// landed yet (rare race) or the pairing key didn't match.
			// Surface in stderr so dogfood can see the rate.
			fmt.Fprintf(stderr, "observer-hook: cursor %s outcome update touched 0 rows (sourceEventID=%s)\n",
				eventName, outcome.SourceEventID)
		}
		return
	}

	ev, ok, err := cursor.BuildEvent(eventName, body, sc)
	if err != nil {
		fmt.Fprintf(stderr, "observer-hook: cursor build %s: %v\n", eventName, err)
		return
	}
	if !ok {
		return
	}
	if ev.ProjectRoot == "" || ev.SessionID == "" {
		fmt.Fprintf(stderr, "observer-hook: cursor %s missing project_root or session_id\n", eventName)
		return
	}

	if deadline <= 0 {
		deadline = 250 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	if _, err := sink.Ingest(ctx, []models.ToolEvent{ev}, nil, store.IngestOptions{}); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			fmt.Fprintf(stderr, "observer-hook: cursor %s insert deadline exceeded\n", eventName)
		} else {
			fmt.Fprintf(stderr, "observer-hook: cursor %s insert: %v\n", eventName, err)
		}
		return
	}
}

// cursorReply is the response shape we emit. Cursor accepts both
// "permission" (for tool gating) and "continue" (for prompt gating); extra
// keys are ignored, so a single reply works across all events.
type cursorReply struct {
	Permission string `json:"permission,omitempty"`
	Continue   bool   `json:"continue,omitempty"`
}

// BuildCursorEvent extracts a policy.Event from a Cursor pre-execution
// hook payload, REUSING the adapter's own payload extraction
// (cursor.BuildEvent — spec §6.1: reuse the per-client extraction the
// receivers already do, don't duplicate). ok=false for non-pre-
// execution channels, unparsable payloads, and non-evaluable action
// kinds — the caller then behaves exactly like the unguarded receiver.
//
// Boundary notes:
//   - Capabilities come from the §6.5 conformance matrix lookup
//     (guard.CapabilitiesFor) — the data table is the single source
//     of per-channel truth, not local constants.
//   - Unlike Claude Code's PreToolUse, the cursor payload carries the
//     workspace root, so ProjectRoot is REAL here and the boundary
//     rules (R-150/151, T-502) are active pre-execution.
//   - The adapter extraction scrubs secrets from the target before we
//     see it; rule patterns match command/path STRUCTURE, which
//     scrubbing preserves (a redacted token never changes whether the
//     command is `rm -rf` or the path is under ~/.ssh).
func BuildCursorEvent(eventName string, body []byte, sc *scrub.Scrubber) (policy.Event, bool) {
	caps, known := guard.CapabilitiesFor(models.ToolCursor, "hook:"+eventName)
	if !known || !caps.PreExecution {
		return policy.Event{}, false
	}
	ev, ok, err := cursor.BuildEvent(eventName, body, sc)
	if err != nil || !ok {
		return policy.Event{}, false
	}
	kind, evaluable := guard.ClassifyActionType(ev.ActionType)
	if !evaluable {
		return policy.Event{}, false
	}
	return policy.Event{
		Kind:        kind,
		ActionType:  ev.ActionType,
		Tool:        models.ToolCursor,
		Target:      ev.Target,
		ProjectRoot: ev.ProjectRoot,
		SessionID:   ev.SessionID,
		Caps:        caps,
		Now:         time.Now().UTC(),
	}, true
}
