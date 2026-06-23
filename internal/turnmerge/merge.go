package turnmerge

// Fidelity ranks how trustworthy a single turn observation is. A higher
// fidelity wins on conflicting authoritative (token/cost) fields. The value is
// resolved AT THE BOUNDARY from the source's capabilities (exactness, which
// extra fields it carries) — the merge logic below compares fidelity numbers
// and never inspects which tool or source produced the row.
type Fidelity int

const (
	// FidelityUnknown is the zero value; treat as the lowest rank.
	FidelityUnknown Fidelity = iota
	// FidelityApprox is a JSONL `usage` envelope (Tier 2) — token counts are
	// approximate and may lag the final billed numbers.
	FidelityApprox
	// FidelityNativeExact is a provider's native telemetry usage block
	// (e.g. Claude Code OTel `api_request`) — exact tokens, but without the
	// proxy-only fields (cache-tier split, 1h surcharge).
	FidelityNativeExact
	// FidelityProxyExact is a proxy intercept — exact tokens with the full
	// cache-tier split and 1h-surcharge attribution. Highest rank.
	FidelityProxyExact
)

// Turn is the merge-relevant view of an API turn. It is intentionally a subset
// of models.APITurn: only the fields that participate in cross-source dedup.
// The store boundary converts models.APITurn <-> Turn. RequestID is the dedup
// key; Merge assumes both rows share it (the boundary guarantees this).
type Turn struct {
	RequestID string
	Fidelity  Fidelity

	// Authoritative block — token/cost. Higher fidelity wins wholesale;
	// equal fidelity takes the per-field MAX (snapshot sources re-emit
	// refined, monotonically non-decreasing counts — see
	// feedback_token_max_upgrade_conflict).
	InputTokens           int64
	OutputTokens          int64
	CacheReadTokens       int64
	CacheCreationTokens   int64
	CacheCreation1hTokens int64
	WebSearchRequests     int64
	CostUSD               float64

	// Enrichment block — fields the proxy/JSONL paths typically lack that a
	// native source fills. Column-merged fill-if-absent regardless of
	// fidelity; an existing non-zero/non-empty value is never overwritten.
	TimeToFirstTokenMS int64
	TotalResponseMS    int64
	StopReason         string
}

// Action is the persistence decision Merge reaches.
type Action int

const (
	// ActionInsert means no row existed for this request_id; persist Turn as-is.
	ActionInsert Action = iota
	// ActionUpdate means an existing row was changed by the merge; persist Turn.
	ActionUpdate
	// ActionNoChange means the incoming observation added nothing new.
	ActionNoChange
)

// String renders an Action for logs and test failures.
func (a Action) String() string {
	switch a {
	case ActionInsert:
		return "insert"
	case ActionUpdate:
		return "update"
	case ActionNoChange:
		return "no-change"
	default:
		return "unknown"
	}
}

// Result is the outcome of a Merge: the decision, the merged row to persist
// (meaningful for Insert and Update), and the names of the fields that changed
// (empty for Insert and NoChange — Insert writes the whole row, NoChange writes
// nothing). Changed is sorted in rule-table order for deterministic tests.
type Result struct {
	Action  Action
	Turn    Turn
	Changed []string
}

// fieldClass selects which precedence rule a field obeys.
type fieldClass int

const (
	// classAuthoritative: higher fidelity wins; equal fidelity takes the MAX.
	classAuthoritative fieldClass = iota
	// classEnrichment: fill only when the existing value is the zero value.
	classEnrichment
)

// int64Rule binds one int64 field to its precedence class via a pointer accessor.
type int64Rule struct {
	name  string
	class fieldClass
	ptr   func(*Turn) *int64
}

// int64Rules is the ordered precedence table for int64 fields, walked top-down.
// Adding a field here is the ONLY change needed to fold it into the merge.
var int64Rules = []int64Rule{
	{"input_tokens", classAuthoritative, func(t *Turn) *int64 { return &t.InputTokens }},
	{"output_tokens", classAuthoritative, func(t *Turn) *int64 { return &t.OutputTokens }},
	{"cache_read_tokens", classAuthoritative, func(t *Turn) *int64 { return &t.CacheReadTokens }},
	{"cache_creation_tokens", classAuthoritative, func(t *Turn) *int64 { return &t.CacheCreationTokens }},
	{"cache_creation_1h_tokens", classAuthoritative, func(t *Turn) *int64 { return &t.CacheCreation1hTokens }},
	{"web_search_requests", classAuthoritative, func(t *Turn) *int64 { return &t.WebSearchRequests }},
	{"time_to_first_token_ms", classEnrichment, func(t *Turn) *int64 { return &t.TimeToFirstTokenMS }},
	{"total_response_ms", classEnrichment, func(t *Turn) *int64 { return &t.TotalResponseMS }},
}

// Merge collapses an incoming observation into the existing row for the same
// request_id and returns the persistence decision.
//
//   - existing == nil → ActionInsert, Turn = incoming (the first observation;
//     a native-only turn lands here at full fidelity — the no-proxy gap fill).
//   - existing != nil → fields are merged per the precedence table; the result
//     is ActionUpdate if anything changed, else ActionNoChange.
//
// Merge never mutates its inputs.
func Merge(existing *Turn, incoming Turn) Result {
	if existing == nil {
		return Result{Action: ActionInsert, Turn: incoming}
	}

	merged := *existing
	var changed []string

	for _, r := range int64Rules {
		dst := r.ptr(&merged)
		src := *r.ptr(&incoming)
		var didChange bool
		switch r.class {
		case classAuthoritative:
			didChange = applyAuthoritativeInt(dst, src, existing.Fidelity, incoming.Fidelity)
		case classEnrichment:
			didChange = applyEnrichmentInt(dst, src)
		}
		if didChange {
			changed = append(changed, r.name)
		}
	}

	// CostUSD is authoritative (float); StopReason is enrichment (string).
	if applyAuthoritativeFloat(&merged.CostUSD, incoming.CostUSD, existing.Fidelity, incoming.Fidelity) {
		changed = append(changed, "cost_usd")
	}
	if applyEnrichmentString(&merged.StopReason, incoming.StopReason) {
		changed = append(changed, "stop_reason")
	}

	// The merged row carries the best fidelity seen. A pure fidelity bump (no
	// data field moved) still counts as an Update so provenance is persisted.
	if incoming.Fidelity > merged.Fidelity {
		merged.Fidelity = incoming.Fidelity
		changed = append(changed, "fidelity")
	}

	if len(changed) == 0 {
		return Result{Action: ActionNoChange, Turn: merged}
	}
	return Result{Action: ActionUpdate, Turn: merged, Changed: changed}
}

// applyAuthoritativeInt overwrites *dst from src when the incoming source is
// strictly higher fidelity, or takes the MAX when fidelity is equal (snapshot
// re-emit). A lower-fidelity source never overwrites. Reports whether it moved.
func applyAuthoritativeInt(dst *int64, src int64, existing, incoming Fidelity) bool {
	switch {
	case incoming > existing:
		if *dst != src {
			*dst = src
			return true
		}
	case incoming == existing:
		if src > *dst {
			*dst = src
			return true
		}
	}
	return false
}

// applyAuthoritativeFloat is applyAuthoritativeInt for CostUSD.
func applyAuthoritativeFloat(dst *float64, src float64, existing, incoming Fidelity) bool {
	switch {
	case incoming > existing:
		if *dst != src {
			*dst = src
			return true
		}
	case incoming == existing:
		if src > *dst {
			*dst = src
			return true
		}
	}
	return false
}

// applyEnrichmentInt fills *dst from src only when *dst is still the zero value
// and src carries one. It never overwrites an existing non-zero value.
func applyEnrichmentInt(dst *int64, src int64) bool {
	if *dst == 0 && src != 0 {
		*dst = src
		return true
	}
	return false
}

// applyEnrichmentString is applyEnrichmentInt for StopReason.
func applyEnrichmentString(dst *string, src string) bool {
	if *dst == "" && src != "" {
		*dst = src
		return true
	}
	return false
}
