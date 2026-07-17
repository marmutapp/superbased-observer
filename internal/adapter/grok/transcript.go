package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter/transcriptutil"
	"github.com/marmutapp/superbased-observer/internal/models"
)

// chatRecord is one JSONL line of a grok session's chat_history.jsonl —
// the canonical conversation, shaped like the OpenAI Responses API
// (system / user / reasoning / assistant / tool_result records). It is the
// re-read source for the session-handoff transcript tier; chat_history is
// preferred over updates.jsonl here because it carries the assembled
// assistant text + tool_calls + per-message model id in one record.
type chatRecord struct {
	Type            string         `json:"type"`
	Content         chatContent    `json:"content"`
	ToolCalls       []chatToolCall `json:"tool_calls"`
	ToolCallID      string         `json:"tool_call_id"`
	ModelID         string         `json:"model_id"`
	SyntheticReason string         `json:"synthetic_reason"`
}

// chatToolCall is an assistant tool-call request. Arguments is a JSON
// STRING (grok serializes the args object into a string), passed through
// to the transcript verbatim.
type chatToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// chatContent is a message's content, which is polymorphic by role: a bare
// string (assistant / system / tool_result) OR an array of {type,text}
// parts (user). A custom unmarshaler normalizes every shape into flat text
// so a strict []part type doesn't silently drop the string-shaped records
// (the polymorphic-content trap, new-adapter-checklist §4.4d).
type chatContent struct {
	Text string
}

// UnmarshalJSON accepts a string, an array of parts, or an object part.
func (c *chatContent) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		c.Text = s
	case '[':
		var parts []struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(b, &parts); err != nil {
			return err
		}
		var sb strings.Builder
		for _, p := range parts {
			if p.Text == "" {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(p.Text)
		}
		c.Text = sb.String()
	case '{':
		var part struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(b, &part); err != nil {
			return err
		}
		c.Text = part.Text
	}
	return nil
}

// transcriptFullCap is the effectively-uncapped bound used by
// ReadTranscriptFull (the get_session_message / full_cache path).
const transcriptFullCap = 1 << 20 // 1 MiB

// ReadTranscript implements handoffsvc.TranscriptReader — the
// session-handoff transcript tier (docs/session-handoff.md). Grok has no
// proxy tier, so the completed transcript is re-read from the session
// bundle's chat_history.jsonl. Bodies are excerpt-capped by the builder.
func (a *Adapter) ReadTranscript(ctx context.Context, sess models.Session, sourceHints []string) ([]models.TranscriptMessage, error) {
	return a.readTranscript(ctx, sess, sourceHints, transcriptutil.New())
}

// ReadTranscriptFull implements handoffsvc.FullTranscriptReader: the same
// normalization with the excerpt caps lifted, backing the full_cache carry
// mode + the get_session_message MCP pull. Nothing is persisted — bodies
// are re-read from grok's own files on demand.
func (a *Adapter) ReadTranscriptFull(ctx context.Context, sess models.Session, sourceHints []string) ([]models.TranscriptMessage, error) {
	return a.readTranscript(ctx, sess, sourceHints,
		transcriptutil.NewWithCaps(transcriptFullCap, transcriptFullCap, transcriptFullCap))
}

// readTranscript builds the normalized transcript from chat_history.jsonl
// into the supplied builder (capped or uncapped).
func (a *Adapter) readTranscript(ctx context.Context, sess models.Session, sourceHints []string, b *transcriptutil.Builder) ([]models.TranscriptMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, ok := a.chatHistoryPath(sess.ID, sourceHints)
	if !ok {
		return nil, fmt.Errorf("grok.ReadTranscript: no chat_history.jsonl for session %s", sess.ID)
	}
	body, err := os.ReadFile(path) //nolint:gosec // path derives from a validated hint / watch root
	if err != nil {
		return nil, fmt.Errorf("grok.ReadTranscript: %w", err)
	}
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec chatRecord
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		a.applyChatRecord(b, &rec)
	}
	return b.Finish(), nil
}

// applyChatRecord folds one chat_history record into the transcript
// builder. Reasoning records are dropped (thinking is not carried at read
// time); synthetic user records (injected system-reminders / skills
// prompts) are skipped so only genuine user turns show.
func (a *Adapter) applyChatRecord(b *transcriptutil.Builder, rec *chatRecord) {
	var zero time.Time
	switch rec.Type {
	case "user":
		if rec.SyntheticReason != "" {
			return
		}
		if text := strings.TrimSpace(rec.Content.Text); text != "" {
			b.User(text, zero)
		}
	case "assistant":
		if text := strings.TrimSpace(rec.Content.Text); text != "" {
			b.AssistantText(text, rec.ModelID, zero)
		}
		for _, tc := range rec.ToolCalls {
			b.AssistantCall(tc.ID, tc.Name, tc.Arguments, rec.ModelID, zero)
		}
	case "tool_result":
		if rec.ToolCallID != "" {
			b.Resolve(rec.ToolCallID, rec.Content.Text, zero)
		}
	}
}

// chatHistoryPath locates a session's chat_history.jsonl from a source
// hint pointing anywhere inside the session bundle, or by globbing the
// sessions watch roots for `*/<id>/chat_history.jsonl`.
func (a *Adapter) chatHistoryPath(sessionID string, hints []string) (string, bool) {
	// A hint inside the session dir: its dir sibling is chat_history.jsonl.
	for _, h := range hints {
		if strings.TrimSpace(h) == "" {
			continue
		}
		cand := filepath.Join(filepath.Dir(h), "chat_history.jsonl")
		if fileReadable(cand) {
			return cand, true
		}
	}
	for _, root := range a.roots {
		if !strings.Contains(filepath.ToSlash(root), "/.grok/sessions") &&
			!strings.HasSuffix(filepath.ToSlash(root), "/sessions") {
			continue
		}
		matches, err := filepath.Glob(filepath.Join(root, "*", sessionID, "chat_history.jsonl"))
		if err != nil {
			continue
		}
		for _, m := range matches {
			if fileReadable(m) {
				return m, true
			}
		}
	}
	return "", false
}

// fileReadable reports whether p is an existing regular file.
func fileReadable(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
