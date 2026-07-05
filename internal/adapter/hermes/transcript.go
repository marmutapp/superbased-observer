package hermes

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter/transcriptutil"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/platform/sqlitedsn"
)

// Transcript reading (session handoff, docs/session-handoff.md P2).
// Hermes keeps the conversation in state.db's messages table (schema
// v14+): role user/assistant/tool rows with `active=1` marking the live
// (non-compacted) stream. Assistant rows carry narration in `content`
// plus a tool_calls JSON array (toolCallWrapper); role='tool' rows carry
// the structured result in `content` and pair by tool_call_id.
// reasoning/reasoning_content are Hermes's thinking — dropped (plan §8).
// Timestamps are REAL unix seconds. Messages carry no per-row model —
// the boundary falls back to the session's model.
func (a *Adapter) ReadTranscript(ctx context.Context, sess models.Session, sourceHints []string) ([]models.TranscriptMessage, error) {
	path, err := a.stateDBPath(sourceHints)
	if err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(2000)",
		sqlitedsn.Escape(path))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("hermes.ReadTranscript: open: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	return readSessionTranscript(ctx, db, sess.ID)
}

// stateDBPath resolves state.db: a usable hint first, else the first
// watch root holding one (D-P0.1 — hook-fed sessions carry the
// "hermes:hook" sentinel, so derivation is the primary lookup).
func (a *Adapter) stateDBPath(hints []string) (string, error) {
	for _, h := range hints {
		if strings.HasSuffix(h, "state.db") && stateDBExists(h) {
			return h, nil
		}
	}
	for _, root := range a.roots {
		p := filepath.Join(root, "state.db")
		if stateDBExists(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("hermes.ReadTranscript: no state.db under any watch root")
}

func readSessionTranscript(ctx context.Context, db *sql.DB, sessionID string) ([]models.TranscriptMessage, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT role, COALESCE(content, ''), COALESCE(tool_call_id, ''),
		       COALESCE(tool_calls, ''), timestamp
		  FROM messages
		 WHERE session_id = ? AND active = 1
		 ORDER BY id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("hermes.ReadTranscript: query: %w", err)
	}
	defer rows.Close()

	b := transcriptutil.New()
	for rows.Next() {
		var role, content, callID, callsRaw string
		var stamp float64
		if err := rows.Scan(&role, &content, &callID, &callsRaw, &stamp); err != nil {
			return nil, err
		}
		ts := unixSecondsTime(stamp)
		switch role {
		case "user":
			b.User(content, ts)
		case "assistant":
			b.AssistantText(content, "", ts)
			calls, err := parseToolCalls(callsRaw)
			if err != nil {
				continue // malformed tool_calls JSON: keep the narration, skip the calls
			}
			for _, c := range calls {
				b.AssistantCall(c.ID, c.Function.Name, c.Function.Arguments, "", ts)
			}
		case "tool":
			// Structured JSON result — excerpted verbatim; the builder caps it.
			b.Resolve(callID, content, ts)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return b.Finish(), nil
}

func unixSecondsTime(s float64) time.Time {
	if s <= 0 {
		return time.Time{}
	}
	sec, frac := math.Modf(s)
	return time.Unix(int64(sec), int64(frac*1e9)).UTC()
}

func stateDBExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
