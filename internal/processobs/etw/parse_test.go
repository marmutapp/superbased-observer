package etw

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"syscall"
	"testing"
)

// These tests are the primary deliverable's proof. CI has no Windows runner, so
// the ONLY automated evidence this package produces is the decode layer running
// on Linux against canned bytes laid out exactly as the
// Microsoft-Windows-Kernel-Network manifest declares them.

// encodeV4 builds an IPv4 data-event payload: PID u32-LE, size u32-LE, daddr
// (4 raw network-order bytes), saddr (4), dport u16-BE, sport u16-BE,
// [startime u32-LE, endtime u32-LE — TCP send templates only], seqnum u32-LE,
// connid u32-LE. 36 bytes with the times, 28 without.
func encodeV4(pid, size uint32, dst, src netip.Addr, dport, sport uint16, withTimes bool) []byte {
	b := make([]byte, 0, 36)
	b = binary.LittleEndian.AppendUint32(b, pid)
	b = binary.LittleEndian.AppendUint32(b, size)
	d := dst.As4()
	s := src.As4()
	b = append(b, d[:]...)
	b = append(b, s[:]...)
	b = binary.BigEndian.AppendUint16(b, dport)
	b = binary.BigEndian.AppendUint16(b, sport)
	if withTimes {
		b = binary.LittleEndian.AppendUint32(b, 1015872) // startime
		b = binary.LittleEndian.AppendUint32(b, 1015879) // endtime
	}
	b = binary.LittleEndian.AppendUint32(b, 0) // seqnum
	b = binary.LittleEndian.AppendUint32(b, 0) // connid
	return b
}

// encodeV6 builds an IPv6 data-event payload: same shape, but daddr/saddr are
// 16-byte binary fields. 60 bytes with the times, 52 without.
func encodeV6(pid, size uint32, dst, src netip.Addr, dport, sport uint16, withTimes bool) []byte {
	b := make([]byte, 0, 60)
	b = binary.LittleEndian.AppendUint32(b, pid)
	b = binary.LittleEndian.AppendUint32(b, size)
	d := dst.As16()
	s := src.As16()
	b = append(b, d[:]...)
	b = append(b, s[:]...)
	b = binary.BigEndian.AppendUint16(b, dport)
	b = binary.BigEndian.AppendUint16(b, sport)
	if withTimes {
		b = binary.LittleEndian.AppendUint32(b, 1015872)
		b = binary.LittleEndian.AppendUint32(b, 1015879)
	}
	b = binary.LittleEndian.AppendUint32(b, 0)
	b = binary.LittleEndian.AppendUint32(b, 0)
	return b
}

func TestClassify(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		eventID uint16
		want    EventClass
	}{
		{"tcp send ipv4", 10, ClassTCPData},
		{"tcp recv ipv4", 11, ClassTCPData},
		{"tcp send ipv6", 26, ClassTCPData},
		{"tcp recv ipv6", 27, ClassTCPData},
		{"udp send ipv4", 42, ClassUDPDatagram},
		{"udp recv ipv4", 43, ClassUDPDatagram},
		{"udp send ipv6", 58, ClassUDPDatagram},
		{"udp recv ipv6", 59, ClassUDPDatagram},
		{"tcp connect ipv4 carries no transferred bytes", 12, ClassIgnored},
		{"tcp disconnect ipv4", 13, ClassIgnored},
		// 14/30 and 18/34 DO carry a size field — same template as receive —
		// and are excluded deliberately for Linux parity, not for lack of a
		// byte count. See the event-id block in parse.go, including the open
		// question about tcp_cleanup_rbuf vs event 11 alone.
		{"tcp retransmit ipv4 carries size but is excluded for parity", 14, ClassIgnored},
		{"tcp retransmit ipv6 carries size but is excluded for parity", 30, ClassIgnored},
		{"tcp copy ipv4 carries size but is excluded for parity", 18, ClassIgnored},
		{"tcp copy ipv6 carries size but is excluded for parity", 34, ClassIgnored},
		{"tcp accept ipv4", 15, ClassIgnored},
		{"tcp reconnect ipv4", 16, ClassIgnored},
		{"tcp fail", 17, ClassIgnored},
		{"tcp connect ipv6", 28, ClassIgnored},
		{"udp fail", 49, ClassIgnored},
		{"unknown id degrades to ignored, never to a wrong class", 9999, ClassIgnored},
		{"zero id", 0, ClassIgnored},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Classify(tc.eventID); got != tc.want {
				t.Fatalf("Classify(%d) = %v, want %v", tc.eventID, got, tc.want)
			}
		})
	}
}

func TestDecodeTCPData(t *testing.T) {
	t.Parallel()

	v4Dst := netip.MustParseAddr("142.250.185.78")
	v4Src := netip.MustParseAddr("192.168.1.128")
	v6Dst := netip.MustParseAddr("2606:4700:4700::1111")
	v6Src := netip.MustParseAddr("2001:db8::5")

	tests := []struct {
		name    string
		eventID uint16
		payload []byte
		want    TCPDataEvent
	}{
		{
			name:    "ipv4 send",
			eventID: 10,
			payload: encodeV4(6780, 187, v4Dst, v4Src, 443, 17371, true),
			want: TCPDataEvent{
				PID: 6780, Bytes: 187,
				Direction: DirectionSend, Family: FamilyIPv4,
				SourceAddr: v4Src, SourcePort: 17371,
				DestAddr: v4Dst, DestPort: 443,
			},
		},
		{
			name:    "ipv4 receive has no startime/endtime pair",
			eventID: 11,
			payload: encodeV4(6780, 1440, v4Dst, v4Src, 443, 17371, false),
			want: TCPDataEvent{
				PID: 6780, Bytes: 1440,
				Direction: DirectionReceive, Family: FamilyIPv4,
				SourceAddr: v4Src, SourcePort: 17371,
				DestAddr: v4Dst, DestPort: 443,
			},
		},
		{
			name:    "ipv6 send",
			eventID: 26,
			payload: encodeV6(4242, 969, v6Dst, v6Src, 443, 51348, true),
			want: TCPDataEvent{
				PID: 4242, Bytes: 969,
				Direction: DirectionSend, Family: FamilyIPv6,
				SourceAddr: v6Src, SourcePort: 51348,
				DestAddr: v6Dst, DestPort: 443,
			},
		},
		{
			name:    "ipv6 receive",
			eventID: 27,
			payload: encodeV6(4242, 8, v6Dst, v6Src, 443, 51348, false),
			want: TCPDataEvent{
				PID: 4242, Bytes: 8,
				Direction: DirectionReceive, Family: FamilyIPv6,
				SourceAddr: v6Src, SourcePort: 51348,
				DestAddr: v6Dst, DestPort: 443,
			},
		},
		{
			name:    "zero-byte event decodes rather than being treated as absent",
			eventID: 11,
			payload: encodeV4(1, 0, v4Dst, v4Src, 80, 1025, false),
			want: TCPDataEvent{
				PID: 1, Bytes: 0,
				Direction: DirectionReceive, Family: FamilyIPv4,
				SourceAddr: v4Src, SourcePort: 1025,
				DestAddr: v4Dst, DestPort: 80,
			},
		},
		{
			name:    "maximum uint32 size does not overflow the int64 field",
			eventID: 11,
			payload: encodeV4(7, 0xFFFFFFFF, v4Dst, v4Src, 80, 1025, false),
			want: TCPDataEvent{
				PID: 7, Bytes: 4294967295,
				Direction: DirectionReceive, Family: FamilyIPv4,
				SourceAddr: v4Src, SourcePort: 1025,
				DestAddr: v4Dst, DestPort: 80,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := DecodeTCPData(tc.eventID, 0, tc.payload)
			if err != nil {
				t.Fatalf("DecodeTCPData(%d): unexpected error: %v", tc.eventID, err)
			}
			if got != tc.want {
				t.Fatalf("DecodeTCPData(%d) = %+v, want %+v", tc.eventID, got, tc.want)
			}
		})
	}
}

func TestDecodeUDPDatagram(t *testing.T) {
	t.Parallel()

	v4Dst := netip.MustParseAddr("8.8.8.8")
	v4Src := netip.MustParseAddr("192.168.1.128")
	v6Dst := netip.MustParseAddr("2001:4860:4860::8888")
	v6Src := netip.MustParseAddr("2001:db8::9")

	tests := []struct {
		name    string
		eventID uint16
		payload []byte
		want    UDPDatagramEvent
	}{
		{
			name:    "ipv4 send",
			eventID: 42,
			payload: encodeV4(900, 64, v4Dst, v4Src, 53, 60123, false),
			want: UDPDatagramEvent{
				PID: 900, Bytes: 64,
				Direction: DirectionSend, Family: FamilyIPv4,
				SourceAddr: v4Src, SourcePort: 60123,
				DestAddr: v4Dst, DestPort: 53,
			},
		},
		{
			name:    "ipv6 receive",
			eventID: 59,
			payload: encodeV6(900, 128, v6Dst, v6Src, 53, 60123, false),
			want: UDPDatagramEvent{
				PID: 900, Bytes: 128,
				Direction: DirectionReceive, Family: FamilyIPv6,
				SourceAddr: v6Src, SourcePort: 60123,
				DestAddr: v6Dst, DestPort: 53,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := DecodeUDPDatagram(tc.eventID, 0, tc.payload)
			if err != nil {
				t.Fatalf("DecodeUDPDatagram(%d): unexpected error: %v", tc.eventID, err)
			}
			if got != tc.want {
				t.Fatalf("DecodeUDPDatagram(%d) = %+v, want %+v", tc.eventID, got, tc.want)
			}
		})
	}
}

// TestUDPIsNeverCountedAsTCP is the scope guard for the parity contract: Linux
// counts TCP only, so a UDP event must not be able to reach a TCP-labelled
// total. Three independent barriers are asserted — the classifier never says
// TCP, the TCP decoder refuses the id outright, and the two decoders return
// structurally unrelated types (enforced by the compiler at the assignment
// below). A future "let's include UDP" edit has to confront all three.
func TestUDPIsNeverCountedAsTCP(t *testing.T) {
	t.Parallel()

	udpIDs := []uint16{42, 43, 58, 59}
	v4Dst := netip.MustParseAddr("8.8.4.4")
	v4Src := netip.MustParseAddr("10.0.0.5")
	v6Dst := netip.MustParseAddr("2001:4860:4860::8844")
	v6Src := netip.MustParseAddr("2001:db8::7")

	for _, id := range udpIDs {
		payload := encodeV4(1234, 512, v4Dst, v4Src, 53, 40000, false)
		if id == 58 || id == 59 {
			payload = encodeV6(1234, 512, v6Dst, v6Src, 53, 40000, false)
		}

		if got := Classify(id); got == ClassTCPData {
			t.Fatalf("Classify(%d) = %v; a UDP event must never classify as TCP", id, got)
		}
		if _, err := DecodeTCPData(id, 0, payload); !errors.Is(err, ErrNotTCPEvent) {
			t.Fatalf("DecodeTCPData(%d) error = %v, want ErrNotTCPEvent", id, err)
		}

		// It decodes fine through the UDP path — the point is that the result
		// is a UDPDatagramEvent, a type with no assignment path into a TCP
		// total. Uncommenting a `var _ TCPDataEvent = udp` here would not
		// compile, which is the guarantee.
		udp, err := DecodeUDPDatagram(id, 0, payload)
		if err != nil {
			t.Fatalf("DecodeUDPDatagram(%d): unexpected error: %v", id, err)
		}
		if udp.Bytes != 512 {
			t.Fatalf("DecodeUDPDatagram(%d).Bytes = %d, want 512", id, udp.Bytes)
		}
	}
}

func TestDecodeRejectsWrongEventClass(t *testing.T) {
	t.Parallel()

	payload := make([]byte, 64) // long enough for any template; the id is what fails

	tests := []struct {
		name    string
		eventID uint16
		decode  func(uint16, []byte) error
		wantErr error
	}{
		{"tcp decoder refuses udp send v4", 42, decodeTCPErr, ErrNotTCPEvent},
		{"tcp decoder refuses udp recv v6", 59, decodeTCPErr, ErrNotTCPEvent},
		{"tcp decoder refuses a connect event", 12, decodeTCPErr, ErrNotTCPEvent},
		{"tcp decoder refuses an unknown id", 7777, decodeTCPErr, ErrNotTCPEvent},
		{"udp decoder refuses tcp send v4", 10, decodeUDPErr, ErrNotUDPEvent},
		{"udp decoder refuses tcp recv v6", 27, decodeUDPErr, ErrNotUDPEvent},
		{"udp decoder refuses a failure event", 49, decodeUDPErr, ErrNotUDPEvent},
		{"udp decoder refuses an unknown id", 7777, decodeUDPErr, ErrNotUDPEvent},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.decode(tc.eventID, payload)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func decodeTCPErr(id uint16, b []byte) error {
	_, err := DecodeTCPData(id, 0, b)
	return err
}

func decodeUDPErr(id uint16, b []byte) error {
	_, err := DecodeUDPDatagram(id, 0, b)
	return err
}

func TestDecodeRejectsShortPayload(t *testing.T) {
	t.Parallel()

	v4Dst := netip.MustParseAddr("1.1.1.1")
	v4Src := netip.MustParseAddr("10.0.0.1")
	v6Dst := netip.MustParseAddr("2606:4700:4700::1001")
	v6Src := netip.MustParseAddr("2001:db8::1")

	tests := []struct {
		name    string
		eventID uint16
		payload []byte
	}{
		{"empty", 11, nil},
		{"ipv4 recv one byte short", 11, encodeV4(1, 1, v4Dst, v4Src, 80, 1, false)[:27]},
		{"ipv4 send truncated to the recv length", 10, encodeV4(1, 1, v4Dst, v4Src, 80, 1, true)[:28]},
		{"ipv6 recv one byte short", 27, encodeV6(1, 1, v6Dst, v6Src, 80, 1, false)[:51]},
		{"ipv6 send truncated to the recv length", 26, encodeV6(1, 1, v6Dst, v6Src, 80, 1, true)[:52]},
		{"prefix-only payload is still rejected", 11, encodeV4(1, 1, v4Dst, v4Src, 80, 1, false)[:20]},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := DecodeTCPData(tc.eventID, 0, tc.payload); !errors.Is(err, ErrShortPayload) {
				t.Fatalf("DecodeTCPData(%d, %d bytes) error = %v, want ErrShortPayload", tc.eventID, len(tc.payload), err)
			}
		})
	}
}

// TestDecodeToleratesTrailingBytes pins the deliberate asymmetry: short is
// fatal, long is fine. Every field read sits in the fixed leading prefix, so a
// build that widened a trailing field (the legacy MOF classes declare connid
// with the WMI Pointer qualifier, which would make it 8 bytes) must still
// decode rather than dropping every event.
func TestDecodeToleratesTrailingBytes(t *testing.T) {
	t.Parallel()

	dst := netip.MustParseAddr("142.250.185.78")
	src := netip.MustParseAddr("192.168.1.128")
	payload := encodeV4(6780, 187, dst, src, 443, 17371, true)
	payload = append(payload, 0, 0, 0, 0) // as if connid were pointer-sized

	got, err := DecodeTCPData(10, 0, payload)
	if err != nil {
		t.Fatalf("DecodeTCPData with 4 trailing bytes: unexpected error: %v", err)
	}
	if got.PID != 6780 || got.Bytes != 187 {
		t.Fatalf("DecodeTCPData = %+v, want PID 6780 / Bytes 187", got)
	}
}

// TestBytesIsPerEventNotCumulative is the W1 half of the parity contract
// (plan §0.1). ETW reports what THIS event moved; the running total is W2's
// accumulator, not the decoder's. If a later refactor ever made the decoder
// stateful, this fails.
func TestBytesIsPerEventNotCumulative(t *testing.T) {
	t.Parallel()

	dst := netip.MustParseAddr("142.250.185.78")
	src := netip.MustParseAddr("192.168.1.128")

	perEvent := []uint32{100, 250, 90}
	wantEach := []int64{100, 250, 90}
	wantCumulativeIfBuggy := []int64{100, 350, 440}

	for i, size := range perEvent {
		ev, err := DecodeTCPData(11, 0, encodeV4(4321, size, dst, src, 443, 5555, false))
		if err != nil {
			t.Fatalf("event %d: unexpected error: %v", i, err)
		}
		if ev.Bytes != wantEach[i] {
			t.Fatalf("event %d: Bytes = %d, want the per-event %d (a running total would read %d)",
				i, ev.Bytes, wantEach[i], wantCumulativeIfBuggy[i])
		}
	}
}

// TestPIDIsTakenFromThePayload pins the attribution decision. The socket owner
// lives in the first 4 bytes of the payload; EVENT_HEADER.ProcessId is the
// interrupt-time context and is never consulted. Decoding a payload whose PID
// is a plausible-but-wrong value like 4 (System) must yield the payload's
// value, not something derived elsewhere.
func TestPIDIsTakenFromThePayload(t *testing.T) {
	t.Parallel()

	dst := netip.MustParseAddr("142.250.185.78")
	src := netip.MustParseAddr("192.168.1.128")

	for _, pid := range []uint32{4, 1, 6780, 0xFFFF} {
		ev, err := DecodeTCPData(11, 0, encodeV4(pid, 10, dst, src, 443, 5555, false))
		if err != nil {
			t.Fatalf("pid %d: unexpected error: %v", pid, err)
		}
		if ev.PID != int(pid) {
			t.Fatalf("PID = %d, want the payload's %d", ev.PID, pid)
		}
	}
}

// TestPortsAreDecodedNetworkOrder pins the port byte order. This is CONFIRMED,
// not inferred: PerfView's KernelTraceEventParser — Microsoft's own consumer
// for these event classes — applies an explicit ByteSwap16 to dport and sport
// at exactly the offsets used here (16/18 for IPv4, 40/42 for IPv6), which is a
// statement that the raw payload holds them big-endian. Ports are inert for
// byte accounting; this test exists so the decision has one documented place.
func TestPortsAreDecodedNetworkOrder(t *testing.T) {
	t.Parallel()

	dst := netip.MustParseAddr("142.250.185.78")
	src := netip.MustParseAddr("192.168.1.128")
	payload := encodeV4(1, 1, dst, src, 443, 80, false)

	// Bytes 16..18 hold dport big-endian: 443 == 0x01BB.
	if payload[16] != 0x01 || payload[17] != 0xBB {
		t.Fatalf("fixture is not big-endian: dport bytes = %#x %#x", payload[16], payload[17])
	}
	got, err := DecodeTCPData(11, 0, payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.DestPort != 443 || got.SourcePort != 80 {
		t.Fatalf("ports = %d/%d, want 443/80", got.DestPort, got.SourcePort)
	}
}

func TestStringers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		got  string
		want string
	}{
		{ClassTCPData.String(), "tcp_data"},
		{ClassUDPDatagram.String(), "udp_datagram"},
		{ClassIgnored.String(), "ignored"},
		{EventClass(200).String(), "ignored"},
		{DirectionSend.String(), "send"},
		{DirectionReceive.String(), "receive"},
		{DirectionUnknown.String(), "unknown"},
		{FamilyIPv4.String(), "ipv4"},
		{FamilyIPv6.String(), "ipv6"},
		{FamilyUnknown.String(), "unknown"},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("String() = %q, want %q", tc.got, tc.want)
		}
	}
}

func TestOptionsValidate(t *testing.T) {
	t.Parallel()

	t.Run("rejects a session with no handler", func(t *testing.T) {
		t.Parallel()
		opts := Options{}
		if err := opts.validate(); !errors.Is(err, ErrNoHandler) {
			t.Fatalf("validate() = %v, want ErrNoHandler", err)
		}
	})

	t.Run("fills defaults", func(t *testing.T) {
		t.Parallel()
		opts := Options{OnTCP: func(TCPDataEvent) {}}
		if err := opts.validate(); err != nil {
			t.Fatalf("validate() unexpected error: %v", err)
		}
		if opts.SessionName != DefaultSessionName {
			t.Errorf("SessionName = %q, want %q", opts.SessionName, DefaultSessionName)
		}
		if opts.BufferSizeKB == 0 || opts.MinimumBuffers == 0 || opts.MaximumBuffers < opts.MinimumBuffers {
			t.Errorf("buffer geometry not filled sanely: %+v", opts)
		}
	})

	t.Run("a UDP-only session is allowed but explicit", func(t *testing.T) {
		t.Parallel()
		opts := Options{OnUDP: func(UDPDatagramEvent) {}}
		if err := opts.validate(); err != nil {
			t.Fatalf("validate() unexpected error: %v", err)
		}
	})

	t.Run("maximum buffers never drops below minimum", func(t *testing.T) {
		t.Parallel()
		opts := Options{OnTCP: func(TCPDataEvent) {}, MinimumBuffers: 512}
		if err := opts.validate(); err != nil {
			t.Fatalf("validate() unexpected error: %v", err)
		}
		if opts.MaximumBuffers < opts.MinimumBuffers {
			t.Fatalf("MaximumBuffers %d < MinimumBuffers %d", opts.MaximumBuffers, opts.MinimumBuffers)
		}
	})
}

// TestKeywordMasks pins the manifest's keyword values. Getting these wrong
// means a session that starts, elevates, and receives nothing — a failure that
// looks exactly like "no network traffic".
func TestKeywordMasks(t *testing.T) {
	t.Parallel()
	if KeywordIPv4 != 0x10 {
		t.Errorf("KeywordIPv4 = %#x, want 0x10", KeywordIPv4)
	}
	if KeywordIPv6 != 0x20 {
		t.Errorf("KeywordIPv6 = %#x, want 0x20", KeywordIPv6)
	}
	if KernelNetworkProviderGUID != "{7dd42a49-5329-4832-8dfd-43d979153a88}" {
		t.Errorf("provider guid = %q", KernelNetworkProviderGUID)
	}
}

// TestTemplateTableMatchesTheManifestRows compares the layout table against a
// LITERAL transcription of the manifest, written out here independently of the
// code's own rows.
//
// What it does and does not claim, precisely — its predecessor
// (TestTemplateLengthsMatchTheManifest) got this wrong and was worthless
// because of it. That test re-derived each expected length FROM the row's own
// family/direction/class, so mutating a row to FamilyIPv6/minLen 60 mutated the
// expectation in lockstep and it stayed green: a self-consistency check wearing
// a manifest check's name.
//
// This one is external: every expected value below is a literal, so any edit to
// `templates` must be mirrored here deliberately. That is ALL it verifies — it
// is a change-detector on the table's contents, not proof the manifest says so.
// The offsets, which are what actually decode wrong, are covered independently
// by TestDecodeTCPData / TestDecodeUDPDatagram, whose fixture encoders are a
// separate oracle written from the field widths rather than from these rows.
//
// The arithmetic each length comes from, kept as documentation rather than as
// the assertion:
//
//	PID u32 + size u32 + daddr + saddr + dport u16 + sport u16 +
//	[startime u32 + endtime u32, TCP SEND only] + seqnum u32 + connid u32
//	with daddr/saddr 4 bytes (IPv4) or 16 bytes (IPv6).
func TestTemplateTableMatchesTheManifestRows(t *testing.T) {
	t.Parallel()

	want := map[uint16]template{
		10: {class: ClassTCPData, direction: DirectionSend, family: FamilyIPv4, minLen: 36},
		11: {class: ClassTCPData, direction: DirectionReceive, family: FamilyIPv4, minLen: 28},
		26: {class: ClassTCPData, direction: DirectionSend, family: FamilyIPv6, minLen: 60},
		27: {class: ClassTCPData, direction: DirectionReceive, family: FamilyIPv6, minLen: 52},
		42: {class: ClassUDPDatagram, direction: DirectionSend, family: FamilyIPv4, minLen: 28},
		43: {class: ClassUDPDatagram, direction: DirectionReceive, family: FamilyIPv4, minLen: 28},
		58: {class: ClassUDPDatagram, direction: DirectionSend, family: FamilyIPv6, minLen: 52},
		59: {class: ClassUDPDatagram, direction: DirectionReceive, family: FamilyIPv6, minLen: 52},
	}

	if len(templates) != len(want) {
		t.Fatalf("templates has %d rows, the manifest transcription has %d", len(templates), len(want))
	}
	for id, w := range want {
		got, ok := templates[id]
		if !ok {
			t.Errorf("event %d: missing from templates", id)
			continue
		}
		if got != w {
			t.Errorf("event %d: templates row = %+v, manifest transcription = %+v", id, got, w)
		}
	}
	for id := range templates {
		if _, ok := want[id]; !ok {
			t.Errorf("event %d: present in templates but not in the manifest transcription", id)
		}
	}
}

// TestNoTemplateRowIsFamilyless pins the invariant decodeCommon's
// FamilyUnknown branch documents as unreachable. If it ever becomes reachable,
// this is what says so — not a mis-labelled ErrShortPayload at runtime.
func TestNoTemplateRowIsFamilyless(t *testing.T) {
	t.Parallel()
	for id, tpl := range templates {
		if tpl.family == FamilyUnknown {
			t.Errorf("event %d: template row has no address family", id)
		}
	}
}

// TestDecodeRefusesUnsupportedEventVersion is the §0.1 guard for a template
// version bump. minLen is a FLOOR and the table is keyed on id alone, so a
// version-1 event with a moved field would otherwise pass the length check and
// decode into plausible-looking garbage. It must be refused with its own
// sentinel so the failure is countable, not silent.
func TestDecodeRefusesUnsupportedEventVersion(t *testing.T) {
	t.Parallel()

	dst := netip.MustParseAddr("142.250.185.78")
	src := netip.MustParseAddr("192.168.1.128")
	v4 := encodeV4(6780, 187, dst, src, 443, 17371, true)
	v6 := encodeV6(4242, 969, netip.MustParseAddr("2606:4700:4700::1111"), netip.MustParseAddr("2001:db8::5"), 443, 51348, true)

	t.Run("version 0 is the only decoded version", func(t *testing.T) {
		t.Parallel()
		if supportedEventVersion != 0 {
			t.Fatalf("supportedEventVersion = %d, want 0", supportedEventVersion)
		}
		if _, err := DecodeTCPData(10, supportedEventVersion, v4); err != nil {
			t.Fatalf("version %d must decode: %v", supportedEventVersion, err)
		}
	})

	t.Run("tcp refuses a newer version even with a long-enough payload", func(t *testing.T) {
		t.Parallel()
		for _, ver := range []uint8{1, 2, 255} {
			if _, err := DecodeTCPData(10, ver, v4); !errors.Is(err, ErrUnsupportedEventVersion) {
				t.Errorf("DecodeTCPData(10, v%d) error = %v, want ErrUnsupportedEventVersion", ver, err)
			}
			if _, err := DecodeTCPData(26, ver, v6); !errors.Is(err, ErrUnsupportedEventVersion) {
				t.Errorf("DecodeTCPData(26, v%d) error = %v, want ErrUnsupportedEventVersion", ver, err)
			}
		}
	})

	t.Run("udp refuses a newer version", func(t *testing.T) {
		t.Parallel()
		udp := encodeV4(900, 64, dst, src, 53, 60123, false)
		if _, err := DecodeUDPDatagram(42, 1, udp); !errors.Is(err, ErrUnsupportedEventVersion) {
			t.Errorf("DecodeUDPDatagram(42, v1) error = %v, want ErrUnsupportedEventVersion", err)
		}
	})

	t.Run("a wrong-class id still reports the class, not the version", func(t *testing.T) {
		t.Parallel()
		// Ordering matters for legibility: being handed a UDP id is a caller
		// bug, being handed version 1 is a Windows change. The first is the
		// more specific complaint and must win.
		if _, err := DecodeTCPData(42, 7, v4); !errors.Is(err, ErrNotTCPEvent) {
			t.Errorf("DecodeTCPData(42, v7) error = %v, want ErrNotTCPEvent", err)
		}
	})

	t.Run("the version refusal does not decode anything", func(t *testing.T) {
		t.Parallel()
		got, err := DecodeTCPData(11, 1, encodeV4(6780, 1440, dst, src, 443, 17371, false))
		if err == nil {
			t.Fatal("version 1 decoded")
		}
		if got != (TCPDataEvent{}) {
			t.Fatalf("refused decode returned %+v, want the zero value", got)
		}
	})
}

// TestKeywordsDefaultToBothFamilies pins the parity decision in Options: the
// Linux backend's probes (fexit/tcp_sendmsg, fentry/tcp_cleanup_rbuf) are
// address-family agnostic and count IPv4 + IPv6 together, so a Windows capture
// that enabled only KERNEL_NETWORK_KEYWORD_IPV4 would silently omit every IPv6
// byte from a number being compared against one that includes them.
func TestKeywordsDefaultToBothFamilies(t *testing.T) {
	t.Parallel()

	if got := (Options{}).keywords(); got != KeywordIPv4|KeywordIPv6 {
		t.Errorf("default keywords = %#x, want %#x (both families, for Linux parity)", got, KeywordIPv4|KeywordIPv6)
	}
	if got := (Options{ExcludeIPv6: true}).keywords(); got != KeywordIPv4 {
		t.Errorf("ExcludeIPv6 keywords = %#x, want %#x", got, KeywordIPv4)
	}
	// The opt-out must never be able to remove IPv4 as well.
	if (Options{ExcludeIPv6: true}).keywords()&KeywordIPv4 == 0 {
		t.Error("ExcludeIPv6 dropped the IPv4 keyword")
	}
}

// TestErrnoFromCall pins the (*LazyProc).Call idiom this package must use.
//
// Call's error result is a syscall.Errno boxed in an error interface, ALWAYS —
// so `if err == nil` after a Call is unreachable, and formatting the zero errno
// renders (on Windows) "The operation completed successfully." as the stated
// reason a call failed, on exactly the path an operator reads while debugging.
// The correct idiom is: check the call's own failure sentinel first, then ask
// errnoFromCall whether there is a real errno worth naming.
// zeroErrnoAsError stands in for (*windows.LazyProc).Call's third result: a
// syscall.Errno, returned through an error interface, even when GetLastError
// reported success.
func zeroErrnoAsError() error { return syscall.Errno(0) }

func TestErrnoFromCall(t *testing.T) {
	t.Parallel()

	t.Run("a boxed zero errno is not nil - the footgun itself", func(t *testing.T) {
		t.Parallel()
		// Routed through a func returning error, the way (*LazyProc).Call
		// itself does, so the comparison is against a genuine interface value
		// rather than one whose concrete type is visible at the comparison
		// (which is also why staticcheck's SA4023 does not fire here: the whole
		// point is that a reader CANNOT see the concrete type at the call site).
		if boxed := zeroErrnoAsError(); boxed == nil {
			t.Fatal("syscall.Errno(0) boxed in an error interface compared nil; " +
				"if this ever holds, the `lastErr == nil` idiom would have been fine")
		}
	})

	t.Run("a zero errno yields no reason to report", func(t *testing.T) {
		t.Parallel()
		if _, ok := errnoFromCall(syscall.Errno(0)); ok {
			t.Error("errnoFromCall(Errno(0)) reported a usable errno")
		}
	})

	t.Run("a nil error yields no reason to report", func(t *testing.T) {
		t.Parallel()
		if _, ok := errnoFromCall(nil); ok {
			t.Error("errnoFromCall(nil) reported a usable errno")
		}
	})

	t.Run("a real errno is reported and unwraps", func(t *testing.T) {
		t.Parallel()
		errno, ok := errnoFromCall(syscall.Errno(5))
		if !ok || errno != syscall.Errno(5) {
			t.Fatalf("errnoFromCall(Errno(5)) = %v/%v, want 5/true", errno, ok)
		}
		wrapped := fmt.Errorf("openTrace: %w", errno)
		if !errors.Is(wrapped, syscall.Errno(5)) {
			t.Error("a wrapped errno no longer unwraps to itself")
		}
	})

	t.Run("a wrapped errno is still found", func(t *testing.T) {
		t.Parallel()
		if _, ok := errnoFromCall(fmt.Errorf("outer: %w", syscall.Errno(87))); !ok {
			t.Error("errnoFromCall did not see through a wrap")
		}
	})

	t.Run("a non-errno error reports nothing", func(t *testing.T) {
		t.Parallel()
		if _, ok := errnoFromCall(errors.New("not an errno")); ok {
			t.Error("errnoFromCall claimed a plain error was an errno")
		}
	})
}
