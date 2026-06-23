package guard

import "sync"

// repeatTracker is the A-610 stuck-loop state (spec §12.2): per
// session, the length of the current consecutive-identical-action
// run. Owned solely by the Guard (one owner per piece of state,
// §17.4); fed from the watcher ingest path. "Identical" means same
// action type AND same target — a formatter re-run on the same file
// counts, touching a different file resets.
type repeatTracker struct {
	mu       sync.Mutex
	sessions map[string]*repeatRun
}

// repeatRun is one session's current run.
type repeatRun struct {
	key   string
	count int
}

// maxRepeatSessions bounds the tracker (the taint-tracker bound
// shape); overflow evicts an arbitrary session — losing a run resets
// a counter, never invents one.
const maxRepeatSessions = 256

// Observe records one action and returns the run length INCLUDING it.
func (rt *repeatTracker) Observe(sessionID, actionType, target string) int {
	if sessionID == "" {
		return 0
	}
	key := actionType + "\x1f" + target
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.sessions == nil {
		rt.sessions = make(map[string]*repeatRun)
	}
	run := rt.sessions[sessionID]
	if run == nil {
		if len(rt.sessions) >= maxRepeatSessions {
			for k := range rt.sessions {
				delete(rt.sessions, k)
				break
			}
		}
		run = &repeatRun{}
		rt.sessions[sessionID] = run
	}
	if run.key == key {
		run.count++
	} else {
		run.key = key
		run.count = 1
	}
	return run.count
}
