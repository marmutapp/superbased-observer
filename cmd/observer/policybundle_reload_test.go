package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/guard"
	"github.com/marmutapp/superbased-observer/internal/orgclient"
	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// P0-7 guard reload TRIGGER tests (§2.3): policyBundleRunner.onResult
// must hot-reload the LIVE guard org layer on an accepted poll, and on
// an unchanged poll ONLY when the running version has diverged from the
// cache (SF7 cold recovery). The guard reload itself is proven in
// internal/guard; these prove the cmd-layer trigger wiring fires it.

// bundleSigner returns a one-key signer + its enrolment pin hash.
func bundleSigner(t *testing.T) (sign func(version int64, bundleTOML string) string, pin string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	sign = func(version int64, bundleTOML string) string {
		b := orgcontract.PolicyBundle{
			Version:    version,
			BundleTOML: bundleTOML,
			Signature:  orgcontract.SignPolicyBundle(priv, version, []byte(bundleTOML)),
			PublicKey:  base64.RawURLEncoding.EncodeToString(pub),
			SignedAt:   "2026-06-11T09:00:00Z",
		}
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("marshal envelope: %v", err)
		}
		return string(raw)
	}
	return sign, orgcontract.PublicKeyPinHash(pub)
}

// mkOrgRuleTOML is a one-rule org bundle body with a version-unique ID.
func mkOrgRuleTOML(id, marker string) string {
	return fmt.Sprintf("[[rule]]\nid = %q\ncategory = \"exfil\"\nseverity = \"high\"\ndecision = \"ask\"\napplies_to = [\"shell_exec\"]\nmatch.command_regex = %q\n", id, marker)
}

// bundleGuardOnDisk builds a guard whose org bundle cache is a REAL file
// at bundlePath (read via os.ReadFile, the daemon path) seeded at v1.
func bundleGuardOnDisk(t *testing.T, sign func(int64, string) string, pin string) (*guard.Guard, string) {
	t.Helper()
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "org-policy-bundle.json")
	if err := os.WriteFile(bundlePath, []byte(sign(1, mkOrgRuleTOML("ORG-V1", "onlyv1"))), 0o600); err != nil {
		t.Fatalf("seed bundle: %v", err)
	}
	cfg := config.Default().Guard
	cfg.Rules.OrgBundle = filepath.ToSlash(bundlePath)
	g, err := guard.New(guard.Options{Config: cfg, Home: dir, OrgKeyPinHash: pin})
	if err != nil {
		t.Fatalf("guard.New: %v", err)
	}
	if got := orgRunningVersion(g); got != 1 {
		t.Fatalf("initial running org version = %d, want 1", got)
	}
	return g, bundlePath
}

func reloadTestRunner(g *guard.Guard) *policyBundleRunner {
	return &policyBundleRunner{
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		orgURL:  "https://org.example",
		acquire: func(context.Context) *guard.Guard { return g },
	}
}

// TestPolicyBundleRunner_AcceptedTriggersLiveReload: an accepted poll
// applies the just-cached version to the live engine with no restart.
// Mutation-ready: dropping reloadGuardLayer from the PolicyApplied arm
// leaves the running version at 1 → this fails.
func TestPolicyBundleRunner_AcceptedTriggersLiveReload(t *testing.T) {
	sign, pin := bundleSigner(t)
	g, bundlePath := bundleGuardOnDisk(t, sign, pin)
	r := reloadTestRunner(g)

	// The cache has advanced to v2 (FetchPolicyBundle wrote it before
	// returning PolicyApplied); the trigger must make it live.
	if err := os.WriteFile(bundlePath, []byte(sign(2, mkOrgRuleTOML("ORG-V2", "onlyv2"))), 0o600); err != nil {
		t.Fatalf("write v2: %v", err)
	}
	r.onResult(orgclient.PolicyResult{Status: orgclient.PolicyApplied, Version: 2})

	if got := orgRunningVersion(g); got != 2 {
		t.Fatalf("accepted poll did not apply to the live engine: running=%d, want 2", got)
	}
}

// TestPolicyBundleRunner_UnchangedReloadsOnDivergence: an unchanged poll
// reloads when the live running version is behind the cache (cold
// recovery), and does NOT change state in steady state. Mutation-ready:
// dropping the PolicyUnchanged reload leaves the divergence unconverged.
func TestPolicyBundleRunner_UnchangedReloadsOnDivergence(t *testing.T) {
	sign, pin := bundleSigner(t)
	g, bundlePath := bundleGuardOnDisk(t, sign, pin)
	r := reloadTestRunner(g)

	// Steady state: running == cached → no reload, running stays 1.
	r.onResult(orgclient.PolicyResult{Status: orgclient.PolicyUnchanged, CachedVersion: 1})
	if got := orgRunningVersion(g); got != 1 {
		t.Fatalf("steady-state unchanged poll changed running version: %d, want 1", got)
	}

	// Divergence: the cache advanced to v5 (some earlier reload never
	// landed) while the running engine is still at v1; the unchanged poll
	// carries CachedVersion=5 and must converge the live engine.
	if err := os.WriteFile(bundlePath, []byte(sign(5, mkOrgRuleTOML("ORG-V5", "onlyv5"))), 0o600); err != nil {
		t.Fatalf("write v5: %v", err)
	}
	r.onResult(orgclient.PolicyResult{Status: orgclient.PolicyUnchanged, CachedVersion: 5})
	if got := orgRunningVersion(g); got != 5 {
		t.Fatalf("divergent unchanged poll did not converge: running=%d, want 5", got)
	}
}
