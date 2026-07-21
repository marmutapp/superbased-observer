package cursor

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter/transcriptutil"
	"github.com/marmutapp/superbased-observer/internal/models"
)

// Transcript reading (session handoff, docs/session-handoff.md P2).
// Cursor's agent transcript lives at
// `.cursor/projects/<slug>/agent-transcripts/<conv>/<conv>.jsonl` —
// Anthropic-shaped `{role, message:{content:[...]}}` lines plus role-less
// `{type:"turn_ended", status}` markers. Live-grounded quirks (this
// node's corpus, 2026-07-04): tool_use parts carry name+input but NO id,
// and NO tool_result parts ever appear — a call is settled once its turn
// ended, so turn markers (and the next user line) resolve the batch via
// the builder's ResolveAll, with excerpts left empty (nothing recorded,
// nothing fabricated). Lines carry no timestamps → messages have zero
// times and timestamp-addressed forks error honestly.
func (a *Adapter) ReadTranscript(ctx context.Context, sess models.Session, sourceHints []string) ([]models.TranscriptMessage, error) {
	path, err := a.transcriptPath(sess.ID, sourceHints)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cursor.ReadTranscript: %w", err)
	}
	defer f.Close()
	return parseAgentTranscript(ctx, f)
}

// transcriptPath resolves the agent transcript: a usable hint first,
// else derivation by globbing every projects root for the conversation
// id (D-P0.1 — cursor sessions are hook-fed and carry the "cursor:hook"
// sentinel, so derivation is the primary lookup).
func (a *Adapter) transcriptPath(sessionID string, hints []string) (string, error) {
	for _, h := range hints {
		if strings.Contains(h, "agent-transcripts") && strings.HasSuffix(h, ".jsonl") && transcriptFileExists(h) {
			return h, nil
		}
	}
	var candidates []string
	for _, root := range a.roots {
		m, _ := filepath.Glob(filepath.Join(root, "*", "agent-transcripts", sessionID, sessionID+".jsonl"))
		candidates = append(candidates, m...)
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("cursor.ReadTranscript: no agent transcript on disk for conversation %s", sessionID)
	}
	sort.Slice(candidates, func(i, j int) bool { return transcriptFileSize(candidates[i]) > transcriptFileSize(candidates[j]) })
	return candidates[0], nil
}

// agentTranscriptLine is one JSONL record: a role-carrying message or a
// role-less turn marker.
type agentTranscriptLine struct {
	Role    string `json:"role"`
	Type    string `json:"type"`
	Message struct {
		Content []agentTranscriptPart `json:"content"`
	} `json:"message"`
}

type agentTranscriptPart struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

func parseAgentTranscript(ctx context.Context, r io.Reader) ([]models.TranscriptMessage, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	b := transcriptutil.New()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line, err := br.ReadString('\n')
		if s := strings.TrimSpace(line); s != "" {
			agentTranscriptRecord(b, s)
		}
		if err != nil {
			break
		}
	}
	return b.Finish(), nil
}

func agentTranscriptRecord(b *transcriptutil.Builder, line string) {
	var rec agentTranscriptLine
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		return // malformed lines are skipped, not fatal
	}
	switch rec.Role {
	case "user":
		// A new user line proves the previous turn settled even when the
		// turn_ended marker is missing (older files).
		b.ResolveAll()
		var text strings.Builder
		for _, p := range rec.Message.Content {
			if p.Type == "text" && strings.TrimSpace(p.Text) != "" {
				if text.Len() > 0 {
					text.WriteString("\n")
				}
				text.WriteString(stripUserQueryWrapper(p.Text))
			}
		}
		b.User(text.String(), time.Time{})
	case "assistant":
		for _, p := range rec.Message.Content {
			switch p.Type {
			case "text":
				b.AssistantText(p.Text, "", time.Time{})
			case "tool_use":
				// No id, no recorded result — settles on the turn marker.
				b.AssistantCall("", p.Name, string(p.Input), "", time.Time{})
			}
		}
	case "":
		if rec.Type == "turn_ended" {
			b.ResolveAll()
		}
	}
}

func transcriptFileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func transcriptFileSize(p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return st.Size()
}
