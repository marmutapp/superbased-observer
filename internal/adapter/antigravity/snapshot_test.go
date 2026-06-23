package antigravity

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSnapshotRoundtrip pins the basic write→read cycle, including
// atomic-rename semantics (no .tmp left behind) and the empty-file =
// missing contract.
func TestSnapshotRoundtrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	// os.UserHomeDir reads USERPROFILE on Windows, not HOME, so set
	// both to keep snapshotDir resolving inside tmp on every test host.
	t.Setenv("USERPROFILE", tmp)
	conv := "test-uuid-roundtrip"
	want := []byte{0x08, 0x01, 0x12, 0x05, 'h', 'e', 'l', 'l', 'o'}

	if _, ok := readSnapshot(conv); ok {
		t.Fatalf("snapshot should not exist before write")
	}
	writeSnapshot(conv, want)
	got, ok := readSnapshot(conv)
	if !ok {
		t.Fatalf("snapshot should exist after write")
	}
	if string(got) != string(want) {
		t.Errorf("snapshot bytes mismatch: got %x, want %x", got, want)
	}
	// No leftover .tmp from the atomic rename.
	matches, _ := filepath.Glob(filepath.Join(snapshotDir(), "*.tmp"))
	if len(matches) > 0 {
		t.Errorf("leftover .tmp files: %v", matches)
	}
}

// TestSnapshotEmptyConversationNoOp pins that empty UUIDs and empty
// payloads are no-ops — neither write nor crash. Matters because
// historyOnlyResult may hand us empty bytes when the bridge returned
// zero-length on a desktop conv with rotated cipher + offline IDE.
func TestSnapshotEmptyConversationNoOp(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	// os.UserHomeDir reads USERPROFILE on Windows, not HOME.
	t.Setenv("USERPROFILE", tmp)
	writeSnapshot("", []byte("data"))
	writeSnapshot("conv-x", nil)
	if _, ok := readSnapshot(""); ok {
		t.Error("empty conversation ID should not produce a readable snapshot")
	}
	if _, ok := readSnapshot("conv-x"); ok {
		t.Error("empty payload should not produce a readable snapshot")
	}
}

// TestSnapshotPathStability pins that the path format doesn't drift
// silently — the snapshot dir lives at ~/.observer/antigravity-
// snapshots/<uuid>.bin and operator-facing docs reference this
// location for the queued doctor prune step.
func TestSnapshotPathStability(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	// os.UserHomeDir reads USERPROFILE on Windows, not HOME.
	t.Setenv("USERPROFILE", tmp)
	want := filepath.Join(tmp, ".observer", "antigravity-snapshots", "abc-123.bin")
	got := snapshotPathFor("abc-123")
	if got != want {
		t.Errorf("snapshot path: got %q, want %q", got, want)
	}
}

// TestLoadSnapshotEnrichmentMissesGracefully pins that
// loadSnapshotEnrichment returns ok=false (not a zero-value
// pretending to be real data) when no snapshot exists for the
// conversation — the fetch* callers depend on this to know whether
// to surface zero-value enrichment vs. fall through to the no-data
// branch.
func TestLoadSnapshotEnrichmentMissesGracefully(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	// os.UserHomeDir reads USERPROFILE on Windows, not HOME.
	t.Setenv("USERPROFILE", tmp)
	a := New()
	en, ok := a.loadSnapshotEnrichment("never-cached", "/p", "/p/x.pb")
	if ok {
		t.Errorf("expected ok=false for missing snapshot, got enrichment %+v", en)
	}
}

// TestReconcileWithSnapshotWritesFirstTime pins the first-write
// contract: when no snapshot exists, reconcileWithSnapshot writes
// raw immediately AND returns its enrichment. This is the path
// every cold start of every conversation takes.
func TestReconcileWithSnapshotWritesFirstTime(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	// os.UserHomeDir reads USERPROFILE on Windows, not HOME.
	t.Setenv("USERPROFILE", tmp)
	a := New()
	raw := []byte{0x0a, 0x05, 'h', 'e', 'l', 'l', 'o'} // arbitrary proto-looking bytes
	conv := "first-time-conv"
	_ = a.reconcileWithSnapshot(conv, "/p", "/p/x.pb", raw)
	got, ok := readSnapshot(conv)
	if !ok {
		t.Fatal("expected snapshot to be written on first reconcile")
	}
	if string(got) != string(raw) {
		t.Errorf("snapshot bytes = %x, want %x", got, raw)
	}
}

// TestSnapshotDirFallbackOnHomeError pins the silent-degradation
// contract: when UserHomeDir errors (rare — chrooted env, broken
// $HOME), the helper returns a temp-dir path instead of returning
// an error or panicking. Snapshots are best-effort acceleration;
// surfacing an error here would block the entire recovery path.
func TestSnapshotDirFallbackOnHomeError(t *testing.T) {
	t.Setenv("HOME", "")
	// Some Go runtimes still resolve UserHomeDir from /etc/passwd
	// even when HOME is empty; only assert the fallback contract
	// when we can actually trigger the error path.
	if _, err := os.UserHomeDir(); err == nil {
		t.Skip("UserHomeDir succeeded despite empty HOME — runtime resolves via passwd")
	}
	dir := snapshotDir()
	if dir == "" {
		t.Fatalf("snapshotDir returned empty string on HomeDir error")
	}
	if filepath.Base(dir) != "observer-antigravity-snapshots" {
		t.Errorf("expected temp-dir fallback name, got dir=%q", dir)
	}
}
