package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter/codex"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/scrub"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// CodexSink is the subset of *store.Store methods needed to record a
// single codex hook event. Defined as an interface so tests can fake it
// without a real SQLite database.
type CodexSink interface {
	Ingest(ctx context.Context, events []models.ToolEvent, tokens []models.TokenEvent, opts store.IngestOptions) (store.IngestResult, error)
}

// HandleCodexEvent reads a Codex hook payload from stdin, replies on
// stdout immediately so the host tool never waits, then synchronously
// inserts the event into the observer DB with a strict deadline.
//
// The reply goes out FIRST so even if the insert fails or hits the
// timeout the host is unblocked. The deadline (default 250ms) caps how
// long the insert can take; spec §14.1 budgets the whole hook at 500ms.
//
// Spec P1: never break the host tool. All error paths log to stderr and
// return without panicking.
//
// Codex events come in two response classes per
// developers.openai.com/codex/hooks:
//
//   - Most events accept exit 0 with empty stdout as "success, no
//     action" — we reply with `{}` (a parseable empty JSON, the
//     universal "do nothing" response across permission, modification
//     and stop hook classes).
//   - The Stop event is documented as "JSON only (plain text invalid)"
//     — `{}` still satisfies that requirement.
//   - PermissionRequest accepts a permission decision; we reply with
//     `{}` to defer to codex's default approval flow rather than
//     overriding it.
func HandleCodexEvent(eventName string, sink CodexSink, sc *scrub.Scrubber, stdin io.Reader, stdout, stderr io.Writer, deadline time.Duration) {
	body, _ := io.ReadAll(io.LimitReader(stdin, 2*1024*1024))
	body = bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF})

	// Reply first — empty JSON object is a safe default across all
	// codex event classes.
	_ = json.NewEncoder(stdout).Encode(struct{}{})

	ev, ok, err := codex.BuildHookEvent(eventName, body, sc)
	if err != nil {
		fmt.Fprintf(stderr, "observer-hook: codex build %s: %v\n", eventName, err)
		return
	}
	if !ok {
		return
	}
	if ev.SessionID == "" {
		fmt.Fprintf(stderr, "observer-hook: codex %s missing session_id\n", eventName)
		return
	}

	if deadline <= 0 {
		deadline = 250 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	if _, err := sink.Ingest(ctx, []models.ToolEvent{ev}, nil, store.IngestOptions{}); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			fmt.Fprintf(stderr, "observer-hook: codex %s insert deadline exceeded\n", eventName)
		} else {
			fmt.Fprintf(stderr, "observer-hook: codex %s insert: %v\n", eventName, err)
		}
	}
}
