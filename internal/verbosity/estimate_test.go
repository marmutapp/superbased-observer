package verbosity

import (
	"strings"
	"testing"
)

// TestEstimateTokens_ApportionsAndSums proves the split sums to the input
// exactly and that denser code claims more tokens per byte than prose. The
// fixture is balanced so the two buckets carry equal weight: 4000 prose bytes
// ÷ 4.0 == 3300 code bytes ÷ 3.3 == 1000 each, so 1000 output tokens split
// 500/500.
func TestEstimateTokens_ApportionsAndSums(t *testing.T) {
	t.Parallel()
	b := NewBreakdown()
	b.AddVisibleText(strings.Repeat("word ", 800)) // 4000 prose bytes, no fences
	b.AddWrite("main.go", 3300)                    // 3300 code bytes

	ts := EstimateTokens(b, 1000)
	if ts.ApportionedTokens != 1000 {
		t.Errorf("ApportionedTokens = %d, want 1000", ts.ApportionedTokens)
	}

	var sum int64
	for _, v := range ts.ByCategory {
		sum += v
	}
	if sum != 1000 {
		t.Errorf("ByCategory sums to %d, want 1000 (no token left unassigned)", sum)
	}
	if ts.CodeTokens != 500 || ts.ExplainTokens != 500 {
		t.Errorf("CodeTokens=%d ExplainTokens=%d, want 500/500 (equal weight)", ts.CodeTokens, ts.ExplainTokens)
	}
}

// TestEstimateTokens_CodeDenserThanProse: equal BYTES of code and prose give
// code more tokens, because code packs more tokens per byte (lower
// chars/token).
func TestEstimateTokens_CodeDenserThanProse(t *testing.T) {
	t.Parallel()
	b := NewBreakdown()
	b.AddVisibleText(strings.Repeat("x", 3300)) // 3300 prose bytes
	b.AddWrite("main.go", 3300)                 // 3300 code bytes

	ts := EstimateTokens(b, 2000)
	if ts.CodeTokens <= ts.ExplainTokens {
		t.Errorf("CodeTokens=%d should exceed ExplainTokens=%d for equal bytes (code is denser)", ts.CodeTokens, ts.ExplainTokens)
	}
}

// TestEstimateTokens_Empty: no tokens or no bytes → empty split, never a panic
// or a divide-by-zero.
func TestEstimateTokens_Empty(t *testing.T) {
	t.Parallel()
	if ts := EstimateTokens(NewBreakdown(), 0); ts.ApportionedTokens != 0 || len(ts.ByCategory) != 0 {
		t.Errorf("zero tokens: got %+v, want empty", ts)
	}
	if ts := EstimateTokens(NewBreakdown(), 1000); ts.ApportionedTokens != 0 {
		t.Errorf("no bytes: got %+v, want empty", ts)
	}
	if ts := EstimateTokens(nil, 1000); ts.ApportionedTokens != 0 {
		t.Errorf("nil breakdown: got %+v, want empty", ts)
	}
}
