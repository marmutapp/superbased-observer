package patterns

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/store"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pat.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

func seedEvents(t *testing.T, database *sql.DB, root string, events []models.ToolEvent) {
	t.Helper()
	if _, err := store.New(database).Ingest(context.Background(), events, nil, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}
}

func evt(srcID, session, tool, action, target string, ts time.Time, success bool) models.ToolEvent {
	return models.ToolEvent{
		SourceFile: "f-" + session, SourceEventID: srcID,
		SessionID: session, Tool: tool,
		Timestamp: ts, ActionType: action, Target: target, Success: success,
	}
}

// nextUnique returns an event id unique across all tests by using a counter.
var nextCounter int

func nextID() string {
	nextCounter++
	return "e" + itoa(nextCounter)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestDerive_HotFiles(t *testing.T) {
	database := openDB(t)
	root := t.TempDir()
	base := time.Date(2026, 4, 16, 10, 0, 0, 0, time.UTC)
	events := []models.ToolEvent{
		evt(nextID(), "sA", models.ToolClaudeCode, models.ActionReadFile, "hot.go", base, true),
		evt(nextID(), "sA", models.ToolClaudeCode, models.ActionEditFile, "hot.go", base.Add(time.Second), true),
		evt(nextID(), "sA", models.ToolClaudeCode, models.ActionReadFile, "hot.go", base.Add(2*time.Second), true),
		evt(nextID(), "sB", models.ToolCodex, models.ActionReadFile, "hot.go", base.Add(3*time.Second), true),
		evt(nextID(), "sA", models.ToolClaudeCode, models.ActionReadFile, "cold.go", base.Add(4*time.Second), true),
	}
	for i := range events {
		events[i].ProjectRoot = root
	}
	seedEvents(t, database, root, events)

	res, err := New(database).Derive(context.Background(), Options{ProjectRoot: root})
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if res.ByType[TypeHotFile] < 2 {
		t.Fatalf("expected at least 2 hot file rows, got %d", res.ByType[TypeHotFile])
	}

	// Top row should be hot.go with 4 touches and 2 source tools.
	var data, tools string
	var conf float64
	var obs, sessions int
	err = database.QueryRowContext(context.Background(),
		`SELECT pattern_data, confidence, observation_count, source_tools, source_session_count
		 FROM project_patterns
		 WHERE pattern_type = ? ORDER BY confidence DESC LIMIT 1`, TypeHotFile,
	).Scan(&data, &conf, &obs, &tools, &sessions)
	if err != nil {
		t.Fatalf("query top: %v", err)
	}
	var hf hotFileData
	if err := json.Unmarshal([]byte(data), &hf); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if hf.FilePath != "hot.go" || hf.TotalTouch != 4 {
		t.Errorf("top row wrong: %+v", hf)
	}
	if sessions != 2 {
		t.Errorf("SourceSessions: %d, want 2", sessions)
	}
	var toolsList []string
	if err := json.Unmarshal([]byte(tools), &toolsList); err != nil {
		t.Fatalf("tools unmarshal: %v", err)
	}
	if len(toolsList) != 2 {
		t.Errorf("tools: %v", toolsList)
	}
	if conf != 1.0 {
		t.Errorf("top confidence: %v, want 1.0", conf)
	}
}

func TestDerive_CoChange(t *testing.T) {
	database := openDB(t)
	root := t.TempDir()
	base := time.Date(2026, 4, 16, 10, 0, 0, 0, time.UTC)
	// Three sessions that all edit a.go + b.go together — pair count = 3.
	// One session edits a.go + c.go — pair count = 1 (filtered out by the
	// "n < 2" heuristic).
	events := []models.ToolEvent{
		evt(nextID(), "s1", models.ToolClaudeCode, models.ActionEditFile, "a.go", base, true),
		evt(nextID(), "s1", models.ToolClaudeCode, models.ActionEditFile, "b.go", base.Add(time.Second), true),
		evt(nextID(), "s2", models.ToolClaudeCode, models.ActionEditFile, "a.go", base.Add(time.Minute), true),
		evt(nextID(), "s2", models.ToolClaudeCode, models.ActionEditFile, "b.go", base.Add(time.Minute+time.Second), true),
		evt(nextID(), "s3", models.ToolClaudeCode, models.ActionEditFile, "a.go", base.Add(2*time.Minute), true),
		evt(nextID(), "s3", models.ToolClaudeCode, models.ActionEditFile, "b.go", base.Add(2*time.Minute+time.Second), true),
		evt(nextID(), "s4", models.ToolClaudeCode, models.ActionEditFile, "a.go", base.Add(3*time.Minute), true),
		evt(nextID(), "s4", models.ToolClaudeCode, models.ActionEditFile, "c.go", base.Add(3*time.Minute+time.Second), true),
	}
	for i := range events {
		events[i].ProjectRoot = root
	}
	seedEvents(t, database, root, events)

	res, err := New(database).Derive(context.Background(), Options{ProjectRoot: root})
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if res.ByType[TypeCoChange] != 1 {
		t.Fatalf("CoChange: got %d, want 1", res.ByType[TypeCoChange])
	}
	var data string
	if err := database.QueryRowContext(context.Background(),
		`SELECT pattern_data FROM project_patterns WHERE pattern_type = ?`, TypeCoChange,
	).Scan(&data); err != nil {
		t.Fatalf("query: %v", err)
	}
	var cc coChangeData
	_ = json.Unmarshal([]byte(data), &cc)
	if cc.FileA != "a.go" || cc.FileB != "b.go" || cc.PairCount != 3 {
		t.Errorf("co_change row: %+v", cc)
	}
}

func TestDerive_CommonCommands(t *testing.T) {
	database := openDB(t)
	root := t.TempDir()
	base := time.Date(2026, 4, 16, 10, 0, 0, 0, time.UTC)
	events := []models.ToolEvent{
		evt(nextID(), "s", models.ToolClaudeCode, models.ActionRunCommand, "go test ./...", base, true),
		evt(nextID(), "s", models.ToolClaudeCode, models.ActionRunCommand, "go test ./...", base.Add(time.Second), true),
		evt(nextID(), "s", models.ToolClaudeCode, models.ActionRunCommand, "go test ./...", base.Add(2*time.Second), false),
		evt(nextID(), "s", models.ToolClaudeCode, models.ActionRunCommand, "ls", base.Add(3*time.Second), true),
	}
	for i := range events {
		events[i].ProjectRoot = root
	}
	seedEvents(t, database, root, events)
	res, err := New(database).Derive(context.Background(), Options{ProjectRoot: root})
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if res.ByType[TypeCommonCommand] != 1 {
		t.Fatalf("CommonCommand: got %d, want 1 (ls has count 1, filtered)", res.ByType[TypeCommonCommand])
	}
	var data string
	if err := database.QueryRowContext(context.Background(),
		`SELECT pattern_data FROM project_patterns WHERE pattern_type = ?`, TypeCommonCommand,
	).Scan(&data); err != nil {
		t.Fatalf("query: %v", err)
	}
	var c commonCommandData
	_ = json.Unmarshal([]byte(data), &c)
	if c.Command != "go test ./..." || c.RunCount != 3 {
		t.Errorf("command row: %+v", c)
	}
	want := 2.0 / 3
	if diff := c.SuccessRate - want; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("SuccessRate: got %v, want %v", c.SuccessRate, want)
	}
}

func TestDerive_EditTestPairs(t *testing.T) {
	database := openDB(t)
	root := t.TempDir()
	base := time.Date(2026, 4, 16, 10, 0, 0, 0, time.UTC)
	// Two sessions: edit → go test → edit → go test. In each session the
	// pair is {edit_target: x.go, test_command: "go test ./..."}.
	events := []models.ToolEvent{
		evt(nextID(), "s1", models.ToolClaudeCode, models.ActionEditFile, "x.go", base, true),
		evt(nextID(), "s1", models.ToolClaudeCode, models.ActionRunCommand, "go test ./...", base.Add(time.Second), true),
		evt(nextID(), "s2", models.ToolClaudeCode, models.ActionEditFile, "x.go", base.Add(time.Hour), true),
		evt(nextID(), "s2", models.ToolClaudeCode, models.ActionRunCommand, "go test ./...", base.Add(time.Hour+time.Second), true),
	}
	for i := range events {
		events[i].ProjectRoot = root
	}
	seedEvents(t, database, root, events)
	res, err := New(database).Derive(context.Background(), Options{ProjectRoot: root})
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if res.ByType[TypeEditTestPair] != 1 {
		t.Fatalf("EditTestPair: got %d, want 1", res.ByType[TypeEditTestPair])
	}
	var data string
	if err := database.QueryRowContext(context.Background(),
		`SELECT pattern_data FROM project_patterns WHERE pattern_type = ?`, TypeEditTestPair,
	).Scan(&data); err != nil {
		t.Fatalf("query: %v", err)
	}
	var et editTestData
	_ = json.Unmarshal([]byte(data), &et)
	if et.EditTarget != "x.go" || et.TestCommand != "go test ./..." || et.PairCount != 2 {
		t.Errorf("edit_test row: %+v", et)
	}
}

func TestDerive_OnboardingSequences(t *testing.T) {
	database := openDB(t)
	root := t.TempDir()
	base := time.Date(2026, 4, 16, 10, 0, 0, 0, time.UTC)
	// Two sessions with the same first-three reads.
	events := []models.ToolEvent{
		evt(nextID(), "s1", models.ToolClaudeCode, models.ActionReadFile, "README.md", base, true),
		evt(nextID(), "s1", models.ToolClaudeCode, models.ActionReadFile, "main.go", base.Add(time.Second), true),
		evt(nextID(), "s1", models.ToolClaudeCode, models.ActionReadFile, "go.mod", base.Add(2*time.Second), true),
		evt(nextID(), "s1", models.ToolClaudeCode, models.ActionReadFile, "internal/x.go", base.Add(3*time.Second), true),
		evt(nextID(), "s2", models.ToolClaudeCode, models.ActionReadFile, "README.md", base.Add(time.Hour), true),
		evt(nextID(), "s2", models.ToolClaudeCode, models.ActionReadFile, "main.go", base.Add(time.Hour+time.Second), true),
		evt(nextID(), "s2", models.ToolClaudeCode, models.ActionReadFile, "go.mod", base.Add(time.Hour+2*time.Second), true),
	}
	for i := range events {
		events[i].ProjectRoot = root
	}
	seedEvents(t, database, root, events)
	res, err := New(database).Derive(context.Background(), Options{ProjectRoot: root})
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if res.ByType[TypeOnboardingSeq] != 1 {
		t.Fatalf("OnboardingSeq: got %d, want 1", res.ByType[TypeOnboardingSeq])
	}
	var data string
	if err := database.QueryRowContext(context.Background(),
		`SELECT pattern_data FROM project_patterns WHERE pattern_type = ?`, TypeOnboardingSeq,
	).Scan(&data); err != nil {
		t.Fatalf("query: %v", err)
	}
	var ob onboardingData
	_ = json.Unmarshal([]byte(data), &ob)
	if len(ob.FirstReads) != 3 {
		t.Errorf("FirstReads: %v", ob.FirstReads)
	}
}

func TestDerive_CrossToolProvenance(t *testing.T) {
	database := openDB(t)
	root := t.TempDir()
	base := time.Date(2026, 4, 16, 10, 0, 0, 0, time.UTC)
	events := []models.ToolEvent{
		evt(nextID(), "s1", models.ToolClaudeCode, models.ActionReadFile, "shared.go", base, true),
		evt(nextID(), "s2", models.ToolCodex, models.ActionReadFile, "shared.go", base.Add(time.Hour), true),
		evt(nextID(), "s3", models.ToolCursor, models.ActionEditFile, "shared.go", base.Add(2*time.Hour), true),
		evt(nextID(), "s1", models.ToolClaudeCode, models.ActionReadFile, "solo.go", base, true),
	}
	for i := range events {
		events[i].ProjectRoot = root
	}
	seedEvents(t, database, root, events)

	res, err := New(database).Derive(context.Background(), Options{ProjectRoot: root})
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if res.ByType[TypeCrossTool] != 1 {
		t.Fatalf("CrossTool: got %d, want 1", res.ByType[TypeCrossTool])
	}
	var data string
	if err := database.QueryRowContext(context.Background(),
		`SELECT pattern_data FROM project_patterns WHERE pattern_type = ?`, TypeCrossTool,
	).Scan(&data); err != nil {
		t.Fatal(err)
	}
	var ct crossToolData
	_ = json.Unmarshal([]byte(data), &ct)
	if ct.FilePath != "shared.go" {
		t.Errorf("file: %s", ct.FilePath)
	}
	if ct.FirstTool != models.ToolClaudeCode {
		t.Errorf("FirstTool: %s", ct.FirstTool)
	}
	if ct.LastTool != models.ToolCursor {
		t.Errorf("LastTool: %s", ct.LastTool)
	}
	if ct.ToolCount != 3 {
		t.Errorf("ToolCount: %d", ct.ToolCount)
	}
}

func TestDerive_IsIdempotent(t *testing.T) {
	database := openDB(t)
	root := t.TempDir()
	base := time.Date(2026, 4, 16, 10, 0, 0, 0, time.UTC)
	events := []models.ToolEvent{
		evt(nextID(), "s", models.ToolClaudeCode, models.ActionReadFile, "a.go", base, true),
		evt(nextID(), "s", models.ToolClaudeCode, models.ActionReadFile, "a.go", base.Add(time.Second), true),
	}
	for i := range events {
		events[i].ProjectRoot = root
	}
	seedEvents(t, database, root, events)
	d := New(database)
	if _, err := d.Derive(context.Background(), Options{ProjectRoot: root}); err != nil {
		t.Fatalf("first derive: %v", err)
	}
	if _, err := d.Derive(context.Background(), Options{ProjectRoot: root}); err != nil {
		t.Fatalf("second derive: %v", err)
	}
	var n int
	if err := database.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM project_patterns WHERE pattern_type = ?`, TypeHotFile,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	// Exactly one hot_file row — not duplicated across runs.
	if n != 1 {
		t.Errorf("expected 1 hot_file row after 2 derives, got %d", n)
	}
}

func TestDecayFactor(t *testing.T) {
	now := time.Date(2026, 4, 16, 0, 0, 0, 0, time.UTC)
	ten := now.Add(-10 * 24 * time.Hour)
	got := DecayFactor(ten, 10, now) // exactly one half-life
	want := 0.5
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("DecayFactor @ 1 half-life: got %v, want %v", got, want)
	}
	// Reinforced right now → 1.
	if got := DecayFactor(now, 30, now); math.Abs(got-1) > 1e-9 {
		t.Errorf("DecayFactor now: %v", got)
	}
	// Zero half-life → 1.
	if got := DecayFactor(ten, 0, now); got != 1 {
		t.Errorf("DecayFactor zero half: %v", got)
	}
}
