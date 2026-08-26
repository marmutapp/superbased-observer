package cursor

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// newTestStateDB creates a minimal state.vscdb fixture (ItemTable +
// cursorDiskKV, matching the real schema this reader queries) and
// returns its path.
func newTestStateDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.vscdb")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE ItemTable (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`,
		`CREATE TABLE cursorDiskKV (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestMatchesStateDBShape(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{`/home/me/AppData/Roaming/Cursor/User/globalStorage/state.vscdb`, true},
		{`C:\Users\me\AppData\Roaming\Cursor\User\globalStorage\state.vscdb`, true},
		{`/home/me/.config/Cursor/User/globalStorage/state.vscdb`, true},
		{`/home/me/AppData/Roaming/Cursor/User/globalStorage/state.vscdb-wal`, false},
		{`/home/me/AppData/Roaming/Code/User/globalStorage/state.vscdb`, false},
		{`/home/me/.cursor/chats/abc/conv/store.db`, false},
	}
	for _, tc := range tests {
		if got := matchesStateDBShape(tc.path); got != tc.want {
			t.Errorf("matchesStateDBShape(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestParseStateDBFile_EmptyWindowSession(t *testing.T) {
	path := newTestStateDB(t)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	composerID := "11111111-1111-1111-1111-111111111111"
	if _, err := db.Exec(`INSERT INTO cursorDiskKV VALUES(?, ?)`,
		"composerData:"+composerID,
		[]byte(`{"name":"Untitled","createdAt":1735689600000,"unifiedMode":"agent","isAgentic":true,"modelConfig":[{"modelName":"claude-4-sonnet"}]}`),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO cursorDiskKV VALUES(?, ?)`,
		"bubbleId:"+composerID+":bubble-1",
		[]byte(`{"type":1,"text":"hello there","createdAt":1735689601000}`),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO cursorDiskKV VALUES(?, ?)`,
		"bubbleId:"+composerID+":bubble-2",
		[]byte(`{"type":2,"text":"hi, how can I help?","createdAt":1735689602000}`),
	); err != nil {
		t.Fatal(err)
	}
	db.Close()

	a := New()
	res, err := a.parseStateDBFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("parseStateDBFile: %v", err)
	}
	if res.NewOffset <= 0 {
		t.Errorf("NewOffset = %d, want > 0", res.NewOffset)
	}
	if len(res.ToolEvents) != 3 {
		t.Fatalf("got %d tool events, want 3 (session_start + user_prompt + assistant_message): %+v", len(res.ToolEvents), res.ToolEvents)
	}

	var sawStart, sawUser, sawAssistant bool
	for _, ev := range res.ToolEvents {
		if ev.SessionID != composerID {
			t.Errorf("event SessionID = %q, want %q", ev.SessionID, composerID)
		}
		switch ev.ActionType {
		case "session_start":
			sawStart = true
		case "user_prompt":
			sawUser = true
			if ev.RawToolInput != "hello there" {
				t.Errorf("user prompt RawToolInput = %q", ev.RawToolInput)
			}
		case "assistant_message":
			sawAssistant = true
		}
	}
	if !sawStart || !sawUser || !sawAssistant {
		t.Errorf("missing expected event types: start=%v user=%v assistant=%v", sawStart, sawUser, sawAssistant)
	}

	// Re-parsing from the returned watermark should yield no new events.
	res2, err := a.parseStateDBFile(context.Background(), path, res.NewOffset)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if len(res2.ToolEvents) != 0 {
		t.Errorf("re-parse from watermark produced %d events, want 0", len(res2.ToolEvents))
	}
}

func TestParseStateDBFile_SkipsSiblingCoveredSession(t *testing.T) {
	home := t.TempDir()
	composerID := "22222222-2222-2222-2222-222222222222"

	// Sibling agent-transcript exists for this composerID — the
	// conversation is already captured via the richer transcript path,
	// so state.vscdb rows for it must be suppressed.
	transcriptDir := filepath.Join(home, ".cursor", "projects", "some-slug", "agent-transcripts", composerID)
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(transcriptDir, composerID+".jsonl"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	// crossmount.AllHomes()'s native-home resolution reads $HOME (via
	// os.UserHomeDir) on Linux — pin it to the fixture so
	// cursorSiblingExists globs the temp dir instead of the real host
	// home.
	t.Setenv("HOME", home)

	path := newTestStateDB(t)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO cursorDiskKV VALUES(?, ?)`,
		"composerData:"+composerID,
		[]byte(`{"name":"Covered","createdAt":1735689600000}`),
	); err != nil {
		t.Fatal(err)
	}
	db.Close()

	a := New()
	res, err := a.parseStateDBFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("parseStateDBFile: %v", err)
	}
	if len(res.ToolEvents) != 0 {
		t.Errorf("got %d tool events for a sibling-covered session, want 0: %+v", len(res.ToolEvents), res.ToolEvents)
	}
}
