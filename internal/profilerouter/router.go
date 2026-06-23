package profilerouter

import "sync"

// DefaultMaxSessions bounds the sticky session map when Options
// leaves MaxSessions unset. Mirrors cachetrack's session LRU bound —
// a session evicted under pressure simply re-resolves on next sight
// (worst case it lands on a newer assignment version mid-session,
// which is the documented bounded-memory tradeoff).
const DefaultMaxSessions = 64

// Options configures a Router. Resolve, Build, and Fallback are
// required; the rest default sensibly.
type Options[C any] struct {
	// Resolve maps a traffic class to an INSTANCE KEY under the
	// CURRENT assignment table: provider ("anthropic", "openai"), the
	// pidbridge-resolved owning tool ("" when unresolved — R2), that
	// session's working directory ("" when unresolved — R3 project
	// overrides), and the session ID ("" on the per-request path —
	// experiment arm splits key on it, P6.4). For plain assignments
	// the key is the profile name; callers may fold extra generation
	// data (project root, file stamp) into it — the router only
	// requires that equal keys mean an identical parameter set.
	// Swapped as a whole by Update so a snapshot is never mutated in
	// place.
	Resolve func(provider, tool, cwd, sessionID string) string
	// Build constructs the compressor stack for an instance key (as
	// produced by Resolve). Called at most once per (key, version) —
	// concurrent first sights of the same key wait on the router
	// mutex rather than building twice. Must be cheap; shared
	// infrastructure belongs in the closure, built once outside.
	Build func(instanceKey string) (C, error)
	// DefaultProfile is the name whose instance is pre-seeded from
	// Fallback (and re-seeded on every Update). Sessions whose
	// profile fails to build fall back to this instance — resolution
	// never blocks traffic on a broken profile.
	DefaultProfile string
	// Fallback is the guaranteed instance for DefaultProfile,
	// constructed eagerly by the caller from master parameters.
	Fallback C
	// MaxSessions bounds the sticky map; ≤0 selects
	// DefaultMaxSessions.
	MaxSessions int
	// Warnf, when non-nil, receives one line per (profile, version)
	// whose Build failed. Printf-shaped so callers can pass a
	// slog-backed closure.
	Warnf func(format string, args ...any)
	// OnResolve, when non-nil, fires once per session at first
	// resolution (sessionID non-empty) — the live-verify /
	// debug-evidence hook. Called with the router mutex held; keep
	// it cheap (a log line). tool/cwd are the R2/R3 inputs ("" when
	// unresolved); key is Resolve's instance key.
	OnResolve func(provider, tool, cwd, sessionID, key string)
}

// Router is a session-sticky compressor router. Safe for concurrent
// use. The zero value is not usable — construct with New.
type Router[C any] struct {
	mu        sync.Mutex
	opts      Options[C]
	version   uint64
	byProfile map[string]C // instance cache, current version only
	failed    map[string]bool
	sessions  map[string]*stickyEntry[C]
	touch     uint64 // monotonic; equal-timestamp ambiguity is the D4 lesson
}

type stickyEntry[C any] struct {
	c         C
	lastTouch uint64
}

// New constructs a Router and pre-seeds the default profile's
// instance from Fallback.
func New[C any](opts Options[C]) *Router[C] {
	if opts.MaxSessions <= 0 {
		opts.MaxSessions = DefaultMaxSessions
	}
	r := &Router[C]{
		opts:     opts,
		failed:   map[string]bool{},
		sessions: map[string]*stickyEntry[C]{},
	}
	r.byProfile = map[string]C{opts.DefaultProfile: opts.Fallback}
	return r
}

// For returns the compressor instance for one request. A non-empty
// sessionID resolves once and sticks; an empty sessionID resolves
// against the current version on every call (deterministic within a
// version, so no stickiness is needed). tool and cwd are the
// pidbridge-resolved R2/R3 inputs ("" skips those tiers).
func (r *Router[C]) For(provider, tool, cwd, sessionID string) C {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.touch++
	if sessionID != "" {
		if e, ok := r.sessions[sessionID]; ok {
			e.lastTouch = r.touch
			return e.c
		}
	}
	key := r.opts.Resolve(provider, tool, cwd, sessionID)
	c := r.instanceLocked(key)
	if sessionID != "" {
		r.stickLocked(sessionID, c)
		if r.opts.OnResolve != nil {
			r.opts.OnResolve(provider, tool, cwd, sessionID, key)
		}
	}
	return c
}

// Update swaps the assignment table AND the default-profile fallback
// instance, bumping the version: the instance cache resets so NEW
// sessions resolve against fresh builds, while existing sticky
// sessions keep the instances they started on. The fallback is
// re-supplied because master parameters may have changed too — the
// caller rebuilds it from the freshly loaded config. This is the P2.5
// hot-reload seam — the dashboard config-save path calls it after a
// [profiles] / compression write.
func (r *Router[C]) Update(resolve func(provider, tool, cwd, sessionID string) string, fallback C) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.version++
	r.opts.Resolve = resolve
	r.opts.Fallback = fallback
	r.byProfile = map[string]C{r.opts.DefaultProfile: fallback}
	r.failed = map[string]bool{}
}

// maxInstances bounds the instance cache. Project-override stamps
// fold file generations into keys, so repeated repo-file edits mint
// new keys; at the cap the cache resets to just the default seed —
// sticky sessions keep their references, and rebuilding a handful of
// pipelines is cheap.
const maxInstances = 256

// instanceLocked returns the cached instance for name, building it on
// first sight. A failed build warns once per (name, version) and
// pins the default-profile instance under the failed name so traffic
// proceeds on master parameters instead of blocking or re-erroring
// per request.
func (r *Router[C]) instanceLocked(name string) C {
	if c, ok := r.byProfile[name]; ok {
		return c
	}
	if len(r.byProfile) >= maxInstances {
		r.byProfile = map[string]C{r.opts.DefaultProfile: r.opts.Fallback}
		r.failed = map[string]bool{}
	}
	c, err := r.opts.Build(name)
	if err != nil {
		if !r.failed[name] {
			r.failed[name] = true
			if r.opts.Warnf != nil {
				r.opts.Warnf("profilerouter: build profile %q failed; falling back to %q: %v",
					name, r.opts.DefaultProfile, err)
			}
		}
		c = r.byProfile[r.opts.DefaultProfile]
	}
	r.byProfile[name] = c
	return c
}

// stickLocked pins sessionID to c, evicting the least-recently
// touched session at the cap (linear scan — the map is small and the
// pattern matches cachetrack's session LRU).
func (r *Router[C]) stickLocked(sessionID string, c C) {
	if len(r.sessions) >= r.opts.MaxSessions {
		var oldestKey string
		var oldestTouch uint64
		first := true
		for k, e := range r.sessions {
			if first || e.lastTouch < oldestTouch {
				oldestKey = k
				oldestTouch = e.lastTouch
				first = false
			}
		}
		delete(r.sessions, oldestKey)
	}
	r.sessions[sessionID] = &stickyEntry[C]{c: c, lastTouch: r.touch}
}
