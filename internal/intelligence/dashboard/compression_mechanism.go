package dashboard

import "sort"

// Compression-mechanism classification — the ONE place the dashboard
// distinguishes lossy eviction from genuine (retrievable) compression.
//
// The compression pipeline (internal/compression/conversation) tags
// every event with a `mechanism`. Most mechanisms replace a payload
// with a smaller, recoverable representation: json/code/logs/text/diff/
// html trim or re-encode bytes; `stash` offloads a large body to the
// local stash and leaves a retrieval marker (SROD); `read_cache`
// swaps a duplicate file body for a "unchanged" marker; `tools`
// re-summarises a tool schema; `rolling_summary` folds old turns into
// a Haiku summary. For all of these `compressed_bytes` is the size of
// the retained form, so `original - compressed` is a real byte saving.
//
// `drop`, by contrast, EVICTS a low-importance message outright —
// budget.go / gemini.go emit it with `compressed_bytes = 0` BY
// CONSTRUCTION because there is no compressed form; the content is
// gone (recoverable only through the search_past_outputs / stash
// markers, not inline). Pricing those evicted bytes at the input rate
// and rendering them as "0 B / 100% saved / $X saved" misrepresents
// lossy eviction as compression savings — the same misleading class
// as the retracted whole-conversation compression-savings claim.
//
// Classified once here so every compression surface (by-model,
// timeseries, per-event) inherits the same honesty: a lossy
// mechanism's bytes are reported as EVICTED, never as saved, and are
// never priced.

// lossyEvictionMechanisms is the set of compression-event mechanisms
// that remove content outright rather than compressing it. Membership
// is a capability (compressed_bytes == 0 by construction), not a
// display heuristic — extend this set if a new evicting mechanism is
// added upstream.
var lossyEvictionMechanisms = map[string]bool{
	"drop": true,
}

// mechanismIsLossy reports whether a compression mechanism evicts
// content (compressed_bytes recorded as 0 by construction) rather than
// producing a smaller, retrievable representation. Callers must treat
// a lossy mechanism's original_bytes as EVICTED — not as bytes saved,
// and never priced as dollars saved.
func mechanismIsLossy(mechanism string) bool {
	return lossyEvictionMechanisms[mechanism]
}

// lossyEvictionMechanismList returns the lossy-eviction mechanisms as a
// deterministically-ordered slice so SQL callers can build an IN(...)
// set from the SAME classifier table (lossyEvictionMechanisms) that
// mechanismIsLossy consults. Keeping the SQL set derived from this one
// owner means adding a new evicting mechanism upstream updates every
// surface — the per-event display AND the byte-subtraction paths —
// from a single edit.
func lossyEvictionMechanismList() []string {
	out := make([]string, 0, len(lossyEvictionMechanisms))
	for m := range lossyEvictionMechanisms {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}
