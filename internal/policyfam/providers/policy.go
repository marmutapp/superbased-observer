package providers

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// reservedAutoLaneID mirrors internal/proxy's autoLaneID constant. "auto" is
// the virtual model-prefix-routed lane (gateway-config-plane-spec-2026-08-15
// Phase 2); a dashboard-managed body can never define or default to a lane
// by that name. The two constants are intentionally duplicated rather than
// shared — this package must not import internal/proxy (purity,
// imports_test.go).
const reservedAutoLaneID = "auto"

// PolicySpec is the compiled, ready-to-apply gateway.providers policy: a
// validated lane table plus an optional default lane for the virtual "auto"
// lane's fallback. Hash is the content address of the policy (audit
// provenance; mirrors policyfam/admission.PolicySpec.Hash).
type PolicySpec struct {
	// Upstreams maps lane id -> validated absolute http/https base URL
	// string. Kept as strings (not net.URL) because the only consumer,
	// internal/proxy.Proxy.SetLaneTable, re-parses and re-validates its own
	// input independently — this package's job is to reject a malformed
	// body before it is ever signed/published, not to hand the proxy a
	// pre-parsed value it must trust.
	Upstreams map[string]string
	// AutoDefaultLane names the lane the virtual "auto" lane falls back to
	// when a request's model prefix matches no configured lane. Empty means
	// no default is configured (the "auto" lane then behaves as an unknown
	// upstream — fail-open per Phase 2).
	AutoDefaultLane string
	Hash            string
}

// UpstreamsAsStringMap returns a defensive copy of the compiled lane table
// (lane id -> base_url) — the exact shape internal/proxy.Proxy.SetLaneTable
// accepts, so the cmd/observer install seam never has to know this
// package's internal representation.
func (s PolicySpec) UpstreamsAsStringMap() map[string]string {
	out := make(map[string]string, len(s.Upstreams))
	for id, base := range s.Upstreams {
		out[id] = base
	}
	return out
}

// PolicyInput is the plain, pre-compile policy. It mirrors BodyV1 field for
// field; the only difference is BodyV1 carries JSON tags and this package
// never imports encoding/json opinions into the compile step itself.
type PolicyInput struct {
	Upstreams       map[string]string
	AutoDefaultLane string
}

// Compile validates a PolicyInput and produces a ready PolicySpec. It
// enforces the wire-shape invariants frozen by the spec:
//
//   - at least one upstream is required;
//   - every lane id is nonempty and not the reserved "auto" id;
//   - every base_url parses as an absolute http/https URL;
//   - auto_default_lane, when set, must name a key of upstreams (which,
//     since "auto" can never be a key, also rejects "auto" as a default).
//
// A malformed body is a hard error so it is caught at compile/publish time,
// never at request time on the proxy hot path.
func Compile(in PolicyInput) (PolicySpec, error) {
	if len(in.Upstreams) == 0 {
		return PolicySpec{}, fmt.Errorf("providers.Compile: at least one upstream is required")
	}
	upstreams := make(map[string]string, len(in.Upstreams))
	for id, raw := range in.Upstreams {
		id = strings.TrimSpace(id)
		if id == "" {
			return PolicySpec{}, fmt.Errorf("providers.Compile: upstream lane id must not be empty")
		}
		if id == reservedAutoLaneID {
			return PolicySpec{}, fmt.Errorf("providers.Compile: %q is a reserved lane id", reservedAutoLaneID)
		}
		if err := validateBaseURL(raw); err != nil {
			return PolicySpec{}, fmt.Errorf("providers.Compile: lane %q: %w", id, err)
		}
		upstreams[id] = raw
	}
	if in.AutoDefaultLane != "" {
		if _, ok := upstreams[in.AutoDefaultLane]; !ok {
			return PolicySpec{}, fmt.Errorf("providers.Compile: auto_default_lane %q does not name a configured upstream", in.AutoDefaultLane)
		}
	}
	return PolicySpec{
		Upstreams:       upstreams,
		AutoDefaultLane: in.AutoDefaultLane,
		Hash:            hashPolicy(in),
	}, nil
}

// validateBaseURL enforces "absolute http/https URL": a scheme of http or
// https and a nonempty host. url.Parse alone is too permissive (it happily
// parses a bare relative path or a non-network scheme like "file:"), so both
// are checked explicitly.
func validateBaseURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("base_url must not be empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("base_url %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("base_url %q must be an absolute http or https URL", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("base_url %q must include a host", raw)
	}
	return nil
}

// hashPolicy computes a stable content hash over the semantic policy fields
// so the same policy always yields the same Hash (audit provenance).
func hashPolicy(in PolicyInput) string {
	return HashLaneTable(in.Upstreams, in.AutoDefaultLane)
}

// HashLaneTable computes the stable 64-hex content address of a lane table
// (lane id -> base URL, plus the auto-default lane id). Field order is fixed
// and lanes are sorted by id so the hash never depends on Go map iteration
// order; the same table always yields the same hash.
//
// It is exported because the P0-6 effective-policy-state reporter needs the
// SAME content address for a lane table the node configured LOCALLY (which
// never passes through Compile and so has no PolicySpec.Hash) as for one
// that arrived over the org rail
// (docs/plans/policy-state-v2-gateway-providers-spec-2026-08-15.md §2.2).
// One algorithm, two call sites, rather than a second hash that could drift.
func HashLaneTable(upstreams map[string]string, autoDefaultLane string) string {
	h := sha256.New()
	writeField := func(parts ...string) {
		for _, p := range parts {
			_, _ = h.Write([]byte(p))
			_, _ = h.Write([]byte{0x1e})
		}
	}
	ids := make([]string, 0, len(upstreams))
	for id := range upstreams {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		writeField("upstream", id, upstreams[id])
	}
	if autoDefaultLane != "" {
		writeField("auto_default_lane", autoDefaultLane)
	}
	return hex.EncodeToString(h.Sum(nil))
}
