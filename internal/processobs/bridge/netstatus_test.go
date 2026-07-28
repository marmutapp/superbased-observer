package bridge

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/processobs"
)

// TestHelloNetworkAccountingRoundTrip pins the additive hello fields across the
// real Encoder/Decoder, including the compatibility direction that matters: a
// hello WITHOUT them (any capturer built before this wire grew the fields) must
// still decode cleanly, as UNKNOWN — an empty mode — never as a confident
// "off".
func TestHelloNetworkAccountingRoundTrip(t *testing.T) {
	tests := []struct {
		name       string
		line       string
		hello      *Hello // when set, encoded via the real Encoder instead
		wantMode   string
		wantReason string
	}{
		{
			name:       "live capture",
			hello:      &Hello{Backend: "poll+etw", OS: "windows", PID: 42, NetworkAccountingMode: processobs.NetworkAccountingTCP},
			wantMode:   processobs.NetworkAccountingTCP,
			wantReason: "",
		},
		{
			name: "not elevated",
			hello: &Hello{
				Backend:                 "poll",
				OS:                      "windows",
				PID:                     42,
				NetworkAccountingMode:   processobs.NetworkAccountingUnavailable,
				NetworkAccountingReason: "ETW network capture could not start: not elevated",
			},
			wantMode:   processobs.NetworkAccountingUnavailable,
			wantReason: "ETW network capture could not start: not elevated",
		},
		{
			name:     "explicit off",
			hello:    &Hello{Backend: "poll", OS: "windows", PID: 42, NetworkAccountingMode: processobs.NetworkAccountingOff},
			wantMode: processobs.NetworkAccountingOff,
		},
		{
			// A literal pre-W2 line: the fields do not exist on the wire at
			// all. Forward-compat is the decoder's job and this proves it.
			name:     "legacy capturer omits the fields",
			line:     `{"v":1,"kind":"hello","hello":{"backend":"poll","boot_id":"b","os":"windows","pid":7}}`,
			wantMode: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line := tt.line
			if tt.hello != nil {
				var sb strings.Builder
				enc := NewEncoder(&sb)
				if err := enc.Hello(*tt.hello); err != nil {
					t.Fatalf("encode: %v", err)
				}
				line = sb.String()
				// Absent fields must not be emitted at all, so a capturer with
				// nothing to say produces the same bytes it always did.
				if tt.hello.NetworkAccountingReason == "" && strings.Contains(line, "network_accounting_reason") {
					t.Errorf("empty reason was serialized: %s", line)
				}
			}
			dec := NewDecoder(strings.NewReader(line))
			f, err := dec.Next()
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if f.Kind != KindHello || f.Hello == nil {
				t.Fatalf("frame = %+v, want a hello", f)
			}
			if f.Hello.NetworkAccountingMode != tt.wantMode {
				t.Errorf("mode = %q, want %q", f.Hello.NetworkAccountingMode, tt.wantMode)
			}
			if f.Hello.NetworkAccountingReason != tt.wantReason {
				t.Errorf("reason = %q, want %q", f.Hello.NetworkAccountingReason, tt.wantReason)
			}
		})
	}
}

// TestHelloOmitsEmptyNetworkFields pins that a hello with nothing to say about
// network accounting serializes to the pre-W2 shape byte for byte — the
// property that lets a NEW daemon and an OLD capturer (and vice versa) meet
// without a WireVersion bump.
func TestHelloOmitsEmptyNetworkFields(t *testing.T) {
	b, err := json.Marshal(Hello{Backend: "poll", BootID: "b", OS: "windows", PID: 7})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(b), `{"backend":"poll","boot_id":"b","os":"windows","pid":7}`; got != want {
		t.Fatalf("hello JSON = %s, want %s", got, want)
	}
}

// TestIsMeasuringNetworkMode pins the vocabulary rule the stale-status logic
// rests on: only a POSITIVE claim counts as measuring, and an unrecognised mode
// is treated as one (a future scope must be invalidated on capturer death too).
func TestIsMeasuringNetworkMode(t *testing.T) {
	tests := []struct {
		mode string
		want bool
	}{
		{"", false},
		{processobs.NetworkAccountingOff, false},
		{processobs.NetworkAccountingUnavailable, false},
		{processobs.NetworkAccountingTCP, true},
		{"tcp+udp", true},
	}
	for _, tt := range tests {
		if got := IsMeasuringNetworkMode(tt.mode); got != tt.want {
			t.Errorf("IsMeasuringNetworkMode(%q) = %v, want %v", tt.mode, got, tt.want)
		}
	}
}

// TestApplyHelloNetworkStatus covers the daemon-side half: a decoded hello sets
// the shared NetworkAccounting handle, and a hello that says nothing leaves
// whatever the daemon already believed untouched.
func TestApplyHelloNetworkStatus(t *testing.T) {
	tests := []struct {
		name       string
		seedMode   string
		seedReason string
		hello      Hello
		wantMode   string
		wantReason string
	}{
		{
			name:       "live capture is carried across the bridge",
			seedMode:   processobs.NetworkAccountingUnavailable,
			seedReason: "needs the linux_ebpf backend",
			hello:      Hello{NetworkAccountingMode: processobs.NetworkAccountingTCP},
			wantMode:   processobs.NetworkAccountingTCP,
		},
		{
			name:       "unavailable carries its reason",
			hello:      Hello{NetworkAccountingMode: processobs.NetworkAccountingUnavailable, NetworkAccountingReason: "not elevated"},
			wantMode:   processobs.NetworkAccountingUnavailable,
			wantReason: "not elevated",
		},
		{
			name:       "silence is unknown, not off",
			seedMode:   processobs.NetworkAccountingUnavailable,
			seedReason: "needs the linux_ebpf backend",
			hello:      Hello{Backend: "poll"},
			wantMode:   processobs.NetworkAccountingUnavailable,
			wantReason: "needs the linux_ebpf backend",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := &processobs.NetworkAccounting{}
			if tt.seedMode != "" {
				status.Set(tt.seedMode, tt.seedReason)
			}
			b := New(Options{NetworkAccounting: status})
			b.applyHelloNetworkStatus(tt.hello)

			mode, reason := status.Status()
			if mode != tt.wantMode || reason != tt.wantReason {
				t.Fatalf("status = (%q, %q), want (%q, %q)", mode, reason, tt.wantMode, tt.wantReason)
			}
		})
	}
}

// TestApplyHelloNetworkStatusNilHandle pins that a bridge which does NOT own
// the shared handle (the eBPF composite's bridge child) is a no-op, so it can
// never clobber the owning backend's status.
func TestApplyHelloNetworkStatusNilHandle(t *testing.T) {
	b := New(Options{})
	b.applyHelloNetworkStatus(Hello{NetworkAccountingMode: processobs.NetworkAccountingTCP})
	if b.netStatus != nil {
		t.Fatal("expected no status handle")
	}
}

// TestInvalidateNetworkStatus pins the stale-claim rule: a capturer that has
// died can no longer be measuring, so a positive claim is withdrawn — while a
// non-positive one ("off" / "unavailable") is left alone, because it stays true
// whether or not a capturer is running.
func TestInvalidateNetworkStatus(t *testing.T) {
	tests := []struct {
		name       string
		hello      *Hello
		wantMode   string
		wantReason string
	}{
		{
			name:       "a live claim is withdrawn",
			hello:      &Hello{NetworkAccountingMode: processobs.NetworkAccountingTCP},
			wantMode:   processobs.NetworkAccountingUnavailable,
			wantReason: "capturer died",
		},
		{
			name:       "off survives — it was never a measurement claim",
			hello:      &Hello{NetworkAccountingMode: processobs.NetworkAccountingOff, NetworkAccountingReason: "not requested"},
			wantMode:   processobs.NetworkAccountingOff,
			wantReason: "not requested",
		},
		{
			name:     "a capturer that never reported leaves the daemon's own view",
			hello:    nil,
			wantMode: processobs.NetworkAccountingOff,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := &processobs.NetworkAccounting{}
			b := New(Options{NetworkAccounting: status})
			if tt.hello != nil {
				b.applyHelloNetworkStatus(*tt.hello)
			}
			b.invalidateNetworkStatus("capturer died")

			mode, reason := status.Status()
			if mode != tt.wantMode || reason != tt.wantReason {
				t.Fatalf("status = (%q, %q), want (%q, %q)", mode, reason, tt.wantMode, tt.wantReason)
			}
			// Idempotent: a second withdrawal (respawn loop + deferred stop)
			// must not re-stamp anything.
			b.invalidateNetworkStatus("second call")
			if mode2, reason2 := status.Status(); mode2 != tt.wantMode || reason2 != tt.wantReason {
				t.Fatalf("after a second invalidate: (%q, %q), want (%q, %q)", mode2, reason2, tt.wantMode, tt.wantReason)
			}
		})
	}
}
