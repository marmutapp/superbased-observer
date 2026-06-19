package learn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRenderMarkdown_Empty(t *testing.T) {
	body := RenderMarkdown(nil, time.Date(2026, 4, 16, 0, 0, 0, 0, time.UTC))
	if !strings.Contains(body, "No recovery patterns observed yet") {
		t.Errorf("empty render: %q", body)
	}
}

func TestRenderMarkdown_IncludesRule(t *testing.T) {
	rules := []Rule{{
		CommandSummary: "go test ./...",
		ErrorCategory:  "test_failure",
		FailureCount:   3,
		RecoveryCount:  2,
		EditedFiles:    []string{"foo.go", "bar.go"},
		ErrorMessage:   "FAIL TestFoo",
	}}
	body := RenderMarkdown(rules, time.Date(2026, 4, 16, 0, 0, 0, 0, time.UTC))
	for _, want := range []string{
		"go test",
		"test_failure",
		"3 failure(s), 2 recovered",
		"`foo.go`",
		"`bar.go`",
		"FAIL TestFoo",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

func TestApply_WritesAndUpdatesManagedBlock(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "CLAUDE.md")
	// Pre-existing content.
	if err := os.WriteFile(path, []byte("# Project\n\nhello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	body := RenderMarkdown([]Rule{{
		CommandSummary: "go test", FailureCount: 2, RecoveryCount: 1, EditedFiles: []string{"x.go"},
	}}, time.Date(2026, 4, 16, 0, 0, 0, 0, time.UTC))
	changed, err := Apply(path, body)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Errorf("first apply should change file")
	}

	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), ManagedBlockStart) {
		t.Errorf("missing start marker: %s", got)
	}
	if !strings.Contains(string(got), "# Project") {
		t.Errorf("pre-existing content lost: %s", got)
	}

	// Idempotent: re-applying same body shouldn't rewrite.
	changed, err = Apply(path, body)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Errorf("second apply with same body should not change")
	}

	// A different body replaces the managed block, preserves outside content.
	body2 := RenderMarkdown([]Rule{{
		CommandSummary: "go vet", FailureCount: 5, RecoveryCount: 3, EditedFiles: []string{"y.go"},
	}}, time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC))
	changed, err = Apply(path, body2)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Errorf("third apply should change")
	}
	got, _ = os.ReadFile(path)
	if !strings.Contains(string(got), "go vet") || strings.Contains(string(got), "go test") {
		t.Errorf("managed block not replaced: %s", got)
	}
	if !strings.Contains(string(got), "# Project") {
		t.Errorf("outside-block content dropped: %s", got)
	}
}

func TestApply_CreatesMissingFile(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "sub", "dir", "CLAUDE.md")
	body := RenderMarkdown([]Rule{{CommandSummary: "foo", FailureCount: 2, RecoveryCount: 2}},
		time.Date(2026, 4, 16, 0, 0, 0, 0, time.UTC))
	changed, err := Apply(path, body)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Errorf("new file apply should change")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file not created: %v", err)
	}
}
