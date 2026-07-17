package remoteauth

import (
	"errors"
	"testing"
	"time"
)

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
