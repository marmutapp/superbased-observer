package govern

import "testing"

// TestW8PerTierGateThroughResolve is MUTATION-PROOF (c) for W-8, driven
// through the REAL Resolve path (RESOLVE-1 lesson: a hand-built Effective
// can encode the answer the test wants rather than the answer the resolver
// actually produces).
//
// The scenario is the exact shape of the over-report bug: a single managed
// grant authorizes ONLY extract.cache, but the org body's share block asks
// for BOTH cache_detail and full_content to be true. The per-tier gate must
// let cache_detail raise (its own token is present) while leaving
// full_content on the floor (its token, extract.folders, was never granted)
// — MergeBoolGated/SourceForBoolGated must report this honestly, not the
// MergeBool/SourceForBool over-report that motivated W-8.
//
// If sharetiers.go's per-tier gate is broken — e.g. reverted to consult only
// the coarse e.Managed flag, or to let extract.cache authorize full_content
// too — this test fails on the full_content assertions while the
// cache_detail assertions keep passing, pinpointing exactly which tier
// leaked.
func TestW8PerTierGateThroughResolve(t *testing.T) {
	grant := managedRaiseGrant(ConsentManaged, AuthorityExtractCache)
	body := `{"schema":2,"share":{"cache_detail":true,"full_content":true}}`
	e := Resolve(phase1bDelivered(t, body), grant, testLive(), testNow)

	// The grant carries no dashboard.visibility authority, so the
	// always-present `sections` class drops and the posture reports
	// StateInert — that is orthogonal to this test (see
	// TestManagedRaiseAppliesEvenWhenPostureIsInert: StateInert never
	// suppresses the directives that WERE authorized) and is asserted here
	// only so a future reader is not surprised by it.
	if e.State != StateInert {
		t.Fatalf("State = %q, want inert (sections has no authority here): Dropped=%+v", e.State, e.Dropped)
	}
	var sawSectionsDropped bool
	for _, d := range e.Dropped {
		if d.Directive == "sections" {
			sawSectionsDropped = true
		}
		if d.Directive == "share" {
			t.Fatalf("share class unexpectedly dropped: %+v (extract.cache should authorize it)", d)
		}
	}
	if !sawSectionsDropped {
		t.Fatalf("expected sections to be dropped: %+v", e.Dropped)
	}

	// The granted tier: MergeBoolGated raises it off a local false, and
	// SourceForBoolGated attributes the raise to the org.
	if got := e.MergeBoolGated("cache_detail", false); !got {
		t.Fatalf("MergeBoolGated(cache_detail, false) = %v, want true (extract.cache is granted)", got)
	}
	if got := e.SourceForBoolGated("cache_detail", false); got != ShareSourceOrgRaised {
		t.Fatalf("SourceForBoolGated(cache_detail, false) = %q, want %q", got, ShareSourceOrgRaised)
	}

	// The ungranted tier: the body asked for it too, and the coarse
	// e.Managed-only check (RaiseBool alone, or MergeBool) WOULD raise it —
	// that is exactly the over-report W-8 closes. MergeBoolGated must not.
	if raw := e.RaiseBool("full_content", false); !raw {
		t.Fatalf("test invariant broken: RaiseBool(full_content, false) = %v, want true (proves the body really carries it and Managed is set, so an ungated caller WOULD over-report)", raw)
	}
	if got := e.MergeBoolGated("full_content", false); got {
		t.Fatalf("MergeBoolGated(full_content, false) = %v, want false — extract.cache must not authorize full_content (that needs extract.folders)", got)
	}
	if got := e.SourceForBoolGated("full_content", false); got != ShareSourceLocal {
		t.Fatalf("SourceForBoolGated(full_content, false) = %q, want %q (the org's raise never took honest effect)", got, ShareSourceLocal)
	}

	// And the push-seam-equivalent surface: lowerShareOptions in
	// cmd/observer/governance_wire.go is the actual consumer of this same
	// gate via shareRaiseFields/govern.ExtractionAuthorized. Pin the
	// predicate this package exports for that loop directly too, so a
	// regression in ExtractionAuthorized itself (not just the Merge*Gated
	// wrappers) is caught here as well.
	if ExtractionAuthorized(e, "cache_detail") == false {
		t.Fatalf("ExtractionAuthorized(cache_detail) = false, want true")
	}
	if ExtractionAuthorized(e, "full_content") {
		t.Fatalf("ExtractionAuthorized(full_content) = true, want false (extract.cache grant must not authorize the folders tier)")
	}
}
