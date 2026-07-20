package remoteauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// Session errors.
var (
	// ErrTooManySessions is returned by SessionStore.Create when MaxSessions
	// is already reached.
	ErrTooManySessions = errors.New("remoteauth: too many active device sessions")
	// ErrNoSession is returned when a session id is unknown, expired, idle-timed
	// out, or belongs to a superseded generation.
	ErrNoSession = errors.New("remoteauth: no such active session")
)

// persistTimeout bounds every SessionPersister call so a slow/locked SQLite
// write can never wedge the SessionStore mutex indefinitely.
const persistTimeout = 5 * time.Second

// HashSessionID maps a RAW device-session bearer to the opaque HASH used as the
// server-side key everywhere (the in-memory map, the persisted row, the
// dashboard fingerprint, the CSRF/capability maps). It is sha256-hex: the RAW
// bearer is a 256-bit crypto/rand value, so a preimage attack on sha256 is
// infeasible and a slow KDF (argon2, used for the low-entropy pairing secret)
// buys nothing while costing a hash per request. A leaked observer.db therefore
// yields only hashes — never a usable cookie.
func HashSessionID(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// PersistedSession is one durable device-session row (migration 066). IDHash is
// the sha256-hex of the bearer — NEVER the raw token. Times are UTC.
type PersistedSession struct {
	IDHash    string
	Gen       uint64
	CreatedAt time.Time
	LastSeen  time.Time
}

// SessionPersister is the injected durable store for device sessions
// (persist-remote-sessions plan). internal/remoteauth stays pure (no
// database/sql, module rule #1); the SQLite implementation lives in
// internal/store. The security invariant is enforced by the CALLER's ordering
// (durable-first for mutations), documented per method here.
type SessionPersister interface {
	// LoadAll returns the durable generation + every persisted session. An
	// error is FATAL to session restore (the store fails closed: no sessions).
	LoadAll(ctx context.Context) (gen uint64, sessions []PersistedSession, err error)
	// Save inserts/updates one session row. Called BEFORE the cookie is handed
	// out (durable-first Create); an error means the cookie is never minted.
	Save(ctx context.Context, s PersistedSession) error
	// Touch is UPDATE-ONLY and generation-fenced: it must never INSERT (a
	// queued touch must not resurrect a Deleted row). Best-effort — a failure
	// only risks an earlier idle-expiry after a restart (the safe direction).
	Touch(ctx context.Context, idHash string, gen uint64, lastSeen time.Time) error
	// Delete removes one session row. Called BEFORE the in-memory drop for a
	// security revoke (durable-first); best-effort for expiry cleanup.
	Delete(ctx context.Context, idHash string) error
	// Reset atomically clears every session row and advances the durable
	// generation to at least gen. Called BEFORE the in-memory rotate
	// (durable-first); an error aborts the rotate with memory untouched.
	Reset(ctx context.Context, gen uint64) error
}

// SessionParams tunes the device-session lifecycle (plan §4.3). Zero fields
// fall back to safe defaults in NewSessionStore.
type SessionParams struct {
	// TTL is the absolute session lifetime. Default 12h.
	TTL time.Duration
	// Idle is the inactivity timeout. Default 1h.
	Idle time.Duration
	// Max is the cap on concurrent live sessions. Default 5. 0 ⇒ default.
	Max int
	// Now is the clock (test hook). Nil defaults to time.Now.UTC.
	Now func() time.Time
	// Persister, when non-nil, makes device sessions durable across a daemon
	// restart (persist-remote-sessions plan). Nil ⇒ pure in-memory behaviour
	// (the local owner dashboard is unaffected).
	Persister SessionPersister
}

// deviceSession is one authenticated device session. id is the HASH of the
// bearer (HashSessionID) — never the raw token, which lives only in the cookie.
type deviceSession struct {
	id        string
	gen       uint64
	createdAt time.Time
	lastSeen  time.Time
	// lastPersisted throttles Touch writes (a continuously-active session is
	// persisted at most every Idle/4, not once per request).
	lastPersisted time.Time
	// revoked is closed when the session is revoked/rotated/expired, so an
	// open privileged socket watching it can tear down (plan §4.3
	// "rotation ⇒ terminate privileged sockets").
	revoked chan struct{}
}

// SessionStore is the in-memory, one-owner device-session registry, optionally
// backed by a durable SessionPersister. Pairing mints a session here (Create);
// every remote request validates its cookie through Validate; rotation (Rotate)
// bumps the generation, invalidating ALL live sessions and closing their
// revocation channels. The map is keyed by the bearer HASH, so a leaked
// persisted DB never yields a usable cookie. Safe for concurrent use.
type SessionStore struct {
	mu        sync.Mutex
	params    SessionParams
	gen       uint64
	sessions  map[string]*deviceSession
	now       func() time.Time
	persister SessionPersister
}

// NewSessionStore builds a store with the given params (defaults applied). When
// a Persister is set it restores the durable generation + every non-expired,
// current-generation session (fresh revocation channels). A load error FAILS
// CLOSED: the store starts with no remote sessions, leaves the DB untouched,
// and logs loudly (remote access is optional — the daemon still boots).
func NewSessionStore(p SessionParams) *SessionStore {
	if p.TTL <= 0 {
		p.TTL = 12 * time.Hour
	}
	if p.Idle <= 0 {
		p.Idle = time.Hour
	}
	if p.Max <= 0 {
		p.Max = 5
	}
	now := p.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	s := &SessionStore{
		params:    p,
		gen:       1,
		sessions:  map[string]*deviceSession{},
		now:       now,
		persister: p.Persister,
	}
	if s.persister != nil {
		s.restore()
	}
	return s
}

// restore loads the durable generation + non-expired current-gen sessions. Fail
// closed on any error (empty store, DB untouched, loud log).
func (s *SessionStore) restore() {
	ctx, cancel := context.WithTimeout(context.Background(), persistTimeout)
	defer cancel()
	gen, rows, err := s.persister.LoadAll(ctx)
	if err != nil {
		slog.Error("remoteauth: failed to load persisted device sessions — starting with none (remote devices must re-pair)", "error", err)
		return
	}
	s.gen = gen
	now := s.now()
	for _, r := range rows {
		if r.Gen != gen {
			continue // superseded generation
		}
		ds := &deviceSession{
			id: r.IDHash, gen: r.Gen, createdAt: r.CreatedAt,
			lastSeen: r.LastSeen, lastPersisted: r.LastSeen,
			revoked: make(chan struct{}),
		}
		if s.isExpiredLocked(ds, now) {
			continue // TTL/idle-expired at load — never restore
		}
		s.sessions[r.IDHash] = ds
	}
}

// pctx returns a bounded context for one persister call.
func (s *SessionStore) pctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), persistTimeout)
}

// Create mints a new session id after evicting any expired sessions and
// enforcing the Max cap. When a persister is set the row is saved DURABLE-FIRST
// (before the cookie is returned): a persist failure returns an error and never
// mints a cookie. The returned id is the RAW 256-bit crypto/rand token (base64url);
// only its HASH is stored in memory and on disk.
func (s *SessionStore) Create() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictExpiredLocked()
	if len(s.sessions) >= s.params.Max {
		return "", ErrTooManySessions
	}
	raw, err := randToken(32)
	if err != nil {
		return "", err
	}
	h := HashSessionID(raw)
	now := s.now()
	if s.persister != nil {
		ctx, cancel := s.pctx()
		err := s.persister.Save(ctx, PersistedSession{IDHash: h, Gen: s.gen, CreatedAt: now, LastSeen: now})
		cancel()
		if err != nil {
			return "", err // durable-first: no cookie that isn't persisted
		}
	}
	s.sessions[h] = &deviceSession{
		id: h, gen: s.gen, createdAt: now, lastSeen: now, lastPersisted: now,
		revoked: make(chan struct{}),
	}
	return raw, nil
}

// Validate reports whether the RAW cookie is a live session of the current
// generation, refreshing its idle clock on success. A stale/expired/superseded
// cookie returns ErrNoSession (and is dropped). The idle refresh is persisted
// best-effort, throttled to at most once per Idle/4.
func (s *SessionStore) Validate(raw string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ds, ok := s.sessions[HashSessionID(raw)]
	if !ok {
		return ErrNoSession
	}
	now := s.now()
	if ds.gen != s.gen || s.isExpiredLocked(ds, now) {
		s.dropLocked(ds)
		return ErrNoSession
	}
	ds.lastSeen = now
	s.touchLocked(ds, now)
	return nil
}

// touchLocked persists the idle refresh best-effort, throttled to Idle/4. A
// failure is logged at debug and ignored — the only cost is a slightly earlier
// idle expiry after a restart (the safe direction). UPDATE-ONLY + gen-fenced in
// the store, so it can never resurrect a deleted row.
func (s *SessionStore) touchLocked(ds *deviceSession, now time.Time) {
	if s.persister == nil {
		return
	}
	if now.Sub(ds.lastPersisted) < s.params.Idle/4 {
		return
	}
	ctx, cancel := s.pctx()
	err := s.persister.Touch(ctx, ds.id, ds.gen, now)
	cancel()
	if err != nil {
		slog.Debug("remoteauth: session touch persist failed (best-effort)", "error", err)
		return
	}
	ds.lastPersisted = now
}

// Watch returns the session's revocation channel — closed when the session is
// revoked, rotated away, or expires. A remotely-exposed WebSocket selects on it
// to terminate the moment the operator rotates (plan §4.3). An unknown/expired
// cookie returns an already-closed channel (nothing to protect).
func (s *SessionStore) Watch(raw string) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ds, ok := s.sessions[HashSessionID(raw)]; ok && ds.gen == s.gen && !s.isExpiredLocked(ds, s.now()) {
		return ds.revoked
	}
	closed := make(chan struct{})
	close(closed)
	return closed
}

// SessionLifetime reports the lifetime binding for a RAW cookie, so a privileged
// read-only viewer can bind its stream to the device session's lifetime
// (session-attach F1b): it returns the session's revocation channel (closed on
// revoke/rotate/expiry-eviction), the duration until its next TTL/idle expiry,
// and whether it is currently live. The caller selects on the revocation channel
// AND a timer of the returned duration, re-calling on the timer so an idle
// refresh that extended the session (via Validate elsewhere) is picked up. A
// dead/unknown/superseded/idle-or-TTL-expired session returns an already-closed
// channel, 0, false (and the expired row is dropped, closing its channel).
//
// It is READ-ONLY: unlike Validate it does NOT refresh the idle clock, so a
// still-open viewer never keeps an otherwise-idle session alive — the viewer is
// bound to, not an extender of, the session's lifetime.
func (s *SessionStore) SessionLifetime(raw string) (<-chan struct{}, time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ds, ok := s.sessions[HashSessionID(raw)]
	if !ok {
		closed := make(chan struct{})
		close(closed)
		return closed, 0, false
	}
	now := s.now()
	if ds.gen != s.gen || s.isExpiredLocked(ds, now) {
		s.dropLocked(ds) // closes ds.revoked
		return ds.revoked, 0, false
	}
	deadline := ds.createdAt.Add(s.params.TTL)
	if idle := ds.lastSeen.Add(s.params.Idle); idle.Before(deadline) {
		deadline = idle
	}
	until := deadline.Sub(now)
	if until < 0 {
		until = 0
	}
	return ds.revoked, until, true
}

// Revoke drops one session (logout) by its RAW cookie and closes its revocation
// channel. DURABLE-FIRST: the persisted row is deleted before the in-memory
// drop, so a persist failure returns an error with memory untouched (the
// session stays consistently valid rather than reappearing after a restart). An
// unknown session is a no-op success (idempotent logout).
func (s *SessionStore) Revoke(raw string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revokeByHashLocked(HashSessionID(raw))
}

// RevokeByHash drops one session by its HASH (the identifier List surfaces to
// the management surface). Same durable-first contract as Revoke.
func (s *SessionStore) RevokeByHash(idHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revokeByHashLocked(idHash)
}

func (s *SessionStore) revokeByHashLocked(idHash string) error {
	ds, ok := s.sessions[idHash]
	if !ok {
		return nil // idempotent: nothing to revoke
	}
	if s.persister != nil {
		ctx, cancel := s.pctx()
		err := s.persister.Delete(ctx, idHash)
		cancel()
		if err != nil {
			return err // durable-first: leave memory valid on failure
		}
	}
	s.dropMemLocked(ds)
	return nil
}

// Rotate invalidates EVERY live session (a fresh pairing secret / `remote
// rotate` / terminate-all): it advances the generation and closes all
// revocation channels so open privileged sockets tear down. DURABLE-FIRST: the
// persisted rows are cleared and the durable generation advanced BEFORE the
// in-memory rotate, so a persist failure returns an error with every session
// still live (rather than a rotate that reappears after a restart).
func (s *SessionStore) Rotate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	newGen := s.gen + 1
	if s.persister != nil {
		ctx, cancel := s.pctx()
		err := s.persister.Reset(ctx, newGen)
		cancel()
		if err != nil {
			return err // durable-first: leave every session live on failure
		}
	}
	s.gen = newGen
	for _, ds := range s.sessions {
		close(ds.revoked)
	}
	s.sessions = map[string]*deviceSession{}
	return nil
}

// Count returns the number of live (non-expired, current-generation) sessions.
func (s *SessionStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictExpiredLocked()
	return len(s.sessions)
}

// SessionInfo is a metadata-only, non-sensitive view of one live device session
// for the dashboard sessions panel (dashboard-management-surface plan §2F). It
// carries a TRUNCATED fingerprint — NEVER a bearer. ID is the session HASH
// (already non-reversible), used server-side to target a revoke; the JSON
// surface must still expose only the fingerprint.
type SessionInfo struct {
	// Fingerprint is the first 8 hex chars of the session HASH — enough to
	// disambiguate + target a revoke, never a bearer.
	Fingerprint string
	// ID is the full session HASH (used server-side to target a revoke via the
	// DELETE handler; the dashboard round-trips the fingerprint the store
	// resolves). The JSON surface must NOT expose it.
	ID string
	// CreatedAt / LastSeen are UTC timestamps; AgeSeconds is now-createdAt.
	CreatedAt  time.Time
	LastSeen   time.Time
	AgeSeconds int64
}

// List returns metadata for every live (non-expired, current-generation)
// device session, sorted newest-first by creation. It evicts expired sessions
// first so the list matches Count. Fingerprints are truncated — the full HASH
// is carried in SessionInfo.ID for server-side revoke targeting only and MUST
// NOT be serialized to a client.
func (s *SessionStore) List() []SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictExpiredLocked()
	now := s.now()
	out := make([]SessionInfo, 0, len(s.sessions))
	for _, ds := range s.sessions {
		out = append(out, SessionInfo{
			Fingerprint: fingerprint(ds.id),
			ID:          ds.id,
			CreatedAt:   ds.createdAt,
			LastSeen:    ds.lastSeen,
			AgeSeconds:  int64(now.Sub(ds.createdAt).Seconds()),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// fingerprint returns a short, non-reversible display token for a session HASH:
// its first 8 characters (the HASH is a sha256-hex, so 8 chars disambiguate
// without revealing anything reversible).
func fingerprint(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func (s *SessionStore) isExpiredLocked(ds *deviceSession, now time.Time) bool {
	return now.Sub(ds.createdAt) >= s.params.TTL || now.Sub(ds.lastSeen) >= s.params.Idle
}

// dropLocked removes an EXPIRED/superseded session: a synchronous in-memory
// drop under the store mutex, then a best-effort durable delete DETACHED onto a
// background goroutine. Detaching keeps the liveness answer — and the store
// mutex — off the SQLite critical path: a slow/locked delete (bounded at
// persistTimeout) can no longer stall the caller that just proved the session
// dead, so SessionLifetime's viewer-lifetime watcher cancels a sensitive viewer
// at expiry instead of up to persistTimeout later. This is SAFE for the
// invariant because the in-memory map — the sole source of truth for liveness —
// is already updated, and load-time expiry re-filters any TTL/idle-expired row a
// failed or still-in-flight delete leaves behind. Every caller (Validate,
// SessionLifetime, evictExpiredLocked) is an expiry path whose answer never
// depends on the delete completing; the security-critical revoke/rotate paths
// use the durable-first revokeByHashLocked / Reset, not this. The map guard
// keeps the drop single-flight, so at most one delete is detached per removal.
func (s *SessionStore) dropLocked(ds *deviceSession) {
	if _, ok := s.sessions[ds.id]; !ok {
		return
	}
	s.dropMemLocked(ds)
	if s.persister != nil {
		s.detachExpiryDelete(ds.id)
	}
}

// detachExpiryDelete runs one best-effort durable delete for an expired session
// off the store mutex on a background goroutine, so an expiry evaluation never
// waits on a slow/locked SQLite write. Best-effort: a failure is logged at debug
// and left for load-time expiry to backstop (dropLocked's documented invariant).
// idHash is captured by value and the goroutine takes no store lock, so it is
// safe to fire while s.mu is held.
func (s *SessionStore) detachExpiryDelete(idHash string) {
	go func() {
		ctx, cancel := s.pctx()
		defer cancel()
		if err := s.persister.Delete(ctx, idHash); err != nil {
			slog.Debug("remoteauth: expiry delete persist failed (best-effort; load-time expiry backstops)", "error", err)
		}
	}()
}

// dropMemLocked removes a session from the in-memory map and closes its
// revocation channel (idempotent on the channel).
func (s *SessionStore) dropMemLocked(ds *deviceSession) {
	if _, ok := s.sessions[ds.id]; !ok {
		return
	}
	delete(s.sessions, ds.id)
	select {
	case <-ds.revoked: // already closed
	default:
		close(ds.revoked)
	}
}

func (s *SessionStore) evictExpiredLocked() {
	now := s.now()
	for _, ds := range s.sessions {
		if ds.gen != s.gen || s.isExpiredLocked(ds, now) {
			s.dropLocked(ds)
		}
	}
}
