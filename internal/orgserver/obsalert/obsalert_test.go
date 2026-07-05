package obsalert

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
)

// TestEvaluator_FiresOnceWithCooldown seeds obs_summaries with a high error
// rate, creates an error_rate rule, and asserts: the evaluator fires the
// webhook + logs an event on the crossing, persists last_value, and the
// cooldown suppresses an immediate second fire.
func TestEvaluator_FiresOnceWithCooldown(t *testing.T) {
	ctx := context.Background()
	db, err := orgdb.Open(ctx, orgdb.Options{Path: filepath.Join(t.TempDir(), "server.db")})
	if err != nil {
		t.Fatalf("orgdb.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	org, err := orgdb.EnsureOrg(ctx, db, "https://org.example")
	if err != nil {
		t.Fatalf("EnsureOrg: %v", err)
	}
	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)

	// Seed obs_summaries: 100 traces, 30 errors → error_rate 0.30 (within 7d).
	if _, err := db.ExecContext(ctx,
		`INSERT INTO obs_summaries (org_id,user_email,day,model,provider,project_hash,source,traces,spans,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,reasoning_tokens,total_tokens,cost_usd,error_traces,duration_ms_sum,duration_ms_count,pushed_at,pushed_by_user_id)
		 VALUES (?, 'a@x','2026-06-27','gpt-4o','openai','ph','otlp_trace',100,300,0,0,0,0,0,0,1.0,30,0,0,'2026-06-28T06:00:00Z','u1')`,
		org.OrgID); err != nil {
		t.Fatalf("seed summaries: %v", err)
	}
	// Rule: error_rate > 0.10.
	ruleID, err := CreateAlertRule(ctx, db, org.OrgID, "u1", NewRuleInput{
		Name: "high errors", Metric: MetricErrorRate, Comparator: "gt", Threshold: 0.10,
		WindowDays: 7, WebhookURL: "http://example.test/hook", CooldownMinutes: 360,
	}, now)
	if err != nil {
		t.Fatalf("CreateAlertRule: %v", err)
	}

	e := NewEvaluator(db, org, time.Minute, nil)
	e.now = func() time.Time { return now }
	var fired []Alert
	e.Deliver = func(_ context.Context, _ string, a Alert) error { fired = append(fired, a); return nil }

	if err := e.EvaluateOnce(ctx); err != nil {
		t.Fatalf("EvaluateOnce: %v", err)
	}
	if len(fired) != 1 {
		t.Fatalf("fired %d times, want 1", len(fired))
	}
	if fired[0].Metric != MetricErrorRate || !near(fired[0].Value, 0.30) {
		t.Errorf("alert = %+v, want error_rate ~0.30", fired[0])
	}
	// last_value persisted + an event logged.
	res, err := LoadAlertRules(ctx, db, org.OrgID)
	if err != nil {
		t.Fatalf("LoadAlertRules: %v", err)
	}
	if len(res.Rules) != 1 || !near(res.Rules[0].LastValue, 0.30) || !res.Rules[0].Breaching {
		t.Errorf("rule status = %+v, want last_value 0.30 + breaching", res.Rules[0])
	}
	if len(res.Events) != 1 || res.Events[0].RuleID != ruleID || !res.Events[0].Delivered {
		t.Errorf("events = %+v, want 1 delivered fire", res.Events)
	}

	// Second immediate evaluation: cooldown suppresses a re-fire.
	fired = nil
	if err := e.EvaluateOnce(ctx); err != nil {
		t.Fatalf("EvaluateOnce 2: %v", err)
	}
	if len(fired) != 0 {
		t.Errorf("re-fired within cooldown (%d), want 0", len(fired))
	}

	// After the cooldown elapses, it fires again.
	e.now = func() time.Time { return now.Add(7 * time.Hour) }
	if err := e.EvaluateOnce(ctx); err != nil {
		t.Fatalf("EvaluateOnce 3: %v", err)
	}
	if len(fired) != 1 {
		t.Errorf("post-cooldown fired %d, want 1", len(fired))
	}

	// A non-breaching rule does not fire.
	if _, err := CreateAlertRule(ctx, db, org.OrgID, "u1", NewRuleInput{
		Metric: MetricErrorRate, Comparator: "gt", Threshold: 0.95, WindowDays: 7, CooldownMinutes: 1,
	}, now); err != nil {
		t.Fatalf("create rule 2: %v", err)
	}
	fired = nil
	e.now = func() time.Time { return now.Add(20 * time.Hour) }
	if err := e.EvaluateOnce(ctx); err != nil {
		t.Fatalf("EvaluateOnce 4: %v", err)
	}
	// Only the first rule (0.10) re-fires; the 0.95 rule stays quiet.
	for _, a := range fired {
		if a.Threshold == 0.95 {
			t.Error("0.95 rule fired but error_rate is 0.30")
		}
	}
}

func near(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-6
}
