package kirocli

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
)

// convInsert is one conversations_v2 row to seed.
type convInsert struct {
	key            string
	conversationID string
	value          string
	createdAt      int64
	updatedAt      int64
}

// newStateDB builds a data.sqlite3 in dir with conversations_v2 seeded
// from rows. When withSecrets is true it ALSO creates the auth_kv +
// state tables and fills them with sentinel secrets — the parser must
// never read them, which the never-read-table test asserts by scanning
// the emitted events for the sentinels.
func newStateDB(t *testing.T, dir string, rows []convInsert, withSecrets bool) string {
	t.Helper()
	// The DB must live under a kiro-cli dir so classifyLayout recognises
	// it (belt-and-braces gate), mirroring the real
	// ~/.local/share/kiro-cli/ location.
	dir = filepath.Join(dir, ".local", "share", "kiro-cli")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "data.sqlite3")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE conversations_v2 (
		key TEXT NOT NULL, conversation_id TEXT NOT NULL, value TEXT NOT NULL,
		created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
		PRIMARY KEY (key, conversation_id));`); err != nil {
		t.Fatalf("create conversations_v2: %v", err)
	}
	for _, r := range rows {
		if _, err := db.Exec(
			`INSERT INTO conversations_v2 (key, conversation_id, value, created_at, updated_at) VALUES (?,?,?,?,?)`,
			r.key, r.conversationID, r.value, r.createdAt, r.updatedAt,
		); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if withSecrets {
		if _, err := db.Exec(`CREATE TABLE auth_kv (key TEXT PRIMARY KEY, value TEXT);`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO auth_kv VALUES ('kirocli:social:token', 'SENTINEL_AUTH_SECRET');`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`CREATE TABLE state (key TEXT PRIMARY KEY, value BLOB);`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO state VALUES ('telemetry-cognito-credentials', 'SENTINEL_STATE_SECRET');`); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func fixtureValue(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "kirocli", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestParseStateDBToolUses(t *testing.T) {
	dir := t.TempDir()
	val := fixtureValue(t, "conversations_v2-value.json")
	path := newStateDB(t, dir, []convInsert{
		{key: "/home/dev/project", conversationID: "cccccccc-3333-4f7f-860e-000000000003", value: val, createdAt: 1783571416000, updatedAt: 1783571424374},
	}, false)

	a := NewWithOptions(nil, dir)
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}

	byAction := map[string]models.ToolEvent{}
	for _, e := range res.ToolEvents {
		byAction[e.ActionType] = e
	}
	// fs_write → write_file, execute_bash → run_command, Response →
	// assistant_message, Prompt → user_prompt.
	fw, ok := byAction[models.ActionWriteFile]
	if !ok {
		t.Fatalf("no write_file event; got %+v", res.ToolEvents)
	}
	if fw.RawToolName != "fs_write" {
		t.Errorf("write_file RawToolName = %q, want fs_write", fw.RawToolName)
	}
	if fw.Target != "/home/dev/project/hello.txt" {
		t.Errorf("write_file Target = %q", fw.Target)
	}
	if fw.ContentBytes != int64(len("hello from kiro")) {
		t.Errorf("write_file ContentBytes = %d, want %d", fw.ContentBytes, len("hello from kiro"))
	}
	if fw.SourceEventID != "tooluse_AAAA0001" {
		t.Errorf("write_file SourceEventID = %q, want tooluse_AAAA0001", fw.SourceEventID)
	}
	if !strings.HasSuffix(fw.SourceFile, "data.sqlite3") {
		t.Errorf("SourceFile = %q, want *data.sqlite3", fw.SourceFile)
	}

	rc, ok := byAction[models.ActionRunCommand]
	if !ok {
		t.Fatalf("no run_command event")
	}
	if rc.Target != "ls" {
		t.Errorf("run_command Target = %q, want ls", rc.Target)
	}
	// Result attached: execute_bash succeeded and its stdout is captured.
	if !rc.Success {
		t.Errorf("run_command Success = false, want true")
	}
	if !strings.Contains(rc.ToolOutput, "hello.txt") {
		t.Errorf("run_command ToolOutput missing result: %q", rc.ToolOutput)
	}

	if _, ok := byAction[models.ActionAssistantMessage]; !ok {
		t.Errorf("no assistant_message event")
	}
	if _, ok := byAction[models.ActionUserPrompt]; !ok {
		t.Errorf("no user_prompt event")
	}

	// Every capture to date has null token fields → no token events.
	if len(res.TokenEvents) != 0 {
		t.Errorf("want 0 token events (all null in capture), got %d", len(res.TokenEvents))
	}

	if res.NewOffset != 1783571424374 {
		t.Errorf("NewOffset = %d, want the row updated_at", res.NewOffset)
	}
}

func TestParseStateDBTokenBearing(t *testing.T) {
	// Synthetic value with NON-null token fields to exercise the token
	// path (real captures are all-null; this proves the mapping).
	const val = `{"conversation_id":"tok-1","history":[
		{"user":{"env_context":{"env_state":{"current_working_directory":"/home/dev/project"}},
		         "content":{"Prompt":{"prompt":"hi"}},"timestamp":null},
		 "assistant":{"Response":{"message_id":"m1","content":"hello"}},
		 "request_metadata":{"request_id":"r1","message_id":"m1","model_id":"auto",
		   "request_start_timestamp_ms":1783571416209,
		   "total_tokens":150,"uncached_input_tokens":100,"output_tokens":40,
		   "cache_read_input_tokens":10,"cache_write_input_tokens":0}}
	]}`
	dir := t.TempDir()
	path := newStateDB(t, dir, []convInsert{
		{key: "/home/dev/project", conversationID: "tok-1", value: val, createdAt: 1, updatedAt: 100},
	}, false)
	a := NewWithOptions(nil, dir)
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.TokenEvents) != 1 {
		t.Fatalf("want 1 token event, got %d", len(res.TokenEvents))
	}
	te := res.TokenEvents[0]
	// uncached_input_tokens is NET → maps straight to InputTokens.
	if te.InputTokens != 100 || te.OutputTokens != 40 || te.CacheReadTokens != 10 {
		t.Errorf("token mapping = in %d out %d cacheRead %d, want 100/40/10",
			te.InputTokens, te.OutputTokens, te.CacheReadTokens)
	}
	if te.SourceEventID != "m1:tok" {
		t.Errorf("token SourceEventID = %q, want m1:tok", te.SourceEventID)
	}
}

func TestParseStateDBWindowsKeyTranslation(t *testing.T) {
	val := fixtureValue(t, "conversations_v2-value.json")
	dir := t.TempDir()
	// Windows-shaped KEY (raw cwd): the adapter must translate it to a
	// /mnt/c path via crossmount BEFORE git.Resolve so the drive-letter
	// string never gets CWD-prefixed onto the observer's own repo.
	path := newStateDB(t, dir, []convInsert{
		{key: `C:\tmp\sbo-capture\kiro`, conversationID: "win-1", value: val, createdAt: 1, updatedAt: 100},
	}, false)
	a := NewWithOptions(nil, dir)
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ToolEvents) == 0 {
		t.Fatal("no events")
	}
	root := res.ToolEvents[0].ProjectRoot
	if !strings.HasPrefix(root, "/mnt/c/tmp/sbo-capture/kiro") {
		t.Errorf("ProjectRoot = %q, want a /mnt/c translation of the C:\\ key", root)
	}
	if strings.Contains(root, "superbased-observer") {
		t.Errorf("ProjectRoot leaked the observer repo (git.Resolve CWD-prefix bug): %q", root)
	}
}

func TestParseStateDBWatermarkResumption(t *testing.T) {
	val := fixtureValue(t, "conversations_v2-value.json")
	dir := t.TempDir()
	path := newStateDB(t, dir, []convInsert{
		{key: "/home/dev/a", conversationID: "conv-a", value: strings.ReplaceAll(val, "cccccccc-3333-4f7f-860e-000000000003", "conv-a"), createdAt: 1, updatedAt: 100},
		{key: "/home/dev/b", conversationID: "conv-b", value: strings.ReplaceAll(val, "cccccccc-3333-4f7f-860e-000000000003", "conv-b"), createdAt: 1, updatedAt: 200},
	}, false)
	a := NewWithOptions(nil, dir)

	full, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if full.NewOffset != 200 {
		t.Errorf("full NewOffset = %d, want 200", full.NewOffset)
	}
	sessions := map[string]bool{}
	for _, e := range full.ToolEvents {
		sessions[e.SessionID] = true
	}
	if !sessions["conv-a"] || !sessions["conv-b"] {
		t.Errorf("full scan missing a session: %v", sessions)
	}

	// Resume past conv-a's watermark → only conv-b's rows.
	inc, err := a.ParseSessionFile(context.Background(), path, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range inc.ToolEvents {
		if e.SessionID == "conv-a" {
			t.Errorf("resume from 100 re-emitted conv-a event %q", e.SourceEventID)
		}
	}
	if inc.NewOffset != 200 {
		t.Errorf("incremental NewOffset = %d, want 200", inc.NewOffset)
	}
}

func TestParseStateDBNeverReadsSecretTables(t *testing.T) {
	val := fixtureValue(t, "conversations_v2-value.json")
	dir := t.TempDir()
	path := newStateDB(t, dir, []convInsert{
		{key: "/home/dev/project", conversationID: "c1", value: val, createdAt: 1, updatedAt: 100},
	}, true) // auth_kv + state seeded with sentinels
	a := NewWithOptions(nil, dir)
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatal(err)
	}
	// No emitted string may contain the auth/state sentinels.
	for _, e := range res.ToolEvents {
		blob := e.Target + e.RawToolInput + e.ToolOutput + e.ErrorMessage
		if strings.Contains(blob, "SENTINEL_AUTH_SECRET") || strings.Contains(blob, "SENTINEL_STATE_SECRET") {
			t.Fatalf("adapter leaked a secret-table value: %q", blob)
		}
	}
	if len(res.ToolEvents) == 0 {
		t.Fatal("expected conversation events even with secret tables present")
	}
}

func TestParseStateDBToleratesMissingTable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".local", "share", "kiro-cli")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "data.sqlite3")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	// Fresh install: only the legacy tables exist, no conversations_v2.
	if _, err := db.Exec(`CREATE TABLE migrations (id INTEGER PRIMARY KEY);`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	a := NewWithOptions(nil, dir)
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("missing conversations_v2 must not error: %v", err)
	}
	if len(res.ToolEvents) != 0 {
		t.Errorf("want no events, got %d", len(res.ToolEvents))
	}
}

func TestReadTranscriptSQLite(t *testing.T) {
	val := fixtureValue(t, "conversations_v2-value.json")
	dir := t.TempDir()
	path := newStateDB(t, dir, []convInsert{
		{key: "/home/dev/project", conversationID: "cccccccc-3333-4f7f-860e-000000000003", value: val, createdAt: 1, updatedAt: 100},
	}, false)
	a := NewWithOptions(nil, dir)
	msgs, err := a.ReadTranscript(context.Background(),
		models.Session{ID: "cccccccc-3333-4f7f-860e-000000000003"}, []string{path})
	if err != nil {
		t.Fatalf("ReadTranscript: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("empty transcript")
	}
	// The tool calls must appear on an assistant exchange.
	var calls int
	for _, m := range msgs {
		calls += len(m.ToolCalls)
	}
	if calls < 2 {
		t.Errorf("want >=2 tool calls in transcript, got %d", calls)
	}
}

func TestNormalizeToolMapping(t *testing.T) {
	cases := []struct {
		name       string
		args       string
		wantAction string
		wantTarget string
		wantBytes  int64
	}{
		{"fs_write", `{"command":"create","path":"/a.txt","file_text":"hello"}`, models.ActionWriteFile, "/a.txt", 5},
		{"fs_write", `{"command":"str_replace","path":"/b.go","new_str":"xyz"}`, models.ActionEditFile, "/b.go", 3},
		{"fs_read", `{"operations":[{"mode":"Line","path":"/c.md"}]}`, models.ActionReadFile, "/c.md", 0},
		{"fs_read", `{"path":"/d.md"}`, models.ActionReadFile, "/d.md", 0},
		{"execute_bash", `{"command":"ls -la","working_dir":"/x"}`, models.ActionRunCommand, "ls -la", 0},
		{"introspect", `{"query":"help"}`, models.ActionUnknown, "", 0},
		{"awslabs.core___read", `{}`, models.ActionMCPCall, "", 0},
		{"mystery_tool", `{}`, models.ActionUnknown, "", 0},
	}
	for _, tc := range cases {
		gotA, gotT, gotB := normalizeTool(tc.name, []byte(tc.args))
		if gotA != tc.wantAction || gotT != tc.wantTarget || gotB != tc.wantBytes {
			t.Errorf("normalizeTool(%q) = (%q,%q,%d), want (%q,%q,%d)",
				tc.name, gotA, gotT, gotB, tc.wantAction, tc.wantTarget, tc.wantBytes)
		}
	}
}

func TestIsForeignMountPath(t *testing.T) {
	withHomes(t, []crossmount.HomeRoot{
		{Path: "/home/dev", OS: crossmount.OSLinux, Origin: "native"},
		{Path: "/mnt/c/Users/win", OS: crossmount.OSWindows, Origin: "wsl-mnt:win"},
	})
	if isForeignMountPath("/home/dev/.local/share/kiro-cli/data.sqlite3") {
		t.Errorf("native path flagged foreign")
	}
	if !isForeignMountPath("/mnt/c/Users/win/AppData/Local/Kiro-Cli/data.sqlite3") {
		t.Errorf("foreign-mount path not flagged")
	}
}
