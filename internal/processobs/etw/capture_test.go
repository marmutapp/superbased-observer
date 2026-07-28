package etw

import (
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/processobs"
)

// TestCaptureOptionsWiresTCPIntoTheAccumulator pins the round trip a live
// capture actually performs: raw payload bytes → DecodeTCPData → the wired
// OnTCP → cumulative totals. It uses the same encodeV4 fixture builder the
// decode tests use, so the bytes are the ones the provider documents rather
// than a hand-made struct.
func TestCaptureOptionsWiresTCPIntoTheAccumulator(t *testing.T) {
	t.Parallel()

	const pid = 8080
	acc := NewAccumulator(0)
	opts := captureOptions(Options{}, acc)
	if opts.OnTCP == nil {
		t.Fatal("captureOptions left OnTCP nil; the capture would decode nothing")
	}

	local := netip.MustParseAddr("192.168.1.3")
	remote := netip.MustParseAddr("104.244.42.65")

	// Two receives and one send, per-event, exactly as ETW delivers them.
	feed := []struct {
		id   uint16
		size uint32
	}{
		{eventIDTCPRecvIPv4, 100},
		{eventIDTCPRecvIPv4, 250},
		{eventIDTCPSendIPv4, 90},
	}
	for _, f := range feed {
		withTimes := f.id == eventIDTCPSendIPv4
		ev, err := DecodeTCPData(f.id, 0, encodeV4(pid, f.size, remote, local, 443, 51000, withTimes))
		if err != nil {
			t.Fatalf("DecodeTCPData(%d): %v", f.id, err)
		}
		opts.OnTCP(ev)
	}

	in, out, ok := acc.NetworkBytes(pid)
	if !ok {
		t.Fatal("NetworkBytes reported no counter after three decoded events")
	}
	if in != 350 || out != 90 {
		t.Fatalf("got in=%d out=%d, want in=350 out=90 (cumulative, not per-event)", in, out)
	}
}

// TestCaptureOptionsNeverWiresUDP is the TCP-only scope guarantee, asserted so
// that a later "let's include UDP as well" edit has to confront it deliberately.
//
// Linux counts TCP payload bytes only and processobs.MetricSample's network
// fields are documented as TCP-only, so folding UDP in would silently widen a
// field's meaning and make a cross-OS comparison apples-to-oranges with nothing
// saying so.
//
// The severing is structural, in three independent layers:
//
//  1. captureOptions FORCES OnUDP to nil, even when the caller set one. Session
//     drops UDP events (counting them in Stats.Ignored) when OnUDP is nil, so
//     no UDP payload is ever decoded on a Capture's session.
//  2. Every UDP event id classifies as ClassUDPDatagram, so dispatch takes the
//     UDP branch — never the TCP one that reaches the accumulator.
//  3. DecodeTCPData refuses UDP ids outright (ErrNotTCPEvent), and
//     UDPDatagramEvent shares no type with TCPDataEvent, so there is no
//     assignment that could smuggle one in. Pinned here and in
//     TestUDPIsNeverCountedAsTCP.
//
// What this CANNOT execute on Linux: Session.dispatch itself is Windows-only.
// Layer 1's consequence — a nil OnUDP meaning "ignored" — is read off
// session_windows.go, not run here.
func TestCaptureOptionsNeverWiresUDP(t *testing.T) {
	t.Parallel()

	acc := NewAccumulator(0)
	opts := captureOptions(Options{
		OnUDP: func(UDPDatagramEvent) {
			t.Error("a UDP handler survived captureOptions and was invoked")
		},
	}, acc)

	if opts.OnUDP != nil {
		t.Fatal("captureOptions kept the caller's OnUDP; a UDP byte count could reach a TCP-only total")
	}

	local := netip.MustParseAddr("192.168.1.3")
	remote := netip.MustParseAddr("8.8.8.8")
	for id, tmpl := range templates {
		if tmpl.class != ClassUDPDatagram {
			continue
		}
		if got := Classify(id); got != ClassUDPDatagram {
			t.Fatalf("Classify(%d) = %v, want ClassUDPDatagram", id, got)
		}

		var payload []byte
		if tmpl.family == FamilyIPv6 {
			payload = encodeV6(1234, 4096,
				netip.MustParseAddr("2001:db8::1"), netip.MustParseAddr("2001:db8::2"), 53, 40000, false)
		} else {
			payload = encodeV4(1234, 4096, remote, local, 53, 40000, false)
		}

		// It decodes as UDP...
		if _, err := DecodeUDPDatagram(id, 0, payload); err != nil {
			t.Fatalf("DecodeUDPDatagram(%d): %v", id, err)
		}
		// ...and can never be decoded as TCP.
		if _, err := DecodeTCPData(id, 0, payload); !errors.Is(err, ErrNotTCPEvent) {
			t.Fatalf("DecodeTCPData(%d) error = %v, want ErrNotTCPEvent", id, err)
		}
	}

	if got := acc.Len(); got != 0 {
		t.Fatalf("the accumulator holds %d pid(s) after a UDP-only feed, want 0", got)
	}
}

// TestCaptureOptionsReplacesTheCallersTCPHandler pins that Capture owns the
// accumulation outright. Chaining a caller's handler would let it skip events
// and desynchronise the totals from the wire, silently.
func TestCaptureOptionsReplacesTheCallersTCPHandler(t *testing.T) {
	t.Parallel()

	acc := NewAccumulator(0)
	opts := captureOptions(Options{
		OnTCP: func(TCPDataEvent) { t.Error("the caller's OnTCP survived captureOptions") },
	}, acc)
	opts.OnTCP(TCPDataEvent{PID: 1, Direction: DirectionReceive, Bytes: 5})

	if in, _, ok := acc.NetworkBytes(1); !ok || in != 5 {
		t.Fatalf("got (%d, ok=%v), want (5, ok=true)", in, ok)
	}
}

// TestCaptureOptionsPreservesEverythingElse pins that binding the handlers does
// not disturb the rest of the caller's configuration.
func TestCaptureOptionsPreservesEverythingElse(t *testing.T) {
	t.Parallel()

	in := Options{
		SessionName:    "SomeOtherName",
		ExcludeIPv6:    true,
		BufferSizeKB:   256,
		MinimumBuffers: 2,
		MaximumBuffers: 9,
	}
	out := captureOptions(in, NewAccumulator(0))

	if out.SessionName != in.SessionName ||
		out.ExcludeIPv6 != in.ExcludeIPv6 ||
		out.BufferSizeKB != in.BufferSizeKB ||
		out.MinimumBuffers != in.MinimumBuffers ||
		out.MaximumBuffers != in.MaximumBuffers {
		t.Fatalf("captureOptions changed non-handler fields: got %+v, want %+v", out, in)
	}
}

// TestCaptureOptionsAlwaysValidates pins that the wired Options satisfy
// Options.validate even when the caller supplied no handler at all — Capture
// supplies its own, so "no handler" is not reachable through StartCapture.
func TestCaptureOptionsAlwaysValidates(t *testing.T) {
	t.Parallel()

	opts := captureOptions(Options{}, NewAccumulator(0))
	if err := opts.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if opts.SessionName != DefaultSessionName {
		t.Fatalf("SessionName = %q, want %q", opts.SessionName, DefaultSessionName)
	}
}

// TestCaptureUnavailableReasonNamesTheCause pins the operator-facing wording of
// a failed start. §0.4 of the plan records that an operator whose capturer
// cannot elevate gets SILENCE today; a reason string that does not name the
// cause would reproduce exactly that.
func TestCaptureUnavailableReasonNamesTheCause(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want []string
	}{
		{
			name: "elevation",
			err:  fmt.Errorf("etw.startTrace: StartTraceW: %w", ErrNeedsElevation),
			want: []string{"not elevated", "Administrator"},
		},
		{
			name: "unsupported os",
			err:  fmt.Errorf("etw.StartCapture: %w", ErrUnsupportedOS),
			want: []string{"unsupported OS", "only available on Windows"},
		},
		{
			name: "session exists",
			err:  fmt.Errorf("etw.startTrace: %w", ErrSessionExists),
			want: []string{"already exists"},
		},
		{
			name: "anything else",
			err:  errors.New("some raw win32 failure"),
			want: []string{"some raw win32 failure"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := captureUnavailableReason(tc.err)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("reason %q does not mention %q", got, want)
				}
			}
		})
	}

	if got := captureUnavailableReason(nil); got != "" {
		t.Errorf("captureUnavailableReason(nil) = %q, want empty", got)
	}
}

// TestCaptureStatusTransitions is table-driven over the mode/reason state
// machine, including the one transition that is timing-dependent in production:
// the pump goroutine returning AFTER Close has already recorded a clean
// shutdown must not relabel it as a failure.
func TestCaptureStatusTransitions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		apply      func(*captureStatus)
		wantMode   string
		wantReason string
	}{
		{
			name:     "zero value is off",
			apply:    func(*captureStatus) {},
			wantMode: processobs.NetworkAccountingOff,
		},
		{
			name:     "live",
			apply:    func(s *captureStatus) { s.set(processobs.NetworkAccountingTCP, "") },
			wantMode: processobs.NetworkAccountingTCP,
		},
		{
			name: "live then degraded",
			apply: func(s *captureStatus) {
				s.set(processobs.NetworkAccountingTCP, "")
				s.degradeIfLive("session died")
			},
			wantMode:   processobs.NetworkAccountingUnavailable,
			wantReason: "session died",
		},
		{
			name: "closed then a late pump return cannot relabel it",
			apply: func(s *captureStatus) {
				s.set(processobs.NetworkAccountingTCP, "")
				s.set(processobs.NetworkAccountingOff, "capture closed")
				s.degradeIfLive("session died")
			},
			wantMode:   processobs.NetworkAccountingOff,
			wantReason: "capture closed",
		},
		{
			name: "a failed start cannot be relabelled either",
			apply: func(s *captureStatus) {
				s.set(processobs.NetworkAccountingUnavailable, "not elevated")
				s.degradeIfLive("session died")
			},
			wantMode:   processobs.NetworkAccountingUnavailable,
			wantReason: "not elevated",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var s captureStatus
			tc.apply(&s)
			mode, reason := s.get()
			if mode != tc.wantMode || reason != tc.wantReason {
				t.Fatalf("got (%q, %q), want (%q, %q)", mode, reason, tc.wantMode, tc.wantReason)
			}
		})
	}
}

// TestNilCaptureStatusIsOff pins the nil receiver, which is what a nil *Capture
// reports through.
func TestNilCaptureStatusIsOff(t *testing.T) {
	t.Parallel()

	var s *captureStatus
	s.set(processobs.NetworkAccountingTCP, "ignored")
	if s.degradeIfLive("x") {
		t.Error("degradeIfLive reported a change on a nil receiver")
	}
	if mode, reason := s.get(); mode != processobs.NetworkAccountingOff || reason != "" {
		t.Fatalf("got (%q, %q), want (%q, \"\")", mode, reason, processobs.NetworkAccountingOff)
	}
}

// TestCaptureStatusIsRaceFree exercises the pump goroutine's writer against the
// metric sampler's reader. Its assertion is the race detector.
func TestCaptureStatusIsRaceFree(t *testing.T) {
	t.Parallel()

	var s captureStatus
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			s.set(processobs.NetworkAccountingTCP, "")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			s.degradeIfLive("session died")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			_, _ = s.get()
		}
	}()
	wg.Wait()
}

// TestDecodeStatsOfCarriesEveryCounter is the regression guard for E6b, and it
// is a TOTALITY check rather than a spot check on purpose.
//
// The original defect was not a wrong value — it was a counter that existed,
// was maintained correctly, and was simply never copied into the value that
// crosses the transport. Asserting "dropped maps to NetworkDropped" would not
// have caught it; asserting that NO field of Stats is left behind does. Each
// input is a distinct number so a swapped pair fails too.
//
// It runs on Linux because the projection deliberately lives in the untagged
// file: `GOOS=windows go build` compiles no tests, and CI has no Windows
// runner, so anything left behind a build tag is asserted by nobody.
func TestDecodeStatsOfCarriesEveryCounter(t *testing.T) {
	t.Parallel()

	in := Stats{Decoded: 11, Dropped: 22, Ignored: 33, UnsupportedVersion: 44}
	got := decodeStatsOf(in)
	want := processobs.CapturerDecodeStats{
		NetworkDecoded: 11, NetworkDropped: 22, NetworkIgnored: 33, NetworkUnsupportedVersion: 44,
	}
	if got != want {
		t.Fatalf("decodeStatsOf(%+v) = %+v, want %+v", in, got, want)
	}

	// Reflection over the SOURCE type, so adding a counter to Stats without
	// carrying it fails here rather than shipping as another invisible one.
	// (The reverse direction is covered by the exact-value check above.)
	srcFields := reflect.TypeOf(Stats{}).NumField()
	dstFields := reflect.TypeOf(processobs.CapturerDecodeStats{}).NumField()
	if srcFields != dstFields {
		t.Fatalf("etw.Stats has %d counters and processobs.CapturerDecodeStats has %d — "+
			"a counter that is computed but never carried reaches no surface at all; "+
			"carry it in decodeStatsOf (and on the bridge wire) or document why it cannot cross",
			srcFields, dstFields)
	}

	// A zero Stats projects to a zero value, which is meaningful ONLY beside
	// the ok=false / presence flag the callers carry — never on its own.
	if zero := decodeStatsOf(Stats{}); zero != (processobs.CapturerDecodeStats{}) {
		t.Fatalf("decodeStatsOf(zero) = %+v, want the zero value", zero)
	}
	if decodeStatsOf(Stats{}).NothingClassified() {
		t.Fatal("a zeroed projection must not raise the renumbered-provider suspicion")
	}
}
