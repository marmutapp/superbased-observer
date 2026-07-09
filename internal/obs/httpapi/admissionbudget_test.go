package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/obs"
	"github.com/marmutapp/superbased-observer/internal/obs/admission"
	"github.com/marmutapp/superbased-observer/internal/obs/span"
	obsstore "github.com/marmutapp/superbased-observer/internal/obs/store"
)

// TestAdmissionBudgetEndpoint confirms GET /api/obs/admission/budget reports the
// configured caps and the top end-user spenders.
func TestAdmissionBudgetEndpoint(t *testing.T) {
	ctx := context.Background()
	conn, err := db.Open(ctx, db.Options{Path: filepath.Join(t.TempDir(), "obs.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	st, err := obsstore.Open(ctx, conn)
	if err != nil {
		t.Fatalf("obsstore.Open: %v", err)
	}
	spec, err := admission.Compile(admission.PolicyInput{Mode: "observe"})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	svc := obs.NewAdmissionService(st, nil, fakeGate{allow: true}, nil, obs.AdmissionOptions{
		Hosting: "off", BudgetEnabled: true,
		BudgetFiveHourUSD: 5, BudgetWeeklyUSD: 20, BudgetMonthlyUSD: 50,
	})
	svc.SetPolicy(ctx, spec)

	now := time.Now().UTC()
	if err := st.UpsertTrace(ctx, span.Trace{TraceID: "t1", User: "alice", Source: span.SourceOTLPTrace, StartedAt: now}); err != nil {
		t.Fatalf("UpsertTrace: %v", err)
	}
	cost := 12.50
	if err := st.UpsertSpansBatch(ctx, []span.Span{{SpanID: "s1", TraceID: "t1", Kind: span.KindLLM, CostUSD: &cost, StartedAt: now}}); err != nil {
		t.Fatalf("UpsertSpansBatch: %v", err)
	}

	mux := http.NewServeMux()
	for _, r := range New(st, nil, svc, nil).Routes() {
		mux.HandleFunc(r.Pattern, r.Handler)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/obs/admission/budget")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var got admissionBudgetResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Enabled || got.FiveHourUSD != 5 || got.WeeklyUSD != 20 || got.MonthlyUSD != 50 {
		t.Errorf("caps = %+v, want enabled 5/20/50", got)
	}
	if len(got.TopSpenders) != 1 || got.TopSpenders[0].User != "alice" || got.TopSpenders[0].Monthly != 12.50 {
		t.Errorf("top spenders = %+v, want one alice@12.50", got.TopSpenders)
	}
}
