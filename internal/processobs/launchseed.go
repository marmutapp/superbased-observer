package processobs

import (
	"sort"
	"strings"
	"time"
)

// Launch-seed matching (migration 086). A launcher records its child pid at
// spawn (the only moment it is knowable); the daemon's correlation sweep
// consumes the seed once the watcher has ingested a REAL session for that
// tool. This file is the pure pairing rule — no SQL, no I/O — so it stays
// pinned inside the processobs pure-logic boundary by imports_test.go.
//
// The bridge row this match produces is authoritative HIGH-confidence
// identity (every pidbridge reader treats it so), so the rule is deliberately
// CONSERVATIVE: exact tool, exact cwd, and a bounded start window, with an
// INJECTIVE pairing so two simultaneous launches in the same project can
// never cross-attribute.

// LaunchSeedBackSkew tolerates a seed recorded BEFORE its session row's
// started_at. The session timestamp comes from the tool's own first logged
// event, which can land after the process spawn (same shape as
// crossOSBackSkew's launch-to-first-prompt gap). Sized to match
// CrossOSWindow so the anchor window is symmetric.
const LaunchSeedBackSkew = 2 * time.Minute

// LaunchSeedForwardWindow bounds how long after a spawn a session may start
// and still match: a tool opened to an idle prompt creates its session only
// on first activity. Past this window a seed can no longer be matched
// reliably (another launch in the same project is likelier) and the sweep
// expires it instead.
const LaunchSeedForwardWindow = 15 * time.Minute

// LaunchSeed is one pending launcher-recorded child process (a pending
// launch_seeds row).
type LaunchSeed struct {
	PID       int
	Tool      string
	CWD       string
	StartedAt time.Time
}

// MatchLaunchSeeds pairs seeds to sessions and returns {pid → session_id}.
//
// A session matches a seed iff tool and project root are equal and the
// session started within [seed−LaunchSeedBackSkew, seed+LaunchSeedForwardWindow].
// Pairing is greedy and injective: seeds are walked oldest-first, each taking
// the EARLIEST-STARTED unclaimed matching session. Injectivity is the
// two-simultaneous-sessions guard — one session can never absorb two seeds,
// and two same-project launches pair deterministically instead of both
// anchoring to whichever session sorts first.
//
// A seed whose pid already appears in claimed is skipped (the caller uses
// this to honour an existing session_pid_bridge row without re-matching).
func MatchLaunchSeeds(seeds []LaunchSeed, sessions []CrossOSSessionRef, claimed map[int]bool) map[int]string {
	type candidate struct {
		session CrossOSSessionRef
		seed    LaunchSeed
	}
	var cands []candidate
	for _, s := range seeds {
		if s.PID <= 0 || s.Tool == "" || claimed[s.PID] {
			continue
		}
		for _, sess := range sessions {
			if sess.SessionID == "" || !launchSeedToolEqual(s.Tool, sess.Tool) {
				continue
			}
			if s.CWD != "" && sess.ProjectRoot != s.CWD {
				continue
			}
			if sess.StartedAt.Before(s.StartedAt.Add(-LaunchSeedBackSkew)) ||
				sess.StartedAt.After(s.StartedAt.Add(LaunchSeedForwardWindow)) {
				continue
			}
			cands = append(cands, candidate{session: sess, seed: s})
		}
	}

	// Deterministic order: seeds oldest-first; within a seed, sessions
	// earliest-started-first (ties broken by id for stability).
	sort.Slice(cands, func(i, j int) bool {
		if !cands[i].seed.StartedAt.Equal(cands[j].seed.StartedAt) {
			return cands[i].seed.StartedAt.Before(cands[j].seed.StartedAt)
		}
		return cands[i].session.StartedAt.Before(cands[j].session.StartedAt)
	})

	out := make(map[int]string)
	takenSession := make(map[string]bool)
	for _, c := range cands {
		if out[c.seed.PID] != "" || takenSession[c.session.SessionID] {
			continue
		}
		out[c.seed.PID] = c.session.SessionID
		takenSession[c.session.SessionID] = true
	}
	return out
}

// launchSeedToolEqual compares tool names case-insensitively: launcher verbs
// and adapter canonical names agree today, but the comparison must not
// silently break if either side changes case.
func launchSeedToolEqual(a, b string) bool {
	return strings.EqualFold(a, b)
}
