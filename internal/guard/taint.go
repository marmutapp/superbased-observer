package guard

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/policy"
)

// taintTimeDecay is the time-based mark lifetime used when turn
// indexes are unavailable. Spec §4.5 defines decay in TURNS (default
// 10), but several adapters deliver actions without a usable
// TurnIndex (it stays 0); a mark that could never expire would make
// the T-5xx rules noisier over a long session, so a wall-clock
// fallback bounds the window. 30 minutes ≈ a generous upper estimate
// of 10 agent turns; documented approximation, revisited with the G6
// conformance data.
const taintTimeDecay = 30 * time.Minute

// maxTaintSessions bounds the tracker's session map; oldest-touched
// sessions are evicted on overflow. Watcher backfills can replay many
// historical sessions through Ingest in one process lifetime — the
// bound keeps the tracker from holding all of them forever.
const maxTaintSessions = 256

// maxMarksPerSession bounds marks kept per session; oldest marks
// drop first (they would decay first anyway).
const maxMarksPerSession = 32

// taintTracker is THE owner of per-session TaintState (spec §17.4).
// Policy rules only ever see the snapshot stamped onto an Event.
type taintTracker struct {
	cfg config.GuardTaintConfig

	mu       sync.Mutex
	sessions map[string]*sessionTaint
}

// sessionTaint is one session's live marks plus bookkeeping for
// decay and eviction.
type sessionTaint struct {
	marks     []policy.TaintMark
	lastTouch time.Time
}

func newTaintTracker(cfg config.GuardTaintConfig) *taintTracker {
	return &taintTracker{cfg: cfg, sessions: make(map[string]*sessionTaint)}
}

// Mark records a taint observation for a session. No-op when taint
// tracking is disabled or the session ID is empty.
func (t *taintTracker) Mark(sessionID string, mark policy.TaintMark) {
	if !t.cfg.Enabled || sessionID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.sessions[sessionID]
	if st == nil {
		if len(t.sessions) >= maxTaintSessions {
			t.evictOldestLocked()
		}
		st = &sessionTaint{}
		t.sessions[sessionID] = st
	}
	st.marks = append(st.marks, mark)
	if len(st.marks) > maxMarksPerSession {
		st.marks = st.marks[len(st.marks)-maxMarksPerSession:]
	}
	st.lastTouch = mark.At
}

// Snapshot returns the session's LIVE marks as of (turn, now),
// pruning decayed ones in place. Decay (spec §4.5): a mark expires
// after [guard.taint].decay_turns turns when BOTH the mark and the
// query carry non-zero turn indexes; otherwise after taintTimeDecay
// of wall-clock (the documented fallback).
func (t *taintTracker) Snapshot(sessionID string, turn int, now time.Time) policy.TaintState {
	if !t.cfg.Enabled || sessionID == "" {
		return policy.TaintState{}
	}
	decayTurns := t.cfg.DecayTurns
	if decayTurns <= 0 {
		decayTurns = 10
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.sessions[sessionID]
	if st == nil {
		return policy.TaintState{}
	}
	live := st.marks[:0]
	for _, m := range st.marks {
		if turn > 0 && m.Turn > 0 {
			if turn-m.Turn >= decayTurns {
				continue
			}
		} else if !m.At.IsZero() && !now.IsZero() && now.Sub(m.At) >= taintTimeDecay {
			continue
		}
		live = append(live, m)
	}
	st.marks = live
	if len(live) == 0 {
		return policy.TaintState{}
	}
	out := make([]policy.TaintMark, len(live))
	copy(out, live)
	return policy.TaintState{Marks: out}
}

// NoteCompaction clears a session's marks (spec §4.5: taint expires
// on session compaction — the tainted content left the context).
func (t *taintTracker) NoteCompaction(sessionID string) {
	if sessionID == "" {
		return
	}
	t.mu.Lock()
	delete(t.sessions, sessionID)
	t.mu.Unlock()
}

// evictOldestLocked removes the least-recently-touched session.
// Caller holds t.mu.
func (t *taintTracker) evictOldestLocked() {
	var oldestID string
	var oldest time.Time
	first := true
	for id, st := range t.sessions {
		if first || st.lastTouch.Before(oldest) {
			oldestID, oldest, first = id, st.lastTouch, false
		}
	}
	if oldestID != "" {
		delete(t.sessions, oldestID)
	}
}

// sha256hex is the shared content-hash helper for policy-state
// descriptors.
func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
