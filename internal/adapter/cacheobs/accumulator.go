package cacheobs

import (
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// DefaultMaxBlocksPerSession is the per-session running block-count
// cap every existing Tier-2 producer uses (claudecode, opencode,
// kilo-code CLI, cline-cli). It bounds the watcher's memory growth
// on a runaway transcript; once exceeded, Emit degrades to
// BlockHashes=nil (the engine falls back to kind=reanchor for that
// turn) rather than accumulating without bound.
const DefaultMaxBlocksPerSession = 4096

// Accumulator is the per-session Tier-2 cache-observation
// accumulator. It carries the running content-block delta since
// the last Emit, the cumulative block-count cap, and the
// compaction-since-last-emit flag.
//
// THE MIRROR-ABLE TEMPLATE (spec §14.3): "ObserveBlocks appends in
// array order; ObserveCompaction resets and flags; Emit produces
// one CacheTurnObservation with the delta since the last emit then
// resets." This shape is adapter-independent — every existing
// Tier-2 producer duplicated it verbatim before this package
// existed. A new adapter constructs one Accumulator per session
// and drives it from its own message/part walk; only the
// per-record canonicalization (which JSON fields become a
// CacheBlockMeta) stays adapter-specific.
type Accumulator struct {
	// pendingBlocks is the delta since the last Emit. The engine
	// receives this slice in CacheTurnObservation.BlockHashes and
	// pushes each entry into its rolling chain IN ORDER — preserving
	// append order here is the chain-determinism guard (spec §0 R3).
	pendingBlocks []models.CacheBlockMeta
	// totalBlocks is the cumulative count across all emits since
	// either accumulator creation or the last ObserveCompaction.
	// Used to gate capExceeded.
	totalBlocks int
	// compactionSeen carries forward to the next Emit so the engine
	// can flip kind=compaction_reset on that turn.
	compactionSeen bool
	// capExceeded latches true once totalBlocks > maxBlocks; every
	// subsequent Emit carries BlockHashes=nil until the accumulator
	// is reset by ObserveCompaction (or a fresh Accumulator is
	// created for a new session).
	capExceeded bool
	// maxBlocks is the cap; <= 0 disables capping entirely (used by
	// tests exercising the uncapped path).
	maxBlocks int
}

// New returns a fresh Accumulator with the given cap. maxBlocks <= 0
// disables the cap. Callers typically pass DefaultMaxBlocksPerSession.
func New(maxBlocks int) *Accumulator {
	return &Accumulator{maxBlocks: maxBlocks}
}

// ObserveBlocks appends already-canonicalized CacheBlockMeta rows —
// produced by the adapter's own part/block marshaller — to the
// running delta, in the order given. A no-op once the cap has been
// exceeded or when blocks is empty.
//
// ORDER PRESERVED: callers must hand blocks in source-transcript
// order (no Go-map iteration upstream) — this is the R3
// byte-stability guard the engine's rolling chain hash depends on.
func (a *Accumulator) ObserveBlocks(blocks []models.CacheBlockMeta) {
	if a.capExceeded || len(blocks) == 0 {
		return
	}
	a.pendingBlocks = append(a.pendingBlocks, blocks...)
	a.totalBlocks += len(blocks)
	if a.maxBlocks > 0 && a.totalBlocks > a.maxBlocks {
		a.capExceeded = true
		// Drop the running buffer too — once capped the engine can't
		// reconstruct the chain anyway, and the memory cost of
		// carrying it forward is the whole reason for the cap.
		a.pendingBlocks = nil
	}
}

// ObserveCompaction marks the next Emit as following a
// compact_boundary lifecycle marker and clears the running
// accumulator (including the cap counter) — a long-running session
// that compacts regularly stays within memory bounds.
func (a *Accumulator) ObserveCompaction() {
	a.pendingBlocks = nil
	a.totalBlocks = 0
	a.capExceeded = false
	a.compactionSeen = true
}

// Emit builds the CacheTurnObservation for one assistant turn with
// usage, resetting pendingBlocks + compactionSeen for the next
// turn's delta. The returned observation carries BlockHashes=nil
// when the accumulator has capExceeded.
//
// SourceEventID is SourceEventID(messageID) so the (SourceFile,
// SourceEventID) idempotency key stays unique per turn and a
// re-parse from offset 0 produces byte-identical observations.
func (a *Accumulator) Emit(
	path, sessionID, messageID, model string,
	ts time.Time,
	usage models.CacheUsage,
	fast bool,
) models.CacheTurnObservation {
	obs := models.CacheTurnObservation{
		SourceFile:     path,
		SourceEventID:  SourceEventID(messageID),
		SessionID:      sessionID,
		MessageID:      messageID,
		Timestamp:      ts,
		Model:          model,
		Fast:           fast,
		Usage:          usage,
		CompactionSeen: a.compactionSeen,
	}
	if !a.capExceeded {
		obs.BlockHashes = a.pendingBlocks
	}
	a.pendingBlocks = nil
	a.compactionSeen = false
	return obs
}
