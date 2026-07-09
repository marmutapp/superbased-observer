package kirocli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/marmutapp/superbased-observer/internal/adapter/transcriptutil"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/platform/sqlitedsn"
)

// ReadTranscript implements handoffsvc.TranscriptReader — the
// session-handoff transcript tier (docs/session-handoff.md). Kiro has
// no proxy tier, so the transcript is re-read from whichever store the
// session lives in: the flat-bundle `.jsonl` (interactive) or the
// SQLite conversations_v2 row (non-interactive). The session id
// resolves the flat bundle by filename; for the SQLite store it
// matches conversations_v2.conversation_id.
func (a *Adapter) ReadTranscript(ctx context.Context, sess models.Session, sourceHints []string) ([]models.TranscriptMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Prefer a flat bundle: a hint or a watch-root `<id>.jsonl`.
	if path, ok := a.flatJSONLPath(sess.ID, sourceHints); ok {
		return a.flatTranscript(path)
	}
	// Otherwise look for the conversation in a kiro-cli data.sqlite3.
	if path, ok := a.stateDBPath(sourceHints); ok {
		return a.sqliteTranscript(ctx, path, sess.ID)
	}
	return nil, fmt.Errorf("kirocli.ReadTranscript: no flat bundle or data.sqlite3 for session %s", sess.ID)
}

func (a *Adapter) flatJSONLPath(sessionID string, hints []string) (string, bool) {
	name := sessionID + ".jsonl"
	for _, h := range hints {
		if filepath.Base(h) == name && fileReadable(h) {
			return h, true
		}
		// A `.json` hint resolves to its `.jsonl` sibling.
		if filepath.Base(h) == sessionID+".json" {
			jl := strings.TrimSuffix(h, ".json") + ".jsonl"
			if fileReadable(jl) {
				return jl, true
			}
		}
	}
	for _, root := range a.roots {
		if strings.Contains(filepath.ToSlash(root), "/.kiro/sessions/cli") {
			p := filepath.Join(root, name)
			if fileReadable(p) {
				return p, true
			}
		}
	}
	return "", false
}

func (a *Adapter) stateDBPath(hints []string) (string, bool) {
	for _, h := range hints {
		if strings.EqualFold(filepath.Base(h), "data.sqlite3") && fileReadable(h) {
			return h, true
		}
	}
	for _, root := range a.roots {
		if strings.Contains(strings.ToLower(filepath.ToSlash(root)), "kiro-cli") {
			p := filepath.Join(root, "data.sqlite3")
			if fileReadable(p) {
				return p, true
			}
		}
	}
	return "", false
}

// flatTranscript builds the normalized transcript from a flat-bundle
// `.jsonl` stream. The stream carries only Prompt / AssistantMessage
// text records; tool uses in interactive sessions are not written to
// the stream (they live in the SQLite store for non-interactive runs).
func (a *Adapter) flatTranscript(jsonlPath string) ([]models.TranscriptMessage, error) {
	body, err := os.ReadFile(jsonlPath) //nolint:gosec // path derives from watch root / validated hint
	if err != nil {
		return nil, fmt.Errorf("kirocli.ReadTranscript: %w", err)
	}
	b := transcriptutil.New()
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		var sl flatStreamLine
		if json.Unmarshal([]byte(line), &sl) != nil {
			continue
		}
		text := flatText(sl.Data.Content)
		switch sl.Kind {
		case "Prompt":
			b.SetNextID(sl.Data.MessageID)
			b.User(text, unixSeconds(sl.Data.Meta.Timestamp))
		case "AssistantMessage":
			b.SetNextID(sl.Data.MessageID)
			b.AssistantText(text, "", unixSeconds(sl.Data.Meta.Timestamp))
		}
	}
	return b.Finish(), nil
}

// sqliteTranscript builds the transcript from a conversations_v2 row
// matching conversation_id == sessionID. Tool uses + their results fold
// into the owning assistant exchange.
func (a *Adapter) sqliteTranscript(ctx context.Context, dbPath, sessionID string) ([]models.TranscriptMessage, error) {
	staged, err := stageMirrorIfForeign(canonicalDBPath(dbPath))
	if err != nil {
		return nil, fmt.Errorf("kirocli.ReadTranscript: %w", err)
	}
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(2000)",
		sqlitedsn.Escape(staged))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("kirocli.ReadTranscript: open: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	var value string
	err = db.QueryRowContext(ctx,
		"SELECT value FROM conversations_v2 WHERE conversation_id = ? ORDER BY updated_at DESC LIMIT 1",
		sessionID).Scan(&value)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("kirocli.ReadTranscript: no conversations_v2 row for %s", sessionID)
	}
	if err != nil {
		return nil, fmt.Errorf("kirocli.ReadTranscript: query: %w", err)
	}

	var conv sqliteConv
	if err := json.Unmarshal([]byte(value), &conv); err != nil {
		return nil, fmt.Errorf("kirocli.ReadTranscript: parse value: %w", err)
	}
	results := indexToolResults(conv)

	b := transcriptutil.New()
	for _, h := range conv.History {
		ts := entryTimestamp(h)
		if h.User.Content.Prompt != nil {
			b.User(h.User.Content.Prompt.Prompt, ts)
		}
		switch {
		case h.Assistant.ToolUse != nil:
			b.SetNextID(h.Assistant.ToolUse.MessageID)
			for _, tu := range h.Assistant.ToolUse.ToolUses {
				b.AssistantCall(tu.ID, tu.Name, string(tu.Args), h.Request.ModelID, ts)
				if rr, ok := results[tu.ID]; ok {
					b.Resolve(tu.ID, rr.output, ts)
				}
			}
		case h.Assistant.Response != nil:
			b.SetNextID(h.Assistant.Response.MessageID)
			b.AssistantText(h.Assistant.Response.Content, h.Request.ModelID, ts)
		}
	}
	return b.Finish(), nil
}

func fileReadable(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
