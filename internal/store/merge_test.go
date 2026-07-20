package store

import (
	"context"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/turnmerge"
)

// readTurnByRequestID pulls the merge-relevant columns back for assertions.
func readTurnByRequestID(t *testing.T, s *Store, reqID string) (count int, row turnmerge.Turn, source string) {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT input_tokens, output_tokens, COALESCE(cost_usd,0),
		        COALESCE(time_to_first_token_ms,0), COALESCE(stop_reason,''),
		        COALESCE(source,'')
		   FROM api_turns WHERE request_id = ? ORDER BY id ASC`, reqID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		count++
		if err := rows.Scan(&row.InputTokens, &row.OutputTokens, &row.CostUSD,
			&row.TimeToFirstTokenMS, &row.StopReason, &source); err != nil {
			t.Fatalf("scan: %v", err)
		}
	}
	return count, row, source
}

func baseTurn(reqID, source string) models.APITurn {
	return models.APITurn{
		Provider:  "anthropic",
		Model:     "claude-opus-4-8",
		RequestID: reqID,
		Timestamp: time.Now().UTC(),
		Source:    source,
	}
}

func TestUpsertTurnByRequestID_ProxyWinsNativeFillsEnrichment(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// Proxy observes the turn first (exact tokens, no ttft/stop_reason).
	proxy := baseTurn("req-1", SourceProxy)
	proxy.InputTokens, proxy.OutputTokens, proxy.CostUSD = 1000, 200, 0.42
	if act, _, err := s.UpsertTurnByRequestID(ctx, proxy); err != nil || act != turnmerge.ActionInsert {
		t.Fatalf("proxy insert: act=%v err=%v", act, err)
	}

	// Native OTel observes the same turn: lower-fidelity token numbers (must
	// not win) but carries ttft + stop_reason the proxy lacked.
	native := baseTurn("req-1", SourceCCOTel)
	native.InputTokens, native.OutputTokens = 999, 199
	native.TimeToFirstTokenMS, native.StopReason = 320, "end_turn"
	act, id, err := s.UpsertTurnByRequestID(ctx, native)
	if err != nil || act != turnmerge.ActionUpdate {
		t.Fatalf("native merge: act=%v id=%v err=%v", act, id, err)
	}

	count, row, source := readTurnByRequestID(t, s, "req-1")
	if count != 1 {
		t.Fatalf("want exactly 1 row for req-1, got %d (DUPLICATION)", count)
	}
	if row.InputTokens != 1000 || row.OutputTokens != 200 || row.CostUSD != 0.42 {
		t.Fatalf("lower-fidelity native overwrote proxy tokens: %+v", row)
	}
	if row.TimeToFirstTokenMS != 320 || row.StopReason != "end_turn" {
		t.Fatalf("native enrichment not merged in: %+v", row)
	}
	if source != SourceProxy {
		t.Fatalf("merged row source = %q, want proxy", source)
	}
}

func TestUpsertTurnByRequestID_NativeOnlyGapFill(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// No proxy ever ran for this turn (user didn't route through the proxy).
	native := baseTurn("req-2", SourceCCOTel)
	native.InputTokens, native.OutputTokens = 500, 80
	native.StopReason = "tool_use"
	act, _, err := s.UpsertTurnByRequestID(ctx, native)
	if err != nil || act != turnmerge.ActionInsert {
		t.Fatalf("native-only insert: act=%v err=%v", act, err)
	}

	count, row, source := readTurnByRequestID(t, s, "req-2")
	if count != 1 || row.InputTokens != 500 || row.StopReason != "tool_use" {
		t.Fatalf("native-only turn not captured at full fidelity: count=%d row=%+v", count, row)
	}
	if source != SourceCCOTel {
		t.Fatalf("source = %q, want cc_otel", source)
	}
}

func TestUpsertTurnByRequestID_Idempotent(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	turn := baseTurn("req-3", SourceCCOTel)
	turn.InputTokens, turn.OutputTokens, turn.StopReason = 100, 10, "end_turn"
	if _, _, err := s.UpsertTurnByRequestID(ctx, turn); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Re-deliver the identical observation (at-least-once telemetry).
	act, _, err := s.UpsertTurnByRequestID(ctx, turn)
	if err != nil || act != turnmerge.ActionNoChange {
		t.Fatalf("re-delivery: act=%v err=%v (want no-change)", act, err)
	}
	if count, _, _ := readTurnByRequestID(t, s, "req-3"); count != 1 {
		t.Fatalf("re-delivery duplicated the row: count=%d", count)
	}
}

func TestUpsertTurnByRequestID_LegacyNullSourceTreatedAsProxy(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// A pre-migration-047 row: written by the proxy with no source tag.
	legacy := baseTurn("req-4", "") // empty source -> NULL -> proxy fidelity
	legacy.InputTokens, legacy.OutputTokens, legacy.CostUSD = 2000, 400, 0.9
	if _, err := s.InsertAPITurn(ctx, legacy); err != nil {
		t.Fatalf("legacy insert: %v", err)
	}

	// Native OTel arrives later with different token numbers — must NOT win
	// over the legacy proxy row, but may fill enrichment.
	native := baseTurn("req-4", SourceCCOTel)
	native.InputTokens, native.OutputTokens = 1, 1
	native.TimeToFirstTokenMS = 150
	if _, _, err := s.UpsertTurnByRequestID(ctx, native); err != nil {
		t.Fatalf("native merge: %v", err)
	}

	count, row, _ := readTurnByRequestID(t, s, "req-4")
	if count != 1 || row.InputTokens != 2000 || row.CostUSD != 0.9 {
		t.Fatalf("native overwrote legacy proxy row: count=%d row=%+v", count, row)
	}
	if row.TimeToFirstTokenMS != 150 {
		t.Fatalf("enrichment not filled onto legacy row: %+v", row)
	}
}

func TestUpsertTurnByRequestID_EmptyRequestIDAlwaysInserts(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		turn := baseTurn("", SourceCCOTel)
		turn.InputTokens = int64(10 + i)
		act, _, err := s.UpsertTurnByRequestID(ctx, turn)
		if err != nil || act != turnmerge.ActionInsert {
			t.Fatalf("empty-reqid insert %d: act=%v err=%v", i, act, err)
		}
	}
	// Two keyless observations stay as two distinct rows (no dedup key).
	// Empty request_id persists as NULL, so count the NULL rows directly.
	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM api_turns WHERE request_id IS NULL`).Scan(&count); err != nil {
		t.Fatalf("count null: %v", err)
	}
	if count != 2 {
		t.Fatalf("keyless turns wrongly deduped: count=%d", count)
	}
}
