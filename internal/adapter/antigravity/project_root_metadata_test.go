package antigravity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCandidateDocumentPaths(t *testing.T) {
	content := `<USER_REQUEST>
hi
</USER_REQUEST>
<ADDITIONAL_METADATA>
The current local time is: 2026-05-24T12:15:55+05:30.

The user's current state is as follows:
Active Document: c:\programsx\regulation\regulation-saas\scripts\security\README.md (LANGUAGE_MARKDOWN)
Cursor is on line: 1
Other open documents:
- c:\programsx\regulation\regulation-saas\scripts\02_session_persistence.sql (LANGUAGE_UNSPECIFIED)
- c:\programsx\regulation\regulation-saas\scripts\security\README.md (LANGUAGE_MARKDOWN)
- c:\programsx\regulation\old\company_regulation_search_v2.py (LANGUAGE_PYTHON)
- /home/u/project/src/main.go (LANGUAGE_GO)
No browser pages are currently open.
</ADDITIONAL_METADATA>`
	got := candidateDocumentPaths(content)
	want := []string{
		`c:\programsx\regulation\regulation-saas\scripts\security\README.md`,
		`c:\programsx\regulation\regulation-saas\scripts\02_session_persistence.sql`,
		// The active-document path is already deduped from the
		// Other-open-documents bullet list.
		`c:\programsx\regulation\old\company_regulation_search_v2.py`,
		`/home/u/project/src/main.go`,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d paths %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCandidateDocumentPathsEmptyMetadata(t *testing.T) {
	// CLI-from-bash sessions have an ADDITIONAL_METADATA block
	// carrying only the timestamp — no Active Document. Must return
	// empty (caller falls back to existing projectRoot).
	content := `<USER_REQUEST>
hi
</USER_REQUEST>
<ADDITIONAL_METADATA>
The current local time is: 2026-05-24T12:22:16+05:30.
</ADDITIONAL_METADATA>`
	if got := candidateDocumentPaths(content); len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

func TestExtractProjectRootFromMetadata_WalksToGitRoot(t *testing.T) {
	// Build a fake project tree with a .git directory; assert that
	// extractProjectRootFromMetadata walks up from the active
	// document and returns the git root.
	tmp := t.TempDir()
	gitRoot := filepath.Join(tmp, "myproject")
	deepFile := filepath.Join(gitRoot, "src", "components", "Footer.tsx")
	if err := os.MkdirAll(filepath.Dir(deepFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deepFile, []byte("// component"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(gitRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// HEAD file so git.FindRoot's marker check is satisfied.
	if err := os.WriteFile(filepath.Join(gitRoot, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	content := "<ADDITIONAL_METADATA>\nActive Document: " + deepFile + " (LANGUAGE_TSX)\n</ADDITIONAL_METADATA>"
	got := extractProjectRootFromMetadata(content)
	if got == "" {
		t.Fatalf("got empty project root, want %q", gitRoot)
	}
	// Use resolved-path comparison since tempdirs on macOS contain
	// /private symlink prefixes the git walker may unwind.
	if !strings.HasSuffix(got, "myproject") {
		t.Errorf("got %q, want path ending in myproject", got)
	}
}

func TestExtractProjectRootFromMetadata_NoGitReturnsEmpty(t *testing.T) {
	// Active document outside any git tree → no project root.
	tmp := t.TempDir()
	doc := filepath.Join(tmp, "no-git-here", "file.md")
	if err := os.MkdirAll(filepath.Dir(doc), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(doc, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	content := "<ADDITIONAL_METADATA>\nActive Document: " + doc + " (LANGUAGE_MARKDOWN)\n</ADDITIONAL_METADATA>"
	if got := extractProjectRootFromMetadata(content); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestExtractProjectRootFromTranscript_PrefersFirstUserInput(t *testing.T) {
	// Two USER_INPUT entries — the function should walk in order and
	// return the first one that yields a git root.
	tmp := t.TempDir()
	gitRoot := filepath.Join(tmp, "good-project")
	doc := filepath.Join(gitRoot, "src", "main.go")
	_ = os.MkdirAll(filepath.Dir(doc), 0o755)
	_ = os.WriteFile(doc, []byte("// main"), 0o644)
	_ = os.MkdirAll(filepath.Join(gitRoot, ".git"), 0o755)
	_ = os.WriteFile(filepath.Join(gitRoot, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)

	entries := []cliTranscriptEntry{
		{
			Source:  "USER_EXPLICIT",
			Type:    "USER_INPUT",
			Status:  "DONE",
			Content: "<ADDITIONAL_METADATA>\nActive Document: " + doc + " (LANGUAGE_GO)\n</ADDITIONAL_METADATA>",
		},
		{
			Source: "MODEL", Type: "PLANNER_RESPONSE", Status: "DONE",
			Content: "ignored — wrong source",
		},
	}
	got := extractProjectRootFromTranscript(entries)
	if !strings.HasSuffix(got, "good-project") {
		t.Errorf("got %q, want suffix good-project", got)
	}
}

func TestExtractProjectRootFromTranscript_NoUserInputReturnsEmpty(t *testing.T) {
	entries := []cliTranscriptEntry{
		{Source: "MODEL", Type: "PLANNER_RESPONSE", Status: "DONE", Content: "ignored"},
		{Source: "SYSTEM", Type: "CONVERSATION_HISTORY", Status: "DONE"},
	}
	if got := extractProjectRootFromTranscript(entries); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
