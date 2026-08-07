package codex

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/scrub"
)

// TestParseRolloutPatchApplyExecNamespace pins the modern-build shape of
// apply_patch, which no id join can reach.
//
// Current Codex invokes apply_patch from inside an `exec`
// custom_tool_call (`tools.apply_patch(patch)` in the sandbox script).
// The executor then stamps patch_apply_end with its OWN id —
// `exec-<uuid>` — while the response_item carries `call_<hash>`. The two
// live in different namespaces, so pending[pa.CallID] cannot match by
// construction. Measured over a 393-rollout reference corpus: 2,059
// exec-uuid vs 416 call_hash.
//
// A missed id join no longer implies a standalone row: the turn-scoped,
// file-set-gated fallback in claimPatchInvocation merges most of these
// into their invocation instead (see adapter_patchpairing_test.go).
// THIS fixture still takes the standalone path, and deliberately so —
// its patch declares one file while the executor reports two, so the
// equality gate abstains. That is the guard working, not the namespace.
//
// Two things had to be true for that row to be useful and neither was:
//
//  1. RawToolInput was never set on the standalone path, so every one of
//     those rows landed with an empty input column.
//  2. patchApplyChange only decoded `content`, but an `update` change
//     carries `unified_diff` and no `content` at all — 2,746 of the
//     3,492 changes in that corpus — so ContentBytes scored zero for the
//     large majority of real edits.
//
// The pre-existing rollout-response-item.jsonl fixture hid (2) because
// its synthesized `update` change carries `content`, a combination the
// producer never emits.
func TestParseRolloutPatchApplyExecNamespace(t *testing.T) {
	t.Parallel()
	a := New()
	res, err := a.ParseSessionFile(context.Background(), fixture(t, "rollout-patch-apply-exec-namespace.jsonl"), 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}

	var patch *models.ToolEvent
	for i := range res.ToolEvents {
		if res.ToolEvents[i].RawToolName == "patch_apply_end" {
			patch = &res.ToolEvents[i]
			break
		}
	}
	if patch == nil {
		var got []string
		for i, evt := range res.ToolEvents {
			got = append(got, formatEventSummary(i, evt))
		}
		t.Fatalf("no patch_apply_end row emitted\n%s", strings.Join(got, "\n"))
	}

	if patch.ActionType != models.ActionEditFile {
		t.Errorf("action: %s want edit_file", patch.ActionType)
	}
	if patch.SourceEventID != "exec-13ec0715-713d-4942-9fe1-e4541968b8ab" {
		t.Errorf("source_event_id: %q want the exec-uuid verbatim", patch.SourceEventID)
	}

	// The whole point of the fix: this column was empty for every row of
	// this shape.
	if patch.RawToolInput == "" {
		t.Fatalf("raw_tool_input is empty; the standalone path must store the `changes` object")
	}
	if !strings.Contains(patch.RawToolInput, "unified_diff") {
		t.Errorf("raw_tool_input should carry the update change's unified_diff: %q", patch.RawToolInput)
	}
	if !strings.Contains(patch.RawToolInput, "notes.txt") {
		t.Errorf("raw_tool_input should carry every changed path: %q", patch.RawToolInput)
	}
	if !json.Valid([]byte(patch.RawToolInput)) {
		t.Errorf("raw_tool_input must stay valid JSON: %q", patch.RawToolInput)
	}

	// Only the `add` change's Content counts. The `update` change's
	// unified_diff is deliberately excluded: its authored bytes are already
	// counted on the invocation row this executor row accompanies, so
	// counting them here would double-count every update.
	const want = int64(len("package added\n"))
	if patch.ContentBytes != want {
		t.Errorf("content_bytes: %d want %d (add content only; the update's diff belongs to the invocation row)", patch.ContentBytes, want)
	}
}

func TestAuthoredBytesFromPatchChanges(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		changes map[string]patchApplyChange
		want    int64
	}{
		{"nil", nil, 0},
		{
			"add carries whole file in content",
			map[string]patchApplyChange{"a.go": {Type: "add", Content: "package a\n"}},
			int64(len("package a\n")),
		},
		{
			// NOT a bug: the invocation row already counts these bytes from
			// its own patch text. Counting them here as well double-counts
			// authored output on every update.
			"update scores zero here — its bytes belong to the invocation row",
			map[string]patchApplyChange{"b.go": {Type: "update"}},
			0,
		},
		{
			"delete is never authored output",
			map[string]patchApplyChange{"c.go": {Type: "delete", Content: "package c\n"}},
			0,
		},
		{
			"mixed change set counts only content-bearing adds",
			map[string]patchApplyChange{
				"a.go": {Type: "add", Content: "package a\n"},
				"b.go": {Type: "update"},
				"c.go": {Type: "delete", Content: "package c\n"},
			},
			int64(len("package a\n")),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := authoredBytesFromPatchChanges(tt.changes); got != tt.want {
				t.Errorf("authoredBytesFromPatchChanges = %d want %d", got, tt.want)
			}
		})
	}
}

// TestAuthoredBytesFromPatch_PlusPrefixedSource pins that added source whose
// own text begins with "+" is counted. Every caller is gated on the
// apply_patch envelope, which never carries unified-diff "+++ b/file"
// headers, so an exclusion for those only ever dropped real content: source
// `++n` encodes as `+++n` and `++ n` as `+++ n`, which is why narrowing the
// predicate to require a trailing space did not fix it either.
func TestAuthoredBytesFromPatch_PlusPrefixedSource(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		patch string
		want  int64
	}{
		{"plain added line", "*** Begin Patch\n+abc\n", int64(len("abc"))},
		{"C-style increment, no space", "*** Begin Patch\n+++n;\n", int64(len("++n;"))},
		{"C-style increment, with space", "*** Begin Patch\n+++ n;\n", int64(len("++ n;"))},
		{"removed lines are not authored", "*** Begin Patch\n-gone\n", 0},
		{"context lines are not authored", "*** Begin Patch\n unchanged\n", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := authoredBytesFromPatch(tt.patch); got != tt.want {
				t.Errorf("authoredBytesFromPatch(%q) = %d want %d", tt.patch, got, tt.want)
			}
		})
	}
}

func TestRenderPatchChangesInput(t *testing.T) {
	t.Parallel()
	sc := scrub.New()
	tests := []struct {
		name     string
		raw      string
		wantText string
	}{
		{"absent", "", ""},
		{"json null", "null", ""},
		{"empty object the producer never writes", "{}", ""},
		{"malformed", "{not json", ""},
		{"populated", `{"a.go":{"type":"update","unified_diff":"@@\n+x\n"}}`, "unified_diff"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := RenderPatchChangesInput(sc, json.RawMessage(tt.raw))
			if tt.wantText == "" {
				if got != "" {
					t.Errorf("RenderPatchChangesInput(%q) = %q want \"\"", tt.raw, got)
				}
				return
			}
			if !strings.Contains(got, tt.wantText) {
				t.Errorf("RenderPatchChangesInput(%q) = %q want it to contain %q", tt.raw, got, tt.wantText)
			}
			if !json.Valid([]byte(got)) {
				t.Errorf("RenderPatchChangesInput(%q) produced invalid JSON: %q", tt.raw, got)
			}
		})
	}
}
