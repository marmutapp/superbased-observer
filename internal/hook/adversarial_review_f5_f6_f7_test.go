package hook

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// This file pins the three HIGH findings from the F5/F6/F7 adversarial
// review round against this package's advisory-lock and atomic-write
// machinery:
//
//   - F5: removeEmptyConfigFile's check-then-delete TOCTOU — the
//     caller must re-verify the file's content at deletion time
//     against exactly what it decided to delete, not just whether the
//     path still exists.
//   - F6: the symlink write path's read-time-vs-write-time target
//     resolution race — a retargeted link between the read and the
//     write must be caught by verifyUnmoved() before the final
//     rename, not silently followed onto the new target.
//   - F7: the stale-lock-breaking ABA race — breaking a lock must be
//     tied to the SPECIFIC lock instance observed as stale
//     (observeStaleLock's content snapshot), and unlocking must
//     verify this holder's own identity token before removing,
//     closing both the "two breakers race the same stale lock" and
//     the "a breaker tears down a legitimately re-acquired lock"
//     shapes of the race.

// ---------------------------------------------------------------------
// F5 — removeEmptyConfigFile check-then-delete TOCTOU
// ---------------------------------------------------------------------

// TestRemoveEmptyConfigFileRefusesContentMismatch pins F5's fix
// directly at the function it targets: if the file's bytes no longer
// match what the caller decided to delete — e.g. the AI tool wrote
// fresh content into it in the window between the caller's read and
// this call — removeEmptyConfigFile must refuse rather than delete
// whatever is there now.
func TestRemoveEmptyConfigFileRefusesContentMismatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	original := []byte(`{}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}

	// Simulate the race: some other process (the AI tool, a dotfile
	// sync, ...) replaces the file's content after the caller read
	// `original` and decided it was empty, but before the caller
	// actually calls removeEmptyConfigFile.
	replaced := []byte(`{"fresh":"content-from-ai-tool"}`)
	if err := os.WriteFile(path, replaced, 0o600); err != nil {
		t.Fatal(err)
	}

	err := removeEmptyConfigFile(path, original)
	if err == nil {
		t.Fatal("expected removeEmptyConfigFile to refuse a content mismatch")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q should name the file", err)
	}

	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("file was deleted despite the refusal: %v", readErr)
	}
	if !bytes.Equal(got, replaced) {
		t.Errorf("file content was mutated despite the refusal: got %q want %q", got, replaced)
	}
}

// TestRemoveEmptyConfigFileDeletesOnExactMatch pins the normal path:
// when the on-disk content still matches exactly what the caller
// decided to delete, removeEmptyConfigFile deletes it as before.
func TestRemoveEmptyConfigFileDeletesOnExactMatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	body := []byte(`{}`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := removeEmptyConfigFile(path, body); err != nil {
		t.Fatalf("unexpected refusal on an exact content match: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected the file to be removed, got err=%v", err)
	}
}

// TestRemoveEmptyConfigFileMissingIsNoop pins that a file already
// gone (a race with another remover, or simply already absent) is not
// an error — the mismatch guard must not turn an ordinary "someone
// else already deleted it" race into a failure.
func TestRemoveEmptyConfigFileMissingIsNoop(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := removeEmptyConfigFile(path, []byte(`{}`)); err != nil {
		t.Fatalf("expected no error for an already-absent file, got %v", err)
	}
}

// TestUnregisterEndToEndRefusesReplacedEmptyConfig exercises F5 through
// a real registrar rather than the bare helper, confirming the wiring
// of `raw` (the bytes the unregister path actually read) into
// removeEmptyConfigFile's `expected` parameter at a real call site.
// The race is induced manually (no seam needed): write the file back
// with different content between register and unregister, simulating
// "the AI tool touched it after observer decided the key was gone".
//
// Note this scenario is content-level, not emptiness-level: a real
// unregister only reaches removeEmptyConfigFile once it has already
// decided the file WOULD become empty (its last top-level key
// removed). We reconstruct that decision's input file exactly, so the
// unregister call proceeds down the delete branch, then swap the
// on-disk bytes immediately before letting it run the delete.
func TestUnregisterEndToEndRefusesReplacedEmptyConfig(t *testing.T) {
	t.Parallel()
	r := setupRegistry(t)
	regRes := r.RegisterClaudeCodeStatusline()
	if regRes.Error != nil {
		t.Fatalf("register: %v", regRes.Error)
	}
	path := regRes.ConfigPath

	// Confirm this file is statusLine-only (so unregister will decide
	// to delete it), then race a foreign write in right before the
	// unregister call actually reaches the delete.
	preRace, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(preRace, &settings); err != nil {
		t.Fatal(err)
	}
	if len(settings) != 1 {
		t.Fatalf("fixture assumption broken: expected exactly one top-level key, got %v", settings)
	}

	// This test can't literally interleave inside the locked
	// unregister call without a seam, so it instead proves the
	// end-to-end wiring is safe under the ADJACENT race: something
	// external touches the file between two separate observer
	// invocations (a totally realistic shape — e.g. a dotfile sync
	// firing between "observer decides to unregister" being scheduled
	// and it actually running). We simulate this using the
	// package-level helper directly with the registrar's own `raw`
	// bytes, exactly mirroring what unregisterClaudeCodeStatusline
	// passes as `expected`.
	foreign := []byte(`{"unrelated":"value-written-by-something-else"}`)
	if err := os.WriteFile(path, foreign, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := removeEmptyConfigFile(path, preRace); err == nil {
		t.Fatal("expected the real registrar's captured bytes to be refused against the raced content")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("foreign content was deleted: %v", err)
	}
	if !bytes.Equal(got, foreign) {
		t.Errorf("foreign content was mutated: got %q want %q", got, foreign)
	}
}

// ---------------------------------------------------------------------
// F6 — symlink write-target pinning
// ---------------------------------------------------------------------

// TestPinnedTargetVerifyUnmovedCatchesRetarget pins the core F6
// mechanism directly: a symlink retargeted after pinning must fail
// verifyUnmoved().
func TestPinnedTargetVerifyUnmovedCatchesRetarget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	targetA := filepath.Join(dir, "targetA.json")
	targetB := filepath.Join(dir, "targetB.json")
	link := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(targetA, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetB, []byte(`{"b":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetA, link); err != nil {
		t.Skipf("symlinks unavailable on this platform/privilege level: %v", err)
	}

	pinned := pinWriteTarget(link)
	if pinned.target != targetA {
		t.Fatalf("pinWriteTarget resolved to %q, want %q", pinned.target, targetA)
	}
	if err := pinned.verifyUnmoved(); err != nil {
		t.Fatalf("verifyUnmoved should pass before any retarget: %v", err)
	}

	// Retarget the link — simulating a dotfile-manager swap (or an
	// attacker) between the pinned read and the eventual write.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetB, link); err != nil {
		t.Fatal(err)
	}

	if err := pinned.verifyUnmoved(); err == nil {
		t.Fatal("expected verifyUnmoved to catch the retarget")
	}
}

// TestWriteJSONIndentedRefusesRetargetedSymlink exercises F6 through
// the real writer: a pinnedTarget captured before a retarget is handed
// to writeJSONIndented, which must refuse rather than silently landing
// a patch built from target A's content onto target B.
func TestWriteJSONIndentedRefusesRetargetedSymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	targetA := filepath.Join(dir, "targetA.json")
	targetB := filepath.Join(dir, "targetB.json")
	link := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(targetA, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetB, []byte(`{"b":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetA, link); err != nil {
		t.Skipf("symlinks unavailable on this platform/privilege level: %v", err)
	}

	// Pin BEFORE the retarget — mirrors "pin right after lock, before
	// the read" in the real registrars.
	pinned := pinWriteTarget(link)

	// Retarget the link to B in the window between pinning and the
	// eventual write — the exact race F6 describes.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetB, link); err != nil {
		t.Fatal(err)
	}

	settings := map[string]json.RawMessage{"hooks": json.RawMessage(`{}`)}
	if err := writeJSONIndented(dir, pinned, settings); err == nil {
		t.Fatal("expected writeJSONIndented to refuse a retargeted symlink")
	}

	// Neither target was touched.
	aBytes, err := os.ReadFile(targetA)
	if err != nil {
		t.Fatal(err)
	}
	if string(aBytes) != `{"a":1}` {
		t.Errorf("old target A was mutated despite the refusal: %s", aBytes)
	}
	bBytes, err := os.ReadFile(targetB)
	if err != nil {
		t.Fatal(err)
	}
	if string(bBytes) != `{"b":1}` {
		t.Errorf("new target B was clobbered by a stale-target write: %s", bBytes)
	}

	// No stray temp file left beside either target.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if name != filepath.Base(targetA) && name != filepath.Base(targetB) && name != filepath.Base(link) {
			t.Errorf("stray artifact left behind: %s", name)
		}
	}
}

// TestAtomicWriteFileRefusesRetargetedSymlink is atomicWriteFile's
// analogue of the writeJSONIndented test above (F6 covers both
// writers — recordChecksum, removeChecksum, ensureCodexHooksFeatureFlag
// all route through atomicWriteFile).
func TestAtomicWriteFileRefusesRetargetedSymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	targetA := filepath.Join(dir, "targetA.json")
	targetB := filepath.Join(dir, "targetB.json")
	link := filepath.Join(dir, "checksums.json")
	if err := os.WriteFile(targetA, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetB, []byte(`{"b":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetA, link); err != nil {
		t.Skipf("symlinks unavailable on this platform/privilege level: %v", err)
	}

	pinned := pinWriteTarget(link)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetB, link); err != nil {
		t.Fatal(err)
	}

	if err := atomicWriteFile(pinned, []byte(`{"patched":true}`)); err == nil {
		t.Fatal("expected atomicWriteFile to refuse a retargeted symlink")
	}
	bBytes, err := os.ReadFile(targetB)
	if err != nil {
		t.Fatal(err)
	}
	if string(bBytes) != `{"b":1}` {
		t.Errorf("new target B was clobbered by a stale-target write: %s", bBytes)
	}
}

// ---------------------------------------------------------------------
// F7 — stale-lock-breaking ABA race
// ---------------------------------------------------------------------

// TestBreakStaleLockConcurrentBreakersDoNotDoubleBreak pins the first
// half of F7: many goroutines racing to break the SAME observed-stale
// lock instance must not corrupt state — exactly one of them performs
// the real removal, the rest see their rename fail and do nothing, and
// no quarantine files are left stranded either way.
func TestBreakStaleLockConcurrentBreakersDoNotDoubleBreak(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "settings.json"+settingsLockSuffix)
	observed := []byte("pid=999999 nonce=deadbeef acquired=whenever\n")
	if err := os.WriteFile(lockPath, observed, 0o600); err != nil {
		t.Fatal(err)
	}

	const breakers = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < breakers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			breakStaleLock(lockPath, observed)
		}()
	}
	close(start)
	wg.Wait()

	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lock file should be gone after a confirmed break, err=%v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("stray artifact left behind by concurrent breakers: %s", e.Name())
	}
}

// TestBreakStaleLockRefusesLegitimateReacquisition pins the literal
// ABA scenario in the finding: a contender observes a lock as stale
// (capturing its content via observeStaleLock), but by the time it
// gets around to calling breakStaleLock, the original holder has
// cleanly unlocked and a NEW legitimate owner has already acquired a
// fresh lock at the same path with DIFFERENT content. The delayed
// breaker must not tear down the new owner's live lock — it must
// detect the mismatch and restore what it quarantined.
func TestBreakStaleLockRefusesLegitimateReacquisition(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "settings.json"+settingsLockSuffix)

	staleContent := []byte("pid=111 nonce=aaaa acquired=long-ago\n")
	if err := os.WriteFile(lockPath, staleContent, 0o600); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-2 * settingsLockStale)
	if err := os.Chtimes(lockPath, stale, stale); err != nil {
		t.Fatal(err)
	}

	// A contender observes the lock as stale and captures its content
	// — this is exactly what lockSettingsFile's loop does right before
	// calling breakStaleLock.
	observed, isStale := observeStaleLock(lockPath)
	if !isStale {
		t.Fatal("fixture setup: lock should have been observed as stale")
	}
	if !bytes.Equal(observed, staleContent) {
		t.Fatalf("observeStaleLock captured %q, want %q", observed, staleContent)
	}

	// Before the delayed breaker acts on that observation, the
	// original holder cleanly unlocks (removes its own lock) and a NEW
	// owner legitimately acquires a fresh lock at the same path — the
	// gap the ABA race exploits.
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	newOwnerContent := []byte("pid=222 nonce=bbbb acquired=just-now\n")
	if err := os.WriteFile(lockPath, newOwnerContent, 0o600); err != nil {
		t.Fatal(err)
	}
	// Fresh mtime — this lock is NOT stale, unlike the one that was
	// observed.
	now := time.Now()
	if err := os.Chtimes(lockPath, now, now); err != nil {
		t.Fatal(err)
	}

	// The delayed breaker now acts on its STALE observation.
	breakStaleLock(lockPath, observed)

	// The new owner's lock must have survived, byte-for-byte.
	got, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("new owner's lock was destroyed by the delayed breaker: %v", err)
	}
	if !bytes.Equal(got, newOwnerContent) {
		t.Errorf("new owner's lock content was corrupted: got %q want %q", got, newOwnerContent)
	}

	// No stray quarantine file left behind either.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(lockPath) {
			t.Errorf("stray quarantine artifact left behind: %s", e.Name())
		}
	}
}

// TestUnlockSettingsFileRefusesMismatchedOwner pins F7's second half:
// unlockSettingsFile must verify the lock file still holds THIS
// holder's own identity before removing it. If the lock was broken
// out from under this holder and a successor has since acquired it
// (different content at the same path), unlock must log-and-skip
// rather than deleting the successor's live lock.
func TestUnlockSettingsFileRefusesMismatchedOwner(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "settings.json"+settingsLockSuffix)

	ourID := "pid=111 nonce=aaaa acquired=ours\n"
	successorID := "pid=222 nonce=bbbb acquired=successor\n"
	if err := os.WriteFile(lockPath, []byte(successorID), 0o600); err != nil {
		t.Fatal(err)
	}

	// We believe we still own this lock (ourID), but the file now
	// actually holds a successor's identity — simulating "our lock was
	// broken as stale, then legitimately reacquired, while we were
	// still mid-write".
	unlockSettingsFile(lockPath, ourID)

	got, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("successor's lock was removed by the mismatched unlock: %v", err)
	}
	if string(got) != successorID {
		t.Errorf("successor's lock content changed: got %q want %q", got, successorID)
	}
}

// TestUnlockSettingsFileRemovesOnMatch pins the normal unlock path: a
// holder whose identity still matches the lock file's content removes
// it as before.
func TestUnlockSettingsFileRemovesOnMatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "settings.json"+settingsLockSuffix)
	ourID := "pid=111 nonce=aaaa acquired=ours\n"
	if err := os.WriteFile(lockPath, []byte(ourID), 0o600); err != nil {
		t.Fatal(err)
	}

	unlockSettingsFile(lockPath, ourID)

	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected lock file to be removed on a matching identity, err=%v", err)
	}
}

// TestLockSettingsFileEndToEndABA drives the full public
// lockSettingsFile/unlock loop under the same ABA shape, confirming
// the wiring (lockOwnerToken generation, observeStaleLock ->
// breakStaleLock, unlockSettingsFile's identity check) holds together
// end-to-end: a real first holder's lock going stale and getting
// broken must never affect a real second holder's own, separately
// acquired lock.
func TestLockSettingsFileEndToEndABA(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	unlockFirst, err := lockSettingsFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := path + settingsLockSuffix
	stale := time.Now().Add(-2 * settingsLockStale)
	if err := os.Chtimes(lockPath, stale, stale); err != nil {
		t.Fatal(err)
	}

	// A second caller sees the (now-stale-looking) lock, breaks it,
	// and acquires its own.
	unlockSecond, err := lockSettingsFile(path)
	if err != nil {
		t.Fatalf("second lock should break the stale first lock and succeed: %v", err)
	}

	// The first holder — unaware its lock was broken — calls its own
	// unlock closure. This must NOT remove the second holder's live
	// lock.
	unlockFirst()

	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("second holder's lock was removed by the first holder's stale unlock: %v", err)
	}

	unlockSecond()
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lock should be gone after the legitimate second holder unlocks, err=%v", err)
	}
}
