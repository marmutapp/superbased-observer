package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAcquireWriterRemoteSingleAuthorizationCallSite is the call-site invariant
// (§8.1 structural): the manager's AcquireWriterRemote — the ONLY path that turns
// a WriterGrant into a live remote writer lease — must be reached from exactly one
// place in cmd, the launchManagerAdapter that runs termlease.Authorize. No other
// cmd code may fabricate a grant and drive the manager. Likewise termlease.Authorize
// (the sole WriterGrant minter) is called from exactly one cmd site. A new caller of
// either fails this test and forces a review.
func TestAcquireWriterRemoteSingleAuthorizationCallSite(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var mgrCallSites, authorizeCallSites []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src := string(b)
		// The manager writer-acquire seam (a.mgr.AcquireWriterRemote / mgr.AcquireWriterRemote).
		if strings.Contains(src, ".AcquireWriterRemote(") {
			mgrCallSites = append(mgrCallSites, f)
		}
		// The sole grant minter.
		if strings.Contains(src, "termlease.Authorize(") {
			authorizeCallSites = append(authorizeCallSites, f)
		}
	}

	// Note: launch_dashboard.go contains BOTH the dashboard-seam adapter method
	// AcquireWriterRemote(req) AND the manager call a.mgr.AcquireWriterRemote — the
	// single authorization assembly point. It must be the only cmd file with either.
	assertSingle(t, "manager AcquireWriterRemote call site", mgrCallSites, "launch_dashboard.go")
	assertSingle(t, "termlease.Authorize call site", authorizeCallSites, "launch_dashboard.go")
}

func assertSingle(t *testing.T, what string, files []string, want string) {
	t.Helper()
	if len(files) != 1 {
		t.Fatalf("%s: expected exactly ONE cmd file, found %v — a fabricated-grant bypass or a second authorizer landed", what, files)
	}
	if filepath.Base(files[0]) != want {
		t.Fatalf("%s: expected it in %s, found %s", what, want, files[0])
	}
}
