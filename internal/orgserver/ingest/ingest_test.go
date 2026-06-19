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
