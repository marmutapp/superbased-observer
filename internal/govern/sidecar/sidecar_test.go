package sidecar

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeSidecar(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "governance-effective.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// TestRoundTrip: what the daemon encodes is what a reader decodes.
func TestRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	in := File{
		Schema: MaxSchema, WriterVersion: "1.31.0", WrittenAt: FormatTime(now),
		State: StateApplied, OrgKey: "ok", Generation: 7, OrgName: "Acme",
		FamilyVersion: 14, EffectiveHash: "deadbeef",
		GrantExpiresAt: FormatTime(now.Add(30 * 24 * time.Hour)),
		Pinned:         map[string]any{"guard.enabled": true, "guard.mode": "enforce"},
		Share:          map[string]any{"full_content": false},
		Features:       map[string]bool{"guard": true},
	}
	raw, err := Encode(in)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	path := writeSidecar(t, t.TempDir(), string(raw))
	got, reason := Read(path, now)
	if got == nil {
		t.Fatalf("Read returned no overlay, reason %q", reason)
	}
	if got.Pinned["guard.enabled"] != true || got.Pinned["guard.mode"] != "enforce" {
		t.Fatalf("pinned = %+v", got.Pinned)
	}
	if got.EffectiveHash != "deadbeef" || got.FamilyVersion != 14 {
		t.Fatalf("identity fields lost: %+v", got)
	}
}

// TestEncodeIsCanonical: two semantically-equal postures must produce
// identical bytes, or the writer's change detection would rewrite the file
// (and churn written_at) on every tick.
func TestEncodeIsCanonical(t *testing.T) {
	a, _ := Encode(File{Schema: 1, State: StateApplied, Pinned: map[string]any{"b": true, "a": false}})
	b, _ := Encode(File{Schema: 1, State: StateApplied, Pinned: map[string]any{"a": false, "b": true}})
	if string(a) != string(b) {
		t.Fatalf("map order leaked into the encoding:\n%s\n%s", a, b)
	}
}

// TestReadFailureTable walks §1.3: EVERY failure mode yields a nil overlay
// and a named reason, and none of them is an error.
func TestReadFailureTable(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	live := `{"schema":1,"state":"applied","pinned":{"guard.enabled":true}}`

	cases := []struct {
		name   string
		body   string
		want   string
		absent bool
		dir    bool
	}{
		{name: "absent", absent: true, want: ReasonAbsent},
		{name: "directory", dir: true, want: ReasonUnreadable},
		{name: "oversize", body: `{"schema":1,"state":"applied","org_name":"` + strings.Repeat("x", MaxBytes) + `"}`, want: ReasonOversize},
		{name: "malformed json", body: `{"schema":1,`, want: ReasonMalformed},
		{name: "unknown field", body: `{"schema":1,"state":"applied","surprise":1}`, want: ReasonMalformed},
		{name: "trailing bytes", body: `{"schema":1,"state":"applied"} {}`, want: ReasonMalformed},
		{name: "schema too new", body: `{"schema":99,"state":"applied"}`, want: ReasonSchemaTooNew},
		{name: "schema zero", body: `{"state":"applied"}`, want: ReasonSchemaTooNew},
		{name: "grant expired", body: `{"schema":1,"state":"applied","grant_expires_at":"2026-08-15T11:00:00Z"}`, want: ReasonGrantExpired},
		{name: "dormant state", body: `{"schema":1,"state":"no_grant"}`, want: ReasonNotApplied},
		{name: "dormant inert without maps", body: `{"schema":1,"state":"inert"}`, want: ReasonNotApplied},
		// Inert WITH a pinned map is a LIVE posture, not a dormant one:
		// govern.Resolve reports "inert" for any PARTIAL application (the
		// always-present sections class drops whenever the grant lacks
		// dashboard.visibility), so a pins-only deployment normally runs
		// inert. Gating the overlay on state=="applied" made every reader
		// discard pins the daemon had materialized and was reporting as
		// live — the writer/reader split the 2026-08-15 hook smoke caught.
		{name: "live inert with pins", body: `{"schema":1,"state":"inert","pinned":{"guard.enabled":true}}`, want: ReasonNone},
		{name: "live", body: live, want: ReasonNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "governance-effective.json")
			switch {
			case tc.absent:
			case tc.dir:
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
			default:
				writeSidecar(t, dir, tc.body)
			}
			got, reason := Read(path, now)
			if reason != tc.want {
				t.Fatalf("reason = %q, want %q", reason, tc.want)
			}
			if (got != nil) != (tc.want == ReasonNone) {
				t.Fatalf("overlay presence disagrees with the reason: got %v for %q", got != nil, reason)
			}
		})
	}
}

// TestExpiredGrantIgnoresSidecar is the offboarding guarantee for
// short-lived processes with a DEAD daemon (§1.5), stated at the file layer:
// it kills the perpetual-lock mutation the resolver-layer test cannot reach.
func TestExpiredGrantIgnoresSidecar(t *testing.T) {
	dir := t.TempDir()
	expiry := time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC)
	path := writeSidecar(t, dir, `{"schema":1,"state":"applied","grant_expires_at":"`+
		expiry.Format(time.RFC3339)+`","pinned":{"guard.enabled":true}}`)

	if f, reason := Read(path, expiry.Add(-time.Hour)); f == nil {
		t.Fatalf("a live grant was ignored: %q", reason)
	}
	f, reason := Read(path, expiry.Add(time.Second))
	if f != nil || reason != ReasonGrantExpired {
		t.Fatalf("an expired grant still governed: overlay=%v reason=%q", f != nil, reason)
	}
}

// TestZeroExpiryNeverExpires: an absent grant_expires_at means "no TTL",
// matching govern.Resolve's treatment of a zero ExpiresAt. It must NOT read
// as the zero instant, which would expire every such sidecar immediately.
func TestZeroExpiryNeverExpires(t *testing.T) {
	path := writeSidecar(t, t.TempDir(), `{"schema":1,"state":"applied","pinned":{"guard.enabled":true}}`)
	if f, reason := Read(path, time.Now().AddDate(50, 0, 0)); f == nil {
		t.Fatalf("a TTL-less sidecar expired: %q", reason)
	}
}

// TestParseTimeAbsentVsZero pins the reason the stamps are strings.
func TestParseTimeAbsentVsZero(t *testing.T) {
	if _, ok := ParseTime(""); ok {
		t.Fatal("empty parsed as a real instant")
	}
	if _, ok := ParseTime("not-a-time"); ok {
		t.Fatal("garbage parsed as a real instant")
	}
	if FormatTime(time.Time{}) != "" {
		t.Fatal("the zero time must render as absent, not as year 1")
	}
}
