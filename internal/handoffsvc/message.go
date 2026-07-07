// message.go — the get_session_message pull: re-read ONE message of a
// source session un-excerpted, on demand, from the source tool's own
// files. Nothing is persisted (the observer DB stays content-free per the
// CLAUDE.md Don'ts); the bytes come straight off disk each call.

package handoffsvc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// MessageRequest addresses one message of a source session for an
// un-excerpted re-read (the get_session_message MCP tool). MessageID (the
// source per-message id — claude-code uuid, surfaced in the handover as
// `[msg <id>]`) is preferred; Index is the 0-based fallback when no id is
// given (Index < 0 means "unset").
type MessageRequest struct {
	SessionID string
	MessageID string
	Index     int
}

// MessageResult is one un-excerpted message plus locating metadata. Found
// is false when the id/index matched no message (the caller reports a
// clean not-found, never an error).
type MessageResult struct {
	SessionID  string
	Tool       string
	Found      bool
	Message    models.TranscriptMessage
	Total      int
	FullBodies bool
}

// ReadMessage re-reads the source session's transcript un-excerpted and
// returns the single message addressed by req (by id, else by index).
// Dispatches through the shared reader (FullTranscriptReader when the
// adapter has it, capped fallback otherwise — FullBodies reports which).
// The returned content is scrubbed with deps.Scrub, matching the handover
// doc's privacy posture; nothing is written or persisted.
func ReadMessage(ctx context.Context, deps Deps, req MessageRequest) (MessageResult, error) {
	sub, err := deps.Store.LoadHandoffSubstrate(ctx, req.SessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return MessageResult{}, fmt.Errorf("%w: %q", ErrSessionNotFound, req.SessionID)
	}
	if err != nil {
		return MessageResult{}, fmt.Errorf("handoffsvc.ReadMessage: %w", err)
	}

	msgs, usedFull, note := readSessionTranscript(ctx, deps.Adapters, sub.Session, sub.SourceFiles, true)
	if len(msgs) == 0 {
		return MessageResult{SessionID: req.SessionID, Tool: sub.Session.Tool},
			fmt.Errorf("no readable transcript for session %q (%s)", req.SessionID, note)
	}

	res := MessageResult{
		SessionID:  req.SessionID,
		Tool:       sub.Session.Tool,
		Total:      len(msgs),
		FullBodies: usedFull,
	}

	var hit *models.TranscriptMessage
	switch {
	case req.MessageID != "":
		for i := range msgs {
			if msgs[i].ID == req.MessageID {
				hit = &msgs[i]
				break
			}
		}
	case req.Index >= 0 && req.Index < len(msgs):
		hit = &msgs[req.Index]
	}
	if hit == nil {
		return res, nil // Found stays false — a clean not-found
	}

	m := *hit
	if deps.Scrub != nil {
		m.Text = deps.Scrub(m.Text)
		calls := make([]models.ToolCallRef, len(m.ToolCalls))
		for i, c := range m.ToolCalls {
			c.InputExcerpt = deps.Scrub(c.InputExcerpt)
			c.ResultExcerpt = deps.Scrub(c.ResultExcerpt)
			calls[i] = c
		}
		m.ToolCalls = calls
	}
	res.Message = m
	res.Found = true
	return res, nil
}
