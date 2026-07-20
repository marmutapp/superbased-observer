package remoteauth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// blockingDeletePersister is a SessionPersister whose Delete blocks until the
// test releases it, so an expiry-path test can prove SessionLifetime returns its
// liveness answer WITHOUT waiting on the (now detached, best-effort) durable
// delete. Save/LoadAll keep enough state for a Create; every other method is a
// no-op — this fake exists only to make Delete slow.
type blockingDeletePersister struct {
	entered chan string   // one send per Delete entry (buffered)
	release chan struct{} // Delete returns once this is closed
	mu      sync.Mutex
	rows    map[string]PersistedSession
}

func newBlockingDeletePersister() *blockingDeletePersister {
	return &blockingDeletePersister{
		entered: make(chan string, 8),
		release: make(chan struct{}),
		rows:    map[string]PersistedSession{},
	}
}

func (p *blockingDeletePersister) LoadAll(context.Context) (uint64, []PersistedSession, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]PersistedSession, 0, len(p.rows))
	for _, r := range p.rows {
		out = append(out, r)
	}
	return 1, out, nil
}

func (p *blockingDeletePersister) Save(_ context.Context, s PersistedSession) error {
	p.mu.Lock()
	p.rows[s.IDHash] = s
	p.mu.Unlock()
	return nil
}

func (p *blockingDeletePersister) Touch(context.Context, string, uint64, time.Time) error {
	return nil
}

func (p *blockingDeletePersister) Delete(ctx context.Context, idHash string) error {
	select {
	case p.entered <- idHash:
	default:
	}
	select {
	case <-p.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	p.mu.Lock()
	delete(p.rows, idHash)
	p.mu.Unlock()
	return nil
}

func (p *blockingDeletePersister) Reset(_ context.Context, _ uint64) error {
	p.mu.Lock()
	p.rows = map[string]PersistedSession{}
	p.mu.Unlock()
	return nil
}

func TestArgon2HashVerify(t *testing.T) {
	_, enc, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	raw, err := DecodeSecret(enc)
	if err != nil {
		t.Fatalf("DecodeSecret: %v", err)
	}
	hash, err := HashSecret(raw)
	if err != nil {
		t.Fatalf("HashSecret: %v", err)
	}
	if !VerifySecret(hash, raw) {
		t.Error("VerifySecret rejected the correct secret")
	}
	if VerifySecret(hash, []byte("wrong-secret-bytes")) {
		t.Error("VerifySecret accepted a wrong secret")
	}
	if VerifySecret("not-a-hash", raw) {
		t.Error("VerifySecret accepted a malformed hash")
	}
	// Distinct salts ⇒ distinct encodings for the same input.
	h2, _ := HashSecret(raw)
	if hash == h2 {
		t.Error("two hashes of the same secret are identical — salt not randomised")
	}
}

func TestSessionLifecycle(t *testing.T) {
	now := time.Now().UTC()
	clock := func() time.Time { return now }
	st := NewSessionStore(SessionParams{TTL: time.Hour, Idle: 10 * time.Minute, Max: 2, Now: clock})

	a, err := st.Create()
	if err != nil {
		t.Fatalf("Create a: %v", err)
	}
	if err := st.Validate(a); err != nil {
		t.Fatalf("Validate a: %v", err)
	}
	// Max cap.
	if _, err := st.Create(); err != nil {
		t.Fatalf("Create b: %v", err)
	}
	if _, err := st.Create(); !errors.Is(err, ErrTooManySessions) {
		t.Fatalf("third Create err=%v, want ErrTooManySessions", err)
	}

	// Idle timeout.
	now = now.Add(11 * time.Minute)
	if err := st.Validate(a); !errors.Is(err, ErrNoSession) {
		t.Errorf("idle session validated: %v", err)
	}

	// TTL expiry: fresh session, advance past TTL.
	now = time.Now().UTC()
	c, _ := st.Create()
	now = now.Add(2 * time.Hour)
	if err := st.Validate(c); !errors.Is(err, ErrNoSession) {
		t.Errorf("expired session validated: %v", err)
	}
}

// TestSessionLifetimeExpiryDoesNotWaitOnDelete pins the round-3 F1b latency fix:
// evaluating a TTL/idle-expired session returns the not-live answer IMMEDIATELY
// — it must not block on the best-effort durable delete (now detached), so a
// bound sensitive viewer is cancelled promptly even when the persistence layer
// is slow or locked. With the old synchronous delete the answer stalled up to
// persistTimeout (5s); this asserts sub-2s.
func TestSessionLifetimeExpiryDoesNotWaitOnDelete(t *testing.T) {
	now := time.Now().UTC()
	clock := func() time.Time { return now }
	p := newBlockingDeletePersister()
	st := NewSessionStore(SessionParams{TTL: time.Hour, Idle: 10 * time.Minute, Max: 5, Now: clock, Persister: p})
	raw, err := st.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Idle-expire it (11m > 10m Idle).
	now = now.Add(11 * time.Minute)

	answered := make(chan bool, 1)
	go func() { _, _, live := st.SessionLifetime(raw); answered <- live }()

	select {
	case live := <-answered:
		if live {
			t.Fatal("expired session reported live")
		}
	case <-time.After(2 * time.Second):
		close(p.release) // unblock the (detached) delete before failing
		t.Fatal("SessionLifetime blocked on the durable delete — the expiry answer must not wait on persistence")
	}

	// The detached best-effort delete IS still attempted, just off the liveness
	// path (the cleanup semantics are preserved).
	select {
	case <-p.entered:
	case <-time.After(2 * time.Second):
		t.Error("expected the detached best-effort delete to be attempted")
	}
	close(p.release) // let the detached goroutine finish cleanly
}

func TestSessionRevokeAndWatch(t *testing.T) {
	st := NewSessionStore(SessionParams{TTL: time.Hour, Idle: time.Hour, Max: 5})
	id, _ := st.Create()
	w := st.Watch(id)
	select {
	case <-w:
		t.Fatal("watch channel closed while session live")
	default:
	}
	st.Revoke(id)
	select {
	case <-w:
	case <-time.After(time.Second):
		t.Fatal("revoke did not close the watch channel")
	}
	if err := st.Validate(id); !errors.Is(err, ErrNoSession) {
		t.Errorf("revoked session validated: %v", err)
	}
}

// TestSessionLifetime pins the additive F1b accessor: a live session reports the
// duration until its next TTL/idle expiry with its revocation channel open; a
// revoked or TTL/idle-expired session reports live=false with an already-closed
// channel — WITHOUT refreshing the idle clock (so a bound viewer never extends
// an otherwise-idle session). Uses an injectable clock.
func TestSessionLifetime(t *testing.T) {
	now := time.Now().UTC()
	clock := func() time.Time { return now }
	st := NewSessionStore(SessionParams{TTL: time.Hour, Idle: 10 * time.Minute, Max: 5, Now: clock})

	id, _ := st.Create()

	// Live: idle (10m) is the nearer bound, channel open, live=true.
	revoked, until, live := st.SessionLifetime(id)
	if !live {
		t.Fatal("fresh session reported not live")
	}
	if until != 10*time.Minute {
		t.Errorf("until = %v, want 10m (nearer idle bound)", until)
	}
	select {
	case <-revoked:
		t.Fatal("revocation channel closed while session live")
	default:
	}

	// SessionLifetime must NOT refresh idle: advance 6m twice; if it refreshed on
	// the first call the session would survive, but idle expiry must still fire.
	now = now.Add(6 * time.Minute)
	if _, _, live := st.SessionLifetime(id); !live {
		t.Fatal("session must still be live 6m in (idle 10m)")
	}
	now = now.Add(6 * time.Minute) // 12m total idle > 10m
	revoked, _, live = st.SessionLifetime(id)
	if live {
		t.Error("idle-expired session reported live — SessionLifetime must not have refreshed the idle clock")
	}
	select {
	case <-revoked:
	case <-time.After(time.Second):
		t.Error("idle expiry did not close the revocation channel")
	}

	// TTL expiry on a fresh session closes the channel too.
	now = time.Now().UTC()
	clock2 := func() time.Time { return now }
	st2 := NewSessionStore(SessionParams{TTL: time.Hour, Idle: time.Hour, Max: 5, Now: clock2})
	id2, _ := st2.Create()
	now = now.Add(2 * time.Hour)
	revoked2, _, live2 := st2.SessionLifetime(id2)
	if live2 {
		t.Error("TTL-expired session reported live")
	}
	select {
	case <-revoked2:
	case <-time.After(time.Second):
		t.Error("TTL expiry did not close the revocation channel")
	}

	// Revoke closes the channel and reports not-live.
	st3 := NewSessionStore(SessionParams{TTL: time.Hour, Idle: time.Hour, Max: 5})
	id3, _ := st3.Create()
	ch, _, live3 := st3.SessionLifetime(id3)
	if !live3 {
		t.Fatal("session not live before revoke")
	}
	st3.Revoke(id3)
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("revoke did not close the SessionLifetime channel")
	}
	if _, _, live := st3.SessionLifetime(id3); live {
		t.Error("revoked session reported live")
	}

	// Unknown cookie: not live, already-closed channel.
	unk, _, live := st3.SessionLifetime("nope")
	if live {
		t.Error("unknown cookie reported live")
	}
	select {
	case <-unk:
	default:
		t.Error("unknown cookie must return an already-closed channel")
	}
}

// TestSessionRevocationTerminatesOpenWS pins plan §4.3: rotation invalidates
// ALL sessions and closes their revocation channels, so an open privileged
// socket (modelled by the Watch channel) tears down.
func TestSessionRevocationTerminatesOpenWS(t *testing.T) {
	st := NewSessionStore(SessionParams{TTL: time.Hour, Idle: time.Hour, Max: 5})
	a, _ := st.Create()
	b, _ := st.Create()
	wa, wb := st.Watch(a), st.Watch(b)

	st.Rotate()

	for name, w := range map[string]<-chan struct{}{"a": wa, "b": wb} {
		select {
		case <-w:
		case <-time.After(time.Second):
			t.Fatalf("rotate did not terminate open socket for session %s", name)
		}
	}
	if err := st.Validate(a); !errors.Is(err, ErrNoSession) {
		t.Errorf("session survived rotation: %v", err)
	}
	// A new session post-rotate is valid (new generation).
	c, err := st.Create()
	if err != nil {
		t.Fatalf("Create post-rotate: %v", err)
	}
	if err := st.Validate(c); err != nil {
		t.Errorf("post-rotate session invalid: %v", err)
	}
}

// TestExecuteCapabilityIsSingleUseAndRevocable pins plan §4.2: an execute
// capability is single-use, session+action-bound, expiring, and dies with its
// session.
func TestExecuteCapabilityIsSingleUseAndRevocable(t *testing.T) {
	now := time.Now().UTC()
	clock := func() time.Time { return now }
	cs := NewCapabilityStore(time.Minute, clock)

	tok, err := cs.Mint("sess-1", "launch")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// Wrong action / wrong session are rejected (and burn the token).
	tok2, _ := cs.Mint("sess-1", "launch")
	if cs.Consume(tok2, "sess-1", "restart") {
		t.Error("capability consumed for the wrong action")
	}
	if cs.Consume(tok2, "sess-1", "launch") {
		t.Error("capability reusable after a failed (burned) match")
	}
	// Correct match once.
	if !cs.Consume(tok, "sess-1", "launch") {
		t.Fatal("valid capability rejected")
	}
	// Single-use: second consume fails.
	if cs.Consume(tok, "sess-1", "launch") {
		t.Error("capability was reusable — must be single-use")
	}
	// Expiry.
	tok3, _ := cs.Mint("sess-1", "launch")
	now = now.Add(2 * time.Minute)
	if cs.Consume(tok3, "sess-1", "launch") {
		t.Error("expired capability consumed")
	}
	// Revoke session drops its capabilities.
	now = time.Now().UTC()
	tok4, _ := cs.Mint("sess-2", "launch")
	cs.RevokeSession("sess-2")
	if cs.Consume(tok4, "sess-2", "launch") {
		t.Error("capability survived session revocation")
	}
}

func TestRateLimiter(t *testing.T) {
	now := time.Now().UTC()
	clock := func() time.Time { return now }
	rl := NewRateLimiter(6, 6, clock)
	for i := 0; i < 6; i++ {
		if !rl.Allow("ip") {
			t.Fatalf("attempt %d denied within burst", i)
		}
	}
	if rl.Allow("ip") {
		t.Error("7th attempt allowed — burst not enforced")
	}
	// A different key has its own bucket.
	if !rl.Allow("other") {
		t.Error("independent key throttled")
	}
	// Refill after a minute.
	now = now.Add(time.Minute)
	if !rl.Allow("ip") {
		t.Error("bucket did not refill after a minute")
	}
	// Disabled limiter always allows.
	off := NewRateLimiter(0, 0, clock)
	for i := 0; i < 100; i++ {
		if !off.Allow("x") {
			t.Fatal("disabled limiter denied")
		}
	}
}
