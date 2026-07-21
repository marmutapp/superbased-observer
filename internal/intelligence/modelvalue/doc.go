// Package modelvalue computes the Model Value Report (model-routing spec
// §R17/§R18 P0 surface): per model × turn-kind × project observations of
// cost per turn, latency p50/p95, error rate, tool-failure rate, and
// volume, plus pairwise deltas against the highest-tier baseline with
// sample sizes and 95% confidence bands.
//
// Design (mirrors the cachetrack/routing purity discipline, stricter
// than the advisor):
//   - The package is PURE aggregation — it imports no database/sql; the
//     SQL lives in the store seam (internal/store/modelvalue.go), which
//     loads the deduped proxy∪JSONL substrate into Facts and hands it
//     over. imports_test.go pins this.
//   - Turn-kind classification and tier placement come from
//     internal/routing (pure → pure import); pricing arrives through the
//     injected routing.PriceFn (backed by cost.Engine.ComputeBreakdown
//     at the boundary — never re-derived here).
//   - Attribution honesty (§R7.2): everything here is correlational —
//     hard tasks are routed to big models. Every Report carries the
//     caveat verbatim, every delta carries both sample sizes, and the
//     parity verdict degrades to insufficient_evidence below the sample
//     floor rather than implying a claim.
package modelvalue
