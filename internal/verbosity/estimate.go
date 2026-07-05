package verbosity

import (
	"math"
	"sort"
)

// estimate.go derives an ESTIMATED token split from the exact byte breakdown
// (plan §7). Bytes are the exact primary unit; tokens are apportioned from a
// turn/session's observed output_tokens across the category buckets using
// per-class chars-per-token constants. Every surface labels these "est." —
// they are never presented as exact. Pure: no I/O, no cost engine (the caller
// multiplies the per-bucket tokens by the model's output rate for dollars).

// charsPerToken is the per-category divisor turning measured UTF-8 bytes into
// an estimated token weight. English prose tokenizes at ~4 chars/token; code
// is denser (~3.3) because of punctuation/identifiers; structured data
// (config/data) denser still (~3.0). These are the plan §7 defaults and are
// calibratable from observed output_tokens vs measured bytes.
var charsPerToken = map[Category]float64{
	Prose:   4.0,
	Code:    3.3,
	Docs:    4.0,
	Config:  3.2,
	Data:    3.0,
	Unknown: 3.8,
}

// defaultCharsPerToken is used for any category missing from the table.
const defaultCharsPerToken = 3.8

// TokenSplit is the estimated apportionment of a scope's output tokens across
// content categories. ByCategory sums to ApportionedTokens exactly (rounding
// drift is folded into the largest bucket). CodeTokens / ExplainTokens are the
// headline code-vs-explanation cut, mirroring Breakdown.CodeBytes /
// ExplainBytes. All values are ESTIMATES.
type TokenSplit struct {
	ApportionedTokens int64
	ByCategory        map[Category]int64
	CodeTokens        int64
	ExplainTokens     int64
}

// EstimateTokens apportions outputTokens across the breakdown's category byte
// buckets, weighting each by bytes ÷ chars-per-token so denser content (code)
// claims proportionally more tokens per byte than prose. outputTokens is the
// NON-reasoning output (reasoning is billed separately at the output rate and
// is not represented in the byte buckets, so the caller accounts for it on its
// own). Returns an empty split when there are no bytes or no tokens.
func EstimateTokens(b *Breakdown, outputTokens int64) TokenSplit {
	ts := TokenSplit{ByCategory: map[Category]int64{}}
	if b == nil || outputTokens <= 0 {
		return ts
	}

	type weighted struct {
		cat    Category
		weight float64
	}
	var ws []weighted
	var totalWeight float64
	for cat, bytes := range b.ByCategory() {
		if bytes <= 0 {
			continue
		}
		cpt := charsPerToken[cat]
		if cpt <= 0 {
			cpt = defaultCharsPerToken
		}
		w := float64(bytes) / cpt
		ws = append(ws, weighted{cat: cat, weight: w})
		totalWeight += w
	}
	if totalWeight <= 0 {
		return ts
	}

	// Descending weight, then category name, for deterministic rounding.
	sort.Slice(ws, func(i, j int) bool {
		if ws[i].weight != ws[j].weight {
			return ws[i].weight > ws[j].weight
		}
		return ws[i].cat < ws[j].cat
	})

	var assigned int64
	for _, x := range ws {
		tok := int64(math.Round(float64(outputTokens) * x.weight / totalWeight))
		ts.ByCategory[x.cat] = tok
		assigned += tok
	}
	// Fold rounding drift into the largest-weight bucket so ByCategory sums to
	// outputTokens exactly.
	if drift := outputTokens - assigned; drift != 0 {
		ts.ByCategory[ws[0].cat] += drift
	}

	ts.ApportionedTokens = outputTokens
	ts.CodeTokens = ts.ByCategory[Code]
	ts.ExplainTokens = ts.ByCategory[Prose]
	return ts
}

// Cost prices the split at a per-token output rate (dollars per token — the
// caller divides the per-million published rate by 1e6 first). code/explain
// cover the apportioned output split; total covers the full output PLUS
// reasoning (billed at the same output rate as a distinct slice, per the cost
// engine), so total ≥ code+explain. All figures are ESTIMATES.
func (ts TokenSplit) Cost(outputRatePerToken float64, reasoningTokens int64) (code, explain, total float64) {
	code = float64(ts.CodeTokens) * outputRatePerToken
	explain = float64(ts.ExplainTokens) * outputRatePerToken
	total = float64(ts.ApportionedTokens+reasoningTokens) * outputRatePerToken
	return code, explain, total
}
