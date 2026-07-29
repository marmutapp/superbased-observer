package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// resumeVariants enumerates the two adapter instances that share this
// parser: the base codex adapter and the Open Interpreter retag of it.
// Every cross-parse dedup guarantee has to hold for BOTH — the defect
// class lives in the shared parser, not in either instance's identity.
func resumeVariants() []struct {
	name     string
	tool     string
	newAdapt func(dir string) *Adapter
} {
	return []struct {
		name     string
		tool     string
		newAdapt func(dir string) *Adapter
	}{
		{
			name:     "codex",
			tool:     models.ToolCodex,
			newAdapt: func(dir string) *Adapter { return NewWithOptions(nil, dir) },
		},
		{
			name:     "open-interpreter",
			tool:     models.ToolOpenInterpreter,
			newAdapt: func(dir string) *Adapter { return NewOpenInterpreterWithOptions(nil, dir) },
		},
	}
}

// TestResumeSuppressesDuplicateTokenCountAcrossPollBoundary pins the
// 2026-07-29 adversarial-review finding: Codex sometimes re-emits a
// byte-identical token_count record (same last_token_usage AND same
// cumulative total_token_usage) 2-3 seconds after the original. Within
// one parse the seenModernTotal map suppresses it. Across a WATCHER
// POLL BOUNDARY it did not: seenModernTotal was rebuilt empty on every
// resumed parse, so the duplicate emitted a second token_usage row.
//
// Nothing downstream catches it — the row's SourceEventID and
// MessageID are both "tk:<file>:L<lineNum>", and the duplicate sits on
// a DIFFERENT line, so store.InsertTokenEvents' UNIQUE(source_file,
// source_event_id) upsert misses AND its (tool, session_id,
// message_id) tuple-dedup sweep misses. The result is a real extra row
// double-counting the turn's tokens.
//
// The fix seeds the resumed parse's seenModernTotal from
// prefetchSessionContext's state-only replay of the pre-offset lines.
func TestResumeSuppressesDuplicateTokenCountAcrossPollBoundary(t *testing.T) {
	t.Parallel()

	const (
		header = `{"timestamp":"2026-07-29T10:00:00.000Z","type":"session_meta","payload":{"id":"sess-dup-tk","cwd":"/repo","model":"gpt-5.6"}}`
		start  = `{"timestamp":"2026-07-29T10:00:01.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1"}}`
		// input=95 cached=5 output=8 reasoning=2 — the reviewer's
		// repro totals. last_token_usage and total_token_usage are
		// identical on both copies, which is exactly the shape Codex
		// re-emits.
		tokenLine = `{"timestamp":"2026-07-29T10:00:03.000Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":95,"cached_input_tokens":5,"output_tokens":8,"reasoning_output_tokens":2,"total_tokens":103},"total_token_usage":{"input_tokens":95,"cached_input_tokens":5,"output_tokens":8,"reasoning_output_tokens":2,"total_tokens":103}}}}`
		// The re-emission, 2s later: different timestamp (so the
		// bytes differ and no line-level cache could collapse it),
		// identical usage payload.
		tokenDup = `{"timestamp":"2026-07-29T10:00:05.000Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":95,"cached_input_tokens":5,"output_tokens":8,"reasoning_output_tokens":2,"total_tokens":103},"total_token_usage":{"input_tokens":95,"cached_input_tokens":5,"output_tokens":8,"reasoning_output_tokens":2,"total_tokens":103}}}}`
	)

	for _, v := range resumeVariants() {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "rollout-2026-07-29T10-00-00-sess-dup-tk.jsonl")

			// Control: an uninterrupted parse of the whole file
			// (duplicate included) emits ONE token row. Whatever the
			// resumed parse does must match this.
			whole := strings.Join([]string{header, start, tokenLine, tokenDup, ""}, "\n")
			if err := os.WriteFile(path, []byte(whole), 0o600); err != nil {
				t.Fatal(err)
			}
			full, err := v.newAdapt(dir).ParseSessionFile(context.Background(), path, 0)
			if err != nil {
				t.Fatalf("full parse: %v", err)
			}
			if len(full.TokenEvents) != 1 {
				t.Fatalf("full parse TokenEvents = %d, want 1 (intra-parse dedup baseline)", len(full.TokenEvents))
			}

			// Now the poll-boundary version: the watcher sees the file
			// WITHOUT the re-emission first, persists the cursor, and
			// resumes after Codex appends the duplicate.
			a := v.newAdapt(dir)
			firstChunk := strings.Join([]string{header, start, tokenLine, ""}, "\n")
			if err := os.WriteFile(path, []byte(firstChunk), 0o600); err != nil {
				t.Fatal(err)
			}
			res1, err := a.ParseSessionFile(context.Background(), path, 0)
			if err != nil {
				t.Fatalf("first parse: %v", err)
			}
			if len(res1.TokenEvents) != 1 {
				t.Fatalf("first parse TokenEvents = %d, want 1", len(res1.TokenEvents))
			}
			if got := res1.TokenEvents[0].Tool; got != v.tool {
				t.Errorf("first parse TokenEvents[0].Tool = %q, want %q", got, v.tool)
			}
			if res1.NewOffset != int64(len(firstChunk)) {
				t.Fatalf("first parse NewOffset = %d, want %d", res1.NewOffset, len(firstChunk))
			}

			if err := os.WriteFile(path, []byte(whole), 0o600); err != nil {
				t.Fatal(err)
			}
			res2, err := a.ParseSessionFile(context.Background(), path, res1.NewOffset)
			if err != nil {
				t.Fatalf("resumed parse: %v", err)
			}
			if len(res2.TokenEvents) != 0 {
				t.Fatalf(
					"resumed parse emitted %d token event(s) for a re-emitted identical token_count; want 0. "+
						"Row would land under a distinct source_event_id/message_id (line-number keyed) and "+
						"double-count the turn. First: %+v",
					len(res2.TokenEvents), res2.TokenEvents[0],
				)
			}
		})
	}
}

// TestResumeSuppressesRepeatedSystemPromptAcrossPollBoundary covers the
// sibling cross-parse state of the same defect class: seenSystemPrompts
// was also rebuilt empty on resume, so a developer_instructions body
// Codex repeats in nearly every turn_context re-emitted a full
// ActionSystemPrompt row (9-18KB) on the far side of a poll boundary —
// again under a fresh "sysprompt:<role>:<hash>:L<lineNum>" id that no
// store-side key can collapse.
func TestResumeSuppressesRepeatedSystemPromptAcrossPollBoundary(t *testing.T) {
	t.Parallel()

	const (
		header = `{"timestamp":"2026-07-29T11:00:00.000Z","type":"session_meta","payload":{"id":"sess-dup-sp","cwd":"/repo","model":"gpt-5.6","base_instructions":{"text":"BASE INSTRUCTIONS BODY"}}}`
		turn1  = `{"timestamp":"2026-07-29T11:00:01.000Z","type":"turn_context","payload":{"turn_id":"t1","cwd":"/repo","model":"gpt-5.6","developer_instructions":"DEVELOPER INSTRUCTIONS BODY"}}`
		// Second turn repeats BOTH bodies verbatim, as real rollouts do.
		turn2 = `{"timestamp":"2026-07-29T11:00:09.000Z","type":"turn_context","payload":{"turn_id":"t2","cwd":"/repo","model":"gpt-5.6","developer_instructions":"DEVELOPER INSTRUCTIONS BODY"}}`
		meta2 = `{"timestamp":"2026-07-29T11:00:10.000Z","type":"session_meta","payload":{"id":"sess-dup-sp","cwd":"/repo","model":"gpt-5.6","base_instructions":{"text":"BASE INSTRUCTIONS BODY"}}}`
	)

	for _, v := range resumeVariants() {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "rollout-2026-07-29T11-00-00-sess-dup-sp.jsonl")

			whole := strings.Join([]string{header, turn1, turn2, meta2, ""}, "\n")
			if err := os.WriteFile(path, []byte(whole), 0o600); err != nil {
				t.Fatal(err)
			}
			full, err := v.newAdapt(dir).ParseSessionFile(context.Background(), path, 0)
			if err != nil {
				t.Fatalf("full parse: %v", err)
			}
			if got := countSystemPrompts(full.ToolEvents); got != 2 {
				t.Fatalf("full parse system-prompt rows = %d, want 2 (one base + one developer)", got)
			}

			a := v.newAdapt(dir)
			firstChunk := strings.Join([]string{header, turn1, ""}, "\n")
			if err := os.WriteFile(path, []byte(firstChunk), 0o600); err != nil {
				t.Fatal(err)
			}
			res1, err := a.ParseSessionFile(context.Background(), path, 0)
			if err != nil {
				t.Fatalf("first parse: %v", err)
			}
			if got := countSystemPrompts(res1.ToolEvents); got != 2 {
				t.Fatalf("first parse system-prompt rows = %d, want 2", got)
			}

			if err := os.WriteFile(path, []byte(whole), 0o600); err != nil {
				t.Fatal(err)
			}
			res2, err := a.ParseSessionFile(context.Background(), path, res1.NewOffset)
			if err != nil {
				t.Fatalf("resumed parse: %v", err)
			}
			if got := countSystemPrompts(res2.ToolEvents); got != 0 {
				t.Fatalf(
					"resumed parse emitted %d system-prompt row(s) for bodies already emitted before the cursor; want 0",
					got,
				)
			}
		})
	}
}

func countSystemPrompts(events []models.ToolEvent) int {
	n := 0
	for _, e := range events {
		if e.ActionType == models.ActionSystemPrompt {
			n++
		}
	}
	return n
}
