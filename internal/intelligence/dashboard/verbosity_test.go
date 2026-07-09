package dashboard

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

func seedVerbositySession(t *testing.T, database *sql.DB, sessionID string, withContentBytes bool) {
	t.Helper()
	ctx := context.Background()
	var projectID int64
	if err := database.QueryRowContext(ctx,
		`INSERT INTO projects (root_path, created_at) VALUES (?, '2026-06-30T00:00:00Z') RETURNING id`,
		"/tmp/verb-"+sessionID).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, tool, project_id, started_at)
		 VALUES (?, 'claude-code', ?, '2026-06-30T00:00:00Z')`,
		sessionID, projectID); err != nil {
		t.Fatal(err)
	}
	ins := func(eid, atype, rawName, target, body string, cb sql.NullInt64) {
		if _, err := database.ExecContext(ctx,
			`INSERT INTO actions (session_id, project_id, timestamp, action_type, tool, target, raw_tool_name, raw_tool_output, content_bytes, source_file, source_event_id)
			 VALUES (?, ?, '2026-06-30T01:00:00Z', ?, 'claude-code', ?, ?, ?, ?, 'f.jsonl', ?)`,
			sessionID, projectID, atype, target, rawName, body, cb, eid); err != nil {
			t.Fatal(err)
		}
	}
	cb := func(n int64) sql.NullInt64 {
		if !withContentBytes {
			return sql.NullInt64{}
		}
		return sql.NullInt64{Int64: n, Valid: true}
	}
	body := "Here is the fix.\n```go\nfunc main() {}\n```\nDone.\n```\nlog output\n```"
	ins("e1", models.ActionTaskComplete, "claudecode.assistant_text", "preview", body, sql.NullInt64{})
	ins("e2", models.ActionWriteFile, "Write", "internal/x.go", "", cb(1000))
	ins("e3", models.ActionRunCommand, "Bash", "go test ./...", "", cb(50))
}

func TestHandleSessionVerbosity(t *testing.T) {
	t.Parallel()
	database, cleanup := openForecastTestDB(t)
	defer cleanup()
	seedVerbositySession(t, database, "sv", true)

	srv := &Server{opts: Options{DB: database}}
	req := httptest.NewRequest(http.MethodGet, "/api/session/sv/verbosity", nil)
	rec := httptest.NewRecorder()
	srv.handleSessionDetail(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp VerbosityResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v — body %s", err, rec.Body.String())
	}

	if !resp.AuthoredCaptured {
		t.Error("AuthoredCaptured should be true when content_bytes present")
	}
	// code = go write (1000) + bash command (50) + fenced go artifact (>0).
	if resp.CodeBytes <= 1050 {
		t.Errorf("CodeBytes = %d, want > 1050 (writes+command+fenced go)", resp.CodeBytes)
	}
	if resp.ExplainBytes == 0 {
		t.Error("ExplainBytes should be > 0 (narrative + untagged fence)")
	}
	if resp.Channels.CommandBytes != 50 {
		t.Errorf("CommandBytes = %d, want 50", resp.Channels.CommandBytes)
	}
	if resp.CodeExplainRatio == nil {
		t.Error("CodeExplainRatio should be set when explanation > 0")
	}
	var sawGo bool
	for _, l := range resp.CodeByLanguage {
		if l.Language == "go" && l.Category == "code" {
			sawGo = true
		}
	}
	if !sawGo {
		t.Errorf("expected go in CodeByLanguage, got %+v", resp.CodeByLanguage)
	}
}

// TestHandleSessionVerbosity_CostEstimate proves the est token/$ split (plan
// §7): with a priced model + token rows the handler apportions output_tokens
// across code/explain and prices them, and totals output+reasoning.
func TestHandleSessionVerbosity_CostEstimate(t *testing.T) {
	t.Parallel()
	database, cleanup := openForecastTestDB(t)
	defer cleanup()
	seedVerbositySession(t, database, "sc", true)
	// claude-opus-4-8 is priced by newForecastTestEngine (Output: 75/M).
	if _, err := database.ExecContext(context.Background(),
		`UPDATE sessions SET model = 'claude-opus-4-8' WHERE id = 'sc'`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(context.Background(),
		`INSERT INTO token_usage(session_id, timestamp, tool, model, input_tokens, output_tokens, reasoning_tokens, source, source_file, source_event_id)
		 VALUES('sc', '2026-06-30T01:00:00Z', 'claude-code', 'claude-opus-4-8', 5000, 10000, 2000, 'proxy', 'f.jsonl', 'tu1')`); err != nil {
		t.Fatal(err)
	}

	srv := &Server{opts: Options{DB: database, CostEngine: newForecastTestEngine(t)}}
	req := httptest.NewRequest(http.MethodGet, "/api/session/sc/verbosity", nil)
	rec := httptest.NewRecorder()
	srv.handleSessionDetail(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp VerbosityResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !resp.CostEstimated {
		t.Fatal("CostEstimated should be true with a priced model + tokens")
	}
	if resp.EstOutputTokens != 10000 || resp.EstReasoningTokens != 2000 {
		t.Errorf("tokens out=%d reason=%d, want 10000/2000", resp.EstOutputTokens, resp.EstReasoningTokens)
	}
	// Code + explain est tokens partition the apportioned output (other
	// categories — docs/config/etc — may take the rest), so each ≤ output.
	if resp.EstCodeTokens <= 0 || resp.EstExplainTokens <= 0 {
		t.Errorf("est code/explain tokens should both be > 0, got %d/%d", resp.EstCodeTokens, resp.EstExplainTokens)
	}
	// $ at 75/M output = 7.5e-5 per token. Total covers output+reasoning.
	const perTok = 75.0 / 1_000_000
	wantTotal := float64(12000) * perTok
	if d := resp.EstTotalUSD - wantTotal; d > 1e-9 || d < -1e-9 {
		t.Errorf("EstTotalUSD = %v, want %v", resp.EstTotalUSD, wantTotal)
	}
	if resp.EstCodeUSD <= 0 || resp.EstCodeUSD >= resp.EstTotalUSD {
		t.Errorf("EstCodeUSD = %v should be >0 and < total %v", resp.EstCodeUSD, resp.EstTotalUSD)
	}
}

// TestHandleVerbosityAggregate proves the analysis-page endpoint: a by-model
// rollup with a priced model returns a group carrying code/explain bytes + an
// est total $ for the group.
func TestHandleVerbosityAggregate(t *testing.T) {
	t.Parallel()
	database, cleanup := openForecastTestDB(t)
	defer cleanup()
	seedVerbositySession(t, database, "sa", true)
	if _, err := database.ExecContext(context.Background(),
		`UPDATE sessions SET model = 'claude-opus-4-8' WHERE id = 'sa'`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(context.Background(),
		`INSERT INTO token_usage(session_id, timestamp, tool, model, input_tokens, output_tokens, reasoning_tokens, source, source_file, source_event_id)
		 VALUES('sa', '2026-06-30T01:00:00Z', 'claude-code', 'claude-opus-4-8', 5000, 10000, 0, 'proxy', 'f.jsonl', 'tu1')`); err != nil {
		t.Fatal(err)
	}

	srv := &Server{opts: Options{DB: database, CostEngine: newForecastTestEngine(t)}}
	req := httptest.NewRequest(http.MethodGet, "/api/verbosity/aggregate?by=model&since_days=0", nil)
	rec := httptest.NewRecorder()
	srv.handleVerbosityAggregate(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp VerbosityAggregateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.By != "model" {
		t.Errorf("By = %q, want model", resp.By)
	}
	var g *VerbosityAggregateGroup
	for i := range resp.Groups {
		if resp.Groups[i].Key == "claude-opus-4-8" {
			g = &resp.Groups[i]
		}
	}
	if g == nil {
		t.Fatalf("no claude-opus-4-8 group: %+v", resp.Groups)
	}
	if g.CodeBytes <= 0 {
		t.Errorf("CodeBytes = %d, want > 0", g.CodeBytes)
	}
	if !g.CostEstimated || g.EstTotalUSD <= 0 {
		t.Errorf("expected priced group: CostEstimated=%v EstTotalUSD=%v", g.CostEstimated, g.EstTotalUSD)
	}
	// 10000 output tokens × $75/M = $0.75.
	const want = 10000 * 75.0 / 1_000_000
	if d := g.EstTotalUSD - want; d > 1e-9 || d < -1e-9 {
		t.Errorf("EstTotalUSD = %v, want %v", g.EstTotalUSD, want)
	}
}

// TestHandleVerbosityAggregate_UnknownDimension returns 400, not 500.
func TestHandleVerbosityAggregate_UnknownDimension(t *testing.T) {
	t.Parallel()
	database, cleanup := openForecastTestDB(t)
	defer cleanup()
	srv := &Server{opts: Options{DB: database}}
	req := httptest.NewRequest(http.MethodGet, "/api/verbosity/aggregate?by=bogus", nil)
	rec := httptest.NewRecorder()
	srv.handleVerbosityAggregate(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestHandleSessionVerbosity_AuthoredNotCaptured proves the honesty hint:
// a session with write/command actions but NULL content_bytes (pre-feature
// data) reports AuthoredCaptured=false so the surface can prompt a backfill.
func TestHandleSessionVerbosity_AuthoredNotCaptured(t *testing.T) {
	t.Parallel()
	database, cleanup := openForecastTestDB(t)
	defer cleanup()
	seedVerbositySession(t, database, "sn", false)

	srv := &Server{opts: Options{DB: database}}
	req := httptest.NewRequest(http.MethodGet, "/api/session/sn/verbosity", nil)
	rec := httptest.NewRecorder()
	srv.handleSessionDetail(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp VerbosityResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.AuthoredCaptured {
		t.Error("AuthoredCaptured should be false when authored actions exist but carry no content_bytes")
	}
}
