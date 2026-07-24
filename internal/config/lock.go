package config

import "sync"

// configRMWMu is the process-wide serialization point for config.toml
// read-modify-write spans. It is deliberately package-level so that EVERY
// in-process writer of the global config — the dashboard's config handlers
// (which hold it across load→patch→validate→write), the obs admission-policy
// persister in cmd/observer, and any future daemon-side editor — contends on
// ONE mutex rather than each guarding the file behind its own lock. Two
// independent locks over the same file give a classic lost update: both read
// the same base, each patches its own section, the second write clobbers the
// first. writeBytesAtomic makes each write atomic PER FILE, but atomicity is
// not serialization — the guard has to span the whole read-modify-write, and
// that span must be shared across packages.
var configRMWMu sync.Mutex

// WithConfigLock runs fn while holding the process-wide config read-modify-write
// lock, then releases it. Callers whose whole load→modify→validate→write span
// fits a `func() error` closure should use this; it serializes that entire span
// against every other in-process config RMW, not merely the final file write.
//
// SCOPE — this serializes IN-PROCESS writers only. Separate OS processes (the
// operator-driven `observer experiment` / `observer profile` / `observer
// config` CLI commands, which each open, edit, and WriteToml the same file in
// their own process) are NOT serialized by this mutex — a mutex has no reach
// across process boundaries. That residual daemon-vs-CLI race is accepted for
// now: the CLI commands are operator-driven and rare, and colliding with a live
// daemon config edit is an unlikely, operator-visible event. The upgrade path
// is an advisory file lock (flock) around the same span so cross-process
// writers coordinate too; it is intentionally NOT attempted here. When it
// lands, WithConfigLock is the single seam to wrap it in.
func WithConfigLock(fn func() error) error {
	configRMWMu.Lock()
	defer configRMWMu.Unlock()
	return fn()
}

// WriteLock returns the process-wide config read-modify-write mutex as a
// *sync.Mutex so callers that cannot express their critical section as a
// `func() error` closure — an HTTP handler that must write its response, set
// restart flags, or conditionally unlock partway through — can hold the SAME
// lock WithConfigLock takes, via defer-style Lock/Unlock. Prefer WithConfigLock
// for clean closure-shaped spans; reach for this only where the closure form
// would force an awkward restructure. Both share configRMWMu, so a handler
// holding this and a persister inside WithConfigLock serialize against each
// other. The same in-process-only scope caveat documented on WithConfigLock
// applies.
func WriteLock() *sync.Mutex {
	return &configRMWMu
}
