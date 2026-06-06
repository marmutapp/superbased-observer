package ingest

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
)

func newServerDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := orgdb.Open(context.Background(), orgdb.Options{Path: filepath.Join(t.TempDir(), "server.db")})
	if err != nil {
		t.Fatalf("orgdb.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func fullEnvelope() orgcontract.PushEnvelope {
	return orgcontract.PushEnvelope{
		AgentVersion: "test", CursorFrom: 0, CursorTo: 4,
		Sessions: []orgcontract.SessionRow{{
			ID: "s1", Tool: "claude-code", StartedAt: "2026-05-26T10:00:00Z", EndedAt: "2026-05-26T10:30:00Z",
			TotalActions: 12, OrgID: "org-1", UserEmail: "dev@acme.example",
		}},
		Actions: []orgcontract.ActionRow{{
			SessionID: "s1", SourceFile: "f.jsonl", SourceEventID: "e1", Timestamp: "2026-05-26T10:00:01Z",
			Tool: "claude-code", ActionType: "read_file", Target: "a.go", Success: true, IsSidechain: true,
			OrgID: "org-1", UserEmail: "dev@acme.example",
		}},
		APITurns: []orgcontract.APITurnRow{{
			SessionID: "s1", Timestamp: "2026-05-26T10:00:02Z", Provider: "anthropic", Model: "claude",
			RequestID: "req-1", InputTokens: 100, OutputTokens: 50, CostUSD: 0.01, HTTPStatus: 200,
			OrgID: "org-1", UserEmail: "dev@acme.example",
		}},
		TokenUsage: []orgcontract.TokenUsageRow{{
			SessionID: "s1", SourceFile: "f.jsonl", SourceEventID: "t1", Timestamp: "2026-05-26T10:00:02Z",
			Tool: "claude-code", Model: "claude", InputTokens: 100, OutputTokens: 50, Source: "proxy",
			Reliability: "reliable", OrgID: "org-1", UserEmail: "dev@acme.example",
		}},
	}
}

func TestPush_AllTablesTaggedAndCounted(t *testing.T) {
	d := newServerDB(t)
	ctx := context.Background()

	res, err := Push(ctx, d, fullEnvelope(), "user-1", "2026-05-26T11:00:00Z")
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Accepted != 4 || res.Deduped != 0 {
		t.Fatalf("result = %+v, want accepted=4 deduped=0", res)
	}

	// One row in each table, all attributed to the authenticated pusher and
	// carrying the row-stamped org_id/user_email.
	for _, table := range []string{"sessions", "actions", "api_turns", "token_usage"} {
		var n int
		q := `SELECT COUNT(*) FROM ` + table +
			` WHERE user_id='user-1' AND pushed_by_user_id='user-1' AND org_id='org-1' AND user_email='dev@acme.example' AND pushed_at='2026-05-26T11:00:00Z'`
		if err := d.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 1 {
			t.Errorf("%s tagged-row count = %d, want 1", table, n)
		}
	}

	// Mutable bool columns round-trip as 0/1.
	var sidechain, success int
	if err := d.QueryRowContext(ctx, `SELECT is_sidechain, success FROM actions WHERE source_event_id='e1'`).
		Scan(&sidechain, &success); err != nil {
		t.Fatal(err)
	}
	if sidechain != 1 || success != 1 {
		t.Errorf("action bools = sidechain:%d success:%d, want 1/1", sidechain, success)
	}
}

func TestPush_DedupByCompositeKey(t *testing.T) {
	d := newServerDB(t)
	ctx := context.Background()
	env := fullEnvelope()

	if _, err := Push(ctx, d, env, "user-1", "2026-05-26T11:00:00Z"); err != nil {
		t.Fatalf("first Push: %v", err)
	}
	// Same batch again: every row collides on its composite key.
	res, err := Push(ctx, d, env, "user-1", "2026-05-26T11:05:00Z")
	if err != nil {
		t.Fatalf("second Push: %v", err)
	}
	if res.Accepted != 0 || res.Deduped != 4 {
		t.Fatalf("re-push result = %+v, want accepted=0 deduped=4", res)
	}

	// The SAME source rows pushed by a DIFFERENT user are distinct (user_id is
	// part of every dedup key), so they are accepted, not deduped.
	res, err = Push(ctx, d, env, "user-2", "2026-05-26T11:10:00Z")
	if err != nil {
		t.Fatalf("other-user Push: %v", err)
	}
	if res.Accepted != 4 || res.Deduped != 0 {
		t.Fatalf("other-user result = %+v, want accepted=4 deduped=0", res)
	}
}

func TestPush_EmptyEnvelope(t *testing.T) {
	d := newServerDB(t)
	res, err := Push(context.Background(), d, orgcontract.PushEnvelope{}, "user-1", "2026-05-26T11:00:00Z")
	if err != nil || res.Accepted != 0 || res.Deduped != 0 {
		t.Fatalf("empty Push = %+v, %v; want zero result", res, err)
	}
}
