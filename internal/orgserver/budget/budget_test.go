package budget

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	gen "github.com/marmutapp/superbased-observer/internal/orgserver/dashboard/gen"
	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
)

func TestCrossing(t *testing.T) {
	th := []float64{0.75, 0.9, 1.0}
	cases := []struct {
		ratio, last float64
		wantFire    bool
		wantNewHigh float64
	}{
		{0.50, 0.0, false, 0.0},
		{0.80, 0.0, true, 0.75},
		{0.80, 0.75, false, 0.75}, // already fired this tier
		{0.95, 0.75, true, 0.90},
		{1.20, 0.0, true, 1.00},
		{0.50, 0.90, false, 0.0}, // spend aged out → lower mark, no fire
		{0.95, 0.0, true, 0.90},  // re-cross after reset → fire
	}
	for _, c := range cases {
		fire, nh := crossing(th, c.ratio, c.last)
		if fire != c.wantFire || nh != c.wantNewHigh {
			t.Errorf("crossing(ratio=%v,last=%v) = (%v,%v), want (%v,%v)", c.ratio, c.last, fire, nh, c.wantFire, c.wantNewHigh)
		}
	}
}

func newEvalDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := orgdb.Open(context.Background(), orgdb.Options{Path: filepath.Join(t.TempDir(), "server.db")})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	ctx := context.Background()
	stmts := []string{
		`INSERT INTO org_members (user_id, user_name, email, active, created_at, updated_at) VALUES ('u-alice','alice','a@x',1,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`,
		`INSERT INTO org_teams (team_id, display_name, created_at, updated_at) VALUES ('team-a','Team A','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`,
		`INSERT INTO org_team_members (team_id, user_id, role) VALUES ('team-a','u-alice','lead')`,
		// 0.22 of spend in-window for alice.
		`INSERT INTO token_usage (source_file, source_event_id, user_id, session_id, project_root, timestamp, tool, model, input_tokens, output_tokens, estimated_cost_usd, source, reliability, pushed_at, pushed_by_user_id)
		 VALUES ('f','t1','u-alice','s','/r','2026-05-20T10:00:00Z','claude-code','claude',100,50,0.22,'jsonl','reliable','2026-05-26T11:00:00Z','u-alice')`,
	}
	for _, s := range stmts {
		if _, err := d.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return d
}

func TestEvaluateOnce_FiresOncePerCrossing(t *testing.T) {
	d := newEvalDB(t)
	ctx := context.Background()
	// cap 0.25 → ratio 0.88 → crosses 0.75 (not 0.9).
	if _, err := d.ExecContext(ctx,
		`INSERT INTO budgets (id, scope, scope_id, monthly_usd_cap, alert_webhook_url, alert_thresholds, last_fired_threshold, created_at, updated_at)
		 VALUES ('b1','team','team-a',0.25,'https://hook','[0.75,0.9,1.0]',0,'2026-05-01T00:00:00Z','2026-05-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}

	e := NewEvaluator(d, orgdb.Org{OrgID: "org-1", OrgName: "Acme"}, time.Minute, nil)
	e.now = func() time.Time { return time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC) }
	var fired []gen.BudgetAlert
	e.Deliver = func(_ context.Context, url string, a gen.BudgetAlert) {
		if url != "https://hook" {
			t.Errorf("webhook url = %q", url)
		}
		fired = append(fired, a)
	}

	if err := e.EvaluateOnce(ctx); err != nil {
		t.Fatalf("EvaluateOnce: %v", err)
	}
	if len(fired) != 1 || fired[0].Threshold != 0.75 {
		t.Fatalf("first eval fired %+v, want one alert at 0.75", fired)
	}
	if fired[0].CurrentRatio < 0.87 || fired[0].CurrentRatio > 0.89 || fired[0].OrgName != "Acme" {
		t.Errorf("alert payload = %+v", fired[0])
	}
	var last float64
	_ = d.QueryRow(`SELECT last_fired_threshold FROM budgets WHERE id='b1'`).Scan(&last)
	if last != 0.75 {
		t.Errorf("last_fired_threshold = %v, want 0.75", last)
	}

	// Second eval at the same spend must NOT re-fire (once per crossing).
	fired = nil
	if err := e.EvaluateOnce(ctx); err != nil {
		t.Fatalf("EvaluateOnce 2: %v", err)
	}
	if len(fired) != 0 {
		t.Errorf("second eval fired %d alerts, want 0 (high-water mark)", len(fired))
	}
}

func TestDeliverWithBackoff_PostsAlert(t *testing.T) {
	d := newEvalDB(t)
	received := make(chan gen.BudgetAlert, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var a gen.BudgetAlert
		_ = json.Unmarshal(body, &a)
		received <- a
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e := NewEvaluator(d, orgdb.Org{OrgID: "org-1", OrgName: "Acme"}, time.Minute, nil)
	e.deliverWithBackoff(context.Background(), srv.URL, gen.BudgetAlert{BudgetId: "b1", Threshold: 0.9})

	select {
	case got := <-received:
		if got.BudgetId != "b1" || got.Threshold != 0.9 {
			t.Errorf("received %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("webhook not delivered within 3s")
	}
	e.Wait()
}
