package dashboard

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/store"
)

func getRoutingJSON(t *testing.T, s *Server, path string) map[string]any {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return out
}

// TestAPIRoutingStatus pins the §R17.1 live-policy surface: defaults
// (disabled, advise, value template) with the expanded rule table and
// a content hash.
func TestAPIRoutingStatus(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	out := getRoutingJSON(t, s, "/api/routing/status")
	if out["enabled"] != false || out["mode"] != "advise" || out["policy"] != "value" {
		t.Errorf("status = %v/%v/%v", out["enabled"], out["mode"], out["policy"])
	}
	if out["policy_hash"] == "" || out["policy_hash"] == nil {
		t.Error("policy hash missing")
	}
	rules := out["rules"].([]any)
	if len(rules) == 0 {
		t.Fatal("expanded rule table empty (§R17.1: never a black box)")
	}
	first := rules[0].(map[string]any)
	if first["name"] == "" || first["when"] == "" || first["action"] == "" {
		t.Errorf("rule view incomplete: %+v", first)
	}
	// R2.4 demotion honesty: without a live router injected (this test
	// server has none), the endpoint must say "can't see" — never an
	// empty set that reads as "none demoted".
	if out["demotions_live"] != false {
		t.Errorf("demotions_live = %v, want false without a live router", out["demotions_live"])
	}
	if _, ok := out["demoted_rules"].(map[string]any); !ok {
		t.Errorf("demoted_rules missing or wrong shape: %T", out["demoted_rules"])
	}
}

// TestAPIRoutingStatus_Demotions pins the R2.4 wire: with the live
// router's accessor injected, the §R18.3 demotion set rides
// /api/routing/status verbatim and demotions_live flips true.
func TestAPIRoutingStatus_Demotions(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	s.opts.RoutingDemotions = func() map[string]string {
		return map[string]string{"read_only_overpowered": "error-rate regression vs baseline"}
	}
	out := getRoutingJSON(t, s, "/api/routing/status")
	if out["demotions_live"] != true {
		t.Errorf("demotions_live = %v, want true with an injected accessor", out["demotions_live"])
	}
	demoted := out["demoted_rules"].(map[string]any)
	if demoted["read_only_overpowered"] != "error-rate regression vs baseline" {
		t.Errorf("demoted_rules = %v", demoted)
	}
}

// TestAPIRoutingDecisionsAndSavings seeds decision rows and pins the
// feed (reason filter) + savings endpoints.
func TestAPIRoutingDecisionsAndSavings(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	st := store.New(s.opts.DB)
	now := time.Now().UTC()
	rows := []store.RouterDecisionRow{
		{
			SessionID: "sA", Timestamp: now.Add(-time.Minute), Mode: "advise", Channel: "B",
			OriginalModel: "claude-opus-4-8", SelectedModel: "claude-haiku-4-5",
			TurnKind: "read_only", PolicyName: "value", PolicyHash: "h",
			ReasonCodes: []string{"overpowered_read"}, EstSavingsUSD: 0.4, EstimateVersion: "p1-v1",
		},
		{
			SessionID: "sA", Timestamp: now.Add(-2 * time.Minute), Mode: "enforce", Channel: "B",
			OriginalModel: "claude-opus-4-8", SelectedModel: "claude-sonnet-4-6",
			TurnKind: "subagent", PolicyName: "value", PolicyHash: "h",
			ReasonCodes: []string{"overpowered_subagent"}, EstSavingsUSD: 0.2,
			EstimateVersion: "p1-v1", Applied: true,
		},
	}
	if err := st.InsertRouterDecisions(context.Background(), rows); err != nil {
		t.Fatalf("seed: %v", err)
	}

	out := getRoutingJSON(t, s, "/api/routing/decisions?days=7")
	if int(out["total"].(float64)) != 2 {
		t.Errorf("total = %v", out["total"])
	}
	if len(out["reasons"].([]any)) == 0 {
		t.Error("reason vocabulary missing")
	}
	filtered := getRoutingJSON(t, s, "/api/routing/decisions?days=7&reason=overpowered_read")
	if got := len(filtered["decisions"].([]any)); got != 1 {
		t.Errorf("reason-filtered = %d, want 1", got)
	}

	sav := getRoutingJSON(t, s, "/api/routing/savings?days=7")
	if sav["realized_usd"].(float64) != 0.2 || sav["would_have_usd"].(float64) != 0.4 {
		t.Errorf("savings = realized %v / would-have %v", sav["realized_usd"], sav["would_have_usd"])
	}
	if sav["note"] == nil || sav["note"] == "" {
		t.Error("savings honesty note missing")
	}
}

// TestAPIRoutingTiersAndHealth pins the tier map (+ calibration
// overlay shape) and the health board.
func TestAPIRoutingTiersAndHealth(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	st := store.New(s.opts.DB)
	if err := st.UpsertModelCalibrations(context.Background(), []store.ModelCalibrationRow{{
		Model: "claude-haiku-4-5", TurnKind: "read_only", ProjectID: 0, WindowDays: 30,
		ComputedAt: time.Now().UTC(), N: 120, ErrorCount: 2, ErrorGraded: 120,
	}}); err != nil {
		t.Fatalf("seed calibration: %v", err)
	}

	tiers := getRoutingJSON(t, s, "/api/routing/tiers")
	tierMap := tiers["tiers"].(map[string]any)
	if len(tierMap["opus-class"].([]any)) == 0 {
		t.Error("opus-class models missing from tier map")
	}
	calib := tiers["calibration"].([]any)
	if len(calib) != 1 {
		t.Fatalf("calibration overlays = %d, want 1", len(calib))
	}
	cell := calib[0].(map[string]any)
	if cell["model"] != "claude-haiku-4-5" || cell["n"].(float64) != 120 {
		t.Errorf("overlay cell: %+v", cell)
	}

	health := getRoutingJSON(t, s, "/api/routing/health")
	if _, ok := health["models"]; !ok {
		t.Error("health board missing models key")
	}
}

// TestAPIRoutingSimulate pins the R1.2 counterfactual-preview
// endpoint: POST-only, defaults to the value template over 30 days,
// returns the §R18.1 SimReport wire shape with the honesty note, and
// rejects unknown templates at the request (the same closed
// vocabulary the CLI enforces).
func TestAPIRoutingSimulate(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)

	post := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("POST", "/api/routing/simulate", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}

	// Empty body → defaults (value / 30 days). An empty substrate is a
	// legitimate zero-report, not an error.
	rec := post("")
	if rec.Code != 200 {
		t.Fatalf("POST simulate = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		WindowDays int    `json:"window_days"`
		Note       string `json:"note"`
		Report     struct {
			PolicyName     string `json:"policy_name"`
			PolicyHash     string `json:"policy_hash"`
			TurnsEvaluated int    `json:"turns_evaluated"`
			WouldReroute   int    `json:"would_reroute"`
		} `json:"report"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.WindowDays != 30 || out.Report.PolicyName != "value" || out.Report.PolicyHash == "" {
		t.Errorf("defaults: window=%d policy=%q hash=%q", out.WindowDays, out.Report.PolicyName, out.Report.PolicyHash)
	}
	if out.Note == "" {
		t.Error("honesty note missing")
	}

	// Explicit template + days.
	rec = post(`{"policy":"frugal","days":7}`)
	if rec.Code != 200 {
		t.Fatalf("POST simulate frugal = %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.WindowDays != 7 || out.Report.PolicyName != "frugal" {
		t.Errorf("explicit: window=%d policy=%q", out.WindowDays, out.Report.PolicyName)
	}

	// Unknown template → 400 with the vocabulary in the message.
	rec = post(`{"policy":"supersmart"}`)
	if rec.Code != 400 {
		t.Errorf("unknown template = %d, want 400", rec.Code)
	}

	// Turn-kind filter: closed vocabulary — a valid kind narrows the
	// replay (R1.5 CLI parity), an unknown kind rejects at the request.
	rec = post(`{"policy":"value","turn_kind":"read_only"}`)
	if rec.Code != 200 {
		t.Errorf("valid turn_kind = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = post(`{"policy":"value","turn_kind":"vibes"}`)
	if rec.Code != 400 {
		t.Errorf("unknown turn_kind = %d, want 400", rec.Code)
	}

	// GET → 405 (compute request, not a view).
	req := httptest.NewRequest("GET", "/api/routing/simulate", nil)
	getRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(getRec, req)
	if getRec.Code != 405 {
		t.Errorf("GET simulate = %d, want 405", getRec.Code)
	}
}
