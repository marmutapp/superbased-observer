package cost

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// GroupBy selects how summary rows are keyed.
type GroupBy string

const (
	GroupByModel      GroupBy = "model"
	GroupBySession    GroupBy = "session"
	GroupByDay        GroupBy = "day"
	GroupByDayModel   GroupBy = "day_model"
	GroupByDayProject GroupBy = "day_project"
	GroupByDayTool    GroupBy = "day_tool"
	GroupByProject    GroupBy = "project"
	GroupByTool       GroupBy = "tool"
	GroupByNone       GroupBy = "none"
)

// dayModelKeySep separates the date and dimension halves of any
// GroupByDay<X> key. Chosen for SQLite/JSON safety (no escaping
// concerns) and because no model / project path / tool id contains
// the literal `||`.
const dayModelKeySep = "||"

// SplitDayModelKey unpacks a Row.Key produced under GroupByDayModel
// into (date, model). Returns ("", key) if the separator is missing,
// so callers can degrade gracefully.
func SplitDayModelKey(key string) (day, model string) {
	return splitDayDimKey(key)
}

// SplitDayProjectKey unpacks a GroupByDayProject Row.Key into
// (date, project_root_path). Same separator as SplitDayModelKey.
func SplitDayProjectKey(key string) (day, project string) {
	return splitDayDimKey(key)
}

// SplitDayToolKey unpacks a GroupByDayTool Row.Key into (date, tool).
func SplitDayToolKey(key string) (day, tool string) {
	return splitDayDimKey(key)
}

func splitDayDimKey(key string) (day, dim string) {
	if i := strings.Index(key, dayModelKeySep); i >= 0 {
		return key[:i], key[i+len(dayModelKeySep):]
	}
	return "", key
}

// Source selects which token sources feed the summary. The default (Proxy)
// is accurate; JSONL is approximate/unreliable. Auto prefers proxy for
// sessions where both exist and falls back to JSONL for the rest — most
// realistic because the proxy often lacks a session_id.
//
// SourceAuto also includes summary_calls (D20 internal Haiku spend that
// goes direct to api.anthropic.com, bypassing the proxy). These rows
// don't dedup against proxy/jsonl because they represent observer-
// initiated calls — different turns, different content. Surfacing them
// makes get_cost_summary / observer cost / dashboard cost-by-model see
// the real total Anthropic spend, not "user-facing turns only".
type Source string

const (
	SourceProxy        Source = "proxy"
	SourceJSONL        Source = "jsonl"
	SourceSummaryCalls Source = "summary_calls"
	SourceAuto         Source = "auto"
)

// Options parameterize Summary.
type Options struct {
	// Days restricts to rows with timestamp ≥ now - Days. Zero means no
	// restriction.
	Days int
	// Since overrides Days when non-zero.
	Since time.Time
	// Until is an optional upper bound on row timestamps (exclusive of
	// "now"). Used by callers that want a closed window — e.g. the
	// Analysis movers endpoint querying the prior period as
	// [now-2N, now-N). Zero means no upper bound (rows up to now).
	Until time.Time
	// GroupBy is the rollup key. Defaults to GroupByModel.
	GroupBy GroupBy
	// Source selects which tables feed the rollup. Defaults to SourceAuto.
	Source Source
	// ProjectRoot filters rows to a single project by matching projects.root_path.
	ProjectRoot string
	// Tool filters rows to a single tool by matching sessions.tool. Empty
	// means no tool filter. Mirrors the dashboard's global Tool dropdown
	// so cost rollups can scope to "what did codex cost me this month".
	Tool string
	// SessionIDs scopes the row scan to a specific set of session_ids
	// across api_turns / token_usage / summary_calls. Empty means no
	// session filter. Lets callers like handleSessions amortize one
	// engine call across exactly the page they're rendering instead of
	// loading every row in the window and discarding most of them.
	SessionIDs []string
	// Limit caps returned rows (after sort by cost_usd desc). Zero means 50.
	Limit int
	// Now overrides time.Now for deterministic tests.
	Now func() time.Time
}

// Row is one grouped summary entry.
type Row struct {
	Key    string      `json:"key"`
	Tokens TokenBundle `json:"tokens"`
	// CostUSD is the legacy total (AI + tool). AICostUSD and
	// ToolCostUSD split it so the dashboard can show "API spend"
	// vs "tool fees" (web_search calls, etc.) separately.
	// CostUSD == AICostUSD + ToolCostUSD always.
	CostUSD     float64 `json:"cost_usd"`
	AICostUSD   float64 `json:"ai_cost_usd"`
	ToolCostUSD float64 `json:"tool_cost_usd"`
	// TurnCount is how many underlying rows (api_turns + token_usage) fed
	// this group.
	TurnCount int `json:"turn_count"`
	// AvgLatencyMS is the proxy-observed mean request → response time
	// across the proxy rows in this bucket (V3-5). 0 when no proxy
	// rows contributed (JSONL-only buckets). Computed as
	// SUM(api_turns.total_response_ms) / COUNT(rows with latency > 0).
	AvgLatencyMS int64 `json:"avg_latency_ms,omitempty"`
	// Source is "proxy", "jsonl", or "mixed" depending on which tables fed
	// the group.
	Source string `json:"source"`
	// Reliability is the weakest reliability tag among the rows in the
	// group: "accurate" > "approximate" > "unreliable" > "unknown". The
	// weakest wins so callers know how much to trust the number.
	Reliability string `json:"reliability"`
	// UnknownModels lists model ids that had no pricing entry. Their token
	// counts are still summed; their cost contribution is zero.
	UnknownModels []string `json:"unknown_models,omitempty"`
	// PricingSource reports how the pricing-table lookup resolved across
	// every row that fed this bucket: "exact" / "date-stripped" /
	// "family" / "miss" / "mixed" (multiple paths used). The dashboard
	// surfaces a "~" badge alongside Reliability when this is anything
	// other than "exact" so users can see which numbers came from
	// fallback rates. Empty when no row in the bucket was priced (e.g.
	// every row had recorded estimated_cost_usd).
	PricingSource string `json:"pricing_source,omitempty"`
	// Compression aggregates conversation-layer compression savings over
	// the turns in this group (spec §10 Layer 3 / §24). Only proxy rows
	// contribute — JSONL doesn't see the pre-forward body.
	Compression CompressionStats `json:"compression"`
	// FastTurnCount / FastCostUSD report the subset of this group's turns
	// served in the provider's low-latency "fast" tier (Anthropic Opus
	// 4.8 speed:"fast"). FastCostUSD already reflects the FastMultiplier
	// premium (it's summed from the same per-row cost as CostUSD). Both
	// zero when no fast turns fed the group; the dashboard badges the row
	// only when FastTurnCount > 0. omitempty keeps the common (no-fast)
	// payload lean.
	FastTurnCount int     `json:"fast_turn_count,omitempty"`
	FastCostUSD   float64 `json:"fast_cost_usd,omitempty"`
}

// CompressionStats aggregates savings metadata across every proxy turn
// in a rollup group. See spec §10 Layer 3 step 11.
//
// Bytes are what the proxy can measure directly (it sees the JSON body
// before and after the pipeline runs). Tokens are what cost dollars,
// so we derive a token-savings estimate using Anthropic's tokenizer
// rule of thumb (~4 chars per token on typical English/code) and a
// dollar-savings estimate using the row's model input rate (since
// dropped/compressed content is prompt context).
//
// TokensSavedEst and CostSavedUSDEst are SIGNED — when compression
// makes the payload larger (small payloads where marker overhead
// exceeds what was dropped), they go negative. Aggregating across many
// turns can still net positive even when individual rows are negative.
type CompressionStats struct {
	// OriginalBytes is the sum of pre-compression request body sizes.
	OriginalBytes int64 `json:"original_bytes"`
	// CompressedBytes is the sum of post-compression request body sizes.
	CompressedBytes int64 `json:"compressed_bytes"`
	// CompressedCount is the number of tool_result bodies rewritten.
	CompressedCount int64 `json:"compressed_count"`
	// DroppedCount is the number of original messages replaced by markers.
	DroppedCount int64 `json:"dropped_count"`
	// MarkerCount is the number of marker messages emitted.
	MarkerCount int64 `json:"marker_count"`
	// Turns is the number of turns in this group that had any
	// compression metadata (i.e. proxy turns with compression enabled).
	Turns int `json:"turns"`
	// TokensSavedEst is the byte-saving converted to tokens via the
	// ~4 chars/token rule of thumb. Signed.
	TokensSavedEst int64 `json:"tokens_saved_est"`
	// CostSavedUSDEst is TokensSavedEst valued at a blended input-side
	// rate that mirrors the row's realized Input vs CacheRead share.
	// V7-6 fix (v1.7.6): pre-v1.7.6 this column used pricing.Input
	// flat, which overstated codex/OpenAI savings by ~10× because
	// codex sessions are cache_read-dominant (cache_read is ~10×
	// cheaper than net input). Anthropic sessions are unchanged
	// (their CacheRead share is low). Signed.
	CostSavedUSDEst float64 `json:"cost_saved_usd_est"`
	// CostSavedUSDEstInputTier is the upper-bound USD savings assuming
	// every compressed token would have been billed at the net-input
	// tier (pricing.Input). Equivalent to pre-v1.7.6 cost_saved_usd_est.
	// Useful for operators who want the "if I had paid the worst-case
	// rate" headline. Signed.
	CostSavedUSDEstInputTier float64 `json:"cost_saved_usd_est_input_tier"`
	// CostSavedUSDEstCacheReadTier is the lower-bound USD savings
	// assuming every compressed token would have been billed at the
	// cache_read tier (pricing.CacheRead). Useful for cache-dominant
	// sessions (codex / OpenAI). Signed.
	CostSavedUSDEstCacheReadTier float64 `json:"cost_saved_usd_est_cache_read_tier"`
}

// Add merges other into c.
func (c *CompressionStats) Add(other CompressionStats) {
	c.OriginalBytes += other.OriginalBytes
	c.CompressedBytes += other.CompressedBytes
	c.CompressedCount += other.CompressedCount
	c.DroppedCount += other.DroppedCount
	c.MarkerCount += other.MarkerCount
	c.Turns += other.Turns
	c.TokensSavedEst += other.TokensSavedEst
	c.CostSavedUSDEst += other.CostSavedUSDEst
	c.CostSavedUSDEstInputTier += other.CostSavedUSDEstInputTier
	c.CostSavedUSDEstCacheReadTier += other.CostSavedUSDEstCacheReadTier
}

// SavedBytesSigned returns OriginalBytes - CompressedBytes (can be
// negative when compression added marker overhead beyond what was
// dropped). SavedBytes() (above) clamps to non-negative for backward
// compatibility with summary text formatting.
func (c CompressionStats) SavedBytesSigned() int64 {
	return c.OriginalBytes - c.CompressedBytes
}

// SavedBytes returns max(0, OriginalBytes - CompressedBytes).
func (c CompressionStats) SavedBytes() int64 {
	if c.CompressedBytes >= c.OriginalBytes {
		return 0
	}
	return c.OriginalBytes - c.CompressedBytes
}

// SavedRatio returns the fraction of bytes saved in [0, 1]. Zero when no
// compression data is present.
func (c CompressionStats) SavedRatio() float64 {
	if c.OriginalBytes <= 0 {
		return 0
	}
	ratio := float64(c.SavedBytes()) / float64(c.OriginalBytes)
	if ratio < 0 {
		return 0
	}
	if ratio > 1 {
		return 1
	}
	return ratio
}

// Summary is the result of Engine.Summary.
type Summary struct {
	GroupBy     GroupBy     `json:"group_by"`
	Source      Source      `json:"source"`
	Days        int         `json:"days,omitempty"`
	Since       string      `json:"since,omitempty"`
	Rows        []Row       `json:"rows"`
	TotalTokens TokenBundle `json:"total_tokens"`
	TotalCost   float64     `json:"total_cost_usd"`
	TurnCount   int         `json:"turn_count"`
	// FastTurnCount / TotalFastCostUSD report fast-tier (Opus 4.8
	// speed:"fast") spend over the whole window. TotalFastCostUSD already
	// carries the FastMultiplier premium. The Cost page renders a small
	// "fast-tier" stat when FastTurnCount > 0. omitempty keeps the
	// common (no-fast) payload lean.
	FastTurnCount    int     `json:"fast_turn_count,omitempty"`
	TotalFastCostUSD float64 `json:"total_fast_cost_usd,omitempty"`
	Reliability      string  `json:"reliability"`
	// UnknownModelCount is the number of distinct unknown models seen.
	UnknownModelCount int `json:"unknown_model_count,omitempty"`
	// UnpricedTokens is input+output token volume on rows that resolved to a
	// pricing MISS (empty or unknown model) and were billed $0. It volume-
	// weights UnknownModelCount so a whole-adapter attribution regression
	// (e.g. copilotcli Tier-1 rows going empty-model) is loud rather than
	// hidden behind a small distinct-model count (often just the single
	// empty string "").
	UnpricedTokens int64 `json:"unpriced_tokens,omitempty"`
	// UnpricedTurnCount is the number of rows that hit a pricing MISS.
	UnpricedTurnCount int `json:"unpriced_turn_count,omitempty"`
	// TotalCompression aggregates conversation compression savings over
	// the whole window.
	TotalCompression CompressionStats `json:"total_compression"`
}

// rawRow is the shape pulled from SQLite; kept unexported because callers
// never see it.
type rawRow struct {
	model       string
	tokens      TokenBundle
	recordedUSD float64 // cost_usd column, when set (proxy only)
	ts          string
	sessionID   string
	projectPath string
	tool        string
	source      string // "proxy" | "jsonl"
	reliability string
	compression CompressionStats // non-zero only for proxy rows with savings
	// latencyMS is the proxy-observed total response time in ms
	// (api_turns.total_response_ms). Zero for JSONL rows — the
	// adapter path doesn't see request → response wall time.
	// V3-5 surfaces the bucket average as Row.AvgLatencyMS.
	latencyMS int64
	// turnID identifies one upstream API turn. Proxy rows populate it
	// from api_turns.request_id (Anthropic message_id, OpenAI completion
	// id). JSONL rows populate it from token_usage.source_event_id, which
	// the Claude Code adapter sets to the same Anthropic message_id. Used
	// by the dedup pass: when both sources have a row with the same
	// turnID, proxy wins. When either side lacks a turnID, dedup falls
	// back to the older session_id + fingerprint matcher.
	turnID string
}

// isNoiseRow drops rawRows that don't represent a real API call:
//
//  1. model == "<synthetic>" — Claude Code's compaction / subagent
//     stitching placeholder. The C4 forward fix dropped these at the
//     adapter, but ~19 historical rows persist in the live install.
//     Surfacing them as a separate model bucket on the Cost tab is
//     confusing (the angle-bracketed string also renders invisibly
//     in HTML), and they always carry zero usage anyway, so the row
//     adds nothing.
//
//  2. Every token column AND the recorded cost is zero. Pre-B2 the
//     proxy logged turns even when the upstream stream ended without
//     any usage event (cancellation, mid-flight error). The B2 forward
//     filter now drops these at insert; this catches the historical
//     residue plus any future shape that lands the same way through
//     the JSONL path.
//
// Returning true here causes the row to be skipped at load time so
// turn_count, total_tokens, and total_cost_usd all stay coherent.
func isNoiseRow(r rawRow) bool {
	if r.model == "<synthetic>" {
		return true
	}
	allZeroTokens := r.tokens.Input == 0 && r.tokens.Output == 0 &&
		r.tokens.CacheRead == 0 && r.tokens.CacheCreation == 0 &&
		r.tokens.Reasoning == 0 && r.tokens.WebSearchRequests == 0
	if allZeroTokens && r.recordedUSD == 0 {
		return true
	}
	return false
}

// fingerprint identifies "this is the same logical API turn" when the
// session_id key isn't available. Used by the proxy ↔ JSONL dedup in
// loadRows when a proxy row landed without a session_id (typical for
// pre-v1.2.1 api_turns rows). Timestamp is bucketed to the same calendar
// minute so a 30-second skew between when the proxy logged the turn and
// when the JSONL was flushed doesn't break the match. Token columns are
// expected to match byte-for-byte: cache_read in particular is the same
// number on both sides because it's reported by Anthropic's API.
//
// False-positive risk: two distinct API calls in the same minute with
// byte-identical token shapes collapse into one. Acceptable trade — the
// alternative is summing both halves of the same call against each other.
// shapeKey identifies one turn by shape only — model, minute,
// token-bundle. Session is intentionally excluded so the dedup pass can
// match an empty-session legacy proxy row against a session-tagged
// JSONL row of the same turn (audit item A2). The dedup pass adds
// session into the lookup separately when both sides have it set.
func shapeKey(r rawRow) string {
	bucket := r.ts
	if len(bucket) >= 16 {
		bucket = bucket[:16] // YYYY-MM-DDTHH:MM
	}
	return fmt.Sprintf("%s|%s|%d|%d|%d|%d",
		r.model, bucket,
		r.tokens.Input, r.tokens.Output,
		r.tokens.CacheRead, r.tokens.CacheCreation)
}

// sessionShapeKey identifies a turn by token-bundle shape WITHOUT the
// minute bucket that shapeKey carries. Used only for the session-anchored
// dedup level, where the session_id already disambiguates: the minute then
// adds no safety but DOES cause false-negatives when a turn's two captures
// straddle a minute boundary. Codex's rollout flush lands ~10s after the
// proxy logs the request, so ~15% of turns near a minute boundary would
// escape a minute-bucketed match and double-count. Two DISTINCT turns in
// one session with byte-identical (model, input, output, cache_read,
// cache_creation) are effectively impossible (cache_read grows
// monotonically across a session), so dropping the minute can't false-drop
// a legitimate turn. See audit F1 (docs/audits/audit-2026-06-08.md).
func sessionShapeKey(r rawRow) string {
	return fmt.Sprintf("%s|%d|%d|%d|%d",
		r.model,
		r.tokens.Input, r.tokens.Output,
		r.tokens.CacheRead, r.tokens.CacheCreation)
}

// Summary runs the query against db and returns a grouped rollup.
func (e *Engine) Summary(ctx context.Context, db *sql.DB, opts Options) (Summary, error) {
	if opts.GroupBy == "" {
		opts.GroupBy = GroupByModel
	}
	if opts.Source == "" {
		opts.Source = SourceAuto
	}
	if opts.Limit <= 0 {
		opts.Limit = 50
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}

	since := opts.Since
	if since.IsZero() && opts.Days > 0 {
		since = opts.Now().Add(-time.Duration(opts.Days) * 24 * time.Hour)
	}

	rows, err := e.loadRows(ctx, db, opts, since)
	if err != nil {
		return Summary{}, err
	}
	s := e.rollup(rows, opts)
	s.Days = opts.Days
	if !since.IsZero() {
		s.Since = since.UTC().Format(time.RFC3339)
	}
	return s, nil
}

//nolint:gocyclo // one sequential scan branch per token-source tier (proxy/JSONL union + dedup); splitting would scatter the two-level dedup invariant.
func (e *Engine) loadRows(ctx context.Context, db *sql.DB, opts Options, since time.Time) ([]rawRow, error) {
	var out []rawRow

	if opts.Source == SourceProxy || opts.Source == SourceAuto {
		proxy, err := e.loadProxyRows(ctx, db, opts, since)
		if err != nil {
			return nil, err
		}
		out = append(out, proxy...)
	}

	if opts.Source == SourceSummaryCalls || opts.Source == SourceAuto {
		summaryRows, err := e.loadSummaryCallRows(ctx, db, opts, since)
		if err != nil {
			return nil, err
		}
		out = append(out, summaryRows...)
	}

	if opts.Source == SourceJSONL || opts.Source == SourceAuto {
		jsonl, err := e.loadJSONLRows(ctx, db, opts, since)
		if err != nil {
			return nil, err
		}
		if opts.Source == SourceAuto {
			// Prefer proxy on a PER-TURN basis. Pre-2026-04-29 we deduped
			// at session granularity (drop ALL JSONL rows for a session
			// once any proxy row exists), which silently dropped 80%+ of
			// tokens whenever the proxy was off for most of a session and
			// JSONL was the only ground truth for those turns. See
			// docs/cost-accuracy-audit-2026-04-29.md, Finding 1.
			//
			// Three-level dedup, tried in order from most to least precise:
			//
			//   1. turnID — api_turns.request_id and the JSONL adapter's
			//      source_event_id store the upstream message id (msg_xxx
			//      for Anthropic, where both sides agree). A per-turn match
			//      is exact when both sides use the SAME scheme.
			//
			//   2. (session_id, sessionShapeKey) — when the turnID match
			//      fails, match by session + token-bundle shape. This is the
			//      ONLY level that catches codex: its proxy request_id
			//      (resp_…) and JSONL source_event_id (tk:rollout-…:L<n>)
			//      are disjoint schemes (zero overlap DB-wide), so level 1
			//      never fires and the two captures of every codex turn
			//      would double-count without this. See audit F1
			//      (docs/audits/audit-2026-06-08.md).
			//
			//   3. shape-only (with minute) against proxy rows with EMPTY
			//      session_id — legacy A2 case: pre-v1.2.1 the proxy didn't
			//      always record a session_id, but the JSONL adapter did.
			//
			// The shape maps are now populated from EVERY proxy row, not
			// just turnID-less ones (the pre-fix bug: codex proxy rows
			// always carry a request_id, so they `continue`d past the shape
			// maps, leaving levels 2/3 dead and every codex turn double-
			// counted — audit F1). Independently re-discovered + fixed on
			// the sibling clone 2026-06-10 as an operator-reported
			// regression: codex CLI session 019eadd3-… (gpt-5.4, single
			// turn) surfaced as input_tokens=15,020 on /api/sessions — an
			// exact 2× of the real 7,510. This (minute-less sessionShapeKey
			// + fast-lift) variant superseded that fix at the 2026-06-10
			// reunion merge.
			proxyTurns := map[string]bool{}
			proxySessionShapes := map[string]bool{} // sessionID + "\x00" + sessionShapeKey
			proxyOrphanShapes := map[string]bool{}  // shapeKey, empty-session proxy rows only
			for _, r := range out {
				// summary_calls are observer-initiated (D20 Haiku) and must
				// never dedup a real user turn — keep them out of every map.
				if r.source == "summary_calls" {
					continue
				}
				if r.turnID != "" {
					proxyTurns[r.turnID] = true
				}
				if r.sessionID == "" {
					proxyOrphanShapes[shapeKey(r)] = true
				} else {
					proxySessionShapes[r.sessionID+"\x00"+sessionShapeKey(r)] = true
				}
			}
			// Codex's priority/fast premium lives ONLY in the JSONL row
			// (sourced from config.toml); the proxy wire reports the default
			// tier (fast=0). When a fast JSONL row is dropped as a proxy
			// duplicate we remember to lift its fast flag onto the surviving
			// proxy twin, so the FastMultiplier premium isn't lost (audit F1,
			// operator decision "keep proxy, OR-in fast").
			fastTurnIDs := map[string]bool{}
			fastSessionShapes := map[string]bool{}
			fastOrphanShapes := map[string]bool{}
			for _, r := range jsonl {
				if r.turnID != "" && proxyTurns[r.turnID] {
					if r.tokens.Fast {
						fastTurnIDs[r.turnID] = true
					}
					continue
				}
				sk := r.sessionID + "\x00" + sessionShapeKey(r)
				if r.sessionID != "" && proxySessionShapes[sk] {
					if r.tokens.Fast {
						fastSessionShapes[sk] = true
					}
					continue
				}
				ok := shapeKey(r)
				if len(proxyOrphanShapes) > 0 && proxyOrphanShapes[ok] {
					if r.tokens.Fast {
						fastOrphanShapes[ok] = true
					}
					continue
				}
				out = append(out, r)
			}
			// Lift inherited fast onto the surviving proxy rows. Zeroing
			// recordedUSD forces rollup to re-price from the table WITH the
			// FastMultiplier premium (the proxy's insert-time cost_usd was
			// computed at the standard wire tier). Only a 0→1 flip recomputes;
			// a proxy row already fast (Anthropic Opus 4.8, premium already in
			// its recorded cost) is left untouched.
			for i := range out {
				if out[i].source != "proxy" || out[i].tokens.Fast {
					continue
				}
				inherit := false
				switch {
				case out[i].turnID != "" && fastTurnIDs[out[i].turnID]:
					inherit = true
				case out[i].sessionID != "" && fastSessionShapes[out[i].sessionID+"\x00"+sessionShapeKey(out[i])]:
					inherit = true
				case out[i].sessionID == "" && fastOrphanShapes[shapeKey(out[i])]:
					inherit = true
				}
				if inherit {
					out[i].tokens.Fast = true
					out[i].recordedUSD = 0
				}
			}
		} else {
			out = append(out, jsonl...)
		}
	}
	// Collapse the copilot family's redundant output-only "shadow" rows so a
	// turn's output isn't double-counted (the two copilot adapters each emit a
	// full-usage row AND an output-only row per turn — see
	// dropCopilotOutputShadows). No-op for every other adapter.
	out = dropCopilotOutputShadows(out)
	return out, nil
}

// dropCopilotOutputShadows removes the redundant output-only "shadow" token
// rows the copilot family (copilot, copilot-cli) emits. Both adapters write a
// full-usage token_usage row AND a separate output-only row (input=0, no cache)
// for the same turn — the adapter set MessageID on both intending a
// (session_id, message_id) merge, but the store keys on (source_file,
// source_event_id), so they never merge and the output double-counts. Drop an
// output-only copilot row when a full-usage sibling in the same session carries
// the same output_tokens. Every other adapter emits one token row per turn, so
// this is a no-op for them and the existing dedup behaviour is unchanged.
func dropCopilotOutputShadows(rows []rawRow) []rawRow {
	// Per-session set of output values "owned" by a full-usage row (input or
	// cache present). An output-only copilot row with the same value in the
	// same session is the redundant shadow.
	fullOutputs := map[string]map[int64]bool{}
	for i := range rows {
		r := rows[i]
		if r.tokens.Output <= 0 {
			continue
		}
		if r.tokens.Input > 0 || r.tokens.CacheRead > 0 || r.tokens.CacheCreation > 0 {
			m := fullOutputs[r.sessionID]
			if m == nil {
				m = map[int64]bool{}
				fullOutputs[r.sessionID] = m
			}
			m[r.tokens.Output] = true
		}
	}
	out := rows[:0]
	for i := range rows {
		r := rows[i]
		if isCopilotOutputShadow(r) && fullOutputs[r.sessionID][r.tokens.Output] {
			continue
		}
		out = append(out, r)
	}
	return out
}

// isCopilotOutputShadow reports whether r is a copilot-family output-only token
// row (input=0, no cache, output>0) — the Tier-3 events.jsonl capture that
// duplicates a full-usage row's output. The tool strings mirror
// models.ToolCopilot / models.ToolCopilotCLI (literals here to keep the cost
// package free of a models import, matching the dashboard CTE's literals).
func isCopilotOutputShadow(r rawRow) bool {
	if r.tool != "copilot" && r.tool != "copilot-cli" {
		return false
	}
	return r.tokens.Input == 0 && r.tokens.CacheRead == 0 &&
		r.tokens.CacheCreation == 0 && r.tokens.Output > 0
}

func (e *Engine) loadProxyRows(ctx context.Context, db *sql.DB, opts Options, since time.Time) ([]rawRow, error) {
	q := `SELECT COALESCE(at.model, ''), COALESCE(at.input_tokens, 0),
	             COALESCE(at.output_tokens, 0), COALESCE(at.cache_read_tokens, 0),
	             COALESCE(at.cache_creation_tokens, 0),
	             COALESCE(at.cache_creation_1h_tokens, 0),
	             COALESCE(at.web_search_requests, 0),
	             COALESCE(at.cost_usd, 0),
	             at.timestamp, COALESCE(at.session_id, ''),
	             COALESCE(p.root_path, ''),
	             COALESCE(s.tool, ''),
	             COALESCE(at.compression_original_bytes, 0),
	             COALESCE(at.compression_compressed_bytes, 0),
	             COALESCE(at.compression_count, 0),
	             COALESCE(at.compression_dropped_count, 0),
	             COALESCE(at.compression_marker_count, 0),
	             COALESCE(at.request_id, ''),
	             COALESCE(at.total_response_ms, 0),
	             COALESCE(at.fast, 0)
	      FROM api_turns at
	      LEFT JOIN projects p ON p.id = at.project_id
	      LEFT JOIN sessions s ON s.id = at.session_id`
	var where []string
	var args []any
	if !since.IsZero() {
		where = append(where, "at.timestamp >= ?")
		args = append(args, since.UTC().Format(time.RFC3339Nano))
	}
	if !opts.Until.IsZero() {
		where = append(where, "at.timestamp < ?")
		args = append(args, opts.Until.UTC().Format(time.RFC3339Nano))
	}
	if opts.ProjectRoot != "" {
		where = append(where, "p.root_path = ?")
		args = append(args, opts.ProjectRoot)
	}
	if opts.Tool != "" {
		where = append(where, "s.tool = ?")
		args = append(args, opts.Tool)
	}
	if len(opts.SessionIDs) > 0 {
		ph := strings.TrimRight(strings.Repeat("?,", len(opts.SessionIDs)), ",")
		where = append(where, "at.session_id IN ("+ph+")")
		for _, id := range opts.SessionIDs {
			args = append(args, id)
		}
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("cost.Summary: proxy query: %w", err)
	}
	defer rows.Close()
	var out []rawRow
	for rows.Next() {
		var r rawRow
		var fastInt int
		if err := rows.Scan(
			&r.model,
			&r.tokens.Input, &r.tokens.Output,
			&r.tokens.CacheRead, &r.tokens.CacheCreation,
			&r.tokens.CacheCreation1h,
			&r.tokens.WebSearchRequests,
			&r.recordedUSD, &r.ts, &r.sessionID, &r.projectPath, &r.tool,
			&r.compression.OriginalBytes, &r.compression.CompressedBytes,
			&r.compression.CompressedCount, &r.compression.DroppedCount,
			&r.compression.MarkerCount,
			&r.turnID,
			&r.latencyMS,
			&fastInt,
		); err != nil {
			return nil, fmt.Errorf("cost.Summary: proxy scan: %w", err)
		}
		r.tokens.Fast = fastInt != 0
		r.source = "proxy"
		r.reliability = "accurate"
		if r.compression.OriginalBytes > 0 || r.compression.CompressedBytes > 0 {
			r.compression.Turns = 1
		}
		if isNoiseRow(r) {
			continue
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cost.Summary: proxy rows: %w", err)
	}
	return out, nil
}

func (e *Engine) loadJSONLRows(ctx context.Context, db *sql.DB, opts Options, since time.Time) ([]rawRow, error) {
	q := `SELECT COALESCE(tu.model, ''), COALESCE(tu.input_tokens, 0),
	             COALESCE(tu.output_tokens, 0), COALESCE(tu.cache_read_tokens, 0),
	             COALESCE(tu.cache_creation_tokens, 0),
	             COALESCE(tu.cache_creation_1h_tokens, 0),
	             COALESCE(tu.reasoning_tokens, 0),
	             COALESCE(tu.web_search_requests, 0),
	             COALESCE(tu.estimated_cost_usd, 0),
	             tu.timestamp, tu.session_id,
	             COALESCE(p.root_path, ''), tu.tool,
	             COALESCE(tu.reliability, 'unknown'),
	             COALESCE(tu.source_event_id, ''),
	             COALESCE(tu.fast, 0)
	      FROM token_usage tu
	      LEFT JOIN sessions s ON s.id = tu.session_id
	      LEFT JOIN projects p ON p.id = s.project_id`
	var where []string
	var args []any
	if !since.IsZero() {
		where = append(where, "tu.timestamp >= ?")
		args = append(args, since.UTC().Format(time.RFC3339Nano))
	}
	if !opts.Until.IsZero() {
		where = append(where, "tu.timestamp < ?")
		args = append(args, opts.Until.UTC().Format(time.RFC3339Nano))
	}
	if opts.ProjectRoot != "" {
		where = append(where, "p.root_path = ?")
		args = append(args, opts.ProjectRoot)
	}
	if opts.Tool != "" {
		where = append(where, "s.tool = ?")
		args = append(args, opts.Tool)
	}
	if len(opts.SessionIDs) > 0 {
		ph := strings.TrimRight(strings.Repeat("?,", len(opts.SessionIDs)), ",")
		where = append(where, "tu.session_id IN ("+ph+")")
		for _, id := range opts.SessionIDs {
			args = append(args, id)
		}
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("cost.Summary: jsonl query: %w", err)
	}
	defer rows.Close()
	var out []rawRow
	for rows.Next() {
		var r rawRow
		var fastInt int
		if err := rows.Scan(
			&r.model,
			&r.tokens.Input, &r.tokens.Output,
			&r.tokens.CacheRead, &r.tokens.CacheCreation,
			&r.tokens.CacheCreation1h,
			&r.tokens.Reasoning,
			&r.tokens.WebSearchRequests,
			&r.recordedUSD, &r.ts, &r.sessionID, &r.projectPath, &r.tool,
			&r.reliability,
			&r.turnID,
			&fastInt,
		); err != nil {
			return nil, fmt.Errorf("cost.Summary: jsonl scan: %w", err)
		}
		r.tokens.Fast = fastInt != 0
		r.source = "jsonl"
		if isNoiseRow(r) {
			continue
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cost.Summary: jsonl rows: %w", err)
	}
	return out, nil
}

// loadSummaryCallRows reads the D20 summary_calls ledger — Anthropic
// Haiku calls that the rolling-summarisation summariser fires direct
// to api.anthropic.com (not through the proxy). Each row produces one
// rawRow with source = "summary_calls" and tool = "rolling_summary"
// so per-tool aggregations naturally surface this internal spend as a
// separate line. recordedUSD is taken verbatim from the ledger's
// pre-priced cost_usd column.
//
// Skipped silently when summary_calls doesn't exist (legacy DB, before
// migration 016). Other failures still surface so genuine bugs aren't
// hidden.
func (e *Engine) loadSummaryCallRows(ctx context.Context, db *sql.DB, opts Options, since time.Time) ([]rawRow, error) {
	q := `SELECT COALESCE(model, ''),
	             COALESCE(input_tokens, 0),
	             COALESCE(output_tokens, 0),
	             COALESCE(cache_read_tokens, 0),
	             COALESCE(cache_creation_tokens, 0),
	             COALESCE(cost_usd, 0),
	             timestamp,
	             COALESCE(session_id, '')
	      FROM summary_calls`
	var where []string
	var args []any
	if !since.IsZero() {
		where = append(where, "timestamp >= ?")
		args = append(args, since.UTC().Format(time.RFC3339Nano))
	}
	if !opts.Until.IsZero() {
		where = append(where, "timestamp < ?")
		args = append(args, opts.Until.UTC().Format(time.RFC3339Nano))
	}
	if len(opts.SessionIDs) > 0 {
		ph := strings.TrimRight(strings.Repeat("?,", len(opts.SessionIDs)), ",")
		where = append(where, "session_id IN ("+ph+")")
		for _, id := range opts.SessionIDs {
			args = append(args, id)
		}
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		// Fresh installs without migration 016 (or schemas where the
		// table was never created) shouldn't fail the whole cost
		// summary — degrade gracefully and return no rows.
		if strings.Contains(err.Error(), "no such table") {
			return nil, nil
		}
		return nil, fmt.Errorf("cost.Summary: summary_calls query: %w", err)
	}
	defer rows.Close()
	var out []rawRow
	for rows.Next() {
		var r rawRow
		if err := rows.Scan(
			&r.model,
			&r.tokens.Input, &r.tokens.Output,
			&r.tokens.CacheRead, &r.tokens.CacheCreation,
			&r.recordedUSD, &r.ts, &r.sessionID,
		); err != nil {
			return nil, fmt.Errorf("cost.Summary: summary_calls scan: %w", err)
		}
		r.source = "summary_calls"
		r.reliability = "accurate"
		// Surface as its own tool so per-tool grouping makes the
		// observer-initiated spend visible without confusing it with
		// real user turns.
		r.tool = "observer-rolling-summary"
		if isNoiseRow(r) {
			continue
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cost.Summary: summary_calls rows: %w", err)
	}
	return out, nil
}

// bucket holds the in-progress aggregation for one group key.
type bucket struct {
	tokens    TokenBundle
	cost      float64
	aiCost    float64
	toolCost  float64
	turnCount int
	// V3-5: cumulative proxy-observed latency. latencyMS sums the
	// non-zero per-turn values; latencyTurnCount counts contributors
	// (proxy rows only — JSONL rows carry 0 and are excluded). Row
	// emits the average as AvgLatencyMS.
	latencyMSSum     int64
	latencyTurnCount int
	sources          map[string]bool
	reliabilities    map[string]bool
	unknownModels    map[string]bool
	// pricingSources tracks which match path the pricing table took
	// for each row contributing to this bucket: PricingSourceExact /
	// DateStripped / Family / Miss. The dashboard surfaces a "~"
	// indicator on the Reliability column when at least one row in
	// the bucket priced via Family or Miss — see audit Phase C.
	pricingSources map[PricingSource]bool
	compression    CompressionStats
	// fastTurnCount / fastCost accumulate the subset of this bucket's
	// turns served in the provider's low-latency "fast" tier (Anthropic
	// Opus 4.8 speed:"fast"). fastCost uses the same per-row cost the
	// bucket total used, so it already reflects the FastMultiplier
	// premium. Surfaced on Row so the dashboard can badge fast-tier spend.
	fastTurnCount int
	fastCost      float64
}

func (e *Engine) rollup(raws []rawRow, opts Options) Summary {
	buckets := map[string]*bucket{}
	order := []string{}

	totalTokens := TokenBundle{}
	totalCost := 0.0
	totalTurns := 0
	totalFastTurns := 0
	totalFastCost := 0.0
	totalReliability := map[string]bool{}
	totalUnknowns := map[string]bool{}
	var unpricedTokens int64
	var unpricedTurnCount int
	totalCompression := CompressionStats{}

	for _, r := range raws {
		key := groupKey(r, opts.GroupBy)
		b, ok := buckets[key]
		if !ok {
			b = &bucket{
				sources:        map[string]bool{},
				reliabilities:  map[string]bool{},
				unknownModels:  map[string]bool{},
				pricingSources: map[PricingSource]bool{},
			}
			buckets[key] = b
			order = append(order, key)
		}
		b.tokens.Add(r.tokens)
		b.turnCount++
		// V3-5: only proxy rows carry observed latency (JSONL adapter
		// path doesn't see request → response wall time). latencyMS == 0
		// is skipped — both 'JSONL row' AND 'proxy row with no
		// recorded latency' produce zero, which would skew the average
		// downward.
		if r.latencyMS > 0 {
			b.latencyMSSum += r.latencyMS
			b.latencyTurnCount++
		}
		b.sources[r.source] = true
		b.reliabilities[r.reliability] = true
		totalReliability[r.reliability] = true

		// Cost: when the row has a recorded estimated_cost_usd (only
		// OpenCode + Pi adapters set this today — they compute cost
		// client-side), use it as-is. Otherwise compute from pricing
		// and track which pricing-table match path was hit.
		//
		// Recorded costs land in AICost (the upstream client doesn't
		// separate tool fees) — those adapters don't model web_search
		// today, so the merge stays correct. Computed costs use
		// ComputeBreakdown to split AI vs tool portions cleanly.
		cost := r.recordedUSD
		var aiCost, toolCost float64
		if cost == 0 {
			pricing, src, ok := e.LookupWithSource(r.model)
			if !ok {
				b.unknownModels[r.model] = true
				totalUnknowns[r.model] = true
				b.pricingSources[PricingSourceMiss] = true
				unpricedTokens += r.tokens.Input + r.tokens.Output
				unpricedTurnCount++
			} else {
				cb := ComputeBreakdown(pricing, r.tokens)
				cost = cb.Total
				aiCost = cb.AICost
				toolCost = cb.ToolCost
				b.pricingSources[src] = true
			}
		} else {
			// Recorded cost — pricing table wasn't consulted, but the
			// row still came with a known model. Mark the pricing path
			// as "exact" since the upstream client (which had to know
			// the model's rates to compute it) effectively certified
			// the rate.
			b.pricingSources[PricingSourceExact] = true
			aiCost = cost
		}
		b.cost += cost
		b.aiCost += aiCost
		b.toolCost += toolCost
		// Fast-tier subtotal: same per-row cost the bucket total used, so it
		// already carries the FastMultiplier premium. Gated on the model
		// actually having a fast-mode premium (FastMultiplier > 0): Codex
		// sends service_tier:"priority" globally so every codex row carries
		// fast=1, but only gpt-5.5 / gpt-5.4 (and Opus 4.8) bill a premium.
		// Counting mini/codex priority turns here would inflate the
		// fast-tier turn count + spend with turns that cost the standard
		// rate. The pill (service_tier on the action row) still surfaces the
		// requested tier for those models.
		if r.tokens.Fast {
			if p, ok := e.Lookup(r.model); ok && p.FastMultiplier > 0 {
				b.fastTurnCount++
				b.fastCost += cost
				totalFastTurns++
				totalFastCost += cost
			}
		}
		// Derive per-row token & $ savings before aggregating. Skip when
		// no compression data was recorded on the row.
		if r.compression.OriginalBytes != 0 || r.compression.CompressedBytes != 0 || r.compression.Turns != 0 {
			savedBytes := r.compression.OriginalBytes - r.compression.CompressedBytes
			// V7-18 clip: when the conversation pipeline runs in pass-
			// through mode (codex-variant recipe with compress_types=[]),
			// per-turn JSON re-marshaling + cache_control envelope
			// overhead can make CompressedBytes > OriginalBytes. The raw
			// bytes are preserved on the row so operators can see the
			// wire inflation, but derived savings clip to zero — a
			// negative "tokens saved" estimate has no operational
			// meaning and misleads operators reading observer cost.
			if savedBytes < 0 {
				savedBytes = 0
			}
			r.compression.TokensSavedEst = savedBytes / 4 // ~4 chars/token
			if pricing, ok := e.Lookup(r.model); ok && pricing.Input > 0 {
				// V7-6 weighted blend: pre-v1.7.6 cost_saved_usd_est
				// used pricing.Input flat, which overstated codex
				// savings ~10× because codex sessions are cache_read-
				// dominant. The main column now reflects the row's
				// realized input-side mix; the two tier columns
				// expose the upper bound (input tier, the prior
				// behavior) and lower bound (cache_read tier) for
				// operators who want the explicit bounds. See
				// docs/v4-codex-compression-recipe-and-issues.md V7-6.
				tokens := float64(r.compression.TokensSavedEst)
				r.compression.CostSavedUSDEstInputTier = tokens * pricing.Input / 1_000_000
				cacheReadRate := pricing.CacheRead
				if cacheReadRate == 0 {
					// Pricing tables without an explicit cache_read
					// rate fall back to Pricing.normalize's 10%-of-
					// input default (pricing.go:187). If even that
					// didn't fire (e.g. zero pricing.Input slipped
					// past), use pricing.Input so the lower-bound
					// column never exceeds the upper bound.
					cacheReadRate = pricing.Input * 0.10
				}
				r.compression.CostSavedUSDEstCacheReadTier = tokens * cacheReadRate / 1_000_000
				inputSlot := r.tokens.Input + r.tokens.CacheRead + r.tokens.CacheCreation
				if inputSlot > 0 {
					inputShare := float64(r.tokens.Input+r.tokens.CacheCreation) / float64(inputSlot)
					cacheShare := float64(r.tokens.CacheRead) / float64(inputSlot)
					weighted := inputShare*pricing.Input + cacheShare*cacheReadRate
					r.compression.CostSavedUSDEst = tokens * weighted / 1_000_000
				} else {
					// No input-side tokens to weight against — fall
					// back to the upper-bound (input-tier) value so
					// the column stays consistent with pre-v1.7.6
					// behavior for tokenless rows.
					r.compression.CostSavedUSDEst = r.compression.CostSavedUSDEstInputTier
				}
			}
		}
		b.compression.Add(r.compression)

		totalTokens.Add(r.tokens)
		totalCost += cost
		totalTurns++
		totalCompression.Add(r.compression)
	}

	rowsOut := make([]Row, 0, len(order))
	for _, key := range order {
		b := buckets[key]
		var avgLatency int64
		if b.latencyTurnCount > 0 {
			avgLatency = b.latencyMSSum / int64(b.latencyTurnCount)
		}
		rowsOut = append(rowsOut, Row{
			Key:           key,
			Tokens:        b.tokens,
			CostUSD:       b.cost,
			AICostUSD:     b.aiCost,
			ToolCostUSD:   b.toolCost,
			TurnCount:     b.turnCount,
			AvgLatencyMS:  avgLatency,
			Source:        collapseSources(b.sources),
			Reliability:   weakestReliability(b.reliabilities),
			UnknownModels: sortedKeys(b.unknownModels),
			PricingSource: collapsePricingSources(b.pricingSources),
			Compression:   b.compression,
			FastTurnCount: b.fastTurnCount,
			FastCostUSD:   b.fastCost,
		})
	}
	sort.SliceStable(rowsOut, func(i, j int) bool {
		if rowsOut[i].CostUSD != rowsOut[j].CostUSD {
			return rowsOut[i].CostUSD > rowsOut[j].CostUSD
		}
		return rowsOut[i].Tokens.Input+rowsOut[i].Tokens.Output >
			rowsOut[j].Tokens.Input+rowsOut[j].Tokens.Output
	})
	if len(rowsOut) > opts.Limit {
		rowsOut = rowsOut[:opts.Limit]
	}

	return Summary{
		GroupBy:           opts.GroupBy,
		Source:            opts.Source,
		Rows:              rowsOut,
		TotalTokens:       totalTokens,
		TotalCost:         totalCost,
		TurnCount:         totalTurns,
		FastTurnCount:     totalFastTurns,
		TotalFastCostUSD:  totalFastCost,
		Reliability:       weakestReliability(totalReliability),
		UnknownModelCount: len(totalUnknowns),
		UnpricedTokens:    unpricedTokens,
		UnpricedTurnCount: unpricedTurnCount,
		TotalCompression:  totalCompression,
	}
}

func groupKey(r rawRow, g GroupBy) string {
	switch g {
	case GroupBySession:
		if r.sessionID == "" {
			return "<unattributed>"
		}
		return r.sessionID
	case GroupByDay:
		if len(r.ts) >= 10 {
			return r.ts[:10]
		}
		return r.ts
	case GroupByDayModel:
		day := r.ts
		if len(r.ts) >= 10 {
			day = r.ts[:10]
		}
		model := r.model
		if model == "" {
			model = "<unknown>"
		}
		return day + dayModelKeySep + model
	case GroupByDayProject:
		day := r.ts
		if len(r.ts) >= 10 {
			day = r.ts[:10]
		}
		proj := r.projectPath
		if proj == "" {
			proj = "<no-project>"
		}
		return day + dayModelKeySep + proj
	case GroupByDayTool:
		day := r.ts
		if len(r.ts) >= 10 {
			day = r.ts[:10]
		}
		tool := r.tool
		if tool == "" {
			tool = "<no-tool>"
		}
		return day + dayModelKeySep + tool
	case GroupByProject:
		if r.projectPath == "" {
			return "<no-project>"
		}
		return r.projectPath
	case GroupByTool:
		if r.tool == "" {
			return "<no-tool>"
		}
		return r.tool
	case GroupByNone:
		return "all"
	default: // GroupByModel
		if r.model == "" {
			return "<unknown>"
		}
		return r.model
	}
}

func collapseSources(set map[string]bool) string {
	if len(set) == 1 {
		for k := range set {
			return k
		}
	}
	return "mixed"
}

// collapsePricingSources reduces a bucket's PricingSource set to a single
// label for the dashboard. Empty set → "" (no rows priced, e.g. all
// recorded). Single tag → that tag. Mixed → "mixed". The dashboard
// renders a "~" indicator unless the result is "exact" or "" (since
// the table-recorded path is also exact-equivalent).
func collapsePricingSources(set map[PricingSource]bool) string {
	if len(set) == 0 {
		return ""
	}
	if len(set) == 1 {
		for k := range set {
			return string(k)
		}
	}
	return "mixed"
}

// weakestReliability picks the weakest tag present — the number should be
// trusted only as much as the least-trustworthy input allows.
func weakestReliability(set map[string]bool) string {
	order := []string{"unknown", "unreliable", "approximate", "accurate"}
	for _, tag := range order {
		if set[tag] {
			return tag
		}
	}
	return "unknown"
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
