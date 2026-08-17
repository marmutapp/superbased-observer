package proxy

import (
	"encoding/json"
	"net/url"
	"strings"
)

// resolveAutoLane resolves the virtual "auto" /up/ lane (Phase 2, gateway
// config plane spec) by the request's top-level model, OpenRouter-style:
// a model of the form "<lane>/<rest>" routes to the configured lane named
// <lane>, with the body's "model" field rewritten to <rest>. lanes is a
// single snapshot of the live lane table (Load()d once by the caller via
// laneSnapshot — resolveAutoLane never re-Loads, so its decision reads the
// upstream map AND the default lane id from the exact same generation,
// even across a concurrent SetUpstreams/SetLaneTable swap; see laneTable's
// doc comment).
//
// Falls back to the configured default lane (lanes.autoDefault, "" when
// unset), body UNREWRITTEN, when: the model has no "/", the prefix before
// the first "/" doesn't name a configured lane, the suffix after it is
// empty, or the prefix DID match but the rewrite itself fails json.Valid
// (fail-open — a malformed rewrite must never reach the matched lane).
//
// Returns (nil, "", nil, false) when neither a prefix match nor a usable
// default lane exists — the caller then treats the request exactly like
// an unknown /up/<id> id (fixed upstream, warn-once, body untouched).
func (p *Proxy) resolveAutoLane(lanes *laneTable, model string, body []byte) (upstream *url.URL, laneID string, newBody []byte, rewritten bool) {
	upstreams := lanes.upstreams
	if prefix, suffix, ok := strings.Cut(model, "/"); ok && prefix != "" && suffix != "" {
		if u, exists := upstreams[prefix]; exists {
			if mutated, valid := rewriteModelViaRawJSON(body, suffix); valid {
				return u, prefix, mutated, true
			}
			// Rewrite failed json.Valid — fail open to the default lane
			// below with the body untouched, not to the matched prefix
			// lane with a possibly-broken body.
		}
	}
	def := lanes.autoDefault
	if def == "" {
		return nil, "", nil, false
	}
	if u, exists := upstreams[def]; exists {
		return u, def, nil, false
	}
	// The configured default lane isn't in the current snapshot (e.g. a
	// hot SetUpstreams/SetLaneTable swap removed it) — unavailable at
	// runtime, per the same "since-removed lane" semantic documented for
	// egress route_upstream targets (Phase 1 spec).
	return nil, "", nil, false
}

// rewriteModelViaRawJSON replaces the top-level "model" field of a JSON
// object with newModel via a map[string]json.RawMessage round-trip, so
// every OTHER field's bytes pass through byte-identical (field order may
// shift — JSON objects are unordered — but no value is touched). This is
// deliberately a different, less delicate strategy than router.go's
// rewriteTopLevelModel byte-span splice (which also preserves field
// order): the auto lane spec calls for the map round-trip specifically.
//
// ok is false when body isn't a JSON object, marshaling fails, or the
// result fails json.Valid — the caller must then leave the ORIGINAL body
// untouched (fail-open), never forward a possibly-corrupt out.
func rewriteModelViaRawJSON(body []byte, newModel string) (out []byte, ok bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, false
	}
	encodedModel, err := json.Marshal(newModel)
	if err != nil {
		return nil, false
	}
	fields["model"] = encodedModel
	out, err = json.Marshal(fields)
	if err != nil || !json.Valid(out) {
		return nil, false
	}
	return out, true
}
