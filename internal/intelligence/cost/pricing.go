package cost

import (
	"regexp"
	"strings"
)

// Pricing is USD per 1M tokens for one model. Zero fields are treated as
// "unknown" rather than "free" — Compute returns ok=false when the looked-up
// pricing has zero Input AND zero Output.
//
// CacheCreation is the 5-minute ephemeral cache-write rate (Anthropic's
// default cache duration). CacheCreation1h is the 1-hour ephemeral tier.
// Anthropic is the only provider with a separate cache-write tier; for
// OpenAI / Gemini / xAI / Moonshot / Cursor entries leave both at 0.
//
// fillDefaults supplies these defaults when the entry leaves them blank:
//
//	CacheRead       = 0.10 × Input            (universal — every provider has cache-read)
//	CacheCreation   = (no default, stays 0)   (Anthropic-only; non-Anthropic stays 0)
//	CacheCreation1h = 2.00 × Input            (only when CacheCreation > 0)
//
// See docs/pricing-reference.md for the full table. Pre-2026-04-29 the 1h
// default was 2 × CacheCreation = 2.5 × Input (25% over) AND CacheCreation
// was unconditionally defaulted to 1.25 × Input (which would over-bill
// non-Anthropic rows that ever carried cache_creation_tokens). Both fixed
// in v1.4.12 / v1.4.14.
//
// LongContextThreshold and the LongContext* fields model providers that
// reprice an entire request at a higher tier when the prompt is large.
// Anthropic Sonnet 4 / 4.5 reprice above 200K input tokens; OpenAI
// gpt-5.4 / gpt-5.5 above 272K; Gemini 2.5 Pro / 3.1 Pro Preview above
// 200K. Threshold is compared against (Input + CacheRead + CacheCreation)
// at Compute time — that's the full prompt window the provider sees,
// including the cached portion. When Threshold is zero the entry has no
// LC tier and standard rates always apply. Each LongContext* rate falls
// back to its standard counterpart when zero, so an entry can override
// only the dimensions that actually change at the LC tier.
type Pricing struct {
	Input           float64 `json:"input"`
	Output          float64 `json:"output"`
	CacheRead       float64 `json:"cache_read"`
	CacheCreation   float64 `json:"cache_creation"`
	CacheCreation1h float64 `json:"cache_creation_1h"`

	LongContextThreshold       int64   `json:"long_context_threshold,omitempty"`
	LongContextInput           float64 `json:"long_context_input,omitempty"`
	LongContextOutput          float64 `json:"long_context_output,omitempty"`
	LongContextCacheRead       float64 `json:"long_context_cache_read,omitempty"`
	LongContextCacheCreation   float64 `json:"long_context_cache_creation,omitempty"`
	LongContextCacheCreation1h float64 `json:"long_context_cache_creation_1h,omitempty"`
}

// Table maps normalized model IDs to Pricing. Lookup is exact first; on miss,
// the date suffix (e.g. "-20250514") is stripped and retried; on miss again,
// family prefixes ("claude-sonnet-4", "gpt-4o") are tried. The zero value is
// a usable empty table.
type Table struct {
	exact map[string]Pricing
}

// NewTable seeds a Table with the baked-in defaults from spec §24 and public
// pricing as of 2026-04. Callers should then Merge() user overrides from
// config.toml on top.
func NewTable() *Table {
	t := &Table{exact: map[string]Pricing{}}
	for k, v := range defaultPricing {
		t.exact[k] = v
	}
	return t
}

// Merge copies overrides into t. Existing entries with the same key are
// replaced wholesale — partial overrides should load the current pricing and
// patch before calling Merge.
func (t *Table) Merge(overrides map[string]Pricing) {
	if t.exact == nil {
		t.exact = map[string]Pricing{}
	}
	for k, v := range overrides {
		t.exact[k] = v
	}
}

// PricingSource categorises how a Lookup resolved a model id, so callers
// can flag rows priced via fallback rather than an exact-match entry. The
// dashboard surfaces a "~" badge for non-exact rows so users can see which
// numbers came from the family prefix and which came from a baked-in rate.
type PricingSource string

const (
	// PricingSourceExact: the model id matched a table entry verbatim.
	PricingSourceExact PricingSource = "exact"
	// PricingSourceDateStripped: a date suffix (-YYYYMMDD) was stripped
	// to find a match. Same family + version, just an undated alias.
	PricingSourceDateStripped PricingSource = "date-stripped"
	// PricingSourceFamily: matched via longest-prefix family lookup
	// (e.g. claude-opus-4-7 → claude-opus-4 family rates). The exact
	// SKU isn't in our table, so the rate is inferred from the family.
	PricingSourceFamily PricingSource = "family"
	// PricingSourceMiss: nothing matched. Caller should treat as $0
	// and tag the row reliability as "unknown".
	PricingSourceMiss PricingSource = "miss"
)

// Lookup returns pricing for the given model id. When the exact id is absent,
// it tries the date-stripped variant, then family prefixes. Returns ok=false
// only when nothing matches.
func (t *Table) Lookup(model string) (Pricing, bool) {
	p, _, ok := t.LookupWithSource(model)
	return p, ok
}

// LookupWithSource is the source-aware variant of Lookup. The second
// return value is a PricingSource describing how the match resolved
// (exact / date-stripped / family) so callers can surface a fallback
// indicator. Match precedence is identical to Lookup.
func (t *Table) LookupWithSource(model string) (Pricing, PricingSource, bool) {
	if t == nil || t.exact == nil || model == "" {
		return Pricing{}, PricingSourceMiss, false
	}
	if p, ok := t.exact[model]; ok {
		return fillDefaults(p), PricingSourceExact, true
	}
	stripped := stripDateSuffix(model)
	if stripped != model {
		if p, ok := t.exact[stripped]; ok {
			return fillDefaults(p), PricingSourceDateStripped, true
		}
	}
	// Longest-prefix fallback: try progressively shorter family prefixes.
	// e.g. "claude-opus-4-1-20250805" → "claude-opus-4-1" → "claude-opus-4"
	// → "claude-opus".
	lower := strings.ToLower(model)
	for _, family := range familyKeys(t.exact) {
		if strings.HasPrefix(lower, family) {
			return fillDefaults(t.exact[family]), PricingSourceFamily, true
		}
	}
	return Pricing{}, PricingSourceMiss, false
}

// Known returns the set of exact model IDs in the table. Intended for
// debugging (`observer cost --debug-pricing`).
func (t *Table) Known() []string {
	if t == nil {
		return nil
	}
	out := make([]string, 0, len(t.exact))
	for k := range t.exact {
		out = append(out, k)
	}
	return out
}

// fillDefaults applies the cache-tier defaults when the caller left them
// zero. This only runs at Lookup time so tests can assert Merge() round-trips
// exactly what was provided.
//
// CacheRead default (10% × Input) applies universally — every provider
// has a cache-read concept (cached_input on OpenAI, context-cache on
// Gemini, cache_read on Anthropic) so the default is a safe floor.
//
// CacheCreation defaults (5m and 1h) are Anthropic-specific. We do NOT
// auto-derive them from Input because Anthropic is the only provider
// with a separate cache-write tier — OpenAI bills caching automatically
// without a write charge, Gemini bills storage by time. Pre-2026-04-29
// fillDefaults set CacheCreation = 1.25 × Input unconditionally, which
// silently inflated rates for any non-Anthropic row that happened to
// carry cache_creation_tokens (a hypothetical adapter bug). After this
// fix:
//
//   - CacheCreation stays zero unless explicitly set; non-Anthropic
//     entries can never accidentally inherit Anthropic-style defaults.
//   - CacheCreation1h is defaulted to 2 × Input ONLY when CacheCreation
//     is explicit non-zero — i.e. the entry is Anthropic-shape. This
//     preserves the v1.4.12 fix where a custom Anthropic config that
//     sets only 5m can still get a sensible 1h rate.
func fillDefaults(p Pricing) Pricing {
	if p.CacheRead == 0 && p.Input > 0 {
		p.CacheRead = p.Input * 0.10
	}
	if p.CacheCreation > 0 && p.CacheCreation1h == 0 && p.Input > 0 {
		// Anthropic-shape entry with explicit 5m rate but missing 1h —
		// default to 2 × Input per Anthropic's published ratios.
		p.CacheCreation1h = p.Input * 2
	}
	return p
}

// dateSuffix matches a trailing "-YYYYMMDD" on a model id.
var dateSuffix = regexp.MustCompile(`-\d{8}$`)

func stripDateSuffix(model string) string {
	return dateSuffix.ReplaceAllString(model, "")
}

// familyKeys returns the exact keys in the table that *look like* family
// prefixes (no date suffix, no version disambiguator longer than 2 chars),
// sorted longest-first so the most-specific family wins. Picked at runtime
// so users can add their own families in config.toml.
func familyKeys(exact map[string]Pricing) []string {
	out := []string{}
	for k := range exact {
		if dateSuffix.MatchString(k) {
			continue
		}
		out = append(out, strings.ToLower(k))
	}
	// Sort longest-first: a simple length-desc sort is enough because
	// "claude-opus-4-1" beats "claude-opus" without alphabetic ordering.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if len(out[j]) > len(out[i]) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// BakedInDefaults returns a fresh copy of the baked-in pricing table.
// The dashboard's Settings page uses this to render a "defaults"
// reference list alongside the user's overrides — clicking a default
// pre-fills an override row with that model's baked-in rates so users
// can tweak from a known-good starting point.
//
// The returned map is a copy; mutating it has no effect on engine state.
func BakedInDefaults() map[string]Pricing {
	out := make(map[string]Pricing, len(defaultPricing))
	for k, v := range defaultPricing {
		out[k] = v
	}
	return out
}

// defaultPricing is the baked-in pricing table. Values are USD per 1M tokens.
// Source-of-truth: docs/pricing-reference.md (last synced 2026-04-29).
// Users can override any entry via [intelligence.pricing.models."<id>"]
// in config.toml.
//
// Family prefixes (no date suffix) are included so new dated releases
// inherit sensible defaults until a user pins them — but where Anthropic
// has changed pricing within a family (e.g. Opus 4.5+ dropped to 1/3 of
// Opus 4 / 4.1 rates), the family prefix is set to the LATEST rates so
// new releases inherit the current family pricing, not the legacy.
//
// Pre-2026-04-29 this table priced Opus 4.5+ at $15/$75 (legacy Opus 4
// rates) and used CacheCreation1h = 2.5 × Input (vs the actual 2 × Input).
// Combined this over-billed Opus 4.5+ traffic by ~3× and over-billed all
// 1h cache writes by 25%.
var defaultPricing = map[string]Pricing{
	// Anthropic — Claude Opus 4.5 / 4.6 / 4.7 (current generation, $5/$25
	// input/output — 1/3 of Opus 4.x legacy rates).
	"claude-opus-4-7":          {Input: 5, Output: 25, CacheRead: 0.50, CacheCreation: 6.25, CacheCreation1h: 10},
	"claude-opus-4-6":          {Input: 5, Output: 25, CacheRead: 0.50, CacheCreation: 6.25, CacheCreation1h: 10},
	"claude-opus-4-6-20251001": {Input: 5, Output: 25, CacheRead: 0.50, CacheCreation: 6.25, CacheCreation1h: 10},
	"claude-opus-4-5":          {Input: 5, Output: 25, CacheRead: 0.50, CacheCreation: 6.25, CacheCreation1h: 10},
	// claude-opus-4 (no minor) — family prefix set to LATEST so new SKUs
	// (claude-opus-4-8, claude-opus-4-9...) inherit current pricing rather
	// than legacy.
	"claude-opus-4": {Input: 5, Output: 25, CacheRead: 0.50, CacheCreation: 6.25, CacheCreation1h: 10},
	// Anthropic — Claude Opus 4 / 4.1 (legacy rates, $15/$75).
	"claude-opus-4-1":          {Input: 15, Output: 75, CacheRead: 1.5, CacheCreation: 18.75, CacheCreation1h: 30},
	"claude-opus-4-1-20250805": {Input: 15, Output: 75, CacheRead: 1.5, CacheCreation: 18.75, CacheCreation1h: 30},
	"claude-opus-4-20250514":   {Input: 15, Output: 75, CacheRead: 1.5, CacheCreation: 18.75, CacheCreation1h: 30},
	// Anthropic — Sonnet 4 family. Pricing identical across 3.7 / 4 / 4.5 / 4.6.
	// 4.5 + 4 (incl. dated -20250514) carry the 200K long-context tier:
	// $6 input, $22.50 output, $0.60 cache_read, $7.50 cache_write 5m,
	// $12 cache_write 1h. 4.6 is flat-rate per the 2026-04-29 snapshot;
	// 3.7 is deprecated. The "claude-sonnet-4" family prefix carries the
	// LC tier so future undated 4.x SKUs (other than 4-6) inherit it.
	"claude-sonnet-4-6": {Input: 3, Output: 15, CacheRead: 0.30, CacheCreation: 3.75, CacheCreation1h: 6},
	"claude-sonnet-4-5": {
		Input: 3, Output: 15, CacheRead: 0.30, CacheCreation: 3.75, CacheCreation1h: 6,
		LongContextThreshold: 200_000,
		LongContextInput:     6, LongContextOutput: 22.50, LongContextCacheRead: 0.60,
		LongContextCacheCreation: 7.50, LongContextCacheCreation1h: 12,
	},
	"claude-sonnet-4-20250514": {
		Input: 3, Output: 15, CacheRead: 0.30, CacheCreation: 3.75, CacheCreation1h: 6,
		LongContextThreshold: 200_000,
		LongContextInput:     6, LongContextOutput: 22.50, LongContextCacheRead: 0.60,
		LongContextCacheCreation: 7.50, LongContextCacheCreation1h: 12,
	},
	"claude-sonnet-4": {
		Input: 3, Output: 15, CacheRead: 0.30, CacheCreation: 3.75, CacheCreation1h: 6,
		LongContextThreshold: 200_000,
		LongContextInput:     6, LongContextOutput: 22.50, LongContextCacheRead: 0.60,
		LongContextCacheCreation: 7.50, LongContextCacheCreation1h: 12,
	},
	"claude-sonnet-3-7": {Input: 3, Output: 15, CacheRead: 0.30, CacheCreation: 3.75, CacheCreation1h: 6}, // deprecated
	// Anthropic — Haiku 4.5.
	"claude-haiku-4-5-20251001": {Input: 1, Output: 5, CacheRead: 0.10, CacheCreation: 1.25, CacheCreation1h: 2},
	"claude-haiku-4-5":          {Input: 1, Output: 5, CacheRead: 0.10, CacheCreation: 1.25, CacheCreation1h: 2},
	"claude-haiku-4":            {Input: 1, Output: 5, CacheRead: 0.10, CacheCreation: 1.25, CacheCreation1h: 2},
	// Anthropic — Claude 3.5.
	"claude-3-5-sonnet-20241022": {Input: 3, Output: 15, CacheRead: 0.30, CacheCreation: 3.75, CacheCreation1h: 6},
	"claude-3-5-sonnet-20240620": {Input: 3, Output: 15, CacheRead: 0.30, CacheCreation: 3.75, CacheCreation1h: 6},
	"claude-3-5-sonnet":          {Input: 3, Output: 15, CacheRead: 0.30, CacheCreation: 3.75, CacheCreation1h: 6},
	"claude-3-5-haiku-20241022":  {Input: 0.80, Output: 4, CacheRead: 0.08, CacheCreation: 1.00, CacheCreation1h: 1.6},
	"claude-3-5-haiku":           {Input: 0.80, Output: 4, CacheRead: 0.08, CacheCreation: 1.00, CacheCreation1h: 1.6},
	// Anthropic — Claude 3 (deprecated).
	"claude-3-opus-20240229":   {Input: 15, Output: 75, CacheRead: 1.5, CacheCreation: 18.75, CacheCreation1h: 30},
	"claude-3-opus":            {Input: 15, Output: 75, CacheRead: 1.5, CacheCreation: 18.75, CacheCreation1h: 30},
	"claude-3-sonnet-20240229": {Input: 3, Output: 15, CacheRead: 0.30, CacheCreation: 3.75, CacheCreation1h: 6},
	"claude-3-haiku-20240307":  {Input: 0.25, Output: 1.25, CacheRead: 0.03, CacheCreation: 0.30, CacheCreation1h: 0.50},
	// OpenAI — GPT-5 family. Cached input → CacheRead. No separate
	// cache-write charge (caching is automatic) so CacheCreation /
	// CacheCreation1h stay 0. The post-2026-04-29 fillDefaults
	// changes don't fold Anthropic-style cache_write defaults in
	// when CacheCreation is left zero, so OpenAI rows can never
	// over-bill via a phantom 1.25×Input rate even if a stray
	// cache_creation_tokens leaked into a row.
	//
	// 5.5 / 5.4 introduce ≤272K vs >272K long-context tiers (entire
	// request reprices when crossed at 2× across input/output/cache_read).
	// The Pro variants don't advertise a separate LC tier so they stay
	// flat. mini/nano lack public LC numbers; left at standard rates and
	// users on >272K mini/nano prompts will see slight under-billing.
	"gpt-5.5": {
		Input: 5, Output: 30, CacheRead: 0.50,
		LongContextThreshold: 272_000,
		LongContextInput:     10, LongContextOutput: 60, LongContextCacheRead: 1,
	},
	"gpt-5.5-pro": {Input: 30, Output: 180, CacheRead: 30}, // OpenCode reports cached = input for Pro
	"gpt-5.4": {
		Input: 2.50, Output: 15, CacheRead: 0.25,
		LongContextThreshold: 272_000,
		LongContextInput:     5, LongContextOutput: 30, LongContextCacheRead: 0.50,
	},
	"gpt-5.4-pro":         {Input: 30, Output: 180, CacheRead: 30}, // see note above
	"gpt-5.4-mini":        {Input: 0.75, Output: 4.50, CacheRead: 0.075},
	"gpt-5.4-nano":        {Input: 0.20, Output: 1.25, CacheRead: 0.02},
	"gpt-5.3-codex":       {Input: 1.75, Output: 14, CacheRead: 0.175},
	"gpt-5.3-codex-spark": {Input: 1.75, Output: 14, CacheRead: 0.175},
	"gpt-5.2":             {Input: 1.75, Output: 14, CacheRead: 0.175},
	"gpt-5.2-codex":       {Input: 1.75, Output: 14, CacheRead: 0.175},
	"gpt-5.2-pro":         {Input: 21, Output: 168}, // no cache tier
	"gpt-5.1":             {Input: 1.07, Output: 8.50, CacheRead: 0.107},
	"gpt-5.1-codex":       {Input: 1.07, Output: 8.50, CacheRead: 0.107},
	"gpt-5.1-codex-max":   {Input: 1.25, Output: 10, CacheRead: 0.125},
	"gpt-5.1-codex-mini":  {Input: 0.25, Output: 2, CacheRead: 0.025},
	"gpt-5":               {Input: 1.07, Output: 8.50, CacheRead: 0.107},
	"gpt-5-codex":         {Input: 1.07, Output: 8.50, CacheRead: 0.107},
	"gpt-5-mini":          {Input: 0.25, Output: 2, CacheRead: 0.025},
	"gpt-5-nano":          {Input: 0, Output: 0, CacheRead: 0}, // Free per OpenAI 2026-04-29 catalog
	"gpt-5-pro":           {Input: 15, Output: 120},            // legacy; no cache tier
	// OpenAI — GPT-4.1 family.
	"gpt-4.1":      {Input: 2.00, Output: 8, CacheRead: 0.50},
	"gpt-4.1-mini": {Input: 0.40, Output: 1.60, CacheRead: 0.10},
	"gpt-4.1-nano": {Input: 0.10, Output: 0.40, CacheRead: 0.025},
	// OpenAI — GPT-4o family.
	"gpt-4o":            {Input: 2.50, Output: 10, CacheRead: 1.25},
	"gpt-4o-mini":       {Input: 0.15, Output: 0.60, CacheRead: 0.075},
	"gpt-4o-2024-05-13": {Input: 5, Output: 15}, // legacy, no cache tier
	// OpenAI — GPT-4 turbo and earlier.
	"gpt-4-turbo-2024-04-09":    {Input: 10, Output: 30},
	"gpt-4-0125-preview":        {Input: 10, Output: 30},
	"gpt-4-1106-preview":        {Input: 10, Output: 30},
	"gpt-4-1106-vision-preview": {Input: 10, Output: 30},
	"gpt-4-0613":                {Input: 30, Output: 60},
	"gpt-4-0314":                {Input: 30, Output: 60},
	"gpt-4-32k":                 {Input: 60, Output: 120},
	// OpenAI — reasoning (o-series).
	"o4-mini": {Input: 1.10, Output: 4.40, CacheRead: 0.275},
	"o3":      {Input: 2.00, Output: 8, CacheRead: 0.50},
	"o3-mini": {Input: 1.10, Output: 4.40, CacheRead: 0.55},
	"o3-pro":  {Input: 20, Output: 80}, // no cache tier
	"o1":      {Input: 15, Output: 60, CacheRead: 7.50},
	"o1-mini": {Input: 1.10, Output: 4.40, CacheRead: 0.55},
	"o1-pro":  {Input: 150, Output: 600}, // no cache tier
	// OpenAI — GPT-3.5 family (legacy).
	"gpt-3.5-turbo":          {Input: 0.50, Output: 1.50},
	"gpt-3.5-turbo-0125":     {Input: 0.50, Output: 1.50},
	"gpt-3.5-turbo-1106":     {Input: 1, Output: 2},
	"gpt-3.5-turbo-0613":     {Input: 1.50, Output: 2},
	"gpt-3.5-0301":           {Input: 1.50, Output: 2},
	"gpt-3.5-turbo-instruct": {Input: 1.50, Output: 2},
	"gpt-3.5-turbo-16k-0613": {Input: 3, Output: 4},
	// OpenAI — base models (legacy).
	"davinci-002": {Input: 2, Output: 2},
	"babbage-002": {Input: 0.40, Output: 0.40},

	// Google Gemini. Context caching → CacheRead; no separate
	// cache-write charge. The Pro tiers (2.5 Pro, 3.1 Pro) carry a
	// 200K long-context tier that doubles every dimension; flash and
	// flash-lite are flat-rate. Family prefixes for 3.x are pointed at
	// the Pro rates (incl. LC) so future SKUs without an explicit
	// entry inherit the same shape — flash variants have explicit
	// entries that override the family fallback.
	"gemini-3.1-pro-preview": {
		Input: 2, Output: 12, CacheRead: 0.20,
		LongContextThreshold: 200_000,
		LongContextInput:     4, LongContextOutput: 24, LongContextCacheRead: 0.40,
	},
	"gemini-3.1-flash-lite-preview": {Input: 0.25, Output: 1.50, CacheRead: 0.025},
	"gemini-3-flash-preview":        {Input: 0.50, Output: 3, CacheRead: 0.05},
	"gemini-2.5-pro": {
		Input: 1.25, Output: 10, CacheRead: 0.125,
		LongContextThreshold: 200_000,
		LongContextInput:     2.50, LongContextOutput: 20, LongContextCacheRead: 0.25,
	},
	"gemini-2.5-flash":                      {Input: 0.30, Output: 2.50, CacheRead: 0.03},
	"gemini-2.5-flash-lite":                 {Input: 0.10, Output: 0.40, CacheRead: 0.01},
	"gemini-2.5-flash-lite-preview-09-2025": {Input: 0.10, Output: 0.40, CacheRead: 0.01},
	"gemini-2.0-flash":                      {Input: 0.10, Output: 0.40, CacheRead: 0.025}, // deprecated
	"gemini-2.0-flash-lite":                 {Input: 0.075, Output: 0.30},                  // deprecated
	// Family prefixes — set to LATEST (3.x) so future SKUs inherit
	// current generation rates rather than legacy. 3.1 / 3 family
	// fallback inherits Pro LC rates since the 3.1 Pro Preview is the
	// representative; 2.5 family fallback inherits 2.5 Pro LC rates.
	"gemini-3.1": {
		Input: 2, Output: 12, CacheRead: 0.20,
		LongContextThreshold: 200_000,
		LongContextInput:     4, LongContextOutput: 24, LongContextCacheRead: 0.40,
	},
	"gemini-3": {
		Input: 2, Output: 12, CacheRead: 0.20,
		LongContextThreshold: 200_000,
		LongContextInput:     4, LongContextOutput: 24, LongContextCacheRead: 0.40,
	},
	"gemini-2.5": {
		Input: 1.25, Output: 10, CacheRead: 0.125,
		LongContextThreshold: 200_000,
		LongContextInput:     2.50, LongContextOutput: 20, LongContextCacheRead: 0.25,
	},
	"gemini-2": {Input: 0.10, Output: 0.40, CacheRead: 0.025},

	// xAI.
	"grok-4-20": {Input: 2, Output: 6, CacheRead: 0.20},
	"grok":      {Input: 2, Output: 6, CacheRead: 0.20}, // family prefix → grok-4-20 rates

	// Moonshot.
	"kimi-k2-5": {Input: 0.60, Output: 3, CacheRead: 0.10},
	"kimi":      {Input: 0.60, Output: 3, CacheRead: 0.10}, // family prefix

	// Cursor — Composer family (only relevant if the cursor adapter
	// ever starts recording token usage from cursor session logs;
	// hook-only today). Forward-compat.
	"composer-1":   {Input: 1.25, Output: 10, CacheRead: 0.125},
	"composer-1.5": {Input: 3.50, Output: 17.50, CacheRead: 0.35},
	"composer-2":   {Input: 0.50, Output: 2.50, CacheRead: 0.20},
	"composer":     {Input: 0.50, Output: 2.50, CacheRead: 0.20}, // family prefix → composer-2 rates
}
