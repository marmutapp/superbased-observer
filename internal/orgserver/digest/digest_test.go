package digest

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	notifydigest "github.com/marmutapp/superbased-observer/internal/notify/digest"
	"github.com/marmutapp/superbased-observer/internal/notify/email"
	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
)

// fixedNow is a Wednesday; the completed weekly window is Mon 2026-06-29 →
// Sun 2026-07-05 (end 2026-07-06), the prior week 2026-06-22 → 2026-06-29.
var fixedNow = time.Date(2026, 7, 8, 15, 0, 0, 0, time.UTC)

func newDigestDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := orgdb.Open(context.Background(), orgdb.Options{Path: filepath.Join(t.TempDir(), "server.db")})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	ctx := context.Background()
	stmts := []string{
		`INSERT INTO org_members (user_id, user_name, email, active, created_at, updated_at) VALUES ('u1','alice','alice@x',1,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`,
		// current-week spend: $5 (2026-06-30)
		`INSERT INTO token_usage (source_file, source_event_id, user_id, session_id, project_root, timestamp, tool, model, input_tokens, output_tokens, estimated_cost_usd, source, reliability, pushed_at, pushed_by_user_id)
		 VALUES ('f','e1','u1','s1','/r','2026-06-30T10:00:00Z','claude-code','claude-opus',100,50,5.0,'jsonl','reliable','2026-07-01T00:00:00Z','u1')`,
		// prior-week spend: $3 (2026-06-23)
		`INSERT INTO token_usage (source_file, source_event_id, user_id, session_id, project_root, timestamp, tool, model, input_tokens, output_tokens, estimated_cost_usd, source, reliability, pushed_at, pushed_by_user_id)
		 VALUES ('f','e2','u1','s2','/r','2026-06-23T10:00:00Z','claude-code','claude-opus',100,50,3.0,'jsonl','reliable','2026-06-24T00:00:00Z','u1')`,
	}
	for _, s := range stmts {
		if _, err := d.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return d
}

func newTestScheduler(t *testing.T, d *sql.DB, rec *[]email.Message) *Scheduler {
	t.Helper()
	org := orgdb.Org{OrgID: "org1", OrgName: "Acme"}
	s := NewScheduler(d, org, notifydigest.Config{Enabled: true, Frequency: "weekly", SendHour: 8}, nil, "v1.19.0", []string{"admin@x"}, nil)
	s.now = func() time.Time { return fixedNow }
	s.send = func(_ context.Context, m email.Message) { *rec = append(*rec, m) }
	return s
}

func TestAssembleTotals(t *testing.T) {
	d := newDigestDB(t)
	var rec []email.Message
	s := newTestScheduler(t, d, &rec)
	period, _ := notifydigest.DuePeriod(notifydigest.Weekly, 8, fixedNow)
	data, err := s.assemble(context.Background(), period)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if data.TotalUSD != 5.0 {
		t.Errorf("TotalUSD = %v, want 5.0", data.TotalUSD)
	}
	if data.PriorTotalUSD != 3.0 {
		t.Errorf("PriorTotalUSD = %v, want 3.0", data.PriorTotalUSD)
	}
	if data.OrgName != "Acme" {
		t.Errorf("OrgName = %q", data.OrgName)
	}
	if len(data.To) != 1 || data.To[0] != "admin@x" {
		t.Errorf("To = %v, want [admin@x]", data.To)
	}
	if !data.HasAlertCount || data.AlertCount != 0 {
		t.Errorf("alert count = %d has=%v, want 0/true", data.AlertCount, data.HasAlertCount)
	}
}

func TestTickSendsOncePerPeriod(t *testing.T) {
	d := newDigestDB(t)
	var rec []email.Message
	s := newTestScheduler(t, d, &rec)
	ctx := context.Background()

	s.tick(ctx) // first tick: ready + unsent → send
	s.tick(ctx) // second tick: same period already sent → no-op
	if len(rec) != 1 {
		t.Fatalf("sends = %d, want 1", len(rec))
	}
	if !strings.Contains(rec[0].Subject, "Acme") || !strings.Contains(rec[0].Subject, "org spend digest") {
		t.Errorf("subject = %q", rec[0].Subject)
	}
	// The marker must record the completed week's key.
	wantKey, _ := notifydigest.DuePeriod(notifydigest.Weekly, 8, fixedNow)
	var got string
	if err := d.QueryRowContext(ctx, `SELECT last_period FROM digest_state WHERE kind='org'`).Scan(&got); err != nil {
		t.Fatalf("marker read: %v", err)
	}
	if got != wantKey.Key {
		t.Errorf("marker = %q, want %q", got, wantKey.Key)
	}
}

func TestTickSendHourGate(t *testing.T) {
	d := newDigestDB(t)
	var rec []email.Message
	s := newTestScheduler(t, d, &rec)
	s.now = func() time.Time { return time.Date(2026, 7, 8, 6, 0, 0, 0, time.UTC) } // 06:00 < send_hour 8
	s.tick(context.Background())
	if len(rec) != 0 {
		t.Fatalf("sends = %d, want 0 (before send_hour)", len(rec))
	}
}

func TestPreviewNoSendNoMarker(t *testing.T) {
	d := newDigestDB(t)
	var rec []email.Message
	s := newTestScheduler(t, d, &rec)
	ctx := context.Background()
	if _, err := s.Preview(ctx); err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(rec) != 0 {
		t.Errorf("preview must not send, got %d", len(rec))
	}
	var n int
	_ = d.QueryRowContext(ctx, `SELECT COUNT(*) FROM digest_state`).Scan(&n)
	if n != 0 {
		t.Errorf("preview must not write a marker, got %d rows", n)
	}
}
