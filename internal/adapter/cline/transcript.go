package cline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter/transcriptutil"
	"github.com/marmutapp/superbased-observer/internal/models"
)

// Transcript reading (session handoff, docs/session-handoff.md P2).
// Cline's task history lives at `tasks/<task-id>/api_conversation_history.json`
// — a JSON array of Anthropic-shaped messages ({role, ts, model, content})
// where content is either a plain string (user prompt) or a block list
// (text / thinking / tool_use / tool_result). tool_result blocks ride in
// user-role carriers and attach to the owning exchange (D-P0.3); thinking
// is dropped. SessionID == the task directory name.
func (a *Adapter) ReadTranscript(ctx context.Context, sess models.Session, sourceHints []string) ([]models.TranscriptMessage, error) {
	path, err := a.historyPath(sess.ID, sourceHints)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cline.ReadTranscript: %w", err)
	}
	return parseConversationHistory(body)
}

func (a *Adapter) historyPath(sessionID string, hints []string) (string, error) {
	for _, h := range hints {
		if filepath.Base(h) == "api_conversation_history.json" && historyFileExists(h) {
			return h, nil
		}
	}
	var candidates []string
	for _, root := range a.WatchPaths() {
		p := filepath.Join(root, sessionID, "api_conversation_history.json")
		if historyFileExists(p) {
			candidates = append(candidates, p)
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("cline.ReadTranscript: no task history on disk for session %s", sessionID)
	}
	sort.Slice(candidates, func(i, j int) bool { return historyFileSize(candidates[i]) > historyFileSize(candidates[j]) })
	return candidates[0], nil
}

// historyMessage is one element of api_conversation_history.json.
// Reuses the block shape rawMessage defines for ingestion but keeps its
// own minimal view (the reader needs no usage/metrics).
type historyMessage struct {
	Role    string          `json:"role"`
	Ts      int64           `json:"ts"` // unix millis; 0 when absent
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
}

type historyBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

func parseConversationHistory(body []byte) ([]models.TranscriptMessage, error) {
	var msgs []historyMessage
	if err := json.Unmarshal(body, &msgs); err != nil {
		return nil, fmt.Errorf("cline.ReadTranscript: parse history: %w", err)
	}
	b := transcriptutil.New()
	for _, m := range msgs {
		ts := millisTime(m.Ts)

		// String content: a plain user prompt (or assistant narration).
		var text string
		if err := json.Unmarshal(m.Content, &text); err == nil {
			if m.Role == "user" {
				b.User(text, ts)
			} else {
				b.AssistantText(text, m.Model, ts)
			}
			continue
		}
		var blocks []historyBlock
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			continue
		}
		if m.Role == "assistant" {
			for _, blk := range blocks {
				switch blk.Type {
				case "text":
					b.AssistantText(blk.Text, m.Model, ts)
				case "tool_use":
					b.AssistantCall(blk.ID, blk.Name, string(blk.Input), m.Model, ts)
				}
				// thinking dropped (plan §8).
			}
			continue
		}
		// user record: real prompt text vs tool_result carrier (D-P0.3).
		for _, blk := range blocks {
			switch blk.Type {
			case "text":
				b.User(blk.Text, ts)
			case "tool_result":
				b.Resolve(blk.ToolUseID, flattenHistoryResult(blk.Content), time.Time{})
			}
		}
	}
	return b.Finish(), nil
}

// flattenHistoryResult renders a tool_result content payload (string or
// text-block array) as plain text.
func flattenHistoryResult(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []historyBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, blk := range blocks {
		if blk.Type == "text" && strings.TrimSpace(blk.Text) != "" {
			parts = append(parts, strings.TrimSpace(blk.Text))
		}
	}
	return strings.Join(parts, " ")
}

func millisTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func historyFileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func historyFileSize(p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return st.Size()
}
