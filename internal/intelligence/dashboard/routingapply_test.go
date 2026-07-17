package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/routing"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// seedSubagentEvidence plants the §R10.3 evidence the apply preview
// derives recommendations from: one session whose dominant model is
// opus-class, a subagent_start marker naming the persona, and enough
// sidechain read actions to clear the §R7.2 evidence floor (n ≥ 50).
func seedSubagentEvidence(t *testing.T, s *Server, root, persona string) {
	t.Helper()
	ctx := context.Background()
	st := store.New(s.opts.DB)
	pid, err := st.UpsertProject(ctx, root, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSession(ctx, models.Session{
		ID: "sub1", ProjectID: pid, Tool: "claude-code", StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-2 * time.Hour)
	if _, err := st.InsertAPITurn(ctx, models.APITurn{
		SessionID: "sub1", ProjectID: pid, Timestamp: base,
		Provider: "anthropic", Model: "claude-opus-4-8",
		InputTokens: 1000, OutputTokens: 200,
	}); err != nil {
		t.Fatal(err)
	}
	actions := []models.Action{{
		Tool: "claude-code", SessionID: "sub1", ProjectID: pid,
		ActionType: models.ActionSubagentStart, Target: persona,
		Timestamp: base, Success: true,
		SourceFile: "f", SourceEventID: "sub-start",
	}}
	for i := 0; i < 60; i++ {
		actions = append(actions, models.Action{
			Tool: "claude-code", SessionID: "sub1", ProjectID: pid,
			ActionType: models.ActionReadFile, Target: "x.go",
			Timestamp: base.Add(time.Duration(i+1) * time.Second),
			Success:   true, IsSidechain: true,
			SourceFile: "f", SourceEventID: fmt.Sprintf("sub-r%d", i),
		})
	}
	if _, err := st.InsertActions(ctx, actions); err != nil {
		t.Fatal(err)
	}
}

// TestAPIRoutingApply pins the R2.1 Channel A dashboard arc end to
// end: tool vocabulary 400s, the honest advisory/snippet modes, the
// claude-code dry-run preview with evidence, the one-file consent
// write re-validated against a fresh plan (stale body → 409, nothing
// written), idempotence after the write, and the per-file revert with
// its containment checks.
func TestAPIRoutingApply(t *testing.T) {
	// No t.Parallel: the handler resolves the user agent dir through
	// os.UserHomeDir, isolated here via t.Setenv.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	s, root := newTestServer(t)
	seedSubagentEvidence(t, s, root, "code-reviewer")

	agentDir := filepath.Join(home, ".claude", "agents")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	agentPath := filepath.Join(agentDir, "code-reviewer.md")
	original := "---\nname: code-reviewer\nmodel: claude-opus-4-8\n---\nbody\n"
	if err := os.WriteFile(agentPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}
	post := func(path, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(body)))
		return rec
	}

	// Closed tool vocabulary.
	if rec := get("/api/routing/apply"); rec.Code != 400 {
		t.Errorf("no tool = %d, want 400", rec.Code)
	}
	if rec := get("/api/routing/apply?tool=vibes"); rec.Code != 400 {
		t.Errorf("unknown tool = %d, want 400", rec.Code)
	}

	// Advisory mode is honest, not a write surface.
	rec := get("/api/routing/apply?tool=cursor")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "advisory") {
		t.Errorf("cursor = %d: %s", rec.Code, rec.Body.String())
	}

	// Snippet mode carries the paste-able block with the weak model.
	rec = get("/api/routing/apply?tool=codex")
	var snip struct {
		Mode      string `json:"mode"`
		WeakModel string `json:"weak_model"`
		Snippet   string `json:"snippet"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &snip); err != nil {
		t.Fatalf("snippet unmarshal: %v", err)
	}
	if snip.Mode != "snippet" || snip.WeakModel == "" || !strings.Contains(snip.Snippet, snip.WeakModel) {
		t.Errorf("codex preview = %+v", snip)
	}

	// claude-code preview: the evidence-backed downshift plan.
	wantTarget, ok := routing.NewTierResolver().Table().Representative(
		routing.ShapeForModel("claude-opus-4-8"), routing.TierHaikuClass,
	)
	if !ok {
		t.Fatal("seed tier table has no haiku-class representative")
	}
	rec = get("/api/routing/apply?tool=claude-code")
	if rec.Code != 200 {
		t.Fatalf("preview = %d: %s", rec.Code, rec.Body.String())
	}
	var preview struct {
		Mode    string `json:"mode"`
		Changes []struct {
			Path      string `json:"path"`
			Agent     string `json:"agent"`
			FromModel string `json:"from_model"`
			ToModel   string `json:"to_model"`
			Rationale string `json:"rationale"`
			Evidence  struct {
				Actions int64 `json:"actions"`
				Reads   int64 `json:"reads"`
			} `json:"evidence"`
			Deltas []any `json:"deltas"`
		} `json:"changes"`
		Backups []any    `json:"backups"`
		Skipped []string `json:"skipped"`
		Note    string   `json:"note"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil {
		t.Fatalf("preview unmarshal: %v", err)
	}
	if preview.Mode != "writable" || len(preview.Changes) != 1 {
		t.Fatalf("preview = %+v", preview)
	}
	c := preview.Changes[0]
	if c.Path != agentPath || c.FromModel != "claude-opus-4-8" || c.ToModel != wantTarget {
		t.Fatalf("change = %+v, want %s -> %s at %s", c, "claude-opus-4-8", wantTarget, agentPath)
	}
	if c.Evidence.Actions != 60 || c.Evidence.Reads != 60 || c.Rationale == "" {
		t.Errorf("evidence basis missing: %+v", c)
	}
	if c.Deltas == nil {
		t.Error("deltas key absent — the §R7.2 rows must be present even when empty")
	}
	if preview.Note == "" {
		t.Error("honesty note missing")
	}

	// Stale/hand-crafted body → 409, file untouched.
	rec = post("/api/routing/apply", fmt.Sprintf(`{"tool":"claude-code","path":%q,"to_model":"claude-bogus"}`, agentPath))
	if rec.Code != 409 {
		t.Errorf("stale write = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if got, _ := os.ReadFile(agentPath); string(got) != original {
		t.Fatal("409 path mutated the file")
	}

	// Snippet tools are never written.
	rec = post("/api/routing/apply", `{"tool":"codex","path":"x","to_model":"y"}`)
	if rec.Code != 400 {
		t.Errorf("codex write = %d, want 400", rec.Code)
	}

	// The consented one-file write.
	rec = post("/api/routing/apply", fmt.Sprintf(`{"tool":"claude-code","path":%q,"to_model":%q}`, agentPath, wantTarget))
	if rec.Code != 200 {
		t.Fatalf("write = %d: %s", rec.Code, rec.Body.String())
	}
	var wrote struct {
		Written bool   `json:"written"`
		Backup  string `json:"backup"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &wrote); err != nil {
		t.Fatal(err)
	}
	if !wrote.Written || wrote.Backup == "" {
		t.Fatalf("write response = %+v", wrote)
	}
	if _, err := os.Stat(wrote.Backup); err != nil {
		t.Errorf("backup missing: %v", err)
	}
	updated, _ := os.ReadFile(agentPath)
	if !strings.Contains(string(updated), "model: "+wantTarget) {
		t.Errorf("file not updated: %q", updated)
	}

	// Idempotence: the plan is now empty, backups visible for revert.
	rec = get("/api/routing/apply?tool=claude-code")
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if len(preview.Changes) != 0 || len(preview.Backups) != 1 {
		t.Errorf("post-write preview: %d changes / %d backups, want 0 / 1", len(preview.Changes), len(preview.Backups))
	}

	// Revert containment: outside the agent dirs → 400; no backup → 404.
	if rec := post("/api/routing/apply/revert", `{"path":"/etc/passwd"}`); rec.Code != 400 {
		t.Errorf("outside-dir revert = %d, want 400", rec.Code)
	}
	bare := filepath.Join(agentDir, "no-backup.md")
	if err := os.WriteFile(bare, []byte("---\nname: n\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rec := post("/api/routing/apply/revert", fmt.Sprintf(`{"path":%q}`, bare)); rec.Code != 404 {
		t.Errorf("no-backup revert = %d, want 404", rec.Code)
	}

	// The consented per-file revert restores the original bytes.
	rec = post("/api/routing/apply/revert", fmt.Sprintf(`{"path":%q}`, agentPath))
	if rec.Code != 200 {
		t.Fatalf("revert = %d: %s", rec.Code, rec.Body.String())
	}
	restored, _ := os.ReadFile(agentPath)
	if string(restored) != original {
		t.Errorf("revert did not restore: %q", restored)
	}

	// R2.5 audit trail: the write and the revert both landed in the
	// ledger, newest first, source=dashboard.
	rec = get("/api/routing/apply/ledger")
	if rec.Code != 200 {
		t.Fatalf("ledger = %d: %s", rec.Code, rec.Body.String())
	}
	var ledger struct {
		Events []struct {
			Kind    string `json:"kind"`
			Source  string `json:"source"`
			Path    string `json:"path"`
			ToModel string `json:"to_model"`
		} `json:"events"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ledger); err != nil {
		t.Fatal(err)
	}
	if len(ledger.Events) != 2 || ledger.Path == "" {
		t.Fatalf("ledger = %+v", ledger)
	}
	if ledger.Events[0].Kind != "revert" || ledger.Events[1].Kind != "write" ||
		ledger.Events[1].ToModel != wantTarget ||
		ledger.Events[0].Source != "dashboard" || ledger.Events[0].Path != agentPath {
		t.Errorf("ledger events = %+v", ledger.Events)
	}

	// Unified revert (all=true): re-apply, then revert everything in
	// one consented operation — the CLI --revert contract.
	rec = post("/api/routing/apply", fmt.Sprintf(`{"tool":"claude-code","path":%q,"to_model":%q}`, agentPath, wantTarget))
	if rec.Code != 200 {
		t.Fatalf("re-apply = %d: %s", rec.Code, rec.Body.String())
	}
	rec = post("/api/routing/apply/revert", `{"all":true}`)
	if rec.Code != 200 {
		t.Fatalf("revert-all = %d: %s", rec.Code, rec.Body.String())
	}
	var all struct {
		Restored bool `json:"restored"`
		Count    int  `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil {
		t.Fatal(err)
	}
	if !all.Restored || all.Count != 1 {
		t.Errorf("revert-all = %+v", all)
	}
	after, _ := os.ReadFile(agentPath)
	if string(after) != original {
		t.Errorf("revert-all did not restore: %q", after)
	}
}
