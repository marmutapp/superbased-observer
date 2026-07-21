package remoteauth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakePersister is an in-memory SessionPersister for exercising crash/restart
// sequences without SQLite. It faithfully enforces the two structural
// guarantees the real store must also honour: Touch is UPDATE-ONLY +
// generation-fenced (it never inserts, so it cannot resurrect a deleted row),
// and Reset advances the generation monotonically. Failure toggles let the
// durable-first tests inject persist errors.
type fakePersister struct {
	mu   sync.Mutex
	gen  uint64
	rows map[string]PersistedSession

	failLoad   bool
	failSave   bool
	failDelete bool
	failReset  bool
}

func newFakePersister() *fakePersister {
	return &fakePersister{gen: 1, rows: map[string]PersistedSession{}}
}

func (f *fakePersister) LoadAll(_ context.Context) (uint64, []PersistedSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failLoad {
		return 0, nil, errors.New("load boom")
	}
	out := make([]PersistedSession, 0, len(f.rows))
	for _, r := range f.rows {
		out = append(out, r)
	}
	return f.gen, out, nil
}

func (f *fakePersister) Save(_ context.Context, s PersistedSession) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSave {
		return errors.New("save boom")
	}
	f.rows[s.IDHash] = s
	return nil
}

func (f *fakePersister) Touch(_ context.Context, idHash string, gen uint64, lastSeen time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// UPDATE-ONLY + gen-fenced: never insert, never touch a superseded row.
	if r, ok := f.rows[idHash]; ok && r.Gen == gen {
		r.LastSeen = lastSeen
		f.rows[idHash] = r
	}
	return nil
}

func (f *fakePersister) Delete(_ context.Context, idHash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failDelete {
		return errors.New("delete boom")
	}
	delete(f.rows, idHash)
	return nil
}

func (f *fakePersister) Reset(_ context.Context, gen uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failReset {
		return errors.New("reset boom")
	}
	f.rows = map[string]PersistedSession{}
	if gen > f.gen {
		f.gen = gen
	}
	return nil
}

func (f *fakePersister) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

func persistParams(p *fakePersister, now func() time.Time) SessionParams {
	return SessionParams{TTL: time.Hour, Idle: 30 * time.Minute, Max: 5, Now: now, Persister: p}
}

// TestPersistedSessionSurvivesRestart is the headline: a paired cookie remains
// valid after a "restart" (a fresh SessionStore over the same durable store).
func TestPersistedSessionSurvivesRestart(t *testing.T) {
	p := newFakePersister()
	s1 := NewSessionStore(persistParams(p, nil))
	raw, err := s1.Create()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s1.Validate(raw); err != nil {
		t.Fatalf("validate pre-restart: %v", err)
	}
	// Restart.
	s2 := NewSessionStore(persistParams(p, nil))
	if err := s2.Validate(raw); err != nil {
		t.Fatalf("validate after restart should succeed, got %v", err)
	}
}

// TestRevokeSurvivesRestart: revoke → restart → cookie refused.
func TestRevokeSurvivesRestart(t *testing.T) {
	p := newFakePersister()
	s1 := NewSessionStore(persistParams(p, nil))
	raw, _ := s1.Create()
	if err := s1.Revoke(raw); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if p.count() != 0 {
		t.Fatalf("revoke must delete the durable row, have %d", p.count())
	}
	s2 := NewSessionStore(persistParams(p, nil))
	if err := s2.Validate(raw); !errors.Is(err, ErrNoSession) {
		t.Fatalf("revoked cookie must be refused after restart, got %v", err)
	}
}

// TestRotateSurvivesRestart: rotate → restart → old cookie refused; a session
// minted post-rotate survives the next restart (the new generation persisted).
func TestRotateSurvivesRestart(t *testing.T) {
	p := newFakePersister()
	s1 := NewSessionStore(persistParams(p, nil))
	a, _ := s1.Create()
	if err := s1.Rotate(); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	s2 := NewSessionStore(persistParams(p, nil))
	if err := s2.Validate(a); !errors.Is(err, ErrNoSession) {
		t.Fatalf("rotated-away cookie must be refused after restart, got %v", err)
	}
	c, err := s2.Create()
	if err != nil {
		t.Fatalf("create post-rotate: %v", err)
	}
	s3 := NewSessionStore(persistParams(p, nil))
	if err := s3.Validate(c); err != nil {
		t.Fatalf("post-rotate cookie must survive a later restart, got %v", err)
	}
}

// TestExpiryFilteredAtLoad: an idle/TTL-expired row is never restored.
func TestExpiryFilteredAtLoad(t *testing.T) {
	now := time.Now().UTC()
	clock := func() time.Time { return now }
	p := newFakePersister()
	s1 := NewSessionStore(persistParams(p, clock))
	raw, _ := s1.Create()
	// Advance past Idle (30m) so the persisted row is idle-expired.
	now = now.Add(45 * time.Minute)
	s2 := NewSessionStore(persistParams(p, clock))
	if err := s2.Validate(raw); !errors.Is(err, ErrNoSession) {
		t.Fatalf("idle-expired cookie must not be restored, got %v", err)
	}
}

// TestCreateFailClosedOnPersistError: a persist failure yields NO cookie and no
// in-memory session (durable-first).
func TestCreateFailClosedOnPersistError(t *testing.T) {
	p := newFakePersister()
	p.failSave = true
	s := NewSessionStore(persistParams(p, nil))
	raw, err := s.Create()
	if err == nil {
		t.Fatal("Create must fail when the durable Save fails")
	}
	if raw != "" {
		t.Fatalf("Create must not return a cookie on persist failure, got %q", raw)
	}
	if s.Count() != 0 {
		t.Fatalf("no in-memory session must exist after a failed Create, have %d", s.Count())
	}
}

// TestRevokeFailClosedLeavesSessionValid: durable-first — a Delete failure
// returns an error and leaves the session valid in memory AND on disk
// (consistent), rather than a half-revoke that reappears after restart.
func TestRevokeFailClosedLeavesSessionValid(t *testing.T) {
	p := newFakePersister()
	s := NewSessionStore(persistParams(p, nil))
	raw, _ := s.Create()
	p.failDelete = true
	if err := s.Revoke(raw); err == nil {
		t.Fatal("Revoke must return an error when the durable Delete fails")
	}
	if err := s.Validate(raw); err != nil {
		t.Fatalf("session must remain valid after a failed durable revoke, got %v", err)
	}
	if p.count() != 1 {
		t.Fatalf("durable row must remain after a failed revoke, have %d", p.count())
	}
}

// TestRotateFailClosedLeavesSessionsValid: durable-first — a Reset failure
// returns an error and leaves every session live.
func TestRotateFailClosedLeavesSessionsValid(t *testing.T) {
	p := newFakePersister()
	s := NewSessionStore(persistParams(p, nil))
	raw, _ := s.Create()
	p.failReset = true
	if err := s.Rotate(); err == nil {
		t.Fatal("Rotate must return an error when the durable Reset fails")
	}
	if err := s.Validate(raw); err != nil {
		t.Fatalf("session must remain valid after a failed durable rotate, got %v", err)
	}
}

// TestLoadErrorFailsClosed: a load failure starts an EMPTY store (no sessions),
// never a panic or partial state.
func TestLoadErrorFailsClosed(t *testing.T) {
	p := newFakePersister()
	// Seed a row, then make LoadAll fail.
	s1 := NewSessionStore(persistParams(p, nil))
	raw, _ := s1.Create()
	p.failLoad = true
	s2 := NewSessionStore(persistParams(p, nil))
	if s2.Count() != 0 {
		t.Fatalf("load failure must start with no sessions, have %d", s2.Count())
	}
	if err := s2.Validate(raw); !errors.Is(err, ErrNoSession) {
		t.Fatalf("no cookie may validate after a load failure, got %v", err)
	}
}

// TestHashSessionIDStable: the hash is deterministic and never the raw token.
func TestHashSessionIDStable(t *testing.T) {
	raw, err := randToken(32)
	if err != nil {
		t.Fatalf("rand gen failed: %v", err)
	}
	h1 := HashSessionID(raw)
	h2 := HashSessionID(raw)
	if h1 != h2 {
		t.Fatal("HashSessionID must be deterministic")
	}
	if h1 == raw {
		t.Fatal("HashSessionID must not return the raw token")
	}
	if len(h1) != 64 {
		t.Fatalf("expected 64-hex sha256, got %d chars", len(h1))
	}
}
