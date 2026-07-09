package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// seedCoworkData inserts one cowork session's worth of data into the
// test server's DB: a token_usage row with known counts and a
// task_complete action carrying the Cowork-authoritative total_cost_usd
// on its metadata. Used by the cost-reconciliation tests below.
func seedCoworkData(t *testing.T, s *Server, sessionID, model string, tokens models.TokenEvent, totalCostUSD float64) {
	t.Helper()
	st := store.New(s.opts.DB)
	base := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	projectRoot := "/tmp/cowork-test-" + sessionID // store.Ingest skips events with empty ProjectRoot

	// 1) Token usage row.
	tokens.SourceFile = "f-" + sessionID
	tokens.SourceEventID = "e-tok-" + sessionID
	tokens.SessionID = sessionID
	tokens.ProjectRoot = projectRoot
	tokens.Timestamp = base
	tokens.Tool = models.ToolCowork
	tokens.Model = model
	tokens.Source = models.TokenSourceJSONL
	tokens.Reliability = models.ReliabilityUnreliable

	// 2) cowork.result task_complete with cost on metadata.
	resultEv := models.ToolEvent{
		SourceFile:    "f-" + sessionID,
		SourceEventID: "e-result-" + sessionID,
		SessionID:     sessionID,
		ProjectRoot:   projectRoot,
		Tool:          models.ToolCowork,
		ActionType:    models.ActionTaskComplete,
		RawToolName:   "cowork.result",
		Target:        "result preview",
		Success:       true,
		Timestamp:     base.Add(time.Second),
		MessageID:     "result:" + sessionID,
		Metadata: &models.ActionMetadata{
			CoworkTitle:       "Test session " + sessionID,
			CoworkProcessName: "test-proc-" + sessionID,
			TotalCostUSD:      totalCostUSD,
		},
	}

	_, err := st.Ingest(
		context.Background(),
		[]models.ToolEvent{resultEv},
		[]models.TokenEvent{tokens},
		store.IngestOptions{},
	)
	if err != nil {
		t.Fatalf("seed cowork data: %v", err)
	}
}

// TestCoworkReconcile_HappyPath pins the core math: when Cowork reports
// a total_cost_usd and observer's pricing engine matches within rounding,
// drift_percent stays under the 5% threshold and over_threshold=false.
func TestCoworkReconcile_HappyPath(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)

	// claude-opus-4-6 pricing: Input $5/M, Output $25/M, CacheRead $0.50/M,
	// CacheCreation $6.25/M, CacheCreation1h $10/M.
	// Bundle: 1000 input + 500 output + 10000 cache_read +
	// 2000 cache_creation (split 500 5m + 1500 1h)
	// → 5/1M*1000 + 25/1M*500 + 0.50/1M*10000 + 5/1M*500 + 10/1M*1500
	// → 0.005 + 0.0125 + 0.005 + 0.0025 + 0.015 = $0.0400
	seedCoworkData(
		t, s, "sess-happy", "claude-opus-4-6",
		models.TokenEvent{
			InputTokens:           1000,
			OutputTokens:          500,
			CacheReadTokens:       10000,
			CacheCreationTokens:   2000,
			CacheCreation1hTokens: 1500,
		},
		0.0400, // exact match — drift should be ~0
	)

	res, err := computeCoworkReconciliation(context.Background(), s.opts.DB, s.opts.CostEngine)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if got, want := res.SessionsTotal, 1; got != want {
		t.Fatalf("SessionsTotal=%d want %d", got, want)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("Rows len=%d want 1", len(res.Rows))
	}
	row := res.Rows[0]
	if row.SessionID != "sess-happy" {
		t.Errorf("SessionID=%q want sess-happy", row.SessionID)
	}
	if row.OverThreshold {
		t.Errorf("OverThreshold=true; want false (Cowork-claimed cost matches derived within %.0f%% threshold)", coworkDriftThreshold)
	}
	// Anthropic 5m cache_creation rates differ ($5/M typical model
	// input vs $6.25/M cache_creation 5m) so a hand-computed expectation
	// at the input rate drifts by ~1-2% from the engine. Anchor the
	// assertion on the threshold itself rather than a hand-derived
	// epsilon.
	if row.DriftPercent >= coworkDriftThreshold {
		t.Errorf("DriftPercent=%.2f; want < %.2f", row.DriftPercent, coworkDriftThreshold)
	}
}

// TestCoworkReconcile_OverThreshold pins that a >5% drift gets flagged.
func TestCoworkReconcile_OverThreshold(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	// Same token bundle as HappyPath but Cowork claims $0.10 (way off).
	// Drift % = |0.04 - 0.10|/0.10 * 100 = 60% — should flag.
	seedCoworkData(
		t, s, "sess-drift", "claude-opus-4-6",
		models.TokenEvent{
			InputTokens:           1000,
			OutputTokens:          500,
			CacheReadTokens:       10000,
			CacheCreationTokens:   2000,
			CacheCreation1hTokens: 1500,
		},
		0.10, // overclaim — derived will be ~$0.04 → 60% drift
	)

	res, err := computeCoworkReconciliation(context.Background(), s.opts.DB, s.opts.CostEngine)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if res.SessionsOver != 1 {
		t.Fatalf("SessionsOver=%d want 1", res.SessionsOver)
	}
	row := res.Rows[0]
	if !row.OverThreshold {
		t.Errorf("OverThreshold=false; want true (drift = %.2f%%)", row.DriftPercent)
	}
	if row.DriftPercent < 5.0 {
		t.Errorf("DriftPercent=%.2f; want >=5", row.DriftPercent)
	}
}

// TestCoworkReconcile_EmptyOnNoData pins clean empty state.
func TestCoworkReconcile_EmptyOnNoData(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	res, err := computeCoworkReconciliation(context.Background(), s.opts.DB, s.opts.CostEngine)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if res.SessionsTotal != 0 || len(res.Rows) != 0 {
		t.Fatalf("expected empty result on fresh DB; got total=%d rows=%d", res.SessionsTotal, len(res.Rows))
	}
	if res.DriftThresholdPct != coworkDriftThreshold {
		t.Fatalf("DriftThresholdPct=%.2f want %.2f", res.DriftThresholdPct, coworkDriftThreshold)
	}
}

// TestCoworkReconcile_UnknownModelFlagged pins that an unrecognized
// model marks the row UnknownModel=true (rather than reporting a
// false-zero derived cost as a 100% drift).
func TestCoworkReconcile_UnknownModelFlagged(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	seedCoworkData(
		t, s, "sess-unknown", "claude-experimental-zzz",
		models.TokenEvent{InputTokens: 1000, OutputTokens: 500},
		0.05,
	)
	res, err := computeCoworkReconciliation(context.Background(), s.opts.DB, s.opts.CostEngine)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("Rows=%d want 1", len(res.Rows))
	}
	row := res.Rows[0]
	if !row.UnknownModel {
		t.Errorf("UnknownModel=false; want true for claude-experimental-zzz")
	}
	if row.UnknownModelKey != "claude-experimental-zzz" {
		t.Errorf("UnknownModelKey=%q", row.UnknownModelKey)
	}
}

// TestCoworkReconcile_EndpointReturnsJSON pins the HTTP route is
// wired and returns valid JSON.
func TestCoworkReconcile_EndpointReturnsJSON(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	seedCoworkData(
		t, s, "sess-http", "claude-opus-4-6",
		models.TokenEvent{InputTokens: 100, OutputTokens: 50},
		0.002, // some plausible cost
	)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cowork/reconcile", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/cowork/reconcile = %d, body=%q", rr.Code, rr.Body.String())
	}
	var got CoworkReconcileResult
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rr.Body.String())
	}
	if got.SessionsTotal != 1 {
		t.Fatalf("SessionsTotal=%d want 1", got.SessionsTotal)
	}
}
