package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter/cursor"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/scrub"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// CursorSink is the subset of *store.Store methods needed to record a single
// hook event. Defined as an interface so tests can fake it without a real
// SQLite database.
type CursorSink interface {
	Ingest(ctx context.Context, events []models.ToolEvent, tokens []models.TokenEvent, opts store.IngestOptions) (store.IngestResult, error)
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
	body, _ := io.ReadAll(io.LimitReader(stdin, 2*1024*1024))
	body = bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF})

	// Reply first — Cursor tolerates extra fields, so a single response
	// shape covers all event types.
	_ = json.NewEncoder(stdout).Encode(cursorReply{
		Permission: "allow",
		Continue:   true,
	})

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
