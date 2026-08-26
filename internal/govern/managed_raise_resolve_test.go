package govern

import "testing"

// managedRaiseGrant builds a grant carrying exactly the tokens under test
// under an explicit consent mode. It deliberately does NOT add
// dashboard.visibility: the live defect was found on a real managed grant
// that carried only enforcement + extraction authority, and padding the
// fixture with an unrelated token would hide the interaction between the
// always-present `sections` class and the share class.
func managedRaiseGrant(consentMode string, tokens ...string) *Grant {
	g := testGrant()
	g.Authority = tokens
	g.ConsentMode = consentMode
	return g
}

// TestExtractionAuthorityAuthorizesShareClass is the regression for the S1
// defect, driven THROUGH Resolve rather than over a hand-built Effective.
//
// The pre-fix resolver authorized the `share` directive class on
// AuthorityCapturePin alone, so a managed grant carrying only extraction
// authority dropped the whole share block. Effective.Share stayed empty,
// RaiseBool had nothing to read, and managed extraction silently never
// applied while the node reported accepted_inert. Every earlier unit test
// missed it because they constructed Effective{Share: ...} directly and so
// never exercised the authority intersection at all.
func TestExtractionAuthorityAuthorizesShareClass(t *testing.T) {
	cases := []struct {
		name        string
		consentMode string
		authority   []string
		body        string
		shareKey    string
		// wantShare: the share block survived the authority intersection.
		wantShare bool
		// wantRaise: RaiseBool actually lifts the tier off its local false.
		wantRaise bool
		// wantGrants: the per-tier predicate the caller gates on.
		wantGrants func(Effective) bool
		// wantDropReason is asserted only when wantShare is false.
		wantDropReason string
	}{
		{
			// (a) THE defect. A managed grant whose only extraction token is
			// the umbrella must populate Share and raise.
			name:        "managed grant with only extract.managed raises",
			consentMode: ConsentManaged,
			authority:   []string{AuthorityExtractManaged},
			body:        `{"schema":2,"share":{"full_tool_bodies":true}}`,
			shareKey:    "full_tool_bodies",
			wantShare:   true,
			wantRaise:   true,
			wantGrants:  Effective.GrantsToolBodiesExtraction,
		},
		{
			// (b) The individual plane, unchanged. HonoredAuthority strips
			// the managed-only token BEFORE the resolve input is built, so
			// the widened gate never sees it. This is verified here rather
			// than re-gated inside the share class.
			name:           "interactive consent strips extract.managed and drops share",
			consentMode:    ConsentInteractive,
			authority:      []string{AuthorityExtractManaged},
			body:           `{"schema":2,"share":{"full_tool_bodies":true}}`,
			shareKey:       "full_tool_bodies",
			wantShare:      false,
			wantRaise:      false,
			wantGrants:     func(e Effective) bool { return !e.GrantsToolBodiesExtraction() },
			wantDropReason: ReasonNotPreauthorized,
		},
		{
			// (c) The lowering path, unchanged: capture.pin alone still
			// applies the share block, and still raises NOTHING (no managed
			// consent, no extraction authority).
			name:        "capture.pin alone still applies share and never raises",
			consentMode: ConsentInteractive,
			authority:   []string{AuthorityCapturePin},
			body:        `{"schema":2,"share":{"full_tool_bodies":true}}`,
			shareKey:    "full_tool_bodies",
			wantShare:   true,
			wantRaise:   false,
			wantGrants:  func(e Effective) bool { return !e.GrantsToolBodiesExtraction() },
		},
		{
			// (d) enforce.* is managed-only but is NOT extraction authority.
			// It must not authorize the share block, or granting routing
			// enforcement would quietly open the extraction door.
			name:           "managed grant with only enforce.* drops share",
			consentMode:    ConsentManaged,
			authority:      []string{AuthorityEnforceRouting, AuthorityEnforceAdmission, AuthorityEnforceEgress},
			body:           `{"schema":2,"share":{"full_tool_bodies":true}}`,
			shareKey:       "full_tool_bodies",
			wantShare:      false,
			wantRaise:      false,
			wantGrants:     func(e Effective) bool { return !e.GrantsToolBodiesExtraction() },
			wantDropReason: ReasonNotPreauthorized,
		},
		{
			// (e) A PER-TIER token is extraction authority too, so it also
			// authorizes the share block on its own.
			name:        "managed grant with only a per-tier extract token raises that tier",
			consentMode: ConsentManaged,
			authority:   []string{AuthorityExtractCache},
			body:        `{"schema":2,"share":{"cache_detail":true}}`,
			shareKey:    "cache_detail",
			wantShare:   true,
			wantRaise:   true,
			wantGrants:  Effective.GrantsCacheExtraction,
		},
		{
			// (e') A high-sensitivity per-tier token, whose predicate does
			// NOT accept the umbrella, still authorizes the share class the
			// same way. The gate is "is this extraction authority", the
			// per-tier semantics stay with the predicate.
			name:        "managed grant with only extract.terminal raises the terminal tier",
			consentMode: ConsentManaged,
			authority:   []string{AuthorityExtractTerminal},
			body:        `{"schema":2,"share":{"terminal_detail":true}}`,
			shareKey:    "terminal_detail",
			wantShare:   true,
			wantRaise:   true,
			wantGrants:  Effective.GrantsTerminalExtraction,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := Resolve(phase1bDelivered(t, tc.body),
				managedRaiseGrant(tc.consentMode, tc.authority...), testLive(), testNow)

			_, gotShare := e.ShareDirective(tc.shareKey)
			if gotShare != tc.wantShare {
				t.Fatalf("ShareDirective(%q) present = %v, want %v (Share=%v, Dropped=%+v)",
					tc.shareKey, gotShare, tc.wantShare, e.Share, e.Dropped)
			}
			// The raise is asserted off a LOCAL false, which is the only
			// interesting starting point: a local true needs no authority.
			if got := e.RaiseBool(tc.shareKey, false); got != tc.wantRaise {
				t.Fatalf("RaiseBool(%q,false) = %v, want %v", tc.shareKey, got, tc.wantRaise)
			}
			if !tc.wantGrants(e) {
				t.Fatalf("per-tier extraction predicate disagreed with the resolved posture: %+v", e)
			}
			if tc.wantShare {
				for _, d := range e.Dropped {
					if d.Directive == "share" {
						t.Fatalf("share dropped anyway: %+v", d)
					}
				}
				return
			}
			var found bool
			for _, d := range e.Dropped {
				if d.Directive == "share" {
					found = true
					if d.Reason != tc.wantDropReason {
						t.Fatalf("share drop reason = %q, want %q", d.Reason, tc.wantDropReason)
					}
				}
			}
			if !found {
				t.Fatalf("share neither applied nor recorded as dropped: %+v", e.Dropped)
			}
		})
	}
}

// TestManagedRaiseAppliesEvenWhenPostureIsInert pins the PARTIAL-APPLICATION
// HONESTY rule against the fix: a grant that carries extraction authority but
// no dashboard.visibility still drops the always-present `sections` class, so
// the posture reports StateInert — and the share raise it DID authorize is
// applied all the same.
//
// That is by design, not a contradiction. StateInert means "something was
// refused", never "nothing ran": each directive class is intersected with the
// grant independently, and Effective.Hash covers the drops so a partial
// application can never hash-match the delivered body. A future reader
// tempted to make StateInert suppress application would silently re-open the
// S1 defect on every managed grant that omits dashboard.visibility.
func TestManagedRaiseAppliesEvenWhenPostureIsInert(t *testing.T) {
	e := Resolve(phase1bDelivered(t, `{"schema":2,"share":{"full_tool_bodies":true}}`),
		managedRaiseGrant(ConsentManaged, AuthorityExtractManaged), testLive(), testNow)

	if e.State != StateInert {
		t.Fatalf("State = %q, want inert (the sections class has no authority here)", e.State)
	}
	if len(e.Dropped) != 1 || e.Dropped[0].Directive != "sections" {
		t.Fatalf("Dropped = %+v, want exactly the sections class", e.Dropped)
	}
	if !e.RaiseBool("full_tool_bodies", false) {
		t.Fatal("an inert posture swallowed the share raise it was authorized to apply")
	}
}

// TestHonoredAuthorityStripsExtractionOnIndividualPlane verifies the claim
// case (b) rests on, directly: the individual plane is kept inert by the ONE
// tenancy gate in HonoredAuthority, so the share class does not re-check
// tenancy and there is still exactly one place that decision lives.
func TestHonoredAuthorityStripsExtractionOnIndividualPlane(t *testing.T) {
	tokens := []string{
		AuthorityExtractManaged, AuthorityExtractCache, AuthorityExtractTerminal,
		AuthorityExtractToolBodies, AuthorityExtractCodeintel, AuthorityExtractProcess,
		AuthorityExtractFolders, AuthorityExtractTraces, AuthorityExtractRouting,
		AuthorityExtractPredictions,
	}
	for _, tok := range tokens {
		if !ExtractionAuthority(tok) {
			t.Fatalf("%s is not classified as extraction authority", tok)
		}
		if !ManagedAuthority(tok) {
			t.Fatalf("%s is extraction authority but not managed-only", tok)
		}
		honored := HonoredAuthority(managedRaiseGrant(ConsentInteractive, AuthorityCapturePin, tok))
		for _, h := range honored {
			if h == tok {
				t.Fatalf("%s survived HonoredAuthority on an individual grant", tok)
			}
		}
	}
	// The enforce.* tokens are managed-only, but NOT extraction: they must
	// never authorize the share class.
	for _, tok := range []string{AuthorityEnforceRouting, AuthorityEnforceAdmission, AuthorityEnforceEgress} {
		if ExtractionAuthority(tok) {
			t.Fatalf("%s classified as extraction authority — enforcement mode is not extraction", tok)
		}
	}
	// And neither is the lowering token, nor the retired one.
	for _, tok := range []string{AuthorityCapturePin, AuthorityCaptureRaise, AuthorityDashboardVisibility, AuthoritySettingsPin, AuthorityFeatureLock} {
		if ExtractionAuthority(tok) {
			t.Fatalf("%s classified as extraction authority", tok)
		}
	}
}
