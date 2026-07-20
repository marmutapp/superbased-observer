package termoob

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"testing"
)

// pipe returns an in-memory Encoder→Decoder pair sharing a buffer, with the
// decoder primed with the given expected auth secret.
func pipe(expected string) (*Encoder, *bytes.Buffer, func() *Decoder) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	dec := func() *Decoder { return NewDecoder(&buf, expected) }
	return enc, &buf, dec
}

func TestNewSessionTokenUnique(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		s, err := NewSessionToken()
		if err != nil {
			t.Fatalf("NewSessionToken: %v", err)
		}
		if s == "" || seen[s] {
			t.Fatalf("empty or duplicate session secret %q", s)
		}
		seen[s] = true
	}
}

func TestHelloThenLifecycleRoundTrip(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3t-session"
	enc, _, mkDec := pipe(secret)

	if err := enc.WriteHello(Hello{AuthToken: secret, CorrelationToken: "corr-1", Tool: "claude-code", PID: 42}); err != nil {
		t.Fatalf("WriteHello: %v", err)
	}
	code := 0
	if err := enc.WriteLifecycle(Lifecycle{Phase: PhaseLauncherStarted, At: 111}); err != nil {
		t.Fatalf("WriteLifecycle started: %v", err)
	}
	if err := enc.WriteLifecycle(Lifecycle{Phase: PhaseToolExecEnd, ExitCode: &code, At: 222}); err != nil {
		t.Fatalf("WriteLifecycle end: %v", err)
	}

	dec := mkDec()
	f1, err := dec.Read()
	if err != nil {
		t.Fatalf("read hello: %v", err)
	}
	if f1.Type != TypeHello || f1.Hello == nil || f1.Hello.CorrelationToken != "corr-1" || f1.Hello.Tool != "claude-code" || f1.Hello.PID != 42 {
		t.Fatalf("hello frame = %+v", f1)
	}
	f2, err := dec.Read()
	if err != nil {
		t.Fatalf("read lifecycle 1: %v", err)
	}
	if f2.Type != TypeLifecycle || f2.Lifecycle.Phase != PhaseLauncherStarted {
		t.Fatalf("lifecycle 1 = %+v", f2)
	}
	f3, err := dec.Read()
	if err != nil {
		t.Fatalf("read lifecycle 2: %v", err)
	}
	if f3.Lifecycle.Phase != PhaseToolExecEnd || f3.Lifecycle.ExitCode == nil || *f3.Lifecycle.ExitCode != 0 {
		t.Fatalf("lifecycle 2 = %+v", f3)
	}
	// Clean EOF at the end.
	if _, err := dec.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF, got %v", err)
	}
}

func TestFirstFrameMustBeHello(t *testing.T) {
	t.Parallel()
	const secret = "abc"
	enc, _, mkDec := pipe(secret)
	// Send a lifecycle frame WITHOUT a preceding hello.
	if err := enc.WriteLifecycle(Lifecycle{Phase: PhaseLauncherStarted}); err != nil {
		t.Fatalf("WriteLifecycle: %v", err)
	}
	dec := mkDec()
	_, err := dec.Read()
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated, got %v", err)
	}
	// The channel is poisoned: subsequent reads keep failing.
	if _, err := dec.Read(); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected poisoned ErrUnauthenticated, got %v", err)
	}
}

func TestWrongAuthTokenRejected(t *testing.T) {
	t.Parallel()
	enc, _, mkDec := pipe("the-right-secret")
	if err := enc.WriteHello(Hello{AuthToken: "the-WRONG-secret", CorrelationToken: "c"}); err != nil {
		t.Fatalf("WriteHello: %v", err)
	}
	dec := mkDec()
	if _, err := dec.Read(); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated for wrong secret, got %v", err)
	}
}

func TestDuplicateHelloIsProtocolError(t *testing.T) {
	t.Parallel()
	const secret = "s"
	enc, _, mkDec := pipe(secret)
	_ = enc.WriteHello(Hello{AuthToken: secret})
	_ = enc.WriteHello(Hello{AuthToken: secret})
	dec := mkDec()
	if _, err := dec.Read(); err != nil {
		t.Fatalf("first hello should succeed: %v", err)
	}
	if _, err := dec.Read(); !errors.Is(err, ErrProtocol) {
		t.Fatalf("expected ErrProtocol on duplicate hello, got %v", err)
	}
}

func TestOversizedFrameRejected(t *testing.T) {
	t.Parallel()
	// A payload larger than MaxFrameBytes must be refused at write time.
	enc, _, _ := pipe("s")
	big := make([]byte, MaxFrameBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	err := enc.WriteHello(Hello{AuthToken: "s", CorrelationToken: string(big)})
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("expected ErrFrameTooLarge on write, got %v", err)
	}
}

func TestDecoderRejectsOversizedLengthHeader(t *testing.T) {
	t.Parallel()
	// Hand-craft a header claiming a payload larger than MaxFrameBytes; the
	// decoder must refuse before allocating/reading it (abuse defense).
	var buf bytes.Buffer
	var hdr [headerBytes]byte
	hdr[0] = wireVersion
	hdr[1] = byte(TypeHello)
	binary.BigEndian.PutUint32(hdr[2:], MaxFrameBytes+1)
	buf.Write(hdr[:])
	dec := NewDecoder(&buf, "s")
	if _, err := dec.Read(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("expected ErrFrameTooLarge, got %v", err)
	}
}

func TestBadWireVersionRejected(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var hdr [headerBytes]byte
	hdr[0] = 99 // wrong version
	hdr[1] = byte(TypeHello)
	binary.BigEndian.PutUint32(hdr[2:], 0)
	buf.Write(hdr[:])
	dec := NewDecoder(&buf, "s")
	if _, err := dec.Read(); !errors.Is(err, ErrProtocol) {
		t.Fatalf("expected ErrProtocol for bad version, got %v", err)
	}
}

func TestUnknownFrameTypeIsForwardCompatible(t *testing.T) {
	t.Parallel()
	const secret = "s"
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	if err := enc.WriteHello(Hello{AuthToken: secret}); err != nil {
		t.Fatalf("WriteHello: %v", err)
	}
	// Hand-craft a future frame type (99) with a small JSON body.
	body := []byte(`{"future":true}`)
	var hdr [headerBytes]byte
	hdr[0] = wireVersion
	hdr[1] = 99
	binary.BigEndian.PutUint32(hdr[2:], uint32(len(body)))
	buf.Write(hdr[:])
	buf.Write(body)

	dec := NewDecoder(&buf, secret)
	if _, err := dec.Read(); err != nil { // hello
		t.Fatalf("read hello: %v", err)
	}
	f, err := dec.Read()
	if err != nil {
		t.Fatalf("unknown frame should not error (forward-compat): %v", err)
	}
	if f.Type != TypeUnknown || f.Hello != nil || f.Lifecycle != nil {
		t.Fatalf("expected inert TypeUnknown frame, got %+v", f)
	}
}

func TestPartialHeaderIsProtocolError(t *testing.T) {
	t.Parallel()
	// A truncated header (not a clean zero-byte EOF) is a framing violation.
	buf := bytes.NewBuffer([]byte{wireVersion, byte(TypeHello)}) // 2 of 6 header bytes
	dec := NewDecoder(buf, "s")
	if _, err := dec.Read(); !errors.Is(err, ErrProtocol) {
		t.Fatalf("expected ErrProtocol on short header, got %v", err)
	}
}

// TestSessionFrameRoundTrip pins the TypeSession frame through the encoder →
// decoder, both WITHOUT a Source hint (the default known-id shape claude.go
// emits — omitempty means the JSON carries no "source" key) and WITH the
// discovered hint (the codex discovery path). The channel must be authenticated
// first, so a Hello leads.
func TestSessionFrameRoundTrip(t *testing.T) {
	t.Parallel()
	const secret = "sekret"
	enc, _, mkDec := pipe(secret)

	if err := enc.WriteHello(Hello{AuthToken: secret, Tool: "codex", PID: 7}); err != nil {
		t.Fatalf("WriteHello: %v", err)
	}
	// Default: no Source → an id the launcher KNEW.
	if err := enc.WriteSession(Session{SessionID: "sess-known"}); err != nil {
		t.Fatalf("WriteSession known: %v", err)
	}
	// Discovered: the heuristic-scan hint.
	if err := enc.WriteSession(Session{SessionID: "sess-disco", Source: SessionSourceDiscovered}); err != nil {
		t.Fatalf("WriteSession discovered: %v", err)
	}

	dec := mkDec()
	if _, err := dec.Read(); err != nil { // Hello
		t.Fatalf("read hello: %v", err)
	}
	f1, err := dec.Read()
	if err != nil {
		t.Fatalf("read session known: %v", err)
	}
	if f1.Type != TypeSession || f1.Session == nil || f1.Session.SessionID != "sess-known" || f1.Session.Source != "" {
		t.Fatalf("known session frame = %+v (want id=sess-known, empty source)", f1.Session)
	}
	f2, err := dec.Read()
	if err != nil {
		t.Fatalf("read session discovered: %v", err)
	}
	if f2.Type != TypeSession || f2.Session == nil || f2.Session.SessionID != "sess-disco" || f2.Session.Source != SessionSourceDiscovered {
		t.Fatalf("discovered session frame = %+v (want id=sess-disco, source=discovered)", f2.Session)
	}
}

// TestSessionFrameOmitsEmptySource pins the wire-shape contract that a default
// (known-id) Session frame carries NO "source" key at all (omitempty) — so a
// pre-existing producer that never set Source is byte-for-byte unchanged.
func TestSessionFrameOmitsEmptySource(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(Session{SessionID: "x"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(body, []byte("source")) {
		t.Fatalf("default Session JSON must omit the source key, got %s", body)
	}
	body2, err := json.Marshal(Session{SessionID: "x", Source: SessionSourceDiscovered})
	if err != nil {
		t.Fatalf("marshal discovered: %v", err)
	}
	if !bytes.Contains(body2, []byte("discovered")) {
		t.Fatalf("discovered Session JSON must carry the source, got %s", body2)
	}
}
