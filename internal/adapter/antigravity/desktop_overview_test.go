package antigravity

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/scrub"
)

func TestDesktopRootFor(t *testing.T) {
	cases := []struct {
		name            string
		sessionPath     string
		wantDesktopRoot string
		wantGeminiRoot  string
	}{
		{
			name:            "WSL2 windows-side desktop .pb",
			sessionPath:     "/mnt/c/Users/auzy_/.gemini/antigravity/conversations/c15725c6.pb",
			wantDesktopRoot: "/mnt/c/Users/auzy_/.gemini/antigravity",
			wantGeminiRoot:  "/mnt/c/Users/auzy_/.gemini",
		},
		{
			name:            "Linux-side desktop .pb",
			sessionPath:     "/home/marmutapp/.gemini/antigravity/conversations/abc-uuid.pb",
			wantDesktopRoot: "/home/marmutapp/.gemini/antigravity",
			wantGeminiRoot:  "/home/marmutapp/.gemini",
		},
		{
			name:            "CLI path does NOT match desktopRootFor",
			sessionPath:     "/home/marmutapp/.gemini/antigravity-cli/conversations/abc-uuid.pb",
			wantDesktopRoot: "",
			wantGeminiRoot:  "",
		},
		{
			name:            "non-antigravity path returns empty",
			sessionPath:     "/home/marmutapp/something/else/abc.pb",
			wantDesktopRoot: "",
			wantGeminiRoot:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			desktop, gemini := desktopRootFor(tc.sessionPath)
			// Normalize the actual values to POSIX separators so the
			// POSIX-literal expectations hold on a Windows test host
			// (production filepath.Join yields '\' there; the Linux
			// daemon yields '/'). Empty stays empty.
			if filepath.ToSlash(desktop) != tc.wantDesktopRoot {
				t.Errorf("desktopRoot = %q, want %q", desktop, tc.wantDesktopRoot)
			}
			if filepath.ToSlash(gemini) != tc.wantGeminiRoot {
				t.Errorf("geminiRoot = %q, want %q", gemini, tc.wantGeminiRoot)
			}
		})
	}
}

func TestTranscriptPathFor(t *testing.T) {
	// All test cases construct paths against directories that don't
	// exist on disk, so desktopTranscriptPath's existence-check
	// fall-through returns the NEW filename (transcript.jsonl) for
	// desktop. The legacy `overview.txt` only wins when it exists
	// on disk while transcript.jsonl doesn't — exercised by
	// TestTranscriptPathForLegacyDesktopOverviewTxt below.
	cases := []struct {
		name        string
		sessionPath string
		convID      string
		want        string
	}{
		{
			name:        "CLI → transcript.jsonl",
			sessionPath: "/home/u/.gemini/antigravity-cli/conversations/abc.pb",
			convID:      "abc",
			want:        "/home/u/.gemini/antigravity-cli/brain/abc/.system_generated/logs/transcript.jsonl",
		},
		{
			name:        "desktop (no sidecar on disk) → transcript.jsonl",
			sessionPath: "/home/u/.gemini/antigravity/conversations/xyz.pb",
			convID:      "xyz",
			want:        "/home/u/.gemini/antigravity/brain/xyz/.system_generated/logs/transcript.jsonl",
		},
		{
			name:        "WSL desktop (no sidecar on disk) → transcript.jsonl under /mnt/c",
			sessionPath: "/mnt/c/Users/auzy_/.gemini/antigravity/conversations/c15725c6.pb",
			convID:      "c15725c6",
			want:        "/mnt/c/Users/auzy_/.gemini/antigravity/brain/c15725c6/.system_generated/logs/transcript.jsonl",
		},
		{
			name:        "unknown layout → empty",
			sessionPath: "/somewhere/random.pb",
			convID:      "abc",
			want:        "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := transcriptPathFor(tc.sessionPath, tc.convID)
			// Normalize the actual value to POSIX separators so the
			// POSIX-literal expectation holds on a Windows test host
			// (production filepath.Join yields '\' there). Empty
			// stays empty.
			if filepath.ToSlash(got) != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTranscriptPathForLegacyDesktopOverviewTxt pins the v1.8.2+
// fallback: when ONLY the legacy `overview.txt` exists on disk (the
// pre-rename desktop Antigravity layout), desktopTranscriptPath
// returns that path even though `transcript.jsonl` is the preferred
// candidate. Without this, an existing operator install that pre-
// dates the rename would silently regress to zero-events-extracted
// after this commit.
func TestTranscriptPathForLegacyDesktopOverviewTxt(t *testing.T) {
	tmp := t.TempDir()
	convID := "legacy-overview-test"
	desktopRoot := filepath.Join(tmp, ".gemini", "antigravity")
	logsDir := filepath.Join(desktopRoot, "brain", convID, ".system_generated", "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Only the legacy file exists. The new-format file does NOT.
	overview := filepath.Join(logsDir, "overview.txt")
	if err := os.WriteFile(overview, []byte(`{"step_index":0,"type":"USER_INPUT","status":"DONE"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(desktopRoot, "conversations", convID+".pb")
	if err := os.MkdirAll(filepath.Dir(sessionPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionPath, []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := transcriptPathFor(sessionPath, convID)
	if got != overview {
		t.Errorf("legacy fallback failed: got %q, want %q", got, overview)
	}
}

// TestTranscriptPathForDesktopTranscriptJsonl pins the v1.8.2+
// preferred path: when `transcript.jsonl` exists on disk (the
// post-rename desktop Antigravity layout, observed 2026-06-06+),
// desktopTranscriptPath returns it even if the legacy `overview.txt`
// also happens to be present (e.g. a stale leftover from before the
// rename). The new file is the source of truth on installs that
// have upgraded.
func TestTranscriptPathForDesktopTranscriptJsonl(t *testing.T) {
	tmp := t.TempDir()
	convID := "new-transcript-test"
	desktopRoot := filepath.Join(tmp, ".gemini", "antigravity")
	logsDir := filepath.Join(desktopRoot, "brain", convID, ".system_generated", "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Both files exist — new format must win.
	newPath := filepath.Join(logsDir, "transcript.jsonl")
	if err := os.WriteFile(newPath, []byte(`{"step_index":0,"type":"USER_INPUT","status":"DONE"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(logsDir, "overview.txt")
	if err := os.WriteFile(legacyPath, []byte(`{"step_index":0,"type":"USER_INPUT","status":"DONE"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(desktopRoot, "conversations", convID+".pb")
	if err := os.MkdirAll(filepath.Dir(sessionPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionPath, []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := transcriptPathFor(sessionPath, convID)
	if got != newPath {
		t.Errorf("preferred path failed: got %q, want %q", got, newPath)
	}
}

// TestDesktopOverviewTxtParsesAsTranscript verifies the desktop
// overview.txt is decoded by the same reader as CLI transcript.jsonl,
// emitting user_prompt + task_complete pairs identically. Catches
// schema drift between the two file names.
func TestDesktopOverviewTxtParsesAsTranscript(t *testing.T) {
	tmp := t.TempDir()
	convID := "c15725c6-dbe1-4747-bab5-b2c8f4755e28"
	desktopRoot := filepath.Join(tmp, ".gemini", "antigravity")
	logsDir := filepath.Join(desktopRoot, "brain", convID, ".system_generated", "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	overview := filepath.Join(logsDir, "overview.txt")
	// One user_input + one planner_response, exactly the schema the
	// operator's live host emits.
	body := `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-05-24T05:05:09Z","content":"<USER_REQUEST>\nCan you summarise this project\n</USER_REQUEST>"}
{"step_index":4,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-05-24T05:05:10Z","content":"Sure — it's a regulatory data platform."}
`
	if err := os.WriteFile(overview, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(desktopRoot, "conversations", convID+".pb")
	if err := os.MkdirAll(filepath.Dir(sessionPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionPath, []byte("encrypted-payload-stub"), 0o644); err != nil {
		t.Fatal(err)
	}

	// transcriptPathFor must resolve the desktop path correctly.
	gotPath := transcriptPathFor(sessionPath, convID)
	if gotPath != overview {
		t.Fatalf("transcriptPathFor = %q, want %q", gotPath, overview)
	}

	// Reader must parse it.
	entries := readCLITranscriptEntries(gotPath)
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].Type != "USER_INPUT" || entries[1].Type != "PLANNER_RESPONSE" {
		t.Errorf("entry types = (%s, %s), want (USER_INPUT, PLANNER_RESPONSE)", entries[0].Type, entries[1].Type)
	}

	// Synthesis end-to-end must produce 2 ToolEvents (user + assistant).
	events := synthesizeTranscriptEvents(
		sessionPath, convID, "/test/project", convID,
		scrub.New(), entries, nil,
		nil, nil,
	)
	if len(events) != 2 {
		t.Fatalf("synthesized %d events, want 2", len(events))
	}
	if events[0].Target == "" || events[1].Target == "" {
		t.Error("synthesized events should carry non-empty Target text")
	}
}

// TestHistoryOnlyResultDesktop verifies the escape-hatch path
// (decrypt + gRPC both failed) surfaces desktop overview.txt content
// — the path the operator's c15725c6 case needed.
func TestHistoryOnlyResultDesktop(t *testing.T) {
	tmp := t.TempDir()
	convID := "abc12345-feed-face-c0de-deadbeef1234"
	desktopRoot := filepath.Join(tmp, ".gemini", "antigravity")
	logsDir := filepath.Join(desktopRoot, "brain", convID, ".system_generated", "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	overview := filepath.Join(logsDir, "overview.txt")
	body := `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-05-24T05:05:09Z","content":"<USER_REQUEST>\nhello\n</USER_REQUEST>"}
{"step_index":4,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-05-24T05:05:10Z","content":"hi there"}
`
	if err := os.WriteFile(overview, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(desktopRoot, "conversations", convID+".pb")
	if err := os.MkdirAll(filepath.Dir(sessionPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionPath, []byte("encrypted-payload-stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	a := NewWithOptions(scrub.New())
	res := a.historyOnlyResult(sessionPath, fi)
	if res == nil {
		t.Fatal("historyOnlyResult returned nil for desktop with overview.txt entries; want populated ParseResult")
	}
	if len(res.ToolEvents) != 2 {
		t.Errorf("got %d ToolEvents, want 2", len(res.ToolEvents))
	}
	if len(res.Warnings) == 0 {
		t.Error("expected a warning naming overview.txt as the surface source")
	}
}

// TestHistoryOnlyResultDesktopNoOverviewReturnsNil — when desktop
// has no plaintext source, historyOnlyResult must return nil (not
// fall through to history.jsonl which is CLI-only) so the caller
// falls back to its existing unrecoverable-marking path.
func TestHistoryOnlyResultDesktopNoOverviewReturnsNil(t *testing.T) {
	tmp := t.TempDir()
	convID := "nope-no-overview"
	desktopRoot := filepath.Join(tmp, ".gemini", "antigravity")
	convDir := filepath.Join(desktopRoot, "conversations")
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(convDir, convID+".pb")
	if err := os.WriteFile(sessionPath, []byte("encrypted-stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(sessionPath)
	a := NewWithOptions(scrub.New())
	if res := a.historyOnlyResult(sessionPath, fi); res != nil {
		t.Errorf("desktop with no overview.txt should return nil; got %+v", res)
	}
}
