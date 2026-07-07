package codex

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

// Transcript reading (session handoff, plan §6 + Phase 0 findings): re-
// reads a session's message content from Codex's own rollout JSONL at
// handoff time. Text comes from the event_msg lane (user_message /
// agent_message — clean strings, no injected environment context); tool
// calls pair response_item function_call ↔ function_call_output by
// call_id; reasoning records are dropped (they are Codex's thinking
// equivalent, plan §8). Consecutive assistant-side records merge into one
// assistant exchange per user message (D-P0.3).

// ReadTranscript implements the handoffsvc TranscriptReader capability.
// When no sourceHint resolves, the rollout is DERIVED by globbing every
// watch root for rollout-*-<session-id>.jsonl (D-P0.1) — both the dated
// YYYY/MM/DD layout and the legacy flat layout.
func (a *Adapter) ReadTranscript(ctx context.Context, sess models.Session, sourceHints []string) ([]models.TranscriptMessage, error) {
	return a.readTranscript(ctx, sess, sourceHints, transcriptutil.New())
}

// ReadTranscriptFull implements the handoffsvc FullTranscriptReader
// capability: the same normalization as ReadTranscript but with UNCAPPED
// excerpts, so function_call_output bodies and message text come through
// whole. It backs the `full_cache` carry (the actual read content inlined
// into the handover). Read on demand from Codex's own rollout files;
// nothing is persisted.
func (a *Adapter) ReadTranscriptFull(ctx context.Context, sess models.Session, sourceHints []string) ([]models.TranscriptMessage, error) {
	return a.readTranscript(ctx, sess, sourceHints, transcriptutil.NewWithCaps(0, 0, 0))
}

func (a *Adapter) readTranscript(ctx context.Context, sess models.Session, sourceHints []string, builder *transcriptutil.Builder) ([]models.TranscriptMessage, error) {
	path, err := a.rolloutPath(sess.ID, sourceHints)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("codex.ReadTranscript: %w", err)
	}
	defer f.Close()
	return parseRolloutTranscript(ctx, f, builder)
}

func (a *Adapter) rolloutPath(sessionID string, hints []string) (string, error) {
	for _, h := range hints {
		if strings.Contains(filepath.Base(h), "rollout-") && rolloutExists(h) {
			return h, nil
		}
	}
	var candidates []string
	for _, root := range a.WatchPaths() {
		for _, pattern := range []string{
			filepath.Join(root, "*", "*", "*", "rollout-*"+sessionID+".jsonl"),
			filepath.Join(root, "rollout-*"+sessionID+".jsonl"),
		} {
			m, _ := filepath.Glob(pattern)
			candidates = append(candidates, m...)
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("codex.ReadTranscript: no rollout on disk for session %s", sessionID)
	}
	sort.Slice(candidates, func(i, j int) bool { return rolloutSize(candidates[i]) > rolloutSize(candidates[j]) })
	return candidates[0], nil
}

type rolloutRecord struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type rolloutPayload struct {
	Type      string          `json:"type"`
	Message   string          `json:"message"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
}

func parseRolloutTranscript(ctx context.Context, r io.Reader, builder *transcriptutil.Builder) ([]models.TranscriptMessage, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	b := rolloutBuilder{b: builder}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line, err := br.ReadString('\n')
		if line != "" {
			b.line(line)
		}
		if err != nil {
			break
		}
	}
	return b.finish(), nil
}

// rolloutBuilder wraps the shared exchange builder with codex's rollout
// record dispatch.
type rolloutBuilder struct {
	b *transcriptutil.Builder
}

func (b *rolloutBuilder) line(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	var rec rolloutRecord
	if err := json.Unmarshal([]byte(line), &rec); err != nil || len(rec.Payload) == 0 {
		return
	}
	var p rolloutPayload
	if err := json.Unmarshal(rec.Payload, &p); err != nil {
		return
	}
	ts := parseRolloutTime(rec.Timestamp)

	switch p.Type {
	case "user_message":
		b.b.User(p.Message, ts)
	case "agent_message":
		b.b.AssistantText(p.Message, "", ts)
	case "function_call":
		b.b.AssistantCall(p.CallID, p.Name, p.Arguments, "", ts)
	case "function_call_output":
		// non-zero ts: the output record's time refreshes the exchange's
		// completion time.
		b.b.Resolve(p.CallID, flattenRolloutOutput(p.Output), ts)
		// reasoning / token_count / task_* / message records are dropped:
		// reasoning is Codex's thinking; response_item message duplicates
		// the event_msg lane and carries injected environment context.
	}
}

func (b *rolloutBuilder) finish() []models.TranscriptMessage {
	return b.b.Finish()
}

// flattenRolloutOutput renders a function_call_output payload: either a
// plain string or an object with an "output" string field.
func flattenRolloutOutput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var obj struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Output != "" {
		return obj.Output
	}
	return ""
}

func parseRolloutTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC()
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

func rolloutExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func rolloutSize(p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return st.Size()
}
