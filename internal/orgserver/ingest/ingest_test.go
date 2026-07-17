package ingest

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
	"github.com/marmutapp/superbased-observer/internal/orgserver/rollup"
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
		GuardEvents: []orgcontract.GuardEventRow{{
			SessionID: "s1", Timestamp: "2026-05-26T10:00:03Z", Tool: "claude-code",
			EventKind: "shell_exec", RuleID: "R-001", Category: "destructive", Severity: "high",
			Decision: "deny", Enforced: true, Source: "hook", TargetHash: "th-1",
			ChainPrev: "", ChainHash: "ch-1", OrgID: "org-1", UserEmail: "dev@acme.example",
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
	if res.Accepted != 5 || res.Deduped != 0 {
		t.Fatalf("result = %+v, want accepted=5 deduped=0", res)
	}

	// One row in each table, all attributed to the authenticated pusher and
	// carrying the row-stamped org_id/user_email.
	for _, table := range []string{"sessions", "actions", "api_turns", "token_usage", "guard_events"} {
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

func TestPush_ForcesPusherEmailOverForgedValue(t *testing.T) {
	d := newServerDB(t)
	ctx := context.Background()

	// Seed the authenticated pusher with their real, SCIM-provisioned email.
	if _, err := d.ExecContext(ctx,
		`INSERT INTO org_members (user_id, user_name, email, created_at, updated_at)
		 VALUES ('user-1', 'user-1', 'real@acme.example', '2026-05-26T09:00:00Z', '2026-05-26T09:00:00Z')`); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	// The envelope carries a FORGED user_email (a victim's address). Ingest must
	// overwrite it with the pusher's own email, exactly as it pins user_id.
	env := fullEnvelope() // rows stamped victim@acme.example... rewritten below
	env.Sessions[0].UserEmail = "victim@acme.example"
	env.RoutingSummaries = []orgcontract.RoutingSummaryRow{{
		Day: "2026-05-26", Tier: "flagship", Reason: "escalate", Mode: "enforce",
		Decisions: 3, Applied: 2, OrgID: "org-1", UserEmail: "victim@acme.example",
	}}

	if _, err := Push(ctx, d, env, "user-1", "2026-05-26T11:00:00Z"); err != nil {
		t.Fatalf("Push: %v", err)
	}

	// No row anywhere carries the forged address; the aggregate row is bucketed
	// under the pusher's own email.
	for _, table := range []string{"sessions", "actions", "api_turns", "token_usage", "guard_events", "routing_summaries"} {
		var forged int
		if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE user_email='victim@acme.example'`).Scan(&forged); err != nil {
			t.Fatalf("count forged %s: %v", table, err)
		}
		if forged != 0 {
			t.Errorf("%s: %d rows kept the forged user_email, want 0", table, forged)
		}
		var real int
		if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE user_email='real@acme.example'`).Scan(&real); err != nil {
			t.Fatalf("count real %s: %v", table, err)
		}
		if real != 1 {
			t.Errorf("%s: %d rows attributed to the pusher, want 1", table, real)
		}
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
	if res.Accepted != 0 || res.Deduped != 5 {
		t.Fatalf("re-push result = %+v, want accepted=0 deduped=5", res)
	}

	// The SAME source rows pushed by a DIFFERENT user are distinct (user_id is
	// part of every dedup key), so they are accepted, not deduped.
	res, err = Push(ctx, d, env, "user-2", "2026-05-26T11:10:00Z")
	if err != nil {
		t.Fatalf("other-user Push: %v", err)
	}
	if res.Accepted != 5 || res.Deduped != 0 {
		t.Fatalf("other-user result = %+v, want accepted=5 deduped=0", res)
	}
}

func TestPush_EmptyEnvelope(t *testing.T) {
	d := newServerDB(t)
	res, err := Push(context.Background(), d, orgcontract.PushEnvelope{}, "user-1", "2026-05-26T11:00:00Z")
	if err != nil || res.Accepted != 0 || res.Deduped != 0 {
		t.Fatalf("empty Push = %+v, %v; want zero result", res, err)
	}
}

// guardEnvelope wraps rows into a guard-events-only envelope.
func guardEnvelope(rows ...orgcontract.GuardEventRow) orgcontract.PushEnvelope {
	return orgcontract.PushEnvelope{AgentVersion: "test", GuardEvents: rows}
}

func TestPush_GuardEvents(t *testing.T) {
	base := orgcontract.GuardEventRow{
		SessionID: "s1", Timestamp: "2026-05-26T10:00:03Z", Tool: "claude-code",
		EventKind: "shell_exec", RuleID: "R-001", Category: "destructive", Severity: "high",
		Decision: "deny", DegradedFrom: "deny", Enforced: true, Source: "hook",
		TargetHash: "th-1", ChainPrev: "ch-0", ChainHash: "ch-1",
		OrgID: "org-1", UserEmail: "dev@acme.example",
	}
	fullContent := base
	fullContent.ChainHash = "ch-2"
	fullContent.ChainPrev = "ch-1"
	fullContent.Reason = "rm -rf outside project"
	fullContent.TargetExcerpt = "rm -rf /etc"
	fullContent.TaintOrigin = "web_fetch"
	noChain := base
	noChain.ChainHash = ""
	noChain.ChainPrev = ""
	noChain.Timestamp = "2026-05-26T10:00:04Z"

	cases := []struct {
		name string
		row  orgcontract.GuardEventRow
		// wantNull lists content columns that must store NULL; wantSet maps
		// column → expected stored value.
		wantNull []string
		wantSet  map[string]string
	}{
		{
			name:     "metadata_only_strips_to_null",
			row:      base,
			wantNull: []string{"reason", "target_excerpt", "taint_origin"},
			wantSet: map[string]string{
				"rule_id": "R-001", "category": "destructive", "severity": "high",
				"decision": "deny", "degraded_from": "deny", "source": "hook",
				"target_hash": "th-1", "chain_prev": "ch-0", "chain_hash": "ch-1",
				"event_kind": "shell_exec", "tool": "claude-code",
			},
		},
		{
			name: "full_content_opt_in_lands_content_columns",
			row:  fullContent,
			wantSet: map[string]string{
				"reason": "rm -rf outside project", "target_excerpt": "rm -rf /etc",
				"taint_origin": "web_fetch", "chain_hash": "ch-2",
			},
		},
		{
			name: "missing_chain_hash_gets_synthesized_dedup_key",
			row:  noChain,
			wantSet: map[string]string{
				"chain_hash": guardDedupKey(noChain), // deterministic, non-empty
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newServerDB(t)
			ctx := context.Background()
			res, err := Push(ctx, d, guardEnvelope(tc.row), "user-1", "2026-05-26T11:00:00Z")
			if err != nil {
				t.Fatalf("Push: %v", err)
			}
			if res.Accepted != 1 {
				t.Fatalf("accepted = %d, want 1", res.Accepted)
			}
			for _, col := range tc.wantNull {
				var n int
				q := `SELECT COUNT(*) FROM guard_events WHERE ` + col + ` IS NULL`
				if err := d.QueryRowContext(ctx, q).Scan(&n); err != nil {
					t.Fatalf("null probe %s: %v", col, err)
				}
				if n != 1 {
					t.Errorf("column %s: stored non-NULL, want NULL (metadata-only strip)", col)
				}
			}
			for col, want := range tc.wantSet {
				var got string
				q := `SELECT COALESCE(` + col + `, '<NULL>') FROM guard_events`
				if err := d.QueryRowContext(ctx, q).Scan(&got); err != nil {
					t.Fatalf("read %s: %v", col, err)
				}
				if got != want {
					t.Errorf("column %s = %q, want %q", col, got, want)
				}
			}
		})
	}
}

func TestPush_GuardEventsDedupByChainHashAndUser(t *testing.T) {
	d := newServerDB(t)
	ctx := context.Background()
	row := orgcontract.GuardEventRow{
		Timestamp: "2026-05-26T10:00:03Z", RuleID: "R-001", Decision: "flag",
		ChainHash: "ch-1", OrgID: "org-1", UserEmail: "dev@acme.example",
	}

	if _, err := Push(ctx, d, guardEnvelope(row), "user-1", "2026-05-26T11:00:00Z"); err != nil {
		t.Fatalf("first Push: %v", err)
	}
	// Same chain_hash, same user → dedup.
	res, err := Push(ctx, d, guardEnvelope(row), "user-1", "2026-05-26T11:05:00Z")
	if err != nil {
		t.Fatalf("re-push: %v", err)
	}
	if res.Accepted != 0 || res.Deduped != 1 {
		t.Fatalf("re-push result = %+v, want accepted=0 deduped=1", res)
	}
	// Same chain_hash, different user → distinct row (chains are per node).
	res, err = Push(ctx, d, guardEnvelope(row), "user-2", "2026-05-26T11:10:00Z")
	if err != nil {
		t.Fatalf("other-user push: %v", err)
	}
	if res.Accepted != 1 || res.Deduped != 0 {
		t.Fatalf("other-user result = %+v, want accepted=1 deduped=0", res)
	}

	// Synthesized-key rows dedup the same way: identical content collides.
	noChain := row
	noChain.ChainHash = ""
	if _, err := Push(ctx, d, guardEnvelope(noChain), "user-3", "2026-05-26T11:15:00Z"); err != nil {
		t.Fatalf("no-chain push: %v", err)
	}
	res, err = Push(ctx, d, guardEnvelope(noChain), "user-3", "2026-05-26T11:20:00Z")
	if err != nil {
		t.Fatalf("no-chain re-push: %v", err)
	}
	if res.Accepted != 0 || res.Deduped != 1 {
		t.Fatalf("no-chain re-push result = %+v, want accepted=0 deduped=1", res)
	}
}

func otelContentEnvelope(rows ...orgcontract.OTelContentRow) orgcontract.PushEnvelope {
	return orgcontract.PushEnvelope{AgentVersion: "test", OTelContent: rows}
}

func TestPush_OTelContent(t *testing.T) {
	ctx := context.Background()
	d := newServerDB(t)

	row := orgcontract.OTelContentRow{
		RequestID: "req-1", SessionID: "s1", Kind: "prompt",
		ContentHash: "hash-abc", Content: "refactor the parser",
		Timestamp: "2026-05-26T10:00:04Z", OrgID: "org-1", UserEmail: "dev@acme.example",
	}
	res, err := Push(ctx, d, otelContentEnvelope(row), "user-1", "2026-05-26T11:00:00Z")
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Accepted != 1 {
		t.Fatalf("accepted = %d, want 1", res.Accepted)
	}

	var count int
	var content sql.NullString
	if err := d.QueryRowContext(ctx,
		`SELECT COUNT(*), MAX(content) FROM otel_content WHERE content_hash='hash-abc' AND user_id='user-1'`).
		Scan(&count, &content); err != nil {
		t.Fatalf("read: %v", err)
	}
	if count != 1 || !content.Valid || content.String != "refactor the parser" {
		t.Fatalf("row not stored: count=%d content=%v", count, content)
	}

	// Re-push the same row → deduped by (content_hash, user_id, request_id, tool_use_id).
	res, err = Push(ctx, d, otelContentEnvelope(row), "user-1", "2026-05-26T11:05:00Z")
	if err != nil {
		t.Fatalf("re-push: %v", err)
	}
	if res.Accepted != 0 || res.Deduped != 1 {
		t.Fatalf("dedup wrong: accepted=%d deduped=%d", res.Accepted, res.Deduped)
	}
}

func TestPush_OTelContentStrippedContentStoresNull(t *testing.T) {
	ctx := context.Background()
	d := newServerDB(t)
	// Metadata-only push: content stripped by the agent (empty), hash present.
	row := orgcontract.OTelContentRow{
		RequestID: "req-2", Kind: "prompt", ContentHash: "hash-xyz",
		Timestamp: "2026-05-26T10:00:05Z", OrgID: "org-1", UserEmail: "dev@acme.example",
	}
	if _, err := Push(ctx, d, otelContentEnvelope(row), "user-1", "2026-05-26T11:00:00Z"); err != nil {
		t.Fatalf("Push: %v", err)
	}
	var content sql.NullString
	if err := d.QueryRowContext(ctx,
		`SELECT content FROM otel_content WHERE content_hash='hash-xyz'`).Scan(&content); err != nil {
		t.Fatalf("read: %v", err)
	}
	if content.Valid {
		t.Fatalf("stripped content should store NULL, got %q", content.String)
	}
}

// TestPush_ObsEndUserSpendCrossInstance confirms the T5 per-end-user tier
// round-trips through ingest into obs_enduser_spend (proving migration 017
// applies), that user_email is RE-PINNED to the pusher while end_user is NOT,
// and that the rollup SUMs the same end-user across two nodes (cross-instance).
func TestPush_ObsEndUserSpendCrossInstance(t *testing.T) {
	d := newServerDB(t)
	ctx := context.Background()
	day := time.Now().UTC().Format("2006-01-02")

	// Two nodes → two SCIM-provisioned pushers, one shared hosted-app end-user.
	for _, m := range []struct{ id, email string }{
		{"user-1", "nodeA@acme.example"},
		{"user-2", "nodeB@acme.example"},
	} {
		if _, err := d.ExecContext(ctx,
			`INSERT INTO org_members (user_id, user_name, email, created_at, updated_at)
			 VALUES (?, ?, ?, '2026-07-06T09:00:00Z', '2026-07-06T09:00:00Z')`, m.id, m.id, m.email); err != nil {
			t.Fatalf("seed member %s: %v", m.id, err)
		}
	}

	envFor := func(cost float64, traces, tokens int64) orgcontract.PushEnvelope {
		return orgcontract.PushEnvelope{
			AgentVersion: "test",
			ObsEndUserSpend: []orgcontract.ObsEndUserSpendRow{{
				OrgID: "org-1", UserEmail: "forged@evil.example", // must be re-pinned to the pusher
				Day: day, EndUser: "cust-42", CostUSD: cost, Traces: traces, TotalTokens: tokens,
			}},
		}
	}
	if _, err := Push(ctx, d, envFor(1.0, 2, 100), "user-1", "2026-07-06T11:00:00Z"); err != nil {
		t.Fatalf("Push node A: %v", err)
	}
	if _, err := Push(ctx, d, envFor(2.0, 3, 200), "user-2", "2026-07-06T11:05:00Z"); err != nil {
		t.Fatalf("Push node B: %v", err)
	}

	// Two distinct rows (UNIQUE key includes the pusher user_email), the forged
	// address never survived, and end_user stayed app-shared (not re-pinned).
	var rows, forged, custRows int
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM obs_enduser_spend`).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 2 {
		t.Fatalf("obs_enduser_spend rows = %d, want 2 (one per node)", rows)
	}
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM obs_enduser_spend WHERE user_email LIKE '%evil%'`).Scan(&forged); err != nil {
		t.Fatalf("count forged: %v", err)
	}
	if forged != 0 {
		t.Errorf("forged user_email survived: %d rows", forged)
	}
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM obs_enduser_spend WHERE end_user='cust-42'`).Scan(&custRows); err != nil {
		t.Fatalf("count cust: %v", err)
	}
	if custRows != 2 {
		t.Errorf("end_user rows for cust-42 = %d, want 2 (end_user not re-pinned)", custRows)
	}

	// Cross-instance rollup SUMs across the two nodes.
	res, err := rollup.ObsEndUserSpend(ctx, d, rollup.Window{Days: 30}, time.Now())
	if err != nil {
		t.Fatalf("ObsEndUserSpend: %v", err)
	}
	if len(res.Users) != 1 {
		t.Fatalf("users = %+v, want 1 (cust-42)", res.Users)
	}
	u := res.Users[0]
	if u.EndUser != "cust-42" || u.Traces != 5 || u.TotalTokens != 300 {
		t.Errorf("cust-42 = %+v, want traces 5 / tokens 300", u)
	}
	if delta := u.CostUSD - 3.0; delta < -1e-9 || delta > 1e-9 {
		t.Errorf("cust-42 cost = %v, want 3.0 (1.0 node A + 2.0 node B)", u.CostUSD)
	}
	if delta := res.TotalCostUSD - 3.0; delta < -1e-9 || delta > 1e-9 {
		t.Errorf("total cost = %v, want 3.0", res.TotalCostUSD)
	}
	if s := u.CostShare; s < 0.999 || s > 1.001 {
		t.Errorf("cost share = %v, want ~1.0 (sole end-user)", s)
	}
}

// seedPusher provisions one SCIM member so forcePusherEmail re-pins the
// pusher's own address over any client-supplied user_email.
func seedPusher(t *testing.T, d *sql.DB, userID, email string) {
	t.Helper()
	if _, err := d.ExecContext(context.Background(),
		`INSERT INTO org_members (user_id, user_name, email, created_at, updated_at)
		 VALUES (?, ?, ?, '2026-07-06T09:00:00Z', '2026-07-06T09:00:00Z')`, userID, userID, email); err != nil {
		t.Fatalf("seed member %s: %v", userID, err)
	}
}

// TestPush_ObsAdmissionRoundTrip confirms the T6 admission tier round-trips
// through ingest into obs_admission_events / obs_admission_policy_versions
// (proving migration 019 applies): verdict user_email is RE-PINNED to the
// pusher while end_user is NOT, the tamper-evidence chain links are retained,
// re-pushed verdicts dedup on row_hash (INSERT OR IGNORE), and a re-pushed
// policy with the same policy_hash DO-UPDATEs its body.
func TestPush_ObsAdmissionRoundTrip(t *testing.T) {
	d := newServerDB(t)
	ctx := context.Background()
	seedPusher(t, d, "user-1", "operator@acme.example")

	env := func(body, reason string) orgcontract.PushEnvelope {
		return orgcontract.PushEnvelope{
			AgentVersion: "test",
			ObsAdmissionEvents: []orgcontract.ObsAdmissionRow{{
				OrgID: "org-1", UserEmail: "forged@evil.example", // must be re-pinned
				TS: "2026-05-26T10:00:00Z", Mode: "enforce", Decision: "deny", Severity: "high",
				CriterionID: "c.pii", PolicyHash: "p-hash-1", JudgeUsed: true, JudgeHosting: "local",
				LatencyMS: 42, MessageHash: "m-1", TraceID: "tr-1", RequestID: "rq-1",
				PrevHash: "", RowHash: "row-1",
				Tenant: "acme", EndUser: "cust-42", ReasonExcerpt: reason,
			}},
			ObsAdmissionPolicies: []orgcontract.ObsAdmissionPolicyRow{{
				OrgID: "org-1", UserEmail: "forged@evil.example",
				PolicyHash: "p-hash-1", CreatedAt: "2026-05-26T09:00:00Z", Mode: "enforce",
				Scope: "global", CriteriaCount: 3, Body: body,
			}},
		}
	}

	if _, err := Push(ctx, d, env(`{"v":1}`, "blocked: quoted request"), "user-1", "2026-05-26T11:00:00Z"); err != nil {
		t.Fatalf("Push: %v", err)
	}

	// Verdict: exactly one row, user_email re-pinned, end_user app-shared,
	// content-bearing columns stored, chain links retained.
	var events int
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM obs_admission_events`).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 1 {
		t.Fatalf("events = %d, want 1", events)
	}
	var userEmail, endUser, prev, row, reason, tenant, decision string
	if err := d.QueryRowContext(ctx,
		`SELECT user_email, COALESCE(end_user,''), prev_hash, row_hash, COALESCE(reason_excerpt,''),
		        COALESCE(tenant,''), decision FROM obs_admission_events`).
		Scan(&userEmail, &endUser, &prev, &row, &reason, &tenant, &decision); err != nil {
		t.Fatalf("scan event: %v", err)
	}
	if userEmail != "operator@acme.example" {
		t.Errorf("user_email = %q, want re-pinned operator@acme.example (forged address survived)", userEmail)
	}
	if endUser != "cust-42" {
		t.Errorf("end_user = %q, want cust-42 (NOT re-pinned — app-shared)", endUser)
	}
	if row != "row-1" || prev != "" {
		t.Errorf("chain links = (prev=%q,row=%q), want (prev='',row='row-1')", prev, row)
	}
	if reason != "blocked: quoted request" || tenant != "acme" || decision != "deny" {
		t.Errorf("content = (reason=%q,tenant=%q,decision=%q), want (blocked…,acme,deny)", reason, tenant, decision)
	}

	// Policy: one row, body stored, user_email re-pinned.
	var polUserEmail, polBody string
	var polCriteria int64
	if err := d.QueryRowContext(ctx,
		`SELECT user_email, body, criteria_count FROM obs_admission_policy_versions`).
		Scan(&polUserEmail, &polBody, &polCriteria); err != nil {
		t.Fatalf("scan policy: %v", err)
	}
	if polUserEmail != "operator@acme.example" || polBody != `{"v":1}` || polCriteria != 3 {
		t.Errorf("policy = (email=%q,body=%q,criteria=%d)", polUserEmail, polBody, polCriteria)
	}

	// Re-push: verdict dedups on row_hash (INSERT OR IGNORE keeps the original,
	// so reason is NOT overwritten); policy content-addresses on policy_hash and
	// DO-UPDATEs its body.
	if _, err := Push(ctx, d, env(`{"v":2}`, "SECOND reason should be ignored"), "user-1", "2026-05-26T12:00:00Z"); err != nil {
		t.Fatalf("re-Push: %v", err)
	}
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM obs_admission_events`).Scan(&events); err != nil {
		t.Fatalf("count events 2: %v", err)
	}
	if events != 1 {
		t.Errorf("events after re-push = %d, want 1 (row_hash dedup)", events)
	}
	if err := d.QueryRowContext(ctx, `SELECT COALESCE(reason_excerpt,'') FROM obs_admission_events`).Scan(&reason); err != nil {
		t.Fatalf("scan reason 2: %v", err)
	}
	if reason != "blocked: quoted request" {
		t.Errorf("reason after re-push = %q, want original (immutable verdict)", reason)
	}
	var policies int
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM obs_admission_policy_versions`).Scan(&policies); err != nil {
		t.Fatalf("count policies: %v", err)
	}
	if policies != 1 {
		t.Errorf("policies after re-push = %d, want 1 (content-addressed)", policies)
	}
	if err := d.QueryRowContext(ctx, `SELECT body FROM obs_admission_policy_versions`).Scan(&polBody); err != nil {
		t.Fatalf("scan body 2: %v", err)
	}
	if polBody != `{"v":2}` {
		t.Errorf("policy body after re-push = %q, want {\"v\":2} (DO UPDATE)", polBody)
	}
}

// TestPush_ObsAdmissionGatedColumnsNull confirms the gated content columns are
// stored NULL (not empty string) when the node shipped them empty, so a
// stripped row is indistinguishable from a never-had-one row (no posture leak),
// while the content-free columns keep their empty-string value.
func TestPush_ObsAdmissionGatedColumnsNull(t *testing.T) {
	d := newServerDB(t)
	ctx := context.Background()
	seedPusher(t, d, "user-1", "operator@acme.example")

	env := orgcontract.PushEnvelope{
		AgentVersion: "test",
		ObsAdmissionEvents: []orgcontract.ObsAdmissionRow{{
			OrgID: "org-1", TS: "2026-05-26T10:00:00Z", Mode: "observe", Decision: "allow",
			Severity: "info", PolicyHash: "p-1", MessageHash: "m-1", RowHash: "row-1",
			// tenant / end_user / reason_excerpt intentionally empty (hash-only node).
		}},
	}
	if _, err := Push(ctx, d, env, "user-1", "2026-05-26T11:00:00Z"); err != nil {
		t.Fatalf("Push: %v", err)
	}
	var tenant, endUser, reason sql.NullString
	var criterion string // content-free → empty string, not NULL
	if err := d.QueryRowContext(ctx,
		`SELECT tenant, end_user, reason_excerpt, criterion_id FROM obs_admission_events`).
		Scan(&tenant, &endUser, &reason, &criterion); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if tenant.Valid || endUser.Valid || reason.Valid {
		t.Errorf("gated columns should be NULL, got tenant=%v end_user=%v reason=%v", tenant, endUser, reason)
	}
	if criterion != "" {
		t.Errorf("criterion_id = %q, want '' (content-free, not NULL)", criterion)
	}
}

// TestPush_NoAdmissionSlicesNoOp confirms an envelope WITHOUT the admission
// slices ingests cleanly (additive compat: pre-feature agents omit the keys).
func TestPush_NoAdmissionSlicesNoOp(t *testing.T) {
	d := newServerDB(t)
	ctx := context.Background()
	if _, err := Push(ctx, d, fullEnvelope(), "user-1", "2026-05-26T11:00:00Z"); err != nil {
		t.Fatalf("Push: %v", err)
	}
	for _, table := range []string{"obs_admission_events", "obs_admission_policy_versions"} {
		var n int
		if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s = %d rows, want 0 (no admission slices in envelope)", table, n)
		}
	}
}
