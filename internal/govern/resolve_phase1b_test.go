package govern

import (
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/policyfam/nodegov"
)

func phase1bSpec(t *testing.T, body string) nodegov.PolicySpec {
	t.Helper()
	spec, _, err := nodegov.CompileBody([]byte(body), 1<<20)
	if err != nil {
		t.Fatalf("CompileBody(%s): %v", body, err)
	}
	return spec
}

func phase1bDelivered(t *testing.T, body string) Delivered {
	return Delivered{Present: true, Version: 14, BodyHash: "bh", Spec: phase1bSpec(t, body)}
}

func phase1bGrant(tokens ...string) *Grant {
	g := testGrant()
	g.Authority = tokens
	return g
}

// TestPhase1bDirectiveClassesIntersectAuthority is one row per NEW directive
// class: each is applied only when the grant carries its own token, and a
// class the grant does not authorize is DROPPED and recorded.
func TestPhase1bDirectiveClassesIntersectAuthority(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		authority []string
		wantApply func(Effective) bool
		wantDrop  string
	}{
		{
			name: "pinned applied with settings.pin",
			body: `{"schema":2,"pinned":{"guard.enabled":true}}`, authority: []string{AuthoritySettingsPin},
			wantApply: func(e Effective) bool { return e.Pinned["guard.enabled"] == true },
		},
		{
			name: "pinned dropped without settings.pin",
			body: `{"schema":2,"pinned":{"guard.enabled":true}}`, authority: []string{AuthorityDashboardVisibility},
			wantApply: func(e Effective) bool { return len(e.Pinned) == 0 },
			wantDrop:  ReasonNotPreauthorized,
		},
		{
			name: "share applied with capture.pin",
			body: `{"schema":2,"share":{"full_content":false}}`, authority: []string{AuthorityCapturePin},
			wantApply: func(e Effective) bool { return e.Share["full_content"] == false },
		},
		{
			name: "share dropped without capture.pin",
			body: `{"schema":2,"share":{"full_content":false}}`, authority: []string{AuthoritySettingsPin},
			wantApply: func(e Effective) bool { return len(e.Share) == 0 },
			wantDrop:  ReasonNotPreauthorized,
		},
		{
			// The Phase-1a grant shape: capture.raise is retired, so the
			// share directive is dropped with the reason that actually helps
			// (the fix is a fresh enrol, not an admin action).
			name: "share dropped as authority_retired when the grant holds only capture.raise",
			body: `{"schema":2,"share":{"full_content":false}}`, authority: []string{AuthorityCaptureRaise},
			wantApply: func(e Effective) bool { return len(e.Share) == 0 },
			wantDrop:  ReasonAuthorityRetired,
		},
		{
			name: "features applied with feature.lock and expand into pins",
			body: `{"schema":2,"features":{"guard":true}}`, authority: []string{AuthorityFeatureLock},
			wantApply: func(e Effective) bool {
				return e.Features["guard"] && e.Pinned["guard.enabled"] == true
			},
		},
		{
			name: "features dropped without feature.lock",
			body: `{"schema":2,"features":{"guard":true}}`, authority: []string{AuthoritySettingsPin},
			wantApply: func(e Effective) bool { return len(e.Features) == 0 && len(e.Pinned) == 0 },
			wantDrop:  ReasonNotPreauthorized,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Every case also carries dashboard.visibility unless the case is
			// specifically about it, so the always-present `sections` class
			// does not add noise to the drop list.
			auth := append([]string{AuthorityDashboardVisibility}, tc.authority...)
			e := Resolve(phase1bDelivered(t, tc.body), phase1bGrant(auth...), testLive(), testNow)
			if !tc.wantApply(e) {
				t.Fatalf("posture = %+v", e)
			}
			if tc.wantDrop == "" {
				if len(e.Dropped) != 0 {
					t.Fatalf("unexpected drops: %+v", e.Dropped)
				}
				if e.State != StateApplied {
					t.Fatalf("State = %q, want applied", e.State)
				}
				return
			}
			if len(e.Dropped) != 1 || e.Dropped[0].Reason != tc.wantDrop {
				t.Fatalf("Dropped = %+v, want one %q", e.Dropped, tc.wantDrop)
			}
			if e.State != StateInert {
				t.Fatalf("State = %q, want inert — a partial application must never report as convergence", e.State)
			}
		})
	}
}

// TestCaptureRaiseGrantsNothing is the retirement, stated as the property.
// It is checked against EVERY directive class, so a future reader cannot
// quietly re-point the token at one of them.
func TestCaptureRaiseGrantsNothing(t *testing.T) {
	if !KnownAuthority(AuthorityCaptureRaise) {
		t.Fatal("capture.raise left KnownAuthority — an older grant carrying it would report as unknown_authority instead of retired")
	}
	if !RetiredAuthority(AuthorityCaptureRaise) {
		t.Fatal("capture.raise is not reported as retired")
	}
	if RetiredAuthority(AuthorityCapturePin) {
		t.Fatal("capture.pin is reported as retired")
	}
	body := `{"schema":2,"sections":{"hidden":["benchmarks"]},"pinned":{"guard.enabled":true},"share":{"full_content":false},"features":{"codeintel":false}}`
	e := Resolve(phase1bDelivered(t, body), phase1bGrant(AuthorityCaptureRaise), testLive(), testNow)
	if len(e.HiddenSections) != 0 || len(e.Pinned) != 0 || len(e.Share) != 0 || len(e.Features) != 0 {
		t.Fatalf("a retired token granted something: %+v", e)
	}
	if len(e.Dropped) != 4 {
		t.Fatalf("Dropped = %+v, want all four classes dropped", e.Dropped)
	}
}

// TestPinnedChangeChangesEffectiveHash: the resolved hash must cover the
// Phase-1b classes, or a changed pin would look like convergence on the
// previous posture.
func TestPinnedChangeChangesEffectiveHash(t *testing.T) {
	grant := phase1bGrant(AuthorityDashboardVisibility, AuthoritySettingsPin, AuthorityCapturePin, AuthorityFeatureLock)
	bodies := []string{
		`{"schema":2,"pinned":{"guard.enabled":true}}`,
		`{"schema":2,"pinned":{"guard.enabled":false}}`,
		`{"schema":2,"share":{"full_content":false}}`,
		`{"schema":2,"features":{"codeintel":false}}`,
		`{"schema":1}`,
	}
	seen := map[string]string{}
	for _, body := range bodies {
		e := Resolve(phase1bDelivered(t, body), grant, testLive(), testNow)
		if prior, dup := seen[e.Hash]; dup {
			t.Fatalf("%s hashes identically to %s", body, prior)
		}
		seen[e.Hash] = body
	}
}

// TestPartialApplicationHashesDifferently restates the §3.7 honesty rule for
// the new classes: a posture that dropped a directive can never hash like
// the one that applied it.
func TestPartialApplicationHashesDifferently(t *testing.T) {
	body := `{"schema":2,"pinned":{"guard.enabled":true}}`
	full := Resolve(phase1bDelivered(t, body),
		phase1bGrant(AuthorityDashboardVisibility, AuthoritySettingsPin), testLive(), testNow)
	partial := Resolve(phase1bDelivered(t, body),
		phase1bGrant(AuthorityDashboardVisibility), testLive(), testNow)
	if full.Hash == partial.Hash {
		t.Fatal("the dropped-pin posture hashes like the applied one — fleet state could not tell them apart")
	}
}

// TestExpiredGrantDropsPinsToo: the offboarding backstop covers every class,
// not just sections.
func TestExpiredGrantDropsPinsToo(t *testing.T) {
	expired := phase1bGrant(AuthorityDashboardVisibility, AuthoritySettingsPin, AuthorityCapturePin)
	expired.ExpiresAt = testNow.Add(-time.Minute)
	e := Resolve(phase1bDelivered(t, `{"schema":2,"pinned":{"guard.enabled":true},"share":{"full_content":false}}`),
		expired, testLive(), testNow)
	if e.Active || e.State != StateGrantExpired {
		t.Fatalf("expired grant still governing: %+v", e)
	}
	if len(e.Pinned) != 0 || len(e.Share) != 0 {
		t.Fatalf("expired grant still pinning: %+v", e)
	}
}
