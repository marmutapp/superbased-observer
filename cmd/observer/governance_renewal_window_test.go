package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// TestRenewalNeverExceedsSignedWindow is mini-spec §6.2's closed-form pin
// (§4.2 / R4): the renewed working expiry is EXACTLY
// now + (signed_expires_at - granted_at) — the window the organization
// actually signed for — never a second more, and a grant with no positive
// signed window never renews at all (the M1 silent-failure class).
func TestRenewalNeverExceedsSignedWindow(t *testing.T) {
	granted := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	signed := granted.Add(30 * 24 * time.Hour)
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		grant   store.EnrolmentGrant
		wantOK  bool
		wantExp time.Time
	}{
		{
			name:    "closed form: now + signed TTL, exactly",
			grant:   store.EnrolmentGrant{GrantedAt: granted, SignedExpiresAt: signed, ExpiresAt: signed},
			wantOK:  true,
			wantExp: now.Add(signed.Sub(granted)),
		},
		{
			name:   "zero signed window never renews (M1)",
			grant:  store.EnrolmentGrant{GrantedAt: granted, ExpiresAt: signed},
			wantOK: false,
		},
		{
			name:   "zero granted_at never renews",
			grant:  store.EnrolmentGrant{SignedExpiresAt: signed, ExpiresAt: signed},
			wantOK: false,
		},
		{
			name:   "non-positive signed TTL never renews",
			grant:  store.EnrolmentGrant{GrantedAt: granted, SignedExpiresAt: granted, ExpiresAt: granted},
			wantOK: false,
		},
		{
			name: "sub-threshold move suppressed (rate limit, not extension)",
			grant: store.EnrolmentGrant{
				GrantedAt: granted, SignedExpiresAt: signed,
				ExpiresAt: now.Add(signed.Sub(granted)).Add(-time.Minute),
			},
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := renewedExpiry(tc.grant, now)
			if ok != tc.wantOK {
				t.Fatalf("renewedExpiry ok = %v, want %v (got %v)", ok, tc.wantOK, got)
			}
			if !ok {
				return
			}
			if !got.Equal(tc.wantExp) {
				t.Fatalf("renewed expiry = %v, want exactly %v", got, tc.wantExp)
			}
			if got.Sub(now) > signed.Sub(granted) {
				t.Fatalf("renewed expiry %v exceeds the signed window %v", got.Sub(now), signed.Sub(granted))
			}
		})
	}
}

// TestOrgBundleWiresGovernanceSidecarPath pins the CLI-side wiring the
// 2026-08-15 smoke found missing: `observer unenroll` runs through
// buildOrgBundle, and unless THAT client learns the sidecar path, unenroll
// orphans the file and pins keep applying until its embedded grant clock
// expires. start.go's wiring alone is not enough.
func TestOrgBundleWiresGovernanceSidecarPath(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	body := "[observer]\ndb_path = \"" + filepath.ToSlash(filepath.Join(dir, "observer.db")) + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	b, err := buildOrgBundle(context.Background(), cfgPath)
	if err != nil {
		t.Fatalf("buildOrgBundle: %v", err)
	}
	defer b.cleanup()
	want := config.ResolveGovernanceSidecarPath(b.cfg, "")
	if want == "" {
		t.Fatal("test setup: sidecar path resolved empty")
	}
	if got := b.client.GovernanceSidecarPath(); got != want {
		t.Fatalf("bundle client sidecar path = %q, want %q", got, want)
	}
}
