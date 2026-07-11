package antigravity

import (
	"os"
	"path/filepath"
)

// Snapshot persistence for bridge / language_server structured
// trajectory responses. Phase 2 of
// docs/plans/antigravity-token-coverage-design-2026-05-24.md.
//
// Premise: agy.exe (and Antigravity's IDE language_server) holds
// each conversation's cascade trajectory in MEMORY ONLY. Once the
// hosting process exits, any future `GetCascadeTrajectory` against
// a different instance returns `trajectory not found` — and the
// adapter falls back to the plaintext transcript path which has
// no token data. Once we ever successfully get the structured
// trajectory bytes for a conversation, snapshotting them to disk
// means future polls always have something better than the empty
// plaintext path to fall back on.
//
// Quality metric: number of TokenEvents the snapshot would yield
// when re-parsed. We keep whichever snapshot has more — preserves
// the case where the bridge sometimes returns a valid-looking but
// turn-truncated response (the originating agy.exe terminated; a
// running sibling instance returns only the first cached turn).
//
// Storage shape: one file per conversation, named `<uuid>.bin`,
// containing the raw `GetCascadeTrajectory` protobuf bytes. The
// directory accumulates one entry per conversation observer has
// ever seen — ~150 KB/file in steady state. An `observer doctor`
// prune step is queued separately (kickoff §6.5).

// snapshotDir returns the directory where structured-trajectory
// snapshots live. Resolves once from the user's home dir; cached
// to avoid repeated UserHomeDir calls.
//
// Falls back to a process-temp subdir when UserHomeDir errors —
// snapshots are best-effort acceleration, NOT correctness-
// critical, so degrading silently is preferred over surfacing a
// recovery-blocking error.
func snapshotDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "observer-antigravity-snapshots")
	}
	return filepath.Join(home, ".observer", "antigravity-snapshots")
}

// snapshotPathFor returns the canonical .bin path for a conversation.
func snapshotPathFor(conversationID string) string {
	return filepath.Join(snapshotDir(), conversationID+".bin")
}

// readSnapshot returns the cached structured-trajectory bytes for
// the conversation, or (nil, false) when no snapshot exists or the
// file can't be read. Empty files are treated as missing.
func readSnapshot(conversationID string) ([]byte, bool) {
	if conversationID == "" {
		return nil, false
	}
	b, err := os.ReadFile(snapshotPathFor(conversationID))
	if err != nil || len(b) == 0 {
		return nil, false
	}
	return b, true
}

// writeSnapshot atomically persists raw bytes as the conversation's
// new snapshot. Atomic via write-to-tmp-then-rename so concurrent
// readers never see a partially-written file (matters when the
// daemon polls one conversation while a probe-cli invocation reads
// the same snapshot from another process). Best-effort — failures
// are surfaced via tracef and do not propagate, because snapshot
// persistence is acceleration, not correctness.
func writeSnapshot(conversationID string, data []byte) {
	if conversationID == "" || len(data) == 0 {
		return
	}
	dir := snapshotDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		tracef("snapshot_mkdir_err dir=%s err=%v", dir, err)
		return
	}
	dst := snapshotPathFor(conversationID)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		tracef("snapshot_write_err path=%s err=%v", tmp, err)
		return
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		tracef("snapshot_rename_err src=%s dst=%s err=%v", tmp, dst, err)
		return
	}
	tracef("snapshot_written conv=%s bytes=%d", conversationID, len(data))
}

// reconcileWithSnapshot parses `raw` (newly returned by the bridge
// or native gRPC) and reconciles it against any existing on-disk
// snapshot. The "winner" is whichever produces more TokenEvents —
// that's the conversation-coverage metric the operator-facing
// dashboard cares about. The losing payload is discarded.
//
// On the first call for a conversation (no snapshot yet), `raw` is
// written and its enrichment returned. On every subsequent call,
// the snapshot is replaced only if the new bytes yield strictly
// more TokenEvents. Ties keep the cached version — the bridge call
// already paid its cost, but writing a same-quality snapshot would
// burn IO without changing observer's view.
//
// projectRoot + path are passed through to ParseStructuredTrajectory
// for both the new and (when present) the cached parse so the
// returned StructuredEnrichment carries the correct attribution
// regardless of which payload won.
func (a *Adapter) reconcileWithSnapshot(
	conversationID, projectRoot, path string, raw []byte,
) StructuredEnrichment {
	newEn := ParseStructuredTrajectory(raw, conversationID, projectRoot, path, a.scrubber)
	cached, ok := readSnapshot(conversationID)
	if !ok {
		writeSnapshot(conversationID, raw)
		return newEn
	}
	cachedEn := ParseStructuredTrajectory(cached, conversationID, projectRoot, path, a.scrubber)
	if len(cachedEn.TokenEvents) > len(newEn.TokenEvents) {
		tracef("snapshot_kept_cached conv=%s cached_tokens=%d new_tokens=%d",
			conversationID, len(cachedEn.TokenEvents), len(newEn.TokenEvents))
		return cachedEn
	}
	writeSnapshot(conversationID, raw)
	return newEn
}

// loadSnapshotEnrichment is the bridge-failure fallback: parse and
// return the on-disk snapshot, or zero-value when none exists.
// Distinct from reconcileWithSnapshot because the failure path
// has no `raw` to compare against — the only choice is "use cache
// or surface nothing".
func (a *Adapter) loadSnapshotEnrichment(
	conversationID, projectRoot, path string,
) (StructuredEnrichment, bool) {
	cached, ok := readSnapshot(conversationID)
	if !ok {
		return StructuredEnrichment{}, false
	}
	en := ParseStructuredTrajectory(cached, conversationID, projectRoot, path, a.scrubber)
	tracef("snapshot_fallback conv=%s bytes=%d tools=%d tokens=%d",
		conversationID, len(cached), len(en.ToolEvents), len(en.TokenEvents))
	return en, true
}
