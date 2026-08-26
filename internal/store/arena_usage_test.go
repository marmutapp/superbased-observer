package store

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
)

func TestArenaUsageBySessions(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	in, out, cost, err := s.ArenaUsageBySessions(ctx, []string{"nope"})
	if err != nil || in != 0 || out != 0 || cost != 0 {
		t.Fatalf("unknown session must return zeros: %d/%d/%v %v", in, out, cost, err)
	}
	in, out, cost, err = s.ArenaUsageBySessions(ctx, nil)
	if err != nil || in != 0 || out != 0 || cost != 0 {
		t.Fatalf("empty id list must short-circuit: %d/%d/%v %v", in, out, cost, err)
	}

	base := time.Date(2026, 8, 22, 9, 9, 40, 0, time.UTC)
	turns := []models.APITurn{
		{
			SessionID: "cand-a", Provider: "anthropic", Model: "claude-fable-5",
			Timestamp: base, InputTokens: 2573, OutputTokens: 111, CostUSD: 0.92094,
		},
		{
			SessionID: "cand-a", Provider: "anthropic", Model: "claude-fable-5",
			Timestamp: base.Add(time.Second), InputTokens: 2, OutputTokens: 623, CostUSD: 0.084314,
		},
		{
			SessionID: "cand-b", Provider: "openai", Model: "gpt-5.4-mini",
			Timestamp: base.Add(2 * time.Second), InputTokens: 8043, OutputTokens: 265, CostUSD: 0.00740715,
		},
		{
			SessionID: models.ArenaSessionIDPrefix + "aider", Provider: "openai", Model: "gpt-5.4-mini",
			Timestamp: base.Add(3 * time.Second), InputTokens: 321, OutputTokens: 45, CostUSD: 0.0007,
		},
	}
	for i := range turns {
		if _, err := s.InsertAPITurn(ctx, turns[i]); err != nil {
			t.Fatalf("InsertAPITurn: %v", err)
		}
	}

	in, out, cost, err = s.ArenaUsageBySessions(ctx, []string{"cand-a"})
	if err != nil {
		t.Fatalf("ArenaUsageBySessions: %v", err)
	}
	if in != 2575 || out != 734 {
		t.Fatalf("cand-a token sums wrong: in=%d out=%d", in, out)
	}
	if math.Abs(cost-1.005254) > 1e-6 {
		t.Fatalf("cand-a cost sum wrong: %v", cost)
	}

	in, out, _, err = s.ArenaUsageBySessions(ctx, []string{"cand-a", "cand-b"})
	if err != nil {
		t.Fatalf("multi-session: %v", err)
	}
	if in != 10618 || out != 999 {
		t.Fatalf("multi-session sums wrong: in=%d out=%d", in, out)
	}

	in, out, cost, err = s.ArenaUsageBySessions(ctx, []string{models.ArenaSessionIDPrefix + "aider"})
	if err != nil || in != 321 || out != 45 || math.Abs(cost-0.0007) > 1e-9 {
		t.Fatalf("synthetic candidate rollup wrong: in=%d out=%d cost=%v err=%v", in, out, cost, err)
	}
}
