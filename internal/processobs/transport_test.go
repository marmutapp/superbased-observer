package processobs

import (
	"testing"
	"time"
)

// transportFake is a FakeBackend that also implements TransportStatsSource,
// like the real accept listener. ok is configurable so the "a child exists but
// reports no transport" case (a nested Composite with no listener) is testable.
type transportFake struct {
	*FakeBackend
	stats TransportStats
	ok    bool
}

func (f transportFake) TransportStats() (TransportStats, bool) { return f.stats, f.ok }

func tsAt(sec int) time.Time { return time.Date(2026, 7, 26, 12, 0, sec, 0, time.UTC) }

// TestCompositeTransportStatsAggregation pins the aggregation contract the
// health surface depends on: counters SUM (not first-wins), Connected ORs,
// timestamps take the MAX, and — the honesty case — a Composite with no
// transport-owning child reports ok=false rather than a zeroed transport,
// because "no such transport" and "a transport nobody ever connected to" are
// different facts that render differently.
func TestCompositeTransportStatsAggregation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		children []Backend
		want     TransportStats
		wantOK   bool
	}{
		{
			name:     "no child implements the capability → ok=false",
			children: []Backend{&FakeBackend{BackendName: "poll"}, &FakeBackend{BackendName: "bridge"}},
			wantOK:   false,
		},
		{
			name: "a child that implements it but reports ok=false is not a transport",
			children: []Backend{
				&FakeBackend{BackendName: "poll"},
				transportFake{
					FakeBackend: &FakeBackend{BackendName: "inner"}, ok: false,
					stats: TransportStats{Connections: 99, AuthFailures: 99},
				},
			},
			wantOK: false,
		},
		{
			name: "single transport child passes through",
			children: []Backend{
				&FakeBackend{BackendName: "poll"},
				transportFake{FakeBackend: &FakeBackend{BackendName: "etw"}, ok: true, stats: TransportStats{
					Addr: "127.0.0.1:8823", Connections: 2, AuthFailures: 1,
					Connected: true, LastConnectAt: tsAt(30), LastDisconnectAt: tsAt(20),
				}},
			},
			want: TransportStats{
				Addr: "127.0.0.1:8823", Connections: 2, AuthFailures: 1,
				Connected: true, LastConnectAt: tsAt(30), LastDisconnectAt: tsAt(20),
			},
			wantOK: true,
		},
		{
			name: "two transports sum their counters and OR Connected",
			children: []Backend{
				transportFake{FakeBackend: &FakeBackend{BackendName: "a"}, ok: true, stats: TransportStats{
					Addr: "127.0.0.1:8823", Connections: 2, AuthFailures: 3, Connected: false,
					LastConnectAt: tsAt(10), LastDisconnectAt: tsAt(40),
				}},
				transportFake{FakeBackend: &FakeBackend{BackendName: "b"}, ok: true, stats: TransportStats{
					Addr: "127.0.0.1:9999", Connections: 5, AuthFailures: 7, Connected: true,
					LastConnectAt: tsAt(50), LastDisconnectAt: tsAt(20),
				}},
			},
			want: TransportStats{
				Addr: "127.0.0.1:8823,127.0.0.1:9999", Connections: 7, AuthFailures: 10, Connected: true,
				// Max of each timestamp INDEPENDENTLY: the newest connect came
				// from child b, the newest disconnect from child a.
				LastConnectAt: tsAt(50), LastDisconnectAt: tsAt(40),
			},
			wantOK: true,
		},
		{
			// The live child comes FIRST and the idle one second, so a
			// last-write-wins aggregation would report "not connected" and
			// lose the connect timestamp — the two mistakes that would make
			// a working capturer look absent.
			name: "one live and one never-connected transport still reads live",
			children: []Backend{
				transportFake{FakeBackend: &FakeBackend{BackendName: "live"}, ok: true, stats: TransportStats{
					Addr: "127.0.0.1:8823", Connections: 1, Connected: true, LastConnectAt: tsAt(5),
				}},
				transportFake{FakeBackend: &FakeBackend{BackendName: "idle"}, ok: true, stats: TransportStats{
					Addr: "127.0.0.1:8823",
				}},
			},
			want: TransportStats{
				// Identical addrs are not duplicated into a list.
				Addr: "127.0.0.1:8823", Connections: 1, Connected: true, LastConnectAt: tsAt(5),
			},
			wantOK: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := NewComposite(tc.children, nil).TransportStats()
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (stats %+v)", ok, tc.wantOK, got)
			}
			if got != tc.want {
				t.Errorf("stats = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestCompositeTransportStatsNested proves the capability survives the real
// wiring shape: selectProcessBackend wraps an existing composite baseline in
// ANOTHER composite when it adds the ETW listener, so the stats must be
// reachable through both layers.
func TestCompositeTransportStatsNested(t *testing.T) {
	t.Parallel()
	baseline := NewComposite([]Backend{
		&FakeBackend{BackendName: "poll"},
		&FakeBackend{BackendName: "bridge"},
	}, nil)
	if _, ok := baseline.TransportStats(); ok {
		t.Fatal("a poll+bridge baseline owns no dial-in transport; want ok=false")
	}
	withETW := NewComposite([]Backend{
		baseline,
		transportFake{FakeBackend: &FakeBackend{BackendName: "etw"}, ok: true, stats: TransportStats{
			Addr: "127.0.0.1:8823", AuthFailures: 4,
		}},
	}, nil)
	got, ok := withETW.TransportStats()
	if !ok {
		t.Fatal("the ETW listener is nested one level down; want ok=true")
	}
	if got.AuthFailures != 4 || got.Addr != "127.0.0.1:8823" {
		t.Errorf("nested stats = %+v", got)
	}
}

// TestTransportStatsOfCapabilityProbe pins that the probe is a CAPABILITY
// check: a backend that does not implement the interface reports ok=false
// regardless of its name.
func TestTransportStatsOfCapabilityProbe(t *testing.T) {
	t.Parallel()
	// Named "etw" on purpose — a name-based check would wrongly say yes.
	if _, ok := TransportStatsOf(&FakeBackend{BackendName: "etw"}); ok {
		t.Error("a backend that does not implement TransportStatsSource must report ok=false")
	}
	if _, ok := TransportStatsOf(transportFake{FakeBackend: &FakeBackend{}, ok: true}); !ok {
		t.Error("a backend that implements TransportStatsSource must report ok=true")
	}
}

// unavailableFake is a FakeBackend that reports a REQUESTED-but-missing
// dial-in transport — the shape cmd/observer's decorator produces when the
// accept listener fails to bind.
type unavailableFake struct {
	*FakeBackend
	reason string
}

func (f unavailableFake) TransportUnavailableReason() string { return f.reason }

// bothFake implements BOTH transport capabilities at once — a live transport
// alongside a recorded failure, the one case where the resolver has to pick.
type bothFake struct {
	transportFake
	reason string
}

func (f bothFake) TransportUnavailableReason() string { return f.reason }

// TestMergeTransportStatsAuthReason pins that the refusal REASON survives
// aggregation and that the aggregate keeps the genuinely most recent one.
// The reason is the only field that may be presented to an operator as the
// cause of a refusal (the counter conflates every handshake failure), so a
// merge that dropped or spliced it would put the surfaces straight back to
// guessing "bad token".
func TestMergeTransportStatsAuthReason(t *testing.T) {
	t.Parallel()
	older := TransportStats{AuthFailures: 1, LastAuthError: "invalid token", LastAuthFailureAt: tsAt(10)}
	newer := TransportStats{AuthFailures: 2, LastAuthError: "capturer speaks protocol v2, this daemon speaks v1", LastAuthFailureAt: tsAt(40)}

	got := mergeTransportStats(mergeTransportStats(TransportStats{}, older), newer)
	if got.AuthFailures != 3 {
		t.Errorf("AuthFailures = %d, want 3", got.AuthFailures)
	}
	if got.LastAuthError != newer.LastAuthError {
		t.Errorf("LastAuthError = %q, want the newest reason %q", got.LastAuthError, newer.LastAuthError)
	}
	// Reversed order must not change the answer: "most recent" is a
	// timestamp fact, not an iteration-order fact.
	rev := mergeTransportStats(mergeTransportStats(TransportStats{}, newer), older)
	if rev.LastAuthError != newer.LastAuthError {
		t.Errorf("reversed merge LastAuthError = %q, want %q", rev.LastAuthError, newer.LastAuthError)
	}
	// A child with no reason must not blank out one that has it.
	kept := mergeTransportStats(mergeTransportStats(TransportStats{}, newer), TransportStats{AuthFailures: 5})
	if kept.LastAuthError != newer.LastAuthError {
		t.Errorf("a reasonless child erased the reason: %q", kept.LastAuthError)
	}
}

// TestCompositeTransportUnavailableForwarding pins that a composite does not
// swallow a child's "the transport you asked for is not running" — the
// production wiring nests the failing baseline inside a composite, and a
// dropped reason renders as the same silence as an install that never asked
// for the feed.
func TestCompositeTransportUnavailableForwarding(t *testing.T) {
	t.Parallel()
	plain := NewComposite([]Backend{&FakeBackend{BackendName: "poll"}}, nil)
	if r := plain.TransportUnavailableReason(); r != "" {
		t.Errorf("a baseline nobody asked for a transport on must report no reason, got %q", r)
	}
	nested := NewComposite([]Backend{
		NewComposite([]Backend{
			&FakeBackend{BackendName: "poll"},
			unavailableFake{FakeBackend: &FakeBackend{BackendName: "etw"}, reason: "listen 127.0.0.1:8823: bind: address already in use"},
		}, nil),
	}, nil)
	if r := nested.TransportUnavailableReason(); r != "listen 127.0.0.1:8823: bind: address already in use" {
		t.Errorf("nested reason = %q", r)
	}
}

// TestHealthSnapshotTransportTriState pins the three states the surfaces must
// keep apart. "none" and "unavailable" are the pair that used to collapse:
// both printed nothing, so "you asked for the cross-OS feed and it never
// bound" was indistinguishable from "you never enabled it".
func TestHealthSnapshotTransportTriState(t *testing.T) {
	t.Parallel()
	t.Run("requested but unavailable carries the reason", func(t *testing.T) {
		t.Parallel()
		be := unavailableFake{FakeBackend: &FakeBackend{BackendName: "poll"}, reason: "bind: address already in use"}
		s := NewObserver(Options{Backend: be}).Health().Snapshot()
		if s.TransportState != TransportStateUnavailable {
			t.Fatalf("state = %q, want %q", s.TransportState, TransportStateUnavailable)
		}
		if s.TransportUnavailableReason != "bind: address already in use" {
			t.Errorf("reason = %q", s.TransportUnavailableReason)
		}
		if s.TransportConfigured() || s.Transport != (TransportStats{}) {
			t.Errorf("an unavailable transport must claim no counters, got %+v", s.Transport)
		}
	})
	t.Run("an empty reason is not a state", func(t *testing.T) {
		t.Parallel()
		be := unavailableFake{FakeBackend: &FakeBackend{BackendName: "poll"}}
		s := NewObserver(Options{Backend: be}).Health().Snapshot()
		if s.TransportState != TransportStateNone {
			t.Errorf("state = %q, want %q — an unavailable state with no reason is not actionable", s.TransportState, TransportStateNone)
		}
	})
	t.Run("a live transport outranks a recorded failure", func(t *testing.T) {
		t.Parallel()
		// Both capabilities on one backend: the counters are the live truth,
		// so a stale "could not start" beside them would contradict them.
		be := bothFake{
			transportFake: transportFake{FakeBackend: &FakeBackend{BackendName: "etw"}, ok: true, stats: TransportStats{Addr: "127.0.0.1:8823"}},
			reason:        "stale failure",
		}
		s := NewObserver(Options{Backend: be}).Health().Snapshot()
		if s.TransportState != TransportStateConfigured || s.TransportUnavailableReason != "" {
			t.Errorf("state = %q, reason = %q", s.TransportState, s.TransportUnavailableReason)
		}
	})
}

// TestHealthSnapshotTransport pins the flow through the EXISTING health seam:
// the Observer probes its backend for the capability once, and the snapshot
// carries the transport state so a consumer can tell "no transport" from
// "a transport with zero connections".
func TestHealthSnapshotTransport(t *testing.T) {
	t.Parallel()
	t.Run("backend without a transport reports none", func(t *testing.T) {
		t.Parallel()
		s := NewObserver(Options{Backend: &FakeBackend{BackendName: "poll"}}).Health().Snapshot()
		if s.TransportState != TransportStateNone || s.TransportConfigured() {
			t.Errorf("a poll backend owns no dial-in transport; want state %q, got %q", TransportStateNone, s.TransportState)
		}
		if s.Transport != (TransportStats{}) {
			t.Errorf("Transport must stay zero when unconfigured, got %+v", s.Transport)
		}
	})
	t.Run("never-connected transport is configured with zero counters", func(t *testing.T) {
		t.Parallel()
		be := transportFake{
			FakeBackend: &FakeBackend{BackendName: "etw"}, ok: true,
			stats: TransportStats{Addr: "127.0.0.1:8823"},
		}
		s := NewObserver(Options{Backend: be}).Health().Snapshot()
		if s.TransportState != TransportStateConfigured {
			t.Fatalf("a configured-but-never-connected transport must report state %q, got %q", TransportStateConfigured, s.TransportState)
		}
		if s.Transport.Addr != "127.0.0.1:8823" || s.Transport.Connections != 0 {
			t.Errorf("Transport = %+v", s.Transport)
		}
	})
	t.Run("counters travel", func(t *testing.T) {
		t.Parallel()
		be := transportFake{FakeBackend: &FakeBackend{BackendName: "etw"}, ok: true, stats: TransportStats{
			Addr: "127.0.0.1:8823", Connections: 3, AuthFailures: 2,
			Connected: true, LastConnectAt: tsAt(1), LastDisconnectAt: tsAt(2),
		}}
		s := NewObserver(Options{Backend: be}).Health().Snapshot()
		if !s.TransportConfigured() || s.Transport.Connections != 3 || s.Transport.AuthFailures != 2 ||
			!s.Transport.Connected || !s.Transport.LastConnectAt.Equal(tsAt(1)) ||
			!s.Transport.LastDisconnectAt.Equal(tsAt(2)) {
			t.Errorf("snapshot transport = %+v (state=%q)", s.Transport, s.TransportState)
		}
	})
	t.Run("nil backend does not panic", func(t *testing.T) {
		t.Parallel()
		if NewObserver(Options{}).Health().Snapshot().TransportConfigured() {
			t.Error("no backend → no transport")
		}
	})
}

// TestHealthSnapshotTransportUnavailableFromOptions pins the OTHER source of
// the unavailable reason: a transport that never became a backend at all, so
// there is nothing to ask a capability for.
//
// It exists because the daemon used to carry that reason by WRAPPING the
// assembled backend in a decorator implementing TransportUnavailableSource,
// and a decorator embedding the Backend INTERFACE promotes only Backend's
// methods — every optional capability of the wrapped value (UnattributedCapturer
// above all) vanished from a type assertion. Options.TransportUnavailableReason
// exists so the fact travels without anything touching the backend.
func TestHealthSnapshotTransportUnavailableFromOptions(t *testing.T) {
	t.Parallel()
	t.Run("a plain option value carries the state", func(t *testing.T) {
		t.Parallel()
		s := NewObserver(Options{
			Backend:                    &FakeBackend{BackendName: "bridge"},
			TransportUnavailableReason: "bind: address already in use",
		}).Health().Snapshot()
		if s.TransportState != TransportStateUnavailable {
			t.Fatalf("state = %q, want %q", s.TransportState, TransportStateUnavailable)
		}
		if s.TransportUnavailableReason != "bind: address already in use" {
			t.Errorf("reason = %q", s.TransportUnavailableReason)
		}
	})
	t.Run("an empty option is still not a state", func(t *testing.T) {
		t.Parallel()
		s := NewObserver(Options{Backend: &FakeBackend{BackendName: "poll"}}).Health().Snapshot()
		if s.TransportState != TransportStateNone {
			t.Errorf("state = %q, want %q", s.TransportState, TransportStateNone)
		}
	})
	t.Run("the backend capability outranks the option", func(t *testing.T) {
		t.Parallel()
		// A backend that OWNS a failed transport knows more than the
		// assembler did; both sources exist, so the order is pinned rather
		// than left to whichever branch ran last.
		be := unavailableFake{FakeBackend: &FakeBackend{BackendName: "poll"}, reason: "from the backend"}
		s := NewObserver(Options{Backend: be, TransportUnavailableReason: "from the caller"}).Health().Snapshot()
		if s.TransportUnavailableReason != "from the backend" {
			t.Errorf("reason = %q, want the backend's own", s.TransportUnavailableReason)
		}
	})
	t.Run("a live transport still outranks both", func(t *testing.T) {
		t.Parallel()
		be := transportFake{FakeBackend: &FakeBackend{BackendName: "etw"}, ok: true, stats: TransportStats{Addr: "127.0.0.1:8823"}}
		s := NewObserver(Options{Backend: be, TransportUnavailableReason: "stale failure"}).Health().Snapshot()
		if s.TransportState != TransportStateConfigured || s.TransportUnavailableReason != "" {
			t.Errorf("state = %q, reason = %q", s.TransportState, s.TransportUnavailableReason)
		}
	})
	t.Run("the option never touches the backend's own capabilities", func(t *testing.T) {
		t.Parallel()
		// The property the decorator broke, stated directly: setting the
		// reason must not change what the backend answers to any capability
		// probe, because nothing wraps it.
		be := &FakeBackend{BackendName: "bridge"}
		o := NewObserver(Options{Backend: be, TransportUnavailableReason: "bind: address already in use"})
		if o.backend != Backend(be) {
			t.Fatalf("the Observer holds %T, not the backend it was given — something wrapped it", o.backend)
		}
	})
}

// TestNormalizeTransportAuthClass pins the closed vocabulary the metrics
// label is drawn from. Its whole job is to make the label's value set a
// property of THIS build rather than of what a remote sent.
func TestNormalizeTransportAuthClass(t *testing.T) {
	t.Parallel()
	for _, known := range transportAuthClasses {
		if got := NormalizeTransportAuthClass(known); got != known {
			t.Errorf("NormalizeTransportAuthClass(%q) = %q, want it unchanged", known, got)
		}
	}
	for _, hostile := range []string{"", "bad_token\" injected=\"1", "protocol_version ", "anything-else"} {
		if got := NormalizeTransportAuthClass(hostile); got != TransportAuthClassUnknown {
			t.Errorf("NormalizeTransportAuthClass(%q) = %q, want %q", hostile, got, TransportAuthClassUnknown)
		}
	}
}

// TestKnownNetworkAccountingMode pins the mode vocabulary for the same
// reason: IsMeasuringNetworkMode reads "not off, not unavailable" as a
// POSITIVE claim that bytes are being counted, which is only sound over
// modes this build defines.
func TestKnownNetworkAccountingMode(t *testing.T) {
	t.Parallel()
	for _, m := range NetworkAccountingModes() {
		if !KnownNetworkAccountingMode(m) {
			t.Errorf("KnownNetworkAccountingMode(%q) = false for a mode this build defines", m)
		}
	}
	for _, m := range []string{"", "tcp+udp", "tcp\"injected", "TCP", " tcp"} {
		if KnownNetworkAccountingMode(m) {
			t.Errorf("KnownNetworkAccountingMode(%q) = true", m)
		}
	}
	// The returned slice is a copy: a caller must not be able to edit the
	// package's own vocabulary.
	got := NetworkAccountingModes()
	got[0] = "mutated"
	if NetworkAccountingModes()[0] == "mutated" {
		t.Error("NetworkAccountingModes returned the package's own slice")
	}
}

// TestMergeTransportStatsAuthClassTravelsWithReason pins that the bounded
// class and the verbatim reason stay ONE fact through aggregation. A class
// from one transport beside a reason from another would be a diagnosis
// nothing recorded.
func TestMergeTransportStatsAuthClassTravelsWithReason(t *testing.T) {
	t.Parallel()
	older := TransportStats{
		AuthFailures: 1, LastAuthError: "invalid token",
		LastAuthErrorClass: TransportAuthClassTokenMismatch, LastAuthFailureAt: tsAt(10),
	}
	newer := TransportStats{
		AuthFailures: 1, LastAuthError: "capturer speaks protocol v2, this daemon speaks v1",
		LastAuthErrorClass: TransportAuthClassProtocolVersion, LastAuthFailureAt: tsAt(40),
	}
	got := mergeTransportStats(mergeTransportStats(TransportStats{}, older), newer)
	if got.LastAuthErrorClass != TransportAuthClassProtocolVersion || got.LastAuthError != newer.LastAuthError {
		t.Errorf("merged (class, reason) = (%q, %q), want the newest pair", got.LastAuthErrorClass, got.LastAuthError)
	}
	rev := mergeTransportStats(mergeTransportStats(TransportStats{}, newer), older)
	if rev.LastAuthErrorClass != TransportAuthClassProtocolVersion {
		t.Errorf("reversed merge class = %q — the pair must follow the timestamp, not iteration order", rev.LastAuthErrorClass)
	}
}

// TestMergeTransportStatsCapturerDecodePresence pins the composite rule: a
// child that never reported may not pull the aggregate's presence flag true
// with its zeroes, which would render "nothing was decoded" as "nothing failed
// to decode".
func TestMergeTransportStatsCapturerDecodePresence(t *testing.T) {
	t.Parallel()

	silent := TransportStats{Connections: 1}
	reported := TransportStats{
		Connections:            1,
		CapturerDecodeReported: true,
		CapturerDecode:         CapturerDecodeStats{NetworkDropped: 2},
		CapturerDecodeAt:       time.Now(),
	}

	if agg := mergeTransportStats(TransportStats{}, silent); agg.CapturerDecodeReported {
		t.Fatalf("a silent child set the aggregate's presence flag: %+v", agg)
	}
	agg := mergeTransportStats(mergeTransportStats(TransportStats{}, silent), reported)
	if !agg.CapturerDecodeReported || agg.CapturerDecode.NetworkDropped != 2 {
		t.Fatalf("a reporting child did not reach the aggregate: %+v", agg)
	}
	agg = mergeTransportStats(agg, reported)
	if agg.CapturerDecode.NetworkDropped != 4 {
		t.Fatalf("two reporting children did not sum: %+v", agg.CapturerDecode)
	}
}

// TestCapturerDecodeAnyExcludesClassificationCounters is the guard on the
// single easiest way to break this feature: folding Ignored into Any().
//
// Ignored counts events the decoder CORRECTLY declined to treat as data —
// control-plane, TCP connect/disconnect/accept/retransmit, UDP with no
// handler — and a healthy elevated capture produces a great many of them. If
// Any() counted them, every healthy capturer would report a decode fault, and
// the surfaces that render Any() would be inverted: noisy exactly when the
// feed is working.
func TestCapturerDecodeAnyExcludesClassificationCounters(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   CapturerDecodeStats
		want bool
	}{
		{"zero value", CapturerDecodeStats{}, false},
		{"a busy healthy capture ignores plenty", CapturerDecodeStats{NetworkIgnored: 1_000_000, NetworkDecoded: 4321}, false},
		{"ignored alone is not a fault", CapturerDecodeStats{NetworkIgnored: 999}, false},
		{"decoded alone is not a fault", CapturerDecodeStats{NetworkDecoded: 999}, false},
		{"dropped is", CapturerDecodeStats{NetworkDropped: 1, NetworkIgnored: 500, NetworkDecoded: 500}, true},
		{"so is an unsupported version", CapturerDecodeStats{NetworkUnsupportedVersion: 1, NetworkIgnored: 500}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.in.Any(); got != tc.want {
				t.Errorf("Any() = %v, want %v for %+v", got, tc.want, tc.in)
			}
		})
	}
}

// TestCapturerDecodeNothingClassified pins the E6b conjunction: events
// arrived, none was classified as data, and none was refused.
//
// It also pins the two things the predicate must NOT do — fire on a healthy
// busy capture, and fire at the same time as Any() (they are a partition, so
// no surface has to decide which of two live signals to render).
func TestCapturerDecodeNothingClassified(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   CapturerDecodeStats
		want bool
	}{
		{
			name: "the renumbered-provider shape: everything ignored, nothing decoded, nothing refused",
			in:   CapturerDecodeStats{NetworkIgnored: 48_211},
			want: true,
		},
		{
			name: "a healthy capture that decoded data events does not fire",
			in:   CapturerDecodeStats{NetworkIgnored: 48_211, NetworkDecoded: 1},
			want: false,
		},
		{
			name: "nothing has arrived at all — not evidence of anything",
			in:   CapturerDecodeStats{},
			want: false,
		},
		{
			name: "a decoder that is refusing speaks through Any(), not through this",
			in:   CapturerDecodeStats{NetworkIgnored: 500, NetworkDropped: 3},
			want: false,
		},
		{
			name: "an unsupported version likewise",
			in:   CapturerDecodeStats{NetworkIgnored: 500, NetworkUnsupportedVersion: 3},
			want: false,
		},
		{
			name: "an older capturer that omits both new counters fires nothing",
			in:   CapturerDecodeStats{NetworkDropped: 0, NetworkUnsupportedVersion: 0},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.in.NothingClassified(); got != tc.want {
				t.Errorf("NothingClassified() = %v, want %v for %+v", got, tc.want, tc.in)
			}
			if tc.in.NothingClassified() && tc.in.Any() {
				t.Errorf("both predicates fired on %+v — they must be mutually exclusive", tc.in)
			}
		})
	}
}

// TestMergeTransportStatsCapturerClassificationCounters pins that the two new
// counters follow the SAME rules as the refusal counters they travel with:
// they sum across reporting children and a silent child contributes nothing.
//
// The silent half matters most. If a never-reported child could contribute
// its zeroed Ignored/Decoded, an aggregate would still read NothingClassified
// false — but only by accident; the rule being pinned is that absence never
// enters the arithmetic at all.
func TestMergeTransportStatsCapturerClassificationCounters(t *testing.T) {
	t.Parallel()

	silent := TransportStats{Connections: 1}
	renumbered := TransportStats{
		Connections:            1,
		CapturerDecodeReported: true,
		CapturerDecode:         CapturerDecodeStats{NetworkIgnored: 900},
		CapturerDecodeAt:       time.Now(),
	}
	healthy := TransportStats{
		Connections:            1,
		CapturerDecodeReported: true,
		CapturerDecode:         CapturerDecodeStats{NetworkIgnored: 100, NetworkDecoded: 25},
		CapturerDecodeAt:       time.Now(),
	}

	agg := mergeTransportStats(mergeTransportStats(TransportStats{}, silent), renumbered)
	if agg.CapturerDecode.NetworkIgnored != 900 || agg.CapturerDecode.NetworkDecoded != 0 {
		t.Fatalf("a silent child perturbed the classification counters: %+v", agg.CapturerDecode)
	}
	if !agg.CapturerDecode.NothingClassified() {
		t.Fatalf("the renumbered signature did not survive the merge: %+v", agg.CapturerDecode)
	}

	agg = mergeTransportStats(agg, healthy)
	if agg.CapturerDecode.NetworkIgnored != 1000 || agg.CapturerDecode.NetworkDecoded != 25 {
		t.Fatalf("two reporting children did not sum: %+v", agg.CapturerDecode)
	}
	if agg.CapturerDecode.NothingClassified() {
		t.Fatalf("one capturer that decoded data events must clear the aggregate signature: %+v", agg.CapturerDecode)
	}
}
