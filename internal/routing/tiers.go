package routing

import (
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
)

// TierSource categorises how a tier lookup resolved, mirroring the cost
// engine's PricingSource so dashboards can flag family-inferred placements
// the same way they flag family-inferred rates.
type TierSource string

// Tier lookup sources.
const (
	// TierSourceExact: the model id matched a table entry verbatim (or
	// the :free suffix guard fired — a known placement, not a guess).
	TierSourceExact TierSource = "exact"
	// TierSourceDateStripped: a -YYYYMMDD suffix was stripped to match.
	TierSourceDateStripped TierSource = "date-stripped"
	// TierSourceFamily: matched via longest-prefix family lookup.
	TierSourceFamily TierSource = "family"
	// TierSourceMiss: nothing matched; the model is TierUnclassified.
	TierSourceMiss TierSource = "miss"
)

// TierTable maps normalized model ids to tiers (§R7.1). Keys reuse the
// cost engine's family vocabulary, and Lookup reuses its normalization
// order — exact → `:free` guard → date-strip → family prefix → last-resort
// router/provider-prefix strip — so any model the cost engine can price,
// this table can place. The zero value is unusable; build via newTierTable.
type TierTable struct {
	exact map[string]Tier
	// representatives is the curated per-(shape, tier) canonical model a
	// downshift selects when a rule routes to a tier (§R11.4 same-shape
	// constraint keeps selection within the original request's shape).
	representatives map[ProviderShape]map[Tier]string
}

// newTierTable builds a table from the seed data plus caller overrides
// (overrides replace seed entries wholesale, like cost.Table.Merge).
// localReps, when non-nil, install per-shape TierLocal representatives
// (§R11.7) on a copy of the seed representative map.
func newTierTable(overrides map[string]Tier, localReps map[ProviderShape]string) *TierTable {
	t := &TierTable{
		exact:           make(map[string]Tier, len(seedTiers)+len(overrides)),
		representatives: seedRepresentatives,
	}
	for k, v := range seedTiers {
		t.exact[k] = v
	}
	for k, v := range overrides {
		t.exact[k] = v
	}
	if len(localReps) > 0 {
		reps := make(map[ProviderShape]map[Tier]string, len(seedRepresentatives))
		for shape, byTier := range seedRepresentatives {
			cells := make(map[Tier]string, len(byTier)+1)
			for tier, model := range byTier {
				cells[tier] = model
			}
			reps[shape] = cells
		}
		for shape, model := range localReps {
			if reps[shape] == nil {
				reps[shape] = map[Tier]string{}
			}
			reps[shape][TierLocal] = model
		}
		t.representatives = reps
	}
	return t
}

// Lookup places a model id in a tier. A miss returns TierUnclassified —
// never auto-routed to, only ever routed from by explicit rule (§R7.1).
// Match precedence mirrors cost.Table.LookupWithSource.
func (t *TierTable) Lookup(model string) (Tier, TierSource) {
	if t == nil || t.exact == nil || model == "" {
		return TierUnclassified, TierSourceMiss
	}
	if tier, ok := t.exact[model]; ok {
		return tier, TierSourceExact
	}
	// `:free` suffix guard — every free tier is TierFree regardless of
	// family, and it runs BEFORE the date-strip and family ladders so a
	// paid `<family>` entry never silently claims `<family>:free`
	// traffic. Known placement → TierSourceExact, mirroring the pricing
	// table's known-$0 semantics.
	if strings.HasSuffix(strings.ToLower(model), ":free") {
		return TierFree, TierSourceExact
	}
	stripped := stripTierDateSuffix(model)
	if stripped != model {
		if tier, ok := t.exact[stripped]; ok {
			return tier, TierSourceDateStripped
		}
	}
	lower := strings.ToLower(model)
	for _, family := range tierFamilyKeys(t.exact) {
		if strings.HasPrefix(lower, family) {
			return t.exact[family], TierSourceFamily
		}
	}
	// Last-resort normalization, mirroring the cost engine: strip router
	// prefixes and leading provider path segments
	// (openrouter/anthropic/foo → foo) and retry exact + family once.
	if norm := normalizeUnplacedModel(model); norm != "" {
		if tier, ok := t.exact[norm]; ok {
			return tier, TierSourceFamily
		}
		lnorm := strings.ToLower(norm)
		for _, family := range tierFamilyKeys(t.exact) {
			if strings.HasPrefix(lnorm, family) {
				return t.exact[family], TierSourceFamily
			}
		}
	}
	return TierUnclassified, TierSourceMiss
}

// Representative returns the curated canonical model for a (shape, tier)
// cell — the model a downshift to that tier selects for requests of that
// provider shape. ok=false when the cell has no curated target (the
// engine then declines the move with ReasonNoCandidate).
func (t *TierTable) Representative(shape ProviderShape, tier Tier) (string, bool) {
	if t == nil {
		return "", false
	}
	byTier, ok := t.representatives[shape]
	if !ok {
		return "", false
	}
	m, ok := byTier[tier]
	return m, ok
}

// RepresentativesForShape returns the shape's curated (tier →
// representative model) cells as ModelCandidates — the engine's
// candidate-assembly source. Empty for shapes with no curated cells.
func (t *TierTable) RepresentativesForShape(shape ProviderShape) []ModelCandidate {
	if t == nil {
		return nil
	}
	byTier, ok := t.representatives[shape]
	if !ok {
		return nil
	}
	out := make([]ModelCandidate, 0, len(byTier))
	for tier, model := range byTier {
		out = append(out, ModelCandidate{Model: model, Tier: tier, Shape: shape})
	}
	return out
}

// Known returns the exact model/family keys in the table, sorted. Intended
// for `observer routing status` and debugging.
func (t *TierTable) Known() []string {
	if t == nil {
		return nil
	}
	out := make([]string, 0, len(t.exact))
	for k := range t.exact {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TierResolver holds the active TierTable behind an atomic.Pointer — the
// pricing-table hot-reload pattern (internal/intelligence/cost.Engine).
// In-flight readers keep their snapshot of the old table; fresh callers
// see the new one. P0 reloads only at construction; the shape is ready
// for `[routing.tiers]` config hot-reload in P1.
type TierResolver struct {
	table atomic.Pointer[TierTable]
}

// NewTierResolver returns a resolver seeded with the shipped tier table.
func NewTierResolver() *TierResolver {
	r := &TierResolver{}
	r.Reload(nil)
	return r
}

// Reload swaps the resolver's table for seed + overrides. Reads against
// the resolver remain valid throughout — atomic.Pointer guarantees readers
// see either the old or the new table, never a torn state.
func (r *TierResolver) Reload(overrides map[string]Tier) {
	r.table.Store(newTierTable(overrides, nil))
}

// ReloadWithLocalUpstreams additionally installs per-shape TierLocal
// representatives (§R11.7) so a privacy-driven local routing decision
// has a concrete candidate to select.
func (r *TierResolver) ReloadWithLocalUpstreams(overrides map[string]Tier, localReps map[ProviderShape]string) {
	r.table.Store(newTierTable(overrides, localReps))
}

// Table returns the active tier-table snapshot. Safe from any goroutine;
// the returned pointer stays valid for reads across a concurrent Reload.
func (r *TierResolver) Table() *TierTable {
	if r == nil {
		return nil
	}
	return r.table.Load()
}

// Lookup is a convenience wrapper over the active table snapshot.
func (r *TierResolver) Lookup(model string) (Tier, TierSource) {
	return r.Table().Lookup(model)
}

// ShapeForModel resolves a model id to its provider wire shape (§24.3).
// Conservative: only families whose shape we are certain of resolve;
// everything else is ShapeUnknown, which the engine treats as
// non-candidate. Router/provider path prefixes are stripped first so
// `anthropic/claude-sonnet-4-6` resolves like `claude-sonnet-4-6` —
// EXCEPT the §R11.7 local-host prefixes (ollama/, lmstudio/, vllm/,
// local/), which carry shape meaning themselves: every local inference
// server we route to speaks the OpenAI wire shape.
func ShapeForModel(model string) ProviderShape {
	m := strings.ToLower(model)
	for _, prefix := range localHostPrefixes {
		if strings.HasPrefix(m, prefix) {
			return ShapeOpenAI
		}
	}
	if norm := normalizeUnplacedModel(m); norm != "" {
		m = norm
	}
	for _, row := range shapePrefixes {
		if strings.HasPrefix(m, row.prefix) {
			return row.shape
		}
	}
	return ShapeUnknown
}

// localHostPrefixes are the §R11.7 local-inference id prefixes — they
// resolve to the OpenAI shape BEFORE the path-segment strip.
var localHostPrefixes = []string{"ollama/", "lmstudio/", "vllm/", "local/"}

// shapePrefixes is the ordered prefix → shape resolution table. Walked
// top-down, first match wins.
var shapePrefixes = []struct {
	prefix string
	shape  ProviderShape
}{
	{"claude", ShapeAnthropic},
	{"gpt-", ShapeOpenAI},
	{"o1", ShapeOpenAI},
	{"o3", ShapeOpenAI},
	{"o4", ShapeOpenAI},
	{"davinci", ShapeOpenAI},
	{"babbage", ShapeOpenAI},
	{"gemini", ShapeGoogle},
}

// tierDateSuffix matches a trailing "-YYYYMMDD" on a model id, identical
// to the cost engine's date-strip step.
var tierDateSuffix = regexp.MustCompile(`-\d{8}$`)

func stripTierDateSuffix(model string) string {
	return tierDateSuffix.ReplaceAllString(model, "")
}

// tierFamilyKeys returns the table keys usable as family prefixes (no
// date suffix), sorted longest-first so the most-specific family wins —
// the cost engine's familyKeys semantics.
func tierFamilyKeys(exact map[string]Tier) []string {
	out := make([]string, 0, len(exact))
	for k := range exact {
		if tierDateSuffix.MatchString(k) {
			continue
		}
		out = append(out, strings.ToLower(k))
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i] < out[j] // deterministic among equal lengths
	})
	return out
}

// normalizeUnplacedModel is the last-resort reducer, mirroring the cost
// engine's normalizeUnpricedModel: strip router prefixes (capi:,
// sweagent-capi:) and ALL leading provider path segments, returning a
// candidate to retry once. "auto"/"" name no real model and stay a miss.
// Returns "" when nothing was stripped.
func normalizeUnplacedModel(model string) string {
	out := model
	switch {
	case strings.HasPrefix(out, "capi:"):
		out = strings.TrimPrefix(out, "capi:")
	case strings.HasPrefix(out, "sweagent-capi:"):
		out = strings.TrimPrefix(out, "sweagent-capi:")
	}
	if i := strings.LastIndex(out, "/"); i >= 0 {
		out = out[i+1:]
	}
	if out == model || out == "" {
		return ""
	}
	return out
}

// seedTiers ships with the binary (§R7.1) and is fully user-overridable
// (config wiring lands in P1; the override seam exists now via Reload).
// Keys reuse the cost engine's family vocabulary — see
// internal/intelligence/cost/pricing.go defaultPricing. Placements are
// quality-class judgments calibrated against published positioning and
// pricing as of 2026-06; per-project calibration (§R7.2, P2) refines them
// from observed outcomes. Models absent here resolve TierUnclassified.
//
// Auto-router sentinels (Cursor "default", kilo-auto, openrouter/auto)
// are deliberately NOT placed: a router is not a quality class, and our
// position is to audit them (§R17.4), not to pretend to know what they
// will serve.
var seedTiers = map[string]Tier{
	// Anthropic. Family prefixes carry the placement; explicit minors
	// pin the flagship SKUs the engine most often reasons about.
	"claude-opus-4-8":   TierOpusClass,
	"claude-opus-4":     TierOpusClass,
	"claude-opus-4-1":   TierOpusClass,
	"claude-opus":       TierOpusClass,
	"claude-3-opus":     TierOpusClass,
	"claude-fable-5":    TierOpusClass,
	"claude-fable":      TierOpusClass,
	"claude-sonnet-4-6": TierSonnetClass,
	"claude-sonnet-4":   TierSonnetClass,
	"claude-sonnet":     TierSonnetClass,
	"claude-3-5-sonnet": TierSonnetClass,
	"claude-3-sonnet":   TierSonnetClass,
	"claude-haiku-4-5":  TierHaikuClass,
	"claude-haiku-4.5":  TierHaikuClass, // dot variant — Copilot CLI emits this
	"claude-haiku":      TierHaikuClass,
	"claude-3-5-haiku":  TierHaikuClass,
	"claude-3-haiku":    TierHaikuClass,

	// OpenAI. gpt-5.5 is the frontier line; gpt-5.4 and earlier 5.x are
	// the mid line; minis are the small class; the $0 nano is free. Pro
	// variants are flagship-priced regardless of minor.
	"gpt-5.5":      TierOpusClass,
	"gpt-5.5-pro":  TierOpusClass,
	"gpt-5.4-pro":  TierOpusClass,
	"gpt-5.2-pro":  TierOpusClass,
	"gpt-5-pro":    TierOpusClass,
	"gpt-5.4":      TierSonnetClass,
	"gpt-5.4-mini": TierHaikuClass,
	"gpt-5.4-nano": TierHaikuClass,
	"gpt-5.3":      TierSonnetClass,
	"gpt-5.2":      TierSonnetClass,
	"gpt-5.1":      TierSonnetClass,
	"gpt-5-mini":   TierHaikuClass,
	"gpt-5-nano":   TierFree, // $0 per OpenAI 2026-04-29 catalog
	"gpt-5":        TierSonnetClass,
	"gpt-4.1":      TierSonnetClass,
	"gpt-4.1-mini": TierHaikuClass,
	"gpt-4.1-nano": TierHaikuClass,
	"gpt-4o":       TierSonnetClass,
	"gpt-4o-mini":  TierHaikuClass,
	"gpt-4":        TierSonnetClass,
	"gpt-3.5":      TierHaikuClass,
	"o1-pro":       TierOpusClass,
	"o1":           TierOpusClass,
	"o1-mini":      TierHaikuClass,
	"o3-pro":       TierOpusClass,
	"o3":           TierSonnetClass,
	"o3-mini":      TierHaikuClass,
	"o4-mini":      TierHaikuClass,

	// Google. 3.x Pro is the frontier line; 2.5 Pro the mid; flash the
	// small class. Family order matters: longest-prefix wins, so the
	// flash families out-match the bare generation families.
	"gemini-3.1-pro":   TierOpusClass,
	"gemini-3-pro":     TierOpusClass,
	"gemini-pro-agent": TierOpusClass,
	"gemini-3.1":       TierOpusClass, // cost family anchors 3.1 at Pro
	"gemini-3.5-flash": TierHaikuClass,
	"gemini-3-flash":   TierHaikuClass,
	"gemini-3":         TierOpusClass, // Pro-representative, mirroring cost
	"gemini-2.5-pro":   TierSonnetClass,
	"gemini-2.5-flash": TierHaikuClass,
	"gemini-2.5":       TierSonnetClass,
	"gemini-2":         TierHaikuClass,

	// xAI / Moonshot / DeepSeek / open-weight families. Mid-class
	// placements unless the SKU is explicitly a small/cheap line.
	"grok-code":        TierSonnetClass,
	"grok":             TierSonnetClass,
	"kimi":             TierSonnetClass,
	"deepseek-v4-pro":  TierSonnetClass,
	"deepseek":         TierHaikuClass, // family → v4-flash, the cheap default
	"gpt-oss":          TierHaikuClass,
	"nemotron-3-ultra": TierSonnetClass,
	"nemotron":         TierHaikuClass,
	"hermes":           TierSonnetClass,
	"qwen":             TierSonnetClass,
	"glm":              TierSonnetClass,
	"mistral-large":    TierSonnetClass,
	"mistral-small":    TierHaikuClass,
	"mistral":          TierSonnetClass,
	"minimax":          TierSonnetClass,
	"composer":         TierSonnetClass, // Cursor's own coding line
	"kilo-auto/free":   TierFree,
	"kilo-auto/small":  TierHaikuClass, // Kilo's title-generation slot
	"ollama":           TierLocal,
}

// seedRepresentatives names the canonical downshift target per
// (provider shape, tier). Selection stays same-shape (§R11.4); a cell
// absent here means "no curated target — decline the move".
var seedRepresentatives = map[ProviderShape]map[Tier]string{
	ShapeAnthropic: {
		TierOpusClass:   "claude-opus-4-8",
		TierSonnetClass: "claude-sonnet-4-6",
		TierHaikuClass:  "claude-haiku-4-5",
	},
	ShapeOpenAI: {
		TierOpusClass:   "gpt-5.5",
		TierSonnetClass: "gpt-5.4",
		TierHaikuClass:  "gpt-5.4-mini",
	},
	ShapeGoogle: {
		TierOpusClass:   "gemini-3.1-pro-preview",
		TierSonnetClass: "gemini-2.5-pro",
		TierHaikuClass:  "gemini-3.5-flash",
	},
}
