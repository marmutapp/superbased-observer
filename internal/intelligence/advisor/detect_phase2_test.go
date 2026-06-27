package advisor

import (
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
)

func mkRows(n int, in, out, cr, cc int64) []TurnFact {
	t0 := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
	rows := make([]TurnFact, n)
	for i := range rows {
		rows[i] = TurnFact{TS: t0.Add(time.Duration(i) * time.Minute), Model: "claude-opus-4-7", Input: in, Output: out, CacheRead: cr, CacheCreation: cc, Source: "jsonl"}
	}
	return rows
}

func TestDetectCacheHitRate(t *testing.T) {
	th := DefaultThresholds()
	cold := SessionFacts{ID: "s-cold", Model: "claude-opus-4-7", Rows: mkRows(10, 40_000, 500, 5_000, 2_000)} // ~11% hit rate, 470K window
	warm := SessionFacts{ID: "s-warm", Model: "claude-opus-4-7", Rows: mkRows(10, 2_000, 500, 40_000, 2_000)} // ~91%
	got := detectCacheHitRate(testFacts(cold, warm), th)
	if len(got) != 1 || got[0].ScopeID != "s-cold" {
		t.Fatalf("want 1 suggestion for the cold session, got %+v", got)
	}
	if got[0].SavingsUSD <= 0 {
		t.Errorf("savings must be positive: %+v", got[0])
	}
}

func TestDetectCacheWriteWasteAndPrefixThrash(t *testing.T) {
	th := DefaultThresholds()
	s := SessionFacts{ID: "s-cw", Model: "claude-opus-4-7", Rows: mkRows(6, 1000, 100, 0, 0)}
	f := testFacts(s)
	f.CacheEvents = map[string][]CacheEventFact{
		"s-cw": {
			{Kind: "write", Cause: "tools_changed", TokensWritten: 300_000, CostDeltaUSD: 1.6},
			{Kind: "write", Cause: "tools_changed", TokensWritten: 300_000, CostDeltaUSD: 1.6},
			{Kind: "write", Cause: "system_changed", TokensWritten: 100_000, CostDeltaUSD: 0.5},
			{Kind: "hit", Cause: "suffix_growth", TokensRead: 50_000},
		},
	}
	ww := detectCacheWriteWaste(f, th)
	if len(ww) != 1 {
		t.Fatalf("write-waste: want 1, got %+v", ww)
	}
	// 700K written at 6.25 − 5.00 = $0.875 premium
	if ww[0].SavingsUSD < 0.8 || ww[0].SavingsUSD > 0.95 {
		t.Errorf("write-waste savings = %.2f, want ~0.875", ww[0].SavingsUSD)
	}
	pt := detectPrefixThrash(f, th)
	if len(pt) != 1 {
		t.Fatalf("prefix-thrash: want 1, got %+v", pt)
	}
	if pt[0].SavingsUSD != 3.7 {
		t.Errorf("thrash savings = %.2f, want 3.70 (sum of cost_delta)", pt[0].SavingsUSD)
	}
	// No events → neither fires.
	f2 := testFacts(s)
	f2.CacheEvents = map[string][]CacheEventFact{}
	if n := len(detectCacheWriteWaste(f2, th)) + len(detectPrefixThrash(f2, th)); n != 0 {
		t.Errorf("no cache events must mean no cache-family suggestions, got %d", n)
	}
}

func TestDetectReadHeavyAndEffortAndFast(t *testing.T) {
	th := DefaultThresholds()
	rh := SessionFacts{
		ID: "s-rh", Model: "claude-opus-4-7", Rows: mkRows(10, 50_000, 1000, 0, 0),
		Mix: ActionMix{Total: 100, Reads: 80, Edits: 5},
	}
	f := testFacts(rh)
	f.Price = modelPrice // model-sensitive (opus 5×, others 1×)
	got := detectReadHeavyExpensiveModel(f, th)
	if len(got) != 1 || got[0].SavingsUSD <= 0 {
		t.Fatalf("read-heavy: want 1 with savings, got %+v", got)
	}
	// Low read share must not fire.
	rh.Mix = ActionMix{Total: 100, Reads: 20, Edits: 40}
	f2 := testFacts(rh)
	f2.Price = modelPrice
	if n := len(detectReadHeavyExpensiveModel(f2, th)); n != 0 {
		t.Errorf("low read share fired: %d", n)
	}

	// Effort: high-effort actions + reasoning ≥ 40% of output.
	eff := SessionFacts{ID: "s-eff", Model: "claude-opus-4-7", Mix: ActionMix{Total: 50, EffortHigh: 15}}
	eff.Rows = mkRows(10, 1000, 10_000, 0, 0)
	for i := range eff.Rows {
		eff.Rows[i].Reasoning = 8_000
	}
	ge := detectEffortOverprovisioning(testFacts(eff), th)
	if len(ge) != 1 {
		t.Fatalf("effort: want 1, got %+v", ge)
	}

	// Fast premium: 10 fast rows at flatPrice double = premium == standard cost.
	fastS := SessionFacts{ID: "s-fast", Model: "claude-opus-4-7", Rows: mkRows(10, 100_000, 2000, 0, 0)}
	for i := range fastS.Rows {
		fastS.Rows[i].Fast = true
	}
	gf := detectFastTierPremium(testFacts(fastS), th)
	if len(gf) != 1 || gf[0].SavingsUSD <= 0 {
		t.Fatalf("fast: want 1 with premium, got %+v", gf)
	}
}

func TestDetectUnrecoveredFailuresNoiseGate(t *testing.T) {
	th := DefaultThresholds()
	f := testFacts()
	f.Failures = []FailureFact{
		{Project: "/p", Command: "ls", Fails: 31, Recovered: false},                 // noise — gated
		{Project: "/p", Command: "npm run build 2>&1", Fails: 10, Recovered: false}, // real
		{Project: "/p", Command: "npm run flaky", Fails: 10, Recovered: true},       // recovered — not C4
		{Project: "/p", Command: "make e2e", Fails: 3, Recovered: false},            // below floor
	}
	got := detectUnrecoveredFailures(f, th)
	if len(got) != 1 || !strings.Contains(got[0].Title, "npm run build") {
		t.Fatalf("want exactly the npm-run-build sink, got %+v", got)
	}
	if got[0].SavingsMin <= 0 {
		t.Errorf("C4 is time-denominated; SavingsMin = %v", got[0].SavingsMin)
	}
}

func TestDetectHygiene(t *testing.T) {
	th := DefaultThresholds()
	// E1: configured, 300 turns, 1 call → fires. Not configured → silent.
	s := SessionFacts{ID: "s-h", Model: "claude-opus-4-7", Rows: mkRows(300, 5000, 500, 0, 0)}
	f := testFacts(s)
	f.MCPConfigured = true
	f.MCPCalls = 1
	if got := detectMCPOverhead(f, th); len(got) != 1 || got[0].SavingsUSD <= 0 {
		t.Fatalf("mcp_overhead: want 1, got %+v", got)
	} else if a := got[0].Action; a == nil || a.Kind != "settings_section" || a.Target != "tools" {
		// Q4 pin: E1 remediation points at the Connected-tools panel
		// (wizard consent flow), never a direct write.
		t.Fatalf("mcp_overhead action: %+v", a)
	}
	f.MCPConfigured = false
	if got := detectMCPOverhead(f, th); len(got) != 0 {
		t.Fatalf("mcp not configured must be silent")
	}

	// E3: 3 proxied sessions, zero compression → fires; any compressed → silent.
	mk := func(id string, comp int64) SessionFacts {
		ss := SessionFacts{ID: id, Model: "claude-opus-4-7", Rows: mkRows(10, 100_000, 1000, 0, 0), CompressionOrig: comp}
		for i := range ss.Rows {
			ss.Rows[i].Source = "proxy"
		}
		return ss
	}
	fc := testFacts(mk("p1", 0), mk("p2", 0), mk("p3", 0))
	if got := detectCompressionOff(fc, th); len(got) != 1 {
		t.Fatalf("compression_off: want 1, got %+v", got)
	} else if a := got[0].Action; a == nil || a.Kind != "settings_section" || a.Target != "compression" {
		t.Fatalf("compression_off action: %+v", a)
	}
	fc2 := testFacts(mk("p1", 0), mk("p2", 9000), mk("p3", 0))
	if got := detectCompressionOff(fc2, th); len(got) != 0 {
		t.Fatalf("any compressed session must silence E3")
	}

	// E4: 3 unproxied $-heavy sessions → fires.
	mkU := func(id string) SessionFacts {
		return SessionFacts{ID: id, Model: "claude-opus-4-7", Rows: mkRows(20, 200_000, 5000, 0, 0)}
	}
	fu := testFacts(mkU("u1"), mkU("u2"), mkU("u3"))
	if got := detectNotProxied(fu, th); len(got) != 1 || got[0].SavingsUSD <= 0 {
		t.Fatalf("not_proxied: want 1, got %+v", got)
	} else if a := got[0].Action; a == nil || a.Kind != "page" || a.Target != "/compression" {
		t.Fatalf("not_proxied action: %+v", a)
	}
}

// modelPrice is a model-sensitive flat price: opus 5×, others 1× base.
func modelPrice(model string, b cost.TokenBundle) (float64, bool) {
	rate := 1.0
	if strings.Contains(model, "opus") {
		rate = 5.0
	}
	return (float64(b.Input+b.CacheRead+b.CacheCreation)*rate + float64(b.Output)*rate*5) / 1e6, true
}
