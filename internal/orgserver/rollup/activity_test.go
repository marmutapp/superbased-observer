package rollup

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func toolRowMap(in []ToolRow) map[string]ToolRow {
	m := map[string]ToolRow{}
	for _, t := range in {
		m[t.Tool] = t
	}
	return m
}

func modelRowMap(in []ModelRow) map[string]ModelRow {
	m := map[string]ModelRow{}
	for _, x := range in {
		m[x.Model] = x
	}
	return m
}

func TestTools_AdminBreakdown(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	got, err := Tools(context.Background(), d, w30, adminScope, fixedNow)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(got.Tools) != 2 || got.Tools[0].Tool != "claude-code" {
		t.Fatalf("tools = %+v, want claude-code first then codex", got.Tools)
	}
	m := toolRowMap(got.Tools)
	cc := m["claude-code"]
	if !near(cc.CostUSD, 0.25) || cc.Tokens != 600 || cc.Sessions != 2 || cc.ActiveDevs != 3 ||
		cc.ActionCount != 4 || !near(cc.SuccessRate, 1.0) || cc.Buckets.NetInput != 400 {
		t.Errorf("claude-code = %+v, want cost 0.25 tok 600 sess 2 devs 3 act 4 success 1.0 input 400", cc)
	}
	cx := m["codex"]
	if !near(cx.CostUSD, 0.20) || cx.Sessions != 1 || cx.ActiveDevs != 1 || cx.ActionCount != 0 {
		t.Errorf("codex = %+v, want cost 0.20 sess 1 devs 1 act 0", cx)
	}
	// No proxy timing in the fixture → TTFT degrades to 0.
	if cc.AvgTTFTMs != 0 {
		t.Errorf("claude-code TTFT = %d, want 0 (no proxy timing)", cc.AvgTTFTMs)
	}
}

func TestModels_AdminBreakdown(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	got, err := Models(context.Background(), d, w30, adminScope, fixedNow)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(got.Models) != 2 || got.Models[0].Model != "gpt" {
		t.Fatalf("models = %+v, want gpt first (0.23) then claude (0.22)", got.Models)
	}
	m := modelRowMap(got.Models)
	if cl := m["claude"]; !near(cl.CostUSD, 0.22) || cl.Tokens != 450 || cl.ActiveDevs != 2 || cl.Buckets.NetInput != 300 {
		t.Errorf("claude = %+v, want cost 0.22 tok 450 devs 2 input 300", cl)
	}
	if gp := m["gpt"]; !near(gp.CostUSD, 0.23) || gp.ActiveDevs != 1 {
		t.Errorf("gpt = %+v, want cost 0.23 devs 1", gp)
	}
}

func TestTools_LeadScoped(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	got, err := Tools(context.Background(), d, w30, aliceScope, fixedNow)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	// Team A = alice + carol; both use claude-code, neither uses codex (bob's).
	if _, leaked := toolRowMap(got.Tools)["codex"]; leaked {
		t.Errorf("lead tools leaked codex (bob/team-b): %+v", got.Tools)
	}
}

func TestActivity_AdminGrids(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	got, err := Activity(context.Background(), d, w30, adminScope, fixedNow)
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	if len(got.CostByDay) != 3 || len(got.ActionsByDay) != 3 {
		t.Errorf("cost/actions by day = %d/%d, want 3/3", len(got.CostByDay), len(got.ActionsByDay))
	}
	// All four actions are at hour 9.
	if len(got.HourOfDay) != 1 || got.HourOfDay[0].Hour != 9 || got.HourOfDay[0].Count != 4 {
		t.Errorf("hour_of_day = %+v, want [{9,4}]", got.HourOfDay)
	}
	// Tokens by day: 05-20 = input 200 / output 100 (alice api + alice jsonl).
	var found bool
	for _, b := range got.TokensByDay {
		if b.Date == "2026-05-20" {
			found = true
			if b.NetInput != 200 || b.Output != 100 {
				t.Errorf("05-20 buckets = %+v, want input 200 output 100", b)
			}
		}
	}
	if !found {
		t.Errorf("tokens_by_day missing 2026-05-20: %+v", got.TokensByDay)
	}
	// dow_hour: 3 cells (one per active day), all at hour 9, dow derived in Go.
	if len(got.DowHour) != 3 {
		t.Errorf("dow_hour = %+v, want 3 cells", got.DowHour)
	}
	var sum int64
	for _, c := range got.DowHour {
		if c.Hour != 9 {
			t.Errorf("dow_hour cell at hour %d, want 9", c.Hour)
		}
		if c.Dow != dowOf("2026-05-20") && c.Dow != dowOf("2026-05-21") && c.Dow != dowOf("2026-05-22") {
			t.Errorf("dow_hour cell dow %d not among the seeded days", c.Dow)
		}
		sum += c.Count
	}
	if sum != 4 {
		t.Errorf("dow_hour total = %d, want 4", sum)
	}
}

func TestPhase3_NoSentinelColumns(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	ctx := context.Background()
	tools, _ := Tools(ctx, d, w30, adminScope, fixedNow)
	models, _ := Models(ctx, d, w30, adminScope, fixedNow)
	activity, _ := Activity(ctx, d, w30, adminScope, fixedNow)
	for _, v := range []any{tools, models, activity} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(b), "/repo/") {
			t.Errorf("Phase 3 rollup leaked a raw project path:\n%s", b)
		}
	}
}
