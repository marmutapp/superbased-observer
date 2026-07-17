package routingapply

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/routing"
)

func writeAgentFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+".md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestPlan_Rows pins the §R10 planner: evidence-backed suggestion
// edits, idempotent skip at target, missing-file and no-frontmatter
// notes, keep-recommendations untouched.
func TestPlan_Rows(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "agents")
	writeAgentFile(t, dir, "code-reviewer", "---\nname: code-reviewer\nmodel: claude-opus-4-8\n---\nbody\n")
	writeAgentFile(t, dir, "already-there", "---\nname: already-there\nmodel: claude-haiku-4-5\n---\nbody\n")
	writeAgentFile(t, dir, "no-frontmatter", "just a prompt body\n")

	recs := []routing.SubagentRecommendation{
		{Name: "code-reviewer", SuggestedModel: "claude-haiku-4-5", Rationale: "read-shaped profile at parity"},
		{Name: "already-there", SuggestedModel: "claude-haiku-4-5", Rationale: "x"},
		{Name: "no-frontmatter", SuggestedModel: "claude-haiku-4-5", Rationale: "x"},
		{Name: "ghost-agent", SuggestedModel: "claude-haiku-4-5", Rationale: "x"},
		{Name: "keeper", Rationale: "insufficient evidence"}, // no suggestion
	}
	changes, skipped := Plan([]string{dir}, recs)
	if len(changes) != 1 || changes[0].Agent != "code-reviewer" ||
		changes[0].FromModel != "claude-opus-4-8" || changes[0].ToModel != "claude-haiku-4-5" {
		t.Fatalf("changes = %+v", changes)
	}
	if len(skipped) != 2 {
		t.Errorf("skipped = %v, want ghost + no-frontmatter notes", skipped)
	}
}

// TestWrite_BackupAndRevert pins the write arc: backup lands, the model
// line is replaced with every other byte preserved, a second plan is a
// no-op (idempotent), ListBackups sees the backup, and Revert restores.
func TestWrite_BackupAndRevert(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "agents")
	original := "---\nname: code-reviewer\ndescription: reviews code\nmodel: claude-opus-4-8\ntools: [Read, Grep]\n---\nThe body stays byte-identical.\n"
	path := writeAgentFile(t, dir, "code-reviewer", original)

	c := Change{Path: path, Agent: "code-reviewer", FromModel: "claude-opus-4-8", ToModel: "claude-haiku-4-5"}
	if err := Write(c, "20260611T000000Z"); err != nil {
		t.Fatalf("write: %v", err)
	}
	updated, _ := os.ReadFile(path)
	if !strings.Contains(string(updated), "model: claude-haiku-4-5") ||
		strings.Contains(string(updated), "claude-opus-4-8") {
		t.Errorf("model line not replaced: %q", updated)
	}
	if !strings.Contains(string(updated), "description: reviews code") ||
		!strings.Contains(string(updated), "The body stays byte-identical.") {
		t.Errorf("other content lost: %q", updated)
	}
	backup, err := os.ReadFile(path + BackupPrefix + "20260611T000000Z")
	if err != nil || string(backup) != original {
		t.Fatalf("backup wrong: %v %q", err, backup)
	}

	// Idempotence: re-planning the same suggestion now yields nothing.
	changes, _ := Plan([]string{dir}, []routing.SubagentRecommendation{
		{Name: "code-reviewer", SuggestedModel: "claude-haiku-4-5"},
	})
	if len(changes) != 0 {
		t.Errorf("second plan = %+v, want idempotent no-op", changes)
	}

	// ListBackups surfaces exactly the one restorable file.
	backups := ListBackups([]string{dir})
	if len(backups) != 1 || backups[0].Path != path || backups[0].Stamp != "20260611T000000Z" {
		t.Fatalf("ListBackups = %+v", backups)
	}

	// Revert restores the original bytes.
	restored, err := Revert([]string{dir})
	if err != nil {
		t.Fatalf("revert: %v", err)
	}
	if len(restored) != 1 || restored[0].Path != path {
		t.Errorf("restored = %+v", restored)
	}
	after, _ := os.ReadFile(path)
	if string(after) != original {
		t.Errorf("revert did not restore: %q", after)
	}
}

// TestRevertFile_NewestWins pins the per-file revert: with two stamps
// on disk, the NEWEST backup is restored; a file with no backup wraps
// os.ErrNotExist (the dashboard's 404 contract).
func TestRevertFile_NewestWins(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "agents")
	path := writeAgentFile(t, dir, "a", "current\n")
	if err := os.WriteFile(path+BackupPrefix+"20260601T000000Z", []byte("older\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+BackupPrefix+"20260610T000000Z", []byte("newer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := RevertFile(path)
	if err != nil {
		t.Fatalf("revert: %v", err)
	}
	if !strings.HasSuffix(res.Backup, "20260610T000000Z") {
		t.Errorf("restored from %s, want the newest stamp", res.Backup)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "newer\n" {
		t.Errorf("content = %q, want the newest backup's", got)
	}

	bare := writeAgentFile(t, dir, "no-backup", "x\n")
	if _, err := RevertFile(bare); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("no-backup revert err = %v, want os.ErrNotExist wrap", err)
	}
}

// TestFrontmatterModel_Rows pins the parser.
func TestFrontmatterModel_Rows(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		body  string
		model string
		hasFM bool
	}{
		{"with_model", "---\nname: x\nmodel: m1\n---\nbody", "m1", true},
		{"without_model_line", "---\nname: x\n---\nbody", "", true},
		{"no_frontmatter", "plain body", "", false},
		{"unterminated", "---\nname: x\nbody", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, has := frontmatterModel(tc.body)
			if m != tc.model || has != tc.hasFM {
				t.Errorf("frontmatterModel = (%q, %v), want (%q, %v)", m, has, tc.model, tc.hasFM)
			}
		})
	}
}

// TestToolModeAndSnippets pins the §R10.2 support matrix and the
// snippet emission: every tool resolves to exactly one mode, snippet
// tools embed the weak model, and WeakModel falls back to the haiku
// representative.
func TestToolModeAndSnippets(t *testing.T) {
	t.Parallel()
	wantModes := map[string]string{
		"claude-code": ModeWritable,
		"codex":       ModeSnippet, "aider": ModeSnippet, "zed": ModeSnippet,
		"kilo-code": ModeSnippet, "cline": ModeSnippet, "opencode": ModeSnippet,
		"cursor": ModeAdvisory, "copilot": ModeAdvisory,
	}
	for _, tool := range Tools() {
		mode, ok := ToolMode(tool)
		if !ok || mode != wantModes[tool] {
			t.Errorf("ToolMode(%s) = (%s, %v), want (%s, true)", tool, mode, ok, wantModes[tool])
		}
	}
	if _, ok := ToolMode("vibes"); ok {
		t.Error("unknown tool resolved")
	}

	for _, tool := range []string{"codex", "aider", "zed", "kilo-code", "cline", "opencode"} {
		snippet, ok := AdvisorySnippet(tool, "claude-haiku-4-5")
		if !ok || !strings.Contains(snippet, "claude-haiku-4-5") {
			t.Errorf("AdvisorySnippet(%s) = (%q, %v)", tool, snippet, ok)
		}
	}
	for _, tool := range []string{"claude-code", "cursor", "copilot"} {
		if _, ok := AdvisorySnippet(tool, "m"); ok {
			t.Errorf("AdvisorySnippet(%s) should have no snippet", tool)
		}
	}

	if got := WeakModel(nil); got != "claude-haiku-4-5" {
		t.Errorf("WeakModel(nil) = %q", got)
	}
	if got := WeakModel([]routing.SubagentRecommendation{{Name: "k"}, {Name: "s", SuggestedModel: "m2"}}); got != "m2" {
		t.Errorf("WeakModel = %q, want the first suggestion", got)
	}
}
