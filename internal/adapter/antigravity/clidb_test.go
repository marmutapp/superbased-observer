package antigravity

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/platform/protowire"
	_ "modernc.org/sqlite"
)

// genBlob builds a gen_metadata.data protobuf blob matching the live CLI
// shape: model at 1.19 and the usage counts at 1.17.2.X (the same field map
// structured.go applies to 1.3.1.17.2.X).
func genBlob(model string, input, cacheCreation, cacheRead, reasoning, output uint64) []byte {
	var usage []byte
	usage = protowire.AppendVarintField(usage, 1, input)
	usage = protowire.AppendVarintField(usage, 2, cacheCreation)
	usage = protowire.AppendVarintField(usage, 5, cacheRead)
	usage = protowire.AppendVarintField(usage, 9, reasoning)
	usage = protowire.AppendVarintField(usage, 10, output)
	inner17 := protowire.AppendBytesField(nil, 2, usage)
	msg1 := protowire.AppendBytesField(nil, 17, inner17)
	msg1 = protowire.AppendBytesField(msg1, 19, []byte(model))
	return protowire.AppendBytesField(nil, 1, msg1)
}

func TestParseCLIDB(t *testing.T) {
	root := t.TempDir()
	uuid := "9b19b445-5ea4-45ec-be7b-704684e694e7"
	dir := filepath.Join(root, ".gemini", "antigravity-cli", "conversations")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, uuid+".db")

	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE gen_metadata (idx integer PRIMARY KEY, data blob, size integer)"); err != nil {
		t.Fatal(err)
	}
	// Two real generations + one all-zero (must be skipped).
	insert := func(idx int, blob []byte) {
		if _, err := db.Exec("INSERT INTO gen_metadata(idx,data,size) VALUES(?,?,?)", idx, blob, len(blob)); err != nil {
			t.Fatal(err)
		}
	}
	insert(0, genBlob("gemini-3-flash-a", 1020, 18091, 0, 169, 71))
	insert(1, genBlob("gemini-3-flash-a", 1020, 6182, 28563, 421, 74))
	insert(2, genBlob("gemini-3-flash-a", 0, 0, 0, 0, 0))
	db.Close()

	a := NewWithOptions(nil, root)
	if !a.IsSessionFile(dbPath) {
		t.Fatalf("IsSessionFile false for %s", dbPath)
	}
	if classifyLayout(dbPath) != LayoutCLIDB {
		t.Fatalf("classifyLayout != LayoutCLIDB")
	}
	res, err := a.ParseSessionFile(context.Background(), dbPath, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.TokenEvents) != 2 {
		t.Fatalf("TokenEvents = %d, want 2 (zero-usage skipped)", len(res.TokenEvents))
	}
	e := res.TokenEvents[1]
	if e.Model != "gemini-3-flash-a" {
		t.Errorf("model = %q", e.Model)
	}
	if e.InputTokens != 1020 || e.CacheCreationTokens != 6182 || e.CacheReadTokens != 28563 || e.ReasoningTokens != 421 || e.OutputTokens != 74 {
		t.Errorf("gen1 tokens wrong: in=%d cC=%d cR=%d reas=%d out=%d", e.InputTokens, e.CacheCreationTokens, e.CacheReadTokens, e.ReasoningTokens, e.OutputTokens)
	}
	if e.SessionID != uuid {
		t.Errorf("session = %q, want %q", e.SessionID, uuid)
	}
}

// TestParseCLIDB_RecoversTextAndProjectRoot covers the two operator-reported
// agy-CLI gaps: (1) the .db carries only token usage, so without the
// transcript augmentation every turn shows "API call (no recovered text)";
// (2) the project root was the "[antigravity]" placeholder because the
// log-regex index missed. Both are fixed by parseCLIDB now running
// augmentResultFromHistory (recovers transcript text) and resolving the
// project root from trajectory_metadata_blob field 18 → config/projects.
func TestParseCLIDB_RecoversTextAndProjectRoot(t *testing.T) {
	root := t.TempDir()
	uuid := "93ad2ba4-bd74-4030-b553-aa4d736e2052"
	projectID := "0f263409-a847-4730-8ef3-a7d028e07091"
	cliRoot := filepath.Join(root, ".gemini", "antigravity-cli")
	convDir := filepath.Join(cliRoot, "conversations")
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(convDir, uuid+".db")

	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE gen_metadata (idx integer PRIMARY KEY, data blob, size integer)"); err != nil {
		t.Fatal(err)
	}
	blob := genBlob("gemini-3.5-flash", 1020, 6182, 28563, 421, 74)
	if _, err := db.Exec("INSERT INTO gen_metadata(idx,data,size) VALUES(0,?,?)", blob, len(blob)); err != nil {
		t.Fatal(err)
	}
	// trajectory_metadata_blob with field 18 = the project id.
	if _, err := db.Exec("CREATE TABLE trajectory_metadata_blob (id text PRIMARY KEY, data blob)"); err != nil {
		t.Fatal(err)
	}
	traj := protowire.AppendBytesField(nil, 6, []byte(uuid))
	traj = protowire.AppendBytesField(traj, 18, []byte(projectID))
	if _, err := db.Exec("INSERT INTO trajectory_metadata_blob(id,data) VALUES('main',?)", traj); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// ~/.gemini/config/projects/<projectID>.json → workspace folderUri.
	projectsDir := filepath.Join(root, ".gemini", "config", "projects")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	projJSON := `{"id":"` + projectID + `","name":"/work/repo","projectResources":{"resources":[{"gitFolder":{"folderUri":"file:///work/repo"}}]}}`
	if err := os.WriteFile(filepath.Join(projectsDir, projectID+".json"), []byte(projJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	// Sibling brain transcript carrying the conversation text.
	trDir := filepath.Join(cliRoot, "brain", uuid, ".system_generated", "logs")
	if err := os.MkdirAll(trDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"<USER_REQUEST>\nSummarize the project.\n</USER_REQUEST>"}` + "\n" +
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","content":"Here is a summary."}` + "\n"
	if err := os.WriteFile(filepath.Join(trDir, "transcript.jsonl"), []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}

	a := NewWithOptions(nil, root)
	res, err := a.ParseSessionFile(context.Background(), dbPath, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.TokenEvents) != 1 {
		t.Fatalf("TokenEvents = %d, want 1", len(res.TokenEvents))
	}
	// Text recovered from the transcript (was zero before the fix).
	if len(res.ToolEvents) == 0 {
		t.Fatal("no recovered text — augmentResultFromHistory not wired into parseCLIDB")
	}
	var sawUser bool
	for _, ev := range res.ToolEvents {
		if ev.ActionType == "user_prompt" {
			sawUser = true
		}
		if ev.ProjectRoot != "/work/repo" {
			t.Fatalf("tool event project root = %q, want /work/repo", ev.ProjectRoot)
		}
	}
	if !sawUser {
		t.Fatal("recovered text missing the user_prompt row")
	}
	// Token rows attribute to the resolved workspace, not "[antigravity]".
	if got := res.TokenEvents[0].ProjectRoot; got != "/work/repo" {
		t.Fatalf("token project root = %q, want /work/repo (trajectory_metadata_blob field 18)", got)
	}
}
