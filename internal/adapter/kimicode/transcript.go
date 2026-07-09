package kimicode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/adapter/transcriptutil"
	"github.com/marmutapp/superbased-observer/internal/models"
)

// ReadTranscript implements handoffsvc.TranscriptReader — the
// session-handoff transcript tier (docs/session-handoff.md). kimi-code has
// no proxy tier, so the transcript is re-read from the session's main
// agent wire trace (`agents/main/wire.jsonl`). The stream carries user
// prompts (turn.prompt), assistant text (content.part), and tool calls +
// results (tool.call / tool.result), which fold into the owning assistant
// exchange. Excerpt caps apply (the P1 default reader).
func (a *Adapter) ReadTranscript(ctx context.Context, sess models.Session, sourceHints []string) ([]models.TranscriptMessage, error) {
	return a.readTranscript(ctx, sess, sourceHints, transcriptutil.New())
}

// ReadTranscriptFull implements handoffsvc.FullTranscriptReader: the same
// normalized stream with message text and tool-result bodies emitted
// whole (excerpt caps lifted) for the full_cache carry mode + the
// get_session_message MCP pull.
func (a *Adapter) ReadTranscriptFull(ctx context.Context, sess models.Session, sourceHints []string) ([]models.TranscriptMessage, error) {
	return a.readTranscript(ctx, sess, sourceHints, transcriptutil.NewWithCaps(0, 0, 0))
}

// readTranscript resolves the main wire.jsonl for the session and folds it
// into the normalized stream using the supplied builder (which fixes the
// excerpt caps).
func (a *Adapter) readTranscript(ctx context.Context, sess models.Session, sourceHints []string, b *transcriptutil.Builder) ([]models.TranscriptMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, ok := a.wirePath(sess.ID, sourceHints)
	if !ok {
		return nil, fmt.Errorf("kimicode.ReadTranscript: no wire.jsonl for session %s", sess.ID)
	}
	f, err := os.Open(path) //nolint:gosec // path is a watch-root/hint-derived session file
	if err != nil {
		return nil, fmt.Errorf("kimicode.ReadTranscript: open: %w", err)
	}
	defer f.Close()

	reader := bufio.NewReaderSize(f, 64*1024)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lineStr, readErr := reader.ReadString('\n')
		if raw := strings.TrimRight(lineStr, "\r\n"); raw != "" {
			foldLine(b, raw)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("kimicode.ReadTranscript: read: %w", readErr)
		}
	}
	return b.Finish(), nil
}

// foldLine adds one wire line to the transcript builder.
func foldLine(b *transcriptutil.Builder, raw string) {
	var line wireLine
	if json.Unmarshal([]byte(raw), &line) != nil {
		return
	}
	switch line.Type {
	case "turn.prompt":
		if line.Origin != nil && line.Origin.Kind != "" && line.Origin.Kind != "user" {
			return
		}
		b.User(promptText(line.Input), unixMillis(line.Time))
	case "usage.record":
		// tokens only — no transcript content
	case "context.append_loop_event":
		foldLoopEvent(b, line, normalizeModel(firstNonEmpty(line.Model, line.ModelAlias)))
	}
}

// foldLoopEvent folds a context.append_loop_event body into the builder.
func foldLoopEvent(b *transcriptutil.Builder, line wireLine, model string) {
	if len(line.Event) == 0 {
		return
	}
	var ev loopEvent
	if json.Unmarshal(line.Event, &ev) != nil {
		return
	}
	switch ev.Type {
	case "content.part":
		if ev.Part != nil && ev.Part.Type == "text" {
			b.AssistantText(ev.Part.Text, model, unixMillis(line.Time))
		}
	case "tool.call":
		b.AssistantCall(firstNonEmpty(ev.ToolCallID, ev.UUID), ev.Name, string(ev.Args), model, unixMillis(line.Time))
	case "tool.result":
		out, _, _ := resultOutcome(ev.Result)
		b.Resolve(firstNonEmpty(ev.ToolCallID, ev.ParentUUID), out, unixMillis(line.Time))
	}
}

// wirePath resolves the session's main wire.jsonl: a usable hint first,
// else a filesystem walk under each watch root for the session dir.
func (a *Adapter) wirePath(sessionID string, hints []string) (string, bool) {
	for _, h := range hints {
		if filepath.Base(h) == "wire.jsonl" && agentNameFromPath(h) == "main" &&
			sessionIDFromPath(h) == sessionID && fileReadable(h) {
			return h, true
		}
	}
	for _, root := range a.roots {
		if p, ok := findMainWire(root, sessionID); ok {
			return p, true
		}
	}
	return "", false
}

// findMainWire walks the wd_* subdirs of a sessions root looking for
// <root>/wd_*/<sessionID>/agents/main/wire.jsonl.
func findMainWire(sessionsRoot, sessionID string) (string, bool) {
	entries, err := os.ReadDir(sessionsRoot)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(sessionsRoot, e.Name(), sessionID, "agents", "main", "wire.jsonl")
		if fileReadable(p) {
			return p, true
		}
	}
	return "", false
}

func fileReadable(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
