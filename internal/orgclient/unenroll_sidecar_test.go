package orgclient

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestUnenrollRemovesSidecar is mini-spec §6.2's offboarding pin (§4.5 step
// 2b): leaving the organisation must delete the governance sidecar, or hook
// processes keep applying pins from the orphan until its embedded grant
// clock expires. The 2026-08-15 live smoke found the MECHANISM present but
// UNWIRED on the CLI path (only start.go set the path) — the cmd side is
// pinned by TestOrgBundleWiresGovernanceSidecarPath; this test pins the
// client mechanism itself.
func TestUnenrollRemovesSidecar(t *testing.T) {
	s := newAgentStore(t)
	bs := &memBearerStore{}
	c := newTestClient(t, s, bs)
	ctx := context.Background()

	enrolFixture(t, s, bs, "https://org.acme.example")

	path := filepath.Join(t.TempDir(), "governance-effective.json")
	if err := os.WriteFile(path, []byte(`{"schema":1,"state":"inert","pinned":{"guard.enabled":true}}`), 0o600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	c.SetGovernanceSidecarPath(path)

	if err := c.Unenroll(ctx); err != nil {
		t.Fatalf("Unenroll: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("governance sidecar survived unenroll (stat err=%v) — pins would keep applying until the orphan's grant clock expires", err)
	}
}

// TestUnenrollWithoutSidecarPathStillSucceeds: a client that never learned
// the path (older wiring, or a caller that has no config) must not fail the
// unenroll — the removal is best-effort by design.
func TestUnenrollWithoutSidecarPathStillSucceeds(t *testing.T) {
	s := newAgentStore(t)
	bs := &memBearerStore{}
	c := newTestClient(t, s, bs)
	enrolFixture(t, s, bs, "https://org.acme.example")
	if err := c.Unenroll(context.Background()); err != nil {
		t.Fatalf("Unenroll without sidecar path: %v", err)
	}
}
