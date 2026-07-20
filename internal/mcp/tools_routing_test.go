package mcp

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// TestGetRoutingStatus_EmptyNode pins the zero-state contract: P0
// phase, mode off, enforcement unavailable, the six §R6.4 templates,
// zero counters — and the honest advisory note.
func TestGetRoutingStatus_EmptyNode(t *testing.T) {
	s, _, _ := testServer(t)
	out := callTool(t, s, "get_routing_status", map[string]any{})

	if out["phase"] != "P0" || out["mode"] != "off" {
		t.Errorf("phase/mode = %v/%v, want P0/off", out["phase"], out["mode"])
	}
	if enforced, _ := out["enforcement_available"].(bool); enforced {
		t.Error("enforcement_available = true in P0")
	}
	templates := out["templates"].([]any)
	if len(templates) != 6 {
		t.Errorf("templates = %d, want 6 (§R6.4)", len(templates))
	}
	if int(out["router_decisions"].(float64)) != 0 || int(out["model_calibration_rows"].(float64)) != 0 {
		t.Errorf("counters = %v / %v, want 0 / 0", out["router_decisions"], out["model_calibration_rows"])
	}
	if _, hasLast := out["last_decision_at"]; hasLast {
		t.Error("last_decision_at present on an empty decision log")
	}
	if out["note"] == nil || out["note"] == "" {
		t.Error("missing advisory note")
	}
}

// TestGetModelRecommendation_EvidenceBacked seeds two models on the
// same read-only turn-kind (opus baseline + cheaper haiku at parity
// scale) and pins the recommendation shape: caveat present, baseline
// identified, the cheaper parity candidate suggested with sample sizes.
func TestGetModelRecommendation_EvidenceBacked(t *testing.T) {
	s, database, _ := testServer(t)
	st := store.New(database)
	ctx := context.Background()

	pid, err := st.UpsertProject(ctx, "/repo/mcp-routing", "")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	now := time.Now().UTC()
	for _, sess := range []string{"s-opus", "s-haiku"} {
		if err := st.UpsertSession(ctx, models.Session{
			ID: sess, ProjectID: pid, Tool: "claude-code", StartedAt: now.Add(-24 * time.Hour),
		}); err != nil {
			t.Fatalf("UpsertSession: %v", err)
		}
	}
	// 60 turns each, all proxy-graded clean — parity at the n=50 floor.
	for i := 0; i < 60; i++ {
		ts := now.Add(time.Duration(-60+i) * time.Minute)
		for _, mc := range []struct{ sess, model string }{
			{"s-opus", "claude-opus-4-8"},
			{"s-haiku", "claude-haiku-4-5"},
		} {
			if _, err := st.InsertAPITurn(ctx, models.APITurn{
				SessionID: mc.sess, ProjectID: pid, Provider: "anthropic",
				Model: mc.model, RequestID: fmt.Sprintf("%s-%d", mc.sess, i),
				Timestamp:   ts,
				InputTokens: 10_000, OutputTokens: 1_000,
				MessageCount: 5, ToolUseCount: 2,
				TotalResponseMS: 1200, HTTPStatus: 200,
			}); err != nil {
				t.Fatalf("InsertAPITurn: %v", err)
			}
			if _, err := st.InsertActions(ctx, []models.Action{{
				SessionID: mc.sess, ProjectID: pid, Tool: "claude-code",
				Timestamp: ts.Add(-30 * time.Second), ActionType: models.ActionReadFile,
				Target: "x.go", Success: true,
				SourceFile: "mv.jsonl", SourceEventID: fmt.Sprintf("act-%s-%d", mc.sess, i),
			}}); err != nil {
				t.Fatalf("InsertActions: %v", err)
			}
		}
	}

	out := callTool(t, s, "get_model_recommendation", map[string]any{"turn_kind": "read_only"})
	if out["caveat"] == nil || out["caveat"] == "" {
		t.Error("missing attribution caveat")
	}
	recs := out["recommendations"].([]any)
	if len(recs) != 1 {
		t.Fatalf("recommendations = %d, want 1: %v", len(recs), recs)
	}
	rec := recs[0].(map[string]any)
	if rec["turn_kind"] != "read_only" || rec["baseline_model"] != "claude-opus-4-8" {
		t.Errorf("baseline = %v", rec)
	}
	if rec["suggested_model"] != "claude-haiku-4-5" || rec["verdict"] != "parity" {
		t.Errorf("suggestion = %v", rec)
	}
	if int(rec["baseline_turns"].(float64)) != 60 || int(rec["suggested_turns"].(float64)) != 60 {
		t.Errorf("sample sizes = %v / %v, want 60 / 60", rec["baseline_turns"], rec["suggested_turns"])
	}
}

// TestGetModelRecommendation_EmptyNode pins the no-evidence zero state:
// the tool answers (caveat + empty recommendations), never errors.
func TestGetModelRecommendation_EmptyNode(t *testing.T) {
	s, _, _ := testServer(t)
	out := callTool(t, s, "get_model_recommendation", map[string]any{})
	recs, ok := out["recommendations"].([]any)
	if ok && len(recs) != 0 {
		t.Errorf("recommendations on an empty node: %v", recs)
	}
	if out["caveat"] == nil || out["caveat"] == "" {
		t.Error("missing attribution caveat")
	}
}
