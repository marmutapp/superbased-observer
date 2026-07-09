package clinecli

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

// Transcript reading (session handoff, docs/session-handoff.md P2).
// Cline CLI's conversation lives at
// `<root>/data/sessions/<id>/<id>.messages.json` — a messagesDoc whose
// messages are Anthropic-shaped records (role + ts millis + content
// blocks; modelInfo on assistant rows). tool_result blocks ride in
// user-role carriers and attach to the owning exchange (D-P0.3);
// thinking is dropped; the `<user_input mode="…">` envelope is stripped
// from user prompts.
func (a *Adapter) ReadTranscript(ctx context.Context, sess models.Session, sourceHints []string) ([]models.TranscriptMessage, error) {
	path, err := a.messagesPath(sess.ID, sourceHints)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("clinecli.ReadTranscript: %w", err)
	}
	return parseMessagesDoc(body)
}

func (a *Adapter) messagesPath(sessionID string, hints []string) (string, error) {
	name := sessionID + ".messages.json"
	for _, h := range hints {
		if filepath.Base(h) == name && messagesFileExists(h) {
			return h, nil
		}
	}
	for _, root := range a.WatchPaths() {
		// Roots are `<home>/.cline`; the session store lives under
		// data/sessions/<id>/ (mirrors sessionRow.messagesPath).
		for _, p := range []string{
			filepath.Join(root, "data", "sessions", sessionID, name),
			filepath.Join(root, "sessions", sessionID, name),
		} {
			if messagesFileExists(p) {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("clinecli.ReadTranscript: no messages.json on disk for session %s", sessionID)
}

func parseMessagesDoc(body []byte) ([]models.TranscriptMessage, error) {
	var doc messagesDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("clinecli.ReadTranscript: parse messages.json: %w", err)
	}
	b := transcriptutil.New()
	for _, m := range doc.Messages {
		ts := transcriptMillis(m.Ts)
		model := ""
		if m.ModelInfo != nil {
			model = m.ModelInfo.ID
		}
		switch m.Role {
		case "assistant":
			for _, blk := range m.Content {
				switch blk.Type {
				case "text":
					b.AssistantText(blk.Text, model, ts)
				case "tool_use":
					b.AssistantCall(blk.ID, blk.Name, string(blk.Input), model, ts)
				}
				// thinking dropped (plan §8).
			}
		case "user":
			for _, blk := range m.Content {
				switch blk.Type {
				case "text":
					b.User(stripUserInputWrapper(blk.Text), ts)
				case "tool_result":
					output, _ := decodeToolResultContent(blk)
					b.Resolve(blk.ToolUseID, output, time.Time{})
				}
			}
		}
	}
	return b.Finish(), nil
}

func transcriptMillis(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func messagesFileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && !strings.HasSuffix(p, string(filepath.Separator))
}
