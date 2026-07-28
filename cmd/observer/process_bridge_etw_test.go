package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/processobs"
	"github.com/marmutapp/superbased-observer/internal/processobs/bridge"
)

// fakeETWCapture stands in for internal/processobs/etw's Capture so the
// capturer's ETW wiring is exercised on a host with no ETW and no elevation
// (i.e. every host CI has). It is the etwNetworkCapture seam and nothing more —
// which is the point of that interface existing.
type fakeETWCapture struct {
	mode     string
	reason   string
	in       int64
	out      int64
	measured bool

	// decodeReported models the etw.Capture.DecodeStats bool: false is "there
	// is no decoder to report on" (no session ever came up), which must
	// produce SILENCE on the wire rather than a zeroed report.
	decodeReported     bool
	dropped            int64
	unsupportedVersion int64
	decoded            int64
	ignored            int64

	mu     sync.Mutex
	closed bool
	calls  int
}

func (f *fakeETWCapture) NetworkBytes(int) (in, out int64, ok bool) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if !f.measured {
		return 0, 0, false
	}
	return f.in, f.out, true
}

func (f *fakeETWCapture) Status() (mode, reason string) { return f.mode, f.reason }

func (f *fakeETWCapture) DecodeStats() (processobs.CapturerDecodeStats, bool) {
	if !f.decodeReported {
		return processobs.CapturerDecodeStats{}, false
	}
	return processobs.CapturerDecodeStats{
		NetworkDropped:            f.dropped,
		NetworkUnsupportedVersion: f.unsupportedVersion,
		NetworkDecoded:            f.decoded,
		NetworkIgnored:            f.ignored,
	}, true
}

func (f *fakeETWCapture) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

func (f *fakeETWCapture) wasClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeETWCapture) sampled() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// bridgeStream is a decoded capturer run: the hello frame, every event, and
// every in-band error diagnostic.
type bridgeStream struct {
	hello  bridge.Hello
	events []processobs.RawEvent
	errs   []string
	stats  []bridge.CapturerStats
}

// runBridgeCapturer runs one capturer to completion against a short deadline
// and decodes its whole stream. The poll interval is deliberately longer than
// the deadline so exactly the initial snapshot is emitted — enough to assert
// the event contract without a multi-second test.
func runBridgeCapturer(t *testing.T, opts processBridgeOptions) bridgeStream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	if opts.Interval == 0 {
		opts.Interval = 5 * time.Second
	}
	var buf bytes.Buffer
	if err := streamProcessBridgeWith(ctx, &buf, opts); err != nil {
		t.Fatalf("streamProcessBridgeWith: %v", err)
	}

	dec := bridge.NewDecoder(&buf)
	first, err := dec.Next()
	if err != nil {
		t.Fatalf("decode hello: %v", err)
	}
	if first.Kind != bridge.KindHello || first.Hello == nil {
		t.Fatalf("first frame is not a hello: %+v", first)
	}
	if first.V != bridge.WireVersion {
		t.Fatalf("hello wire version = %d, want %d", first.V, bridge.WireVersion)
	}
	got := bridgeStream{hello: *first.Hello}
	for {
		f, derr := dec.Next()
		if errors.Is(derr, io.EOF) {
			break
		}
		if derr != nil {
			t.Fatalf("decode: %v", derr)
		}
		switch f.Kind {
		case bridge.KindEvent:
			if f.Event == nil {
				t.Fatal("event frame has nil Event")
			}
			got.events = append(got.events, *f.Event)
		case bridge.KindError:
			got.errs = append(got.errs, f.Error)
		case bridge.KindStats:
			if f.Stats == nil {
				t.Fatal("stats frame has nil Stats")
			}
			got.stats = append(got.stats, *f.Stats)
		case bridge.KindHello:
			t.Fatalf("unexpected second hello frame: %+v", f.Hello)
		}
	}
	if len(got.events) == 0 {
		t.Fatal("expected at least one process event from the initial snapshot")
	}
	return got
}

// countNetworkMetrics reports how many events carry measured network bytes.
func countNetworkMetrics(evs []processobs.RawEvent) int {
	n := 0
	for i := range evs {
		if evs[i].HasNetworkMetrics {
			n++
		}
	}
	return n
}

// TestStreamProcessBridgeETWModes is the fail-open matrix for `--etw`. Every
// row asserts the SAME invariant from a different angle: process capture never
// depends on ETW, and an absent network feed is reported as UNMEASURED rather
// than as zero bytes.
func TestStreamProcessBridgeETWModes(t *testing.T) {
	startFails := errors.New("etw: controlling a trace session requires an elevated process")

	tests := []struct {
		name string
		// capture/err are what the injected starter returns.
		capture *fakeETWCapture
		err     error
		etw     bool

		wantBackend    string
		wantMode       string
		wantReasonHas  string
		wantErrFrame   bool
		wantNetMetrics bool
	}{
		{
			// The shipped path: nothing about it changes.
			name:          "etw not requested",
			etw:           false,
			wantBackend:   "poll",
			wantMode:      processobs.NetworkAccountingOff,
			wantReasonHas: "without --etw",
		},
		{
			// THE common case: ETW session control always requires elevation,
			// so an ordinary capturer run lands here. Capture must continue.
			name:          "etw requested but not elevated",
			etw:           true,
			err:           startFails,
			wantBackend:   "poll",
			wantMode:      processobs.NetworkAccountingUnavailable,
			wantReasonHas: "elevated",
			wantErrFrame:  true,
		},
		{
			name:           "etw live",
			etw:            true,
			capture:        &fakeETWCapture{mode: processobs.NetworkAccountingTCP, in: 4096, out: 512, measured: true},
			wantBackend:    "poll+etw",
			wantMode:       processobs.NetworkAccountingTCP,
			wantNetMetrics: true,
		},
		{
			// Started, but the capture itself says it is not counting. The
			// backend name must not claim "+etw" for a feed that measures
			// nothing.
			name:          "etw started but degraded",
			etw:           true,
			capture:       &fakeETWCapture{mode: processobs.NetworkAccountingUnavailable, reason: "the ETW session stopped unexpectedly"},
			wantBackend:   "poll",
			wantMode:      processobs.NetworkAccountingUnavailable,
			wantReasonHas: "stopped unexpectedly",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := processBridgeOptions{ETW: tt.etw}
			if tt.etw {
				opts.startETW = func() (etwNetworkCapture, error) {
					if tt.err != nil {
						return nil, tt.err
					}
					return tt.capture, nil
				}
			}
			got := runBridgeCapturer(t, opts)

			if got.hello.Backend != tt.wantBackend {
				t.Errorf("hello.Backend = %q, want %q", got.hello.Backend, tt.wantBackend)
			}
			if got.hello.NetworkAccountingMode != tt.wantMode {
				t.Errorf("hello.NetworkAccountingMode = %q, want %q", got.hello.NetworkAccountingMode, tt.wantMode)
			}
			if tt.wantReasonHas != "" && !strings.Contains(got.hello.NetworkAccountingReason, tt.wantReasonHas) {
				t.Errorf("hello.NetworkAccountingReason = %q, want it to mention %q", got.hello.NetworkAccountingReason, tt.wantReasonHas)
			}
			if got.hello.OS == "" || got.hello.PID == 0 {
				t.Errorf("hello identity fields missing: %+v", got.hello)
			}

			switch n := len(got.errs); {
			case tt.wantErrFrame && n == 0:
				t.Error("expected an in-band capturer error frame explaining the ETW refusal")
			case !tt.wantErrFrame && n != 0:
				t.Errorf("unexpected capturer error frames: %q", got.errs)
			}

			measured := countNetworkMetrics(got.events)
			if tt.wantNetMetrics {
				if measured == 0 {
					t.Fatal("expected at least one event carrying measured network bytes")
				}
				for i := range got.events {
					ev := got.events[i]
					if !ev.HasNetworkMetrics {
						continue
					}
					if ev.NetworkBytesIn != tt.capture.in || ev.NetworkBytesOut != tt.capture.out {
						t.Fatalf("event bytes = (%d, %d), want the sampler's cumulative (%d, %d)",
							ev.NetworkBytesIn, ev.NetworkBytesOut, tt.capture.in, tt.capture.out)
					}
				}
			} else if measured != 0 {
				// The whole point: no ETW must mean UNMEASURED, never a
				// fabricated zero (or any other number).
				t.Errorf("%d events carry network metrics with no live ETW capture", measured)
			}

			if tt.capture != nil {
				if !tt.capture.wasClosed() {
					t.Error("capture was not closed when the capturer returned")
				}
				if tt.capture.sampled() == 0 {
					t.Error("the sampler was never consulted — NetworkBytes was not wired into poll.Options")
				}
			}
		})
	}
}

// TestStreamProcessBridgeKeepsPinnedSignature pins that the pre-existing entry
// point still runs the plain poll capturer, so process_bridge_test.go's
// contract is not quietly re-routed through the ETW path.
func TestStreamProcessBridgeKeepsPinnedSignature(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	var buf bytes.Buffer
	if err := streamProcessBridge(ctx, &buf, 5*time.Second); err != nil {
		t.Fatalf("streamProcessBridge: %v", err)
	}
	dec := bridge.NewDecoder(&buf)
	f, err := dec.Next()
	if err != nil {
		t.Fatalf("decode hello: %v", err)
	}
	if f.Hello == nil || f.Hello.Backend != "poll" {
		t.Fatalf("hello = %+v, want Backend %q", f.Hello, "poll")
	}
	if f.Hello.NetworkAccountingMode != processobs.NetworkAccountingOff {
		t.Fatalf("NetworkAccountingMode = %q, want %q", f.Hello.NetworkAccountingMode, processobs.NetworkAccountingOff)
	}
}

// TestCapturerDecodeStatsReporting pins the ONLY path by which "the
// payload-length assumption is wrong on this host" reaches the daemon at all
// — and, just as importantly, pins the silence that must accompany its
// absence.
//
// The distinction the rows encode is the whole feature: a capturer with no
// running network decoder has decoded NOTHING, so reporting "0 dropped" for it
// would tell an operator the fixed-offset layout was exercised and held. A
// capturer that IS decoding and refused nothing reports a genuine zero, which
// is the state the elevated validation is looking for and therefore must be
// distinguishable from the first.
func TestCapturerDecodeStatsReporting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		etw      bool
		capture  *fakeETWCapture
		startErr error
		want     []bridge.CapturerStats
	}{
		{
			name: "no --etw at all reports nothing",
			etw:  false,
			want: nil,
		},
		{
			name:     "a capture that could not start reports nothing",
			etw:      true,
			startErr: errors.New("Access is denied"),
			want:     nil,
		},
		{
			name: "a live capture with no decoder to report on stays SILENT, never a zeroed report",
			etw:  true,
			capture: &fakeETWCapture{
				mode: processobs.NetworkAccountingTCP, measured: true,
				decodeReported: false,
			},
			want: nil,
		},
		{
			name: "a live decoder that refused nothing reports a REAL zero",
			etw:  true,
			capture: &fakeETWCapture{
				mode: processobs.NetworkAccountingTCP, measured: true,
				decodeReported: true,
			},
			want: []bridge.CapturerStats{{}},
		},
		{
			name: "a refusing decoder carries both counters verbatim",
			etw:  true,
			capture: &fakeETWCapture{
				mode: processobs.NetworkAccountingTCP, measured: true,
				decodeReported: true, dropped: 7, unsupportedVersion: 2,
			},
			want: []bridge.CapturerStats{{NetworkDecodeDropped: 7, NetworkDecodeUnsupportedVersion: 2}},
		},
		{
			// E6b: the counters that say whether the decoder measured
			// ANYTHING must cross the wire too. Without them a renumbered
			// provider reports 0/0 — clean on both refusal counters — and the
			// validation passes while nothing is being decoded.
			name: "a healthy decoder carries the classified/ignored pair verbatim",
			etw:  true,
			capture: &fakeETWCapture{
				mode: processobs.NetworkAccountingTCP, measured: true,
				decodeReported: true, decoded: 4321, ignored: 1_000_000,
			},
			want: []bridge.CapturerStats{{NetworkDecodeDecoded: 4321, NetworkDecodeIgnored: 1_000_000}},
		},
		{
			name: "the renumbered-provider shape crosses the wire as itself, not as a clean zero",
			etw:  true,
			capture: &fakeETWCapture{
				mode: processobs.NetworkAccountingTCP, measured: true,
				decodeReported: true, ignored: 48_211,
			},
			want: []bridge.CapturerStats{{NetworkDecodeIgnored: 48_211}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := processBridgeOptions{ETW: tc.etw}
			if tc.etw {
				capture, startErr := tc.capture, tc.startErr
				opts.startETW = func() (etwNetworkCapture, error) {
					if startErr != nil {
						return nil, startErr
					}
					return capture, nil
				}
			}
			got := runBridgeCapturer(t, opts)
			if len(got.stats) != len(tc.want) {
				t.Fatalf("got %d stats frame(s) %+v, want %d", len(got.stats), got.stats, len(tc.want))
			}
			for i := range tc.want {
				if got.stats[i] != tc.want[i] {
					t.Errorf("stats[%d] = %+v, want %+v", i, got.stats[i], tc.want[i])
				}
			}
		})
	}
}

// TestClassificationCountersSurviveTheWholeChain walks the E6b signal end to
// end through the boundary the daemon actually publishes:
//
//	capturer stats frame → bridge.CapturerStats.DecodeStats
//	  → processobs.TransportStats → processHealthRecord → diag.ProcessHealth
//	  → the predicate and the prose an operator reads.
//
// Each hop drops values that are not explicitly carried, and E6b exists
// because exactly one such hop (etw → CapturerDecodeStats) silently discarded
// the counter. A per-hop unit test would not have caught that; this one walks
// the joins.
func TestClassificationCountersSurviveTheWholeChain(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		wire              bridge.CapturerStats
		wantNothingClass  bool
		wantLineFragment  string
		wantSilentDecode  bool
		wantDecoded       int64
		wantIgnoredOnRec  int64
		wantAnyOnDecoding bool
	}{
		{
			name:             "the renumbered-provider shape reaches the operator as a suspicion, not a pass",
			wire:             bridge.CapturerStats{NetworkDecodeIgnored: 48_211},
			wantNothingClass: true,
			wantLineFragment: "NO DATA EVENTS CLASSIFIED",
			wantIgnoredOnRec: 48_211,
		},
		{
			name:             "a healthy busy capture stays quiet even with a huge ignore count",
			wire:             bridge.CapturerStats{NetworkDecodeIgnored: 1_000_000, NetworkDecodeDecoded: 4321},
			wantSilentDecode: true,
			wantDecoded:      4321,
			wantIgnoredOnRec: 1_000_000,
		},
		{
			name:              "a refusing decoder still speaks through the REFUSAL line, not the new one",
			wire:              bridge.CapturerStats{NetworkDecodeDropped: 7, NetworkDecodeIgnored: 500},
			wantLineFragment:  "DECODE FAILURES",
			wantAnyOnDecoding: true,
			wantIgnoredOnRec:  500,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if any := tc.wire.DecodeStats().Any(); any != tc.wantAnyOnDecoding {
				t.Errorf("Any() = %v, want %v", any, tc.wantAnyOnDecoding)
			}
			rec := processHealthRecord(processobs.HealthSnapshot{
				BackendName:    "bridge-listen",
				TransportState: processobs.TransportStateConfigured,
				Transport: processobs.TransportStats{
					Addr: "127.0.0.1:8823", Connections: 1, Connected: true,
					CapturerDecode:         tc.wire.DecodeStats(),
					CapturerDecodeReported: true,
					CapturerDecodeAt:       time.Now(),
				},
			})
			if rec.TransportCapturerIgnored != tc.wantIgnoredOnRec || rec.TransportCapturerDecoded != tc.wantDecoded {
				t.Fatalf("record carried (ignored=%d decoded=%d), want (%d, %d)",
					rec.TransportCapturerIgnored, rec.TransportCapturerDecoded,
					tc.wantIgnoredOnRec, tc.wantDecoded)
			}
			if got := rec.CapturerDecodeNothingClassified(); got != tc.wantNothingClass {
				t.Errorf("CapturerDecodeNothingClassified() = %v, want %v", got, tc.wantNothingClass)
			}
			line := rec.CapturerDecodeLine()
			if tc.wantSilentDecode {
				if line != "" {
					t.Errorf("a healthy decoding capture must stay silent, got %q", line)
				}
				return
			}
			if !strings.Contains(line, tc.wantLineFragment) {
				t.Errorf("decode line = %q, want it to contain %q", line, tc.wantLineFragment)
			}
			// And it reaches the line an operator actually reads on doctor.
			if tl := rec.TransportLine(time.Now()); !strings.Contains(tl, tc.wantLineFragment) {
				t.Errorf("transport line = %q, want it to carry the decode clause", tl)
			}
		})
	}
}

// TestAbsentCapturerDecodeStaysAbsentThroughTheChain pins design constraint 3:
// a capturer that never ran must not start reporting a zeroed classification
// pair, because "ignored 0 / decoded 0" is exactly what a never-started
// decoder and a renumbered one would both look like if presence were dropped.
func TestAbsentCapturerDecodeStaysAbsentThroughTheChain(t *testing.T) {
	t.Parallel()

	rec := processHealthRecord(processobs.HealthSnapshot{
		BackendName:    "bridge-listen",
		TransportState: processobs.TransportStateConfigured,
		Transport: processobs.TransportStats{
			Addr: "127.0.0.1:8823", Connections: 1, Connected: true,
			// No report has ever arrived.
			CapturerDecodeReported: false,
		},
	})
	if rec.TransportCapturerDecodeReported {
		t.Fatal("absence became a report")
	}
	if rec.TransportCapturerIgnored != 0 || rec.TransportCapturerDecoded != 0 {
		t.Fatalf("counters materialised without a report: %+v", rec)
	}
	if rec.CapturerDecodeNothingClassified() {
		t.Fatal("a capturer that never reported must not raise the renumbered-provider suspicion")
	}
	if line := rec.CapturerDecodeLine(); line != "" {
		t.Fatalf("absence produced prose: %q", line)
	}
}
