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
type Source string

const (
	SourceProxy Source = "proxy"
	SourceJSONL Source = "jsonl"
	SourceAuto  Source = "auto"
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
	// Limit caps returned rows (after sort by cost_usd desc). Zero means 50.
	Limit int
	// Now overrides time.Now for deterministic tests.
	Now func() time.Time
}

// Row is one grouped summary entry.
type Row struct {
	Key     string      `json:"key"`
	Tokens  TokenBundle `json:"tokens"`
	CostUSD float64     `json:"cost_usd"`
	// TurnCount is how many underlying rows (api_turns + token_usage) fed
	// this group.
	TurnCount int `json:"turn_count"`
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
	// CostSavedUSDEst is TokensSavedEst valued at the row's model input
	// rate (compressed/dropped content is prompt context). Signed.
	CostSavedUSDEst float64 `json:"cost_saved_usd_est"`
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
	Reliability string      `json:"reliability"`
	// UnknownModelCount is the number of distinct unknown models seen.
	UnknownModelCount int `json:"unknown_model_count,omitempty"`
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
		r.tokens.CacheRead == 0 && r.tokens.CacheCreation == 0
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

// fingerprint is kept as an alias to shapeKey for tests that still
// reference it directly. Internal callers should prefer shapeKey.
func fingerprint(r rawRow) string { return shapeKey(r) }

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

func (e *Engine) loadRows(ctx context.Context, db *sql.DB, opts Options, since time.Time) ([]rawRow, error) {
	var out []rawRow

	if opts.Source == SourceProxy || opts.Source == SourceAuto {
		proxy, err := e.loadProxyRows(ctx, db, opts, since)
		if err != nil {
			return nil, err
		}
		out = append(out, proxy...)
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
			//   1. turnID — both api_turns.request_id and the JSONL
			//      adapter's source_event_id store the upstream message
			//      id (msg_xxx for Anthropic, completion id for OpenAI),
			//      so a per-turn match is exact when both sides set it.
			//
			//   2. (session_id, shape) — when neither side has turnID,
			//      match by session + shape so two rows in the same
			//      session at the same minute with the same token
			//      bundle are treated as the same turn. Different
			//      sessions never collide.
			//
			//   3. shape-only against proxy rows with EMPTY session_id —
			//      legacy A2 case: pre-v1.2.1 the proxy didn't always
			//      record a session_id, but the JSONL adapter did.
			//      Without this fallback those rows double-count. Only
			//      proxy-side empty-session rows feed this map; if the
			//      JSONL row's session matches the same shape it gets
			//      deduped against the orphan proxy row.
			proxyTurns := map[string]bool{}
			proxySessionShapes := map[string]bool{} // sessionID + "\x00" + shapeKey
			proxyOrphanShapes := map[string]bool{}  // shape-only, only for proxy rows with empty sessionID
			for _, r := range out {
				if r.turnID != "" {
					proxyTurns[r.turnID] = true
					continue
				}
				key := shapeKey(r)
				if r.sessionID == "" {
					proxyOrphanShapes[key] = true
				} else {
					proxySessionShapes[r.sessionID+"\x00"+key] = true
				}
			}
			for _, r := range jsonl {
				if r.turnID != "" && proxyTurns[r.turnID] {
					continue
				}
				key := shapeKey(r)
				if r.sessionID != "" && proxySessionShapes[r.sessionID+"\x00"+key] {
					continue
				}
				if len(proxyOrphanShapes) > 0 && proxyOrphanShapes[key] {
					continue
				}
				out = append(out, r)
			}
		} else {
			out = append(out, jsonl...)
		}
	}
	return out, nil
}

func (e *Engine) loadProxyRows(ctx context.Context, db *sql.DB, opts Options, since time.Time) ([]rawRow, error) {
	q := `SELECT COALESCE(at.model, ''), COALESCE(at.input_tokens, 0),
	             COALESCE(at.output_tokens, 0), COALESCE(at.cache_read_tokens, 0),
	             COALESCE(at.cache_creation_tokens, 0),
	             COALESCE(at.cache_creation_1h_tokens, 0),
	             COALESCE(at.cost_usd, 0),
	             at.timestamp, COALESCE(at.session_id, ''),
	             COALESCE(p.root_path, ''),
	             COALESCE(s.tool, ''),
	             COALESCE(at.compression_original_bytes, 0),
	             COALESCE(at.compression_compressed_bytes, 0),
	             COALESCE(at.compression_count, 0),
	             COALESCE(at.compression_dropped_count, 0),
	             COALESCE(at.compression_marker_count, 0),
	             COALESCE(at.request_id, '')
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
		if err := rows.Scan(
			&r.model,
			&r.tokens.Input, &r.tokens.Output,
			&r.tokens.CacheRead, &r.tokens.CacheCreation,
			&r.tokens.CacheCreation1h,
			&r.recordedUSD, &r.ts, &r.sessionID, &r.projectPath, &r.tool,
			&r.compression.OriginalBytes, &r.compression.CompressedBytes,
			&r.compression.CompressedCount, &r.compression.DroppedCount,
			&r.compression.MarkerCount,
			&r.turnID,
		); err != nil {
			return nil, fmt.Errorf("cost.Summary: proxy scan: %w", err)
		}
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
	             COALESCE(tu.estimated_cost_usd, 0),
	             tu.timestamp, tu.session_id,
	             COALESCE(p.root_path, ''), tu.tool,
	             COALESCE(tu.reliability, 'unknown'),
	             COALESCE(tu.source_event_id, '')
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
		if err := rows.Scan(
			&r.model,
			&r.tokens.Input, &r.tokens.Output,
			&r.tokens.CacheRead, &r.tokens.CacheCreation,
			&r.tokens.CacheCreation1h,
			&r.recordedUSD, &r.ts, &r.sessionID, &r.projectPath, &r.tool,
			&r.reliability,
			&r.turnID,
		); err != nil {
			return nil, fmt.Errorf("cost.Summary: jsonl scan: %w", err)
		}
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

// bucket holds the in-progress aggregation for one group key.
type bucket struct {
	tokens        TokenBundle
	cost          float64
	turnCount     int
	sources       map[string]bool
	reliabilities map[string]bool
	unknownModels map[string]bool
	// pricingSources tracks which match path the pricing table took
	// for each row contributing to this bucket: PricingSourceExact /
	// DateStripped / Family / Miss. The dashboard surfaces a "~"
	// indicator on the Reliability column when at least one row in
	// the bucket priced via Family or Miss — see audit Phase C.
	pricingSources map[PricingSource]bool
	compression    CompressionStats
}

func (e *Engine) rollup(raws []rawRow, opts Options) Summary {
	buckets := map[string]*bucket{}
	order := []string{}

	totalTokens := TokenBundle{}
	totalCost := 0.0
	totalTurns := 0
	totalReliability := map[string]bool{}
	totalUnknowns := map[string]bool{}
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
		b.sources[r.source] = true
		b.reliabilities[r.reliability] = true
		totalReliability[r.reliability] = true

		// Cost: when the row has a recorded estimated_cost_usd (only
		// OpenCode + Pi adapters set this today — they compute cost
		// client-side), use it as-is. Otherwise compute from pricing
		// and track which pricing-table match path was hit.
		cost := r.recordedUSD
		if cost == 0 {
			pricing, src, ok := e.LookupWithSource(r.model)
			if !ok {
				b.unknownModels[r.model] = true
				totalUnknowns[r.model] = true
				b.pricingSources[PricingSourceMiss] = true
			} else {
				cost = Compute(pricing, r.tokens)
				b.pricingSources[src] = true
			}
		} else {
			// Recorded cost — pricing table wasn't consulted, but the
			// row still came with a known model. Mark the pricing path
			// as "exact" since the upstream client (which had to know
			// the model's rates to compute it) effectively certified
			// the rate.
			b.pricingSources[PricingSourceExact] = true
		}
		b.cost += cost
		// Derive per-row token & $ savings before aggregating. Skip when
		// no compression data was recorded on the row.
		if r.compression.OriginalBytes != 0 || r.compression.CompressedBytes != 0 || r.compression.Turns != 0 {
			savedBytes := r.compression.OriginalBytes - r.compression.CompressedBytes
			r.compression.TokensSavedEst = savedBytes / 4 // ~4 chars/token
			if pricing, ok := e.Lookup(r.model); ok && pricing.Input > 0 {
				r.compression.CostSavedUSDEst = float64(r.compression.TokensSavedEst) * pricing.Input / 1_000_000
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
		rowsOut = append(rowsOut, Row{
			Key:           key,
			Tokens:        b.tokens,
			CostUSD:       b.cost,
			TurnCount:     b.turnCount,
			Source:        collapseSources(b.sources),
			Reliability:   weakestReliability(b.reliabilities),
			UnknownModels: sortedKeys(b.unknownModels),
			PricingSource: collapsePricingSources(b.pricingSources),
			Compression:   b.compression,
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
		Reliability:       weakestReliability(totalReliability),
		UnknownModelCount: len(totalUnknowns),
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
