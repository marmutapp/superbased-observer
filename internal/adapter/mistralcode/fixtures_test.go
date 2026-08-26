package mistralcode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// fixturePath resolves a path under testdata/mistralcode/ relative to this
// package (matches the convention in internal/adapter/hermes and
// internal/adapter/clinecli).
func fixturePath(elem ...string) string {
	return filepath.Join(append([]string{"..", "..", "..", "testdata", "mistralcode"}, elem...)...)
}

// copyFile copies src to dst, creating dst's parent dir if needed.
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src) //nolint:gosec // test fixture
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestModelName_PriceMatchFixture exercises the price-match model-resolution
// path against the "happy path" fixture: active_model and routed_default_model
// are both empty, and only one of the 3 config.models candidates
// (mistral-medium-3.5) has prices matching stats.*_price_per_million. Run
// repeatedly to guard against the map-iteration-order regression this
// fixture was built to catch (an early version of modelName returned a
// different, wrong model on some runs because config.models is a Go map).
func TestModelName_PriceMatchFixture(t *testing.T) {
	msgPath := fixturePath("session_20260815_090512_a1b2c3d4", "messages.jsonl")
	a := New()
	for i := 0; i < 20; i++ {
		meta := a.readMeta(msgPath)
		if got := modelName(meta); got != "mistral-medium-3.5" {
			t.Fatalf("run %d: modelName() = %q, want mistral-medium-3.5", i, got)
		}
	}
}

// TestModelName_SortedFallbackNoPriceSignal covers the last-resort path: no
// active_model, no routed_default_model, and no price signal in stats (both
// zero) — modelName must deterministically return the alphabetically-first
// config.models key rather than a random map key.
func TestModelName_SortedFallbackNoPriceSignal(t *testing.T) {
	meta := vibeMeta{}
	meta.Config.Models = map[string]vibeModelCfg{
		"zeta":  {InputPrice: 1, OutputPrice: 2},
		"alpha": {InputPrice: 3, OutputPrice: 4},
		"mid":   {InputPrice: 5, OutputPrice: 6},
	}
	for i := 0; i < 20; i++ {
		if got := modelName(meta); got != "alpha" {
			t.Fatalf("run %d: modelName() = %q, want alpha (sorted-first fallback)", i, got)
		}
	}
}

// TestModelName_EmptyModelsMap covers the degenerate case (partial1 fixture
// shape): no config block at all, so config.Models is nil/empty — modelName
// must return "" without panicking.
func TestModelName_EmptyModelsMap(t *testing.T) {
	msgPath := fixturePath("session_20260816_140033_partial1", "messages.jsonl")
	a := New()
	meta := a.readMeta(msgPath)
	if got := modelName(meta); got != "" {
		t.Errorf("modelName() = %q, want empty string for a session with no config.models", got)
	}
}

// TestParseSessionFile_ToolErrorPrecedenceAndMappings exercises the
// "happy path" fixture's full tool taxonomy: <tool_error> failure detection,
// content-over-tool_result precedence, and the ask_user_question/skill
// action-type mappings.
func TestParseSessionFile_ToolErrorPrecedenceAndMappings(t *testing.T) {
	msgPath := fixturePath("session_20260815_090512_a1b2c3d4", "messages.jsonl")
	a := New()
	res, err := a.ParseSessionFile(context.Background(), msgPath, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Two calls share RawToolName "bash" (ls -la, then python hello_world.py)
	// — index by Target, not by RawToolName, so the map doesn't collapse
	// them to whichever was processed last.
	byTool := map[string]models.ToolEvent{}
	byTarget := map[string]models.ToolEvent{}
	for _, ev := range res.ToolEvents {
		if ev.RawToolName != "" {
			byTool[ev.RawToolName] = ev
			byTarget[ev.RawToolName+":"+ev.Target] = ev
		}
	}

	bash, ok := byTarget["bash:ls -la"]
	if !ok {
		t.Fatal("missing bash `ls -la` tool event")
	}
	// The successful `ls -la` call: content should win over tool_result (the
	// flattened text, not the raw structured JSON).
	if !strings.Contains(bash.ToolOutput, "status: completed") {
		t.Errorf("bash ToolOutput = %q, want it to contain the flattened `content` text", bash.ToolOutput)
	}
	if strings.Contains(bash.ToolOutput, "exit_code") {
		t.Errorf("bash ToolOutput = %q, should NOT surface the raw tool_result JSON (exit_code) over content", bash.ToolOutput)
	}
	if !bash.Success {
		t.Error("bash `ls -la` call should be marked success")
	}

	// The failing `python hello_world.py` call is wrapped as
	// <tool_error>...</tool_error> in content, not an "error"-prefixed string
	// or a Python traceback — looksLikeError must still catch it.
	pyCall, ok := byTarget["bash:python hello_world.py"]
	if !ok {
		t.Fatal("missing the failing python hello_world.py bash call")
	}
	if pyCall.Success {
		t.Errorf("python hello_world.py call should be marked NOT success (content is <tool_error>-wrapped), ToolOutput=%q", pyCall.ToolOutput)
	}

	ask, ok := byTool["ask_user_question"]
	if !ok {
		t.Fatal("missing ask_user_question tool event")
	}
	if ask.ActionType != models.ActionAskUser {
		t.Errorf("ask_user_question ActionType = %q, want %q", ask.ActionType, models.ActionAskUser)
	}

	skill, ok := byTool["skill"]
	if !ok {
		t.Fatal("missing skill tool event")
	}
	if skill.ActionType != models.ActionSkillInvoke {
		t.Errorf("skill ActionType = %q, want %q", skill.ActionType, models.ActionSkillInvoke)
	}
}

// TestParseSessionFile_MissingMetaJSON covers a messages.jsonl with NO
// sibling meta.json at all (e.g. the session directory was only partially
// written): ParseSessionFile must not error or panic, must emit zero token
// events, and must still parse the transcript's actions.
func TestParseSessionFile_MissingMetaJSON(t *testing.T) {
	dir := t.TempDir()
	msgPath := filepath.Join(dir, "messages.jsonl")
	copyFile(t, fixturePath("session_20260816_140033_partial1", "messages.jsonl"), msgPath)
	// deliberately do NOT copy a meta.json sibling

	a := New()
	res, err := a.ParseSessionFile(context.Background(), msgPath, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile with no sibling meta.json: %v", err)
	}
	if len(res.TokenEvents) != 0 {
		t.Errorf("TokenEvents = %d, want 0 with no meta.json", len(res.TokenEvents))
	}
	if len(res.ToolEvents) != 2 { // one user prompt + one assistant message
		t.Errorf("ToolEvents = %d, want 2 (user prompt + assistant message)", len(res.ToolEvents))
	}
}

// TestParseSessionFile_PartialMetaJSON covers the partial1 fixture: a real
// meta.json is present but has no "stats" and no "config" block at all
// (e.g. the session ended before its first turn completed). No token event
// should be emitted; actions should still parse.
func TestParseSessionFile_PartialMetaJSON(t *testing.T) {
	msgPath := fixturePath("session_20260816_140033_partial1", "messages.jsonl")
	a := New()
	res, err := a.ParseSessionFile(context.Background(), msgPath, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.TokenEvents) != 0 {
		t.Errorf("TokenEvents = %d, want 0 for a meta.json with no stats block", len(res.TokenEvents))
	}
	if len(res.ToolEvents) != 2 {
		t.Errorf("ToolEvents = %d, want 2", len(res.ToolEvents))
	}
}

// TestParseSessionFile_GrowthReparse simulates the SAME session file growing
// between two parses (the real watcher/cursor scenario): parse the t1
// snapshot fully, then overwrite the same path with the t2 snapshot (2 more
// transcript lines appended, larger cumulative stats) and re-parse from the
// offset returned by the first pass. Only the NEW tool event should appear
// (no duplication of the first pass's events), and the session-level token
// event must re-emit under the SAME SourceEventID with larger totals — the
// shape the store's ON CONFLICT MAX-upgrade depends on.
func TestParseSessionFile_GrowthReparse(t *testing.T) {
	// The nested "session_..._deadbeef" dir name matters: sessionIDFromPath
	// derives the session id from the dir's trailing 8-hex suffix, falling
	// back to meta.json's session_id only when the dir shape is unusual —
	// so the temp copy must preserve the real naming convention, not just
	// live loose in t.TempDir()'s own (unrelated) directory name.
	dir := filepath.Join(t.TempDir(), "session_20260817_101500_deadbeef")
	msgPath := filepath.Join(dir, "messages.jsonl")
	metaPath := filepath.Join(dir, "meta.json")

	t1Dir := fixturePath("growth-snapshots", "t1", "session_20260817_101500_deadbeef")
	copyFile(t, filepath.Join(t1Dir, "messages.jsonl"), msgPath)
	copyFile(t, filepath.Join(t1Dir, "meta.json"), metaPath)

	a := New()
	first, err := a.ParseSessionFile(context.Background(), msgPath, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.ToolEvents) != 2 { // user prompt + assistant message, no tool calls yet
		t.Fatalf("first pass ToolEvents = %d, want 2", len(first.ToolEvents))
	}
	if len(first.TokenEvents) != 1 {
		t.Fatalf("first pass TokenEvents = %d, want 1", len(first.TokenEvents))
	}
	tk1 := first.TokenEvents[0]
	if tk1.SourceEventID != "tokens:session:deadbeef" {
		t.Errorf("first pass SourceEventID = %q, want tokens:session:deadbeef", tk1.SourceEventID)
	}
	if tk1.InputTokens != 1500 || tk1.OutputTokens != 120 || tk1.CacheReadTokens != 2500 {
		t.Errorf("first pass tokens = in=%d out=%d cache=%d, want in=1500 out=120 cache=2500",
			tk1.InputTokens, tk1.OutputTokens, tk1.CacheReadTokens)
	}

	// The file "grows": same path, t2 content (more lines, larger stats).
	t2Dir := fixturePath("growth-snapshots", "t2", "session_20260817_101500_deadbeef")
	copyFile(t, filepath.Join(t2Dir, "messages.jsonl"), msgPath)
	copyFile(t, filepath.Join(t2Dir, "meta.json"), metaPath)

	second, err := a.ParseSessionFile(context.Background(), msgPath, first.NewOffset)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.ToolEvents) != 1 {
		t.Fatalf("second pass ToolEvents = %d, want exactly 1 NEW tool event (write_file), not a re-emit of pass 1's events", len(second.ToolEvents))
	}
	if second.ToolEvents[0].RawToolName != "write_file" {
		t.Errorf("second pass tool event = %q, want write_file", second.ToolEvents[0].RawToolName)
	}
	if len(second.TokenEvents) != 1 {
		t.Fatalf("second pass TokenEvents = %d, want 1 (session-level event re-emits every parse)", len(second.TokenEvents))
	}
	tk2 := second.TokenEvents[0]
	if tk2.SourceEventID != tk1.SourceEventID {
		t.Errorf("second pass SourceEventID = %q, want the SAME id as pass 1 (%q) so the store's MAX-upgrade applies", tk2.SourceEventID, tk1.SourceEventID)
	}
	if tk2.InputTokens <= tk1.InputTokens || tk2.OutputTokens <= tk1.OutputTokens || tk2.CacheReadTokens <= tk1.CacheReadTokens {
		t.Errorf("second pass tokens (in=%d out=%d cache=%d) should be strictly larger than pass 1 (in=%d out=%d cache=%d)",
			tk2.InputTokens, tk2.OutputTokens, tk2.CacheReadTokens, tk1.InputTokens, tk1.OutputTokens, tk1.CacheReadTokens)
	}
}

// TestDefaultRoots_VibeHomeOverride covers the VIBE_HOME env-var override:
// when set, defaultRoots must include $VIBE_HOME/logs/session.
func TestDefaultRoots_VibeHomeOverride(t *testing.T) {
	vh := t.TempDir()
	t.Setenv("VIBE_HOME", vh)
	roots := defaultRoots()
	want := filepath.Join(vh, "logs", "session")
	found := false
	for _, r := range roots {
		if r == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("defaultRoots() = %v, want it to contain VIBE_HOME override %q", roots, want)
	}
}
