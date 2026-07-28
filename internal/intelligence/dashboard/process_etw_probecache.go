package dashboard

import (
	"context"
	"path/filepath"
	"sync"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/processbridge/setup"
)

// etwProbeTTL bounds how often GET /api/process/etw/status may actually run
// its probes.
//
// It is a RATE FLOOR, not a freshness policy, and the distinction decides the
// value. What it exists to stop is a paired remote device (capability View
// reaches this route) driving repeated LOCAL PROCESS EXECUTION: every
// applicable GET execs `schtasks.exe /Query`, and an absent task additionally
// shells out to cmd.exe for the Windows user name. Nothing else bounded that —
// the card's own ~20s poll is a convention, not a limit, and N devices multiply
// it. Five seconds caps the whole install at ~12 probe rounds a minute however
// many tabs and devices poll, while staying far shorter than the human loop it
// sits in (open the card, approve a UAC prompt, come back), so no operator ever
// waits on a stale answer.
//
// The config that keys the entry is re-read on EVERY request, so the one place
// staleness would actually be felt — the card's own "turn it on" button, which
// writes config and immediately re-reads status — misses the cache by
// construction rather than by luck.
const etwProbeTTL = 5 * time.Second

// etwProbeKey is the config-derived identity of a cached probe round. Every
// field is an input ResolveInputs actually consults; a change in any of them
// means the cached answer describes a different question, so the entry is
// recomputed regardless of its age.
type etwProbeKey struct {
	processEnabled bool
	etwEnabled     bool
	listenAddr     string
	tokenPath      string
	windowsBinary  string
	observerDir    string
}

// etwProbeResult is one cached round: everything the status endpoint learns by
// touching the machine.
type etwProbeResult struct {
	inputs          setup.Inputs
	schtasksPresent bool
}

// etwProbeCache holds the single most recent probe round. One entry is enough:
// the key is the config, and a daemon has exactly one config.
type etwProbeCache struct {
	mu    sync.Mutex
	key   etwProbeKey
	at    time.Time
	res   etwProbeResult
	valid bool
}

// etwProbe returns the resolved planner inputs for cfg, running the real probes
// at most once per etwProbeTTL per distinct config.
//
// It is used ONLY by the status endpoint. The elevation broker deliberately
// calls setup.ResolveInputs directly: its second pass exists to re-check
// preconditions AT the spawn boundary, and a re-check served from a cache
// populated by an earlier request would be a re-check in name only.
func (s *Server) etwProbe(ctx context.Context, cfg config.Config, env setup.Env) etwProbeResult {
	pc := cfg.Observer.Process
	observerDir := filepath.Dir(cfg.Observer.DBPath)
	key := etwProbeKey{
		processEnabled: pc.Enabled,
		etwEnabled:     pc.ETW.Enabled,
		listenAddr:     pc.ETW.ListenAddr,
		tokenPath:      pc.ETW.TokenPath,
		windowsBinary:  pc.WindowsBinaryPath,
		observerDir:    observerDir,
	}
	now := s.now()

	s.etwProbes.mu.Lock()
	if s.etwProbes.valid && s.etwProbes.key == key && now.Sub(s.etwProbes.at) < etwProbeTTL {
		res := s.etwProbes.res
		s.etwProbes.mu.Unlock()
		return res
	}
	s.etwProbes.mu.Unlock()

	// Probing happens OUTSIDE the lock: it execs, and holding a mutex across a
	// process spawn would serialise every poller behind the slowest interop
	// shell. Two concurrent misses may both probe — harmless duplication of a
	// read-only query, and far better than a lock held across an exec.
	res := etwProbeResult{
		inputs:          setup.ResolveInputs(ctx, pc, observerDir, env),
		schtasksPresent: setup.HasSchtasks(env),
	}
	// A probe cut short by the CALLER going away (a browser navigating mid-poll
	// cancels the request context) resolves to ProbeUnknown — an honest answer
	// for that request, but not one to hand the next five seconds of callers.
	// Return it, do not remember it.
	if ctx.Err() != nil {
		return res
	}
	s.etwProbes.mu.Lock()
	s.etwProbes.key, s.etwProbes.at, s.etwProbes.res, s.etwProbes.valid = key, now, res, true
	s.etwProbes.mu.Unlock()
	return res
}
