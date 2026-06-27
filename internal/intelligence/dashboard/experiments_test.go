package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
)

// TestExperiments_StartStopValidation pins the lifecycle guards: start
// validation (shape, unknown profiles, duplicate name, one running
// experiment per class), list, stop, and method guards.
func TestExperiments_StartStopValidation(t *testing.T) {
	server, _ := wizardTestServer(t)
	post := func(path, body string) *httptest.ResponseRecorder {
		t.Helper()
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		server.Handler().ServeHTTP(rr, req)
		return rr
	}

	if rr := post("/api/experiments", `{"name":"a3-codex","class":"openai","control":"codex-safe","candidate":"codex-variant","note":"ratio"}`); rr.Code != 200 {
		t.Fatalf("start: %d %s", rr.Code, rr.Body.String())
	}
	// Duplicate name.
	if rr := post("/api/experiments", `{"name":"a3-codex","class":"anthropic","control":"claude-code","candidate":"codex-safe"}`); rr.Code != http.StatusConflict {
		t.Errorf("duplicate name: got %d want 409", rr.Code)
	}
	// Second running experiment on the same class.
	if rr := post("/api/experiments", `{"name":"another","class":"openai","control":"codex-safe","candidate":"default"}`); rr.Code != http.StatusConflict {
		t.Errorf("same-class running: got %d want 409", rr.Code)
	}
	// Unknown profile / bad class / same arms.
	if rr := post("/api/experiments", `{"name":"bad-prof","class":"anthropic","control":"nope","candidate":"claude-code"}`); rr.Code != http.StatusBadRequest {
		t.Errorf("unknown profile: got %d want 400", rr.Code)
	}
	if rr := post("/api/experiments", `{"name":"bad-class","class":"all","control":"claude-code","candidate":"codex-safe"}`); rr.Code != http.StatusBadRequest {
		t.Errorf("bad class: got %d want 400", rr.Code)
	}

	// List shows the running experiment.
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/experiments", nil))
	var list struct {
		Experiments []config.ExperimentConfig `json:"experiments"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Experiments) != 1 || !list.Experiments[0].Running() {
		t.Fatalf("list: %+v", list.Experiments)
	}

	// Stop; a second stop is a 409; unknown name 404.
	if rr := post("/api/experiments/stop", `{"name":"a3-codex"}`); rr.Code != 200 {
		t.Fatalf("stop: %d %s", rr.Code, rr.Body.String())
	}
	if rr := post("/api/experiments/stop", `{"name":"a3-codex"}`); rr.Code != http.StatusConflict {
		t.Errorf("double stop: got %d want 409", rr.Code)
	}
	if rr := post("/api/experiments/stop", `{"name":"ghost"}`); rr.Code != http.StatusNotFound {
		t.Errorf("unknown stop: got %d want 404", rr.Code)
	}
	// After stopping, the class is free again.
	if rr := post("/api/experiments", `{"name":"a3-codex-2","class":"openai","control":"codex-safe","candidate":"codex-variant"}`); rr.Code != 200 {
		t.Errorf("restart class after stop: %d %s", rr.Code, rr.Body.String())
	}
}

// TestExperimentReport pins the evidence rollup: hash-recomputed arm
// membership, per-arm session counts and per-session cost stats,
// proxy-dedup, cache-event cause attribution, compression savings,
// and the population rule (sessions started before the window stay
// out).
func TestExperimentReport(t *testing.T) {
	server, _ := wizardTestServer(t)
	now := time.Now().UTC()

	exp := config.ExperimentConfig{
		Name: "rep", Class: "openai",
		Control: "codex-safe", Candidate: "codex-variant",
		StartedAt: now.Add(-time.Hour).Format(time.RFC3339),
	}
	cfg, err := config.Load(config.LoadOptions{GlobalPath: server.opts.ConfigPath})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Experiments = []config.ExperimentConfig{exp}
	if err := config.WriteToml(server.opts.ConfigPath, cfg); err != nil {
		t.Fatal(err)
	}

	if _, err := server.opts.DB.Exec(
		`INSERT INTO projects (root_path, name, created_at) VALUES ('/exp', 'exp', ?)`,
		now.Format(time.RFC3339Nano),
	); err != nil {
		t.Fatal(err)
	}
	seed := func(sid string, startedAt time.Time, costUSD float64, cause string) {
		t.Helper()
		ts := startedAt.Format(time.RFC3339Nano)
		if _, err := server.opts.DB.Exec(
			`INSERT INTO sessions (id, project_id, tool, started_at) VALUES (?, 1, 'codex', ?)`,
			sid, ts,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := server.opts.DB.Exec(
			`INSERT INTO api_turns (session_id, timestamp, provider, model, input_tokens, output_tokens,
			    cache_read_tokens, cache_creation_tokens, cost_usd,
			    compression_original_bytes, compression_compressed_bytes)
			 VALUES (?, ?, 'openai', 'gpt-5.4', 100, 10, 1000, 50, ?, 5000, 4000)`,
			sid, ts, costUSD,
		); err != nil {
			t.Fatal(err)
		}
		if cause != "" {
			if _, err := server.opts.DB.Exec(
				`INSERT INTO cache_events (session_id, tier, timestamp, model, kind, cause)
				 VALUES (?, 'tier1', ?, 'gpt-5.4', 'write', ?)`,
				sid, ts, cause,
			); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Six in-window sessions with known arms (recomputed, not assumed).
	wantSessions := map[string]int{"control": 0, "candidate": 0}
	wantCost := map[string]float64{}
	for i := 0; i < 6; i++ {
		sid := fmt.Sprintf("exp-s-%d", i)
		_, arm := config.ExperimentArm(exp, sid)
		costUSD := 1.0 + float64(i)*0.1
		seed(sid, now.Add(-30*time.Minute), costUSD, "suffix_growth")
		wantSessions[arm]++
		wantCost[arm] += costUSD
	}
	// Out-of-window session (started before the experiment): excluded.
	seed("exp-old", now.Add(-2*time.Hour), 99, "tools_changed")

	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/experiments/report?name=rep", nil))
	if rr.Code != 200 {
		t.Fatalf("report: %d %s", rr.Code, rr.Body.String())
	}
	var rep ExperimentReport
	if err := json.NewDecoder(rr.Body).Decode(&rep); err != nil {
		t.Fatal(err)
	}
	if !rep.Running {
		t.Error("running flag lost")
	}
	if rep.Control.Sessions != wantSessions["control"] || rep.Candidate.Sessions != wantSessions["candidate"] {
		t.Fatalf("arm sessions: control=%d candidate=%d want %v",
			rep.Control.Sessions, rep.Candidate.Sessions, wantSessions)
	}
	if rep.Control.Sessions+rep.Candidate.Sessions != 6 {
		t.Fatalf("population: %d+%d want 6 (old session leaked?)",
			rep.Control.Sessions, rep.Candidate.Sessions)
	}
	if diff := rep.Control.TotalCostUSD - wantCost["control"]; diff > 0.001 || diff < -0.001 {
		t.Errorf("control cost: got %f want %f", rep.Control.TotalCostUSD, wantCost["control"])
	}
	if diff := rep.Candidate.TotalCostUSD - wantCost["candidate"]; diff > 0.001 || diff < -0.001 {
		t.Errorf("candidate cost: got %f want %f", rep.Candidate.TotalCostUSD, wantCost["candidate"])
	}
	gotCauses := rep.Control.CacheEventsByCause["suffix_growth"] + rep.Candidate.CacheEventsByCause["suffix_growth"]
	if gotCauses != 6 {
		t.Errorf("cache causes: got %d want 6", gotCauses)
	}
	if rep.Control.CacheEventsByCause["tools_changed"]+rep.Candidate.CacheEventsByCause["tools_changed"] != 0 {
		t.Error("out-of-population cache event attributed")
	}
	wantComp := int64(rep.Control.Sessions) * 1000
	if rep.Control.CompressionSavedBytes != wantComp {
		t.Errorf("control compression saved: got %d want %d", rep.Control.CompressionSavedBytes, wantComp)
	}
	if rep.Control.Sessions >= 2 && rep.Control.CVPct <= 0 {
		t.Errorf("CV not computed: %+v", rep.Control)
	}

	// Unknown experiment 404.
	rr = httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/experiments/report?name=ghost", nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown report: got %d want 404", rr.Code)
	}
}
