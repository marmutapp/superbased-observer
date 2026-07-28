package etw

import (
	"encoding/binary"
	"fmt"
	"net/netip"
)

// KernelNetworkProviderGUID is the string form of the
// Microsoft-Windows-Kernel-Network provider GUID. It lives in the untagged file
// so tests and docs on any OS can refer to it; the windows build parses it into
// a windows.GUID once.
//
// Source: the provider's own instrumentation manifest,
// <provider name="Microsoft-Windows-Kernel-Network" guid="{7dd42a49-...}">.
const KernelNetworkProviderGUID = "{7dd42a49-5329-4832-8dfd-43d979153a88}"

// Keyword masks from the Microsoft-Windows-Kernel-Network manifest
// (<keyword name="KERNEL_NETWORK_KEYWORD_IPV4" mask="0x10"/> and
// <keyword name="KERNEL_NETWORK_KEYWORD_IPV6" mask="0x20"/>). Passed as
// EnableTraceEx2's MatchAnyKeyword; MatchAllKeyword stays 0, because setting it
// to the OR would admit ONLY the two dual-tagged failure events.
const (
	KeywordIPv4 uint64 = 0x10
	KeywordIPv6 uint64 = 0x20
)

// Manifest event ids for the Kernel-Network data-transfer events. Every other
// id the provider emits is classified as ignored, but for two different
// reasons, and the distinction matters:
//
//   - connect/accept/disconnect/reconnect/failure genuinely carry no
//     transferred-byte count. Nothing to exclude.
//   - retransmit (14 v4 / 30 v6) and tcp-copy (18 v4 / 34 v6) DO carry a size
//     field — they use the same 28-byte (v4) / 52-byte (v6) template as the
//     receive events, verified in the provider manifest and in microsoft/Tx's
//     generated bindings (KNetEvt_RetransmitIPV4.size,
//     KNetEvt_TcpCopyIPV4.size). They are excluded DELIBERATELY, for Linux
//     parity: the Linux backend attaches fexit/tcp_sendmsg and
//     fentry/tcp_cleanup_rbuf, and tcp_sendmsg does not see retransmissions,
//     so counting them here would inflate the Windows number against a Linux
//     number that cannot contain them.
//
// THE 11-vs-11+18 QUESTION — ANSWERED 2026-07-26 (W2), not assumed.
//
// W1 recorded an open question: does Linux's fentry/tcp_cleanup_rbuf(copied)
// — which counts bytes the APPLICATION has consumed out of the receive queue —
// correspond to Kernel-Network event 11 (recv) ALONE, or to 11 + 18
// (recv + tcp-copy)? It was left at 11 alone as the conservative reading.
//
// The answer is 11 ALONE, and it is stronger than "conservative": summing
// 11 + 18 would DOUBLE-COUNT, so it is affirmatively wrong rather than a
// different trade-off. The evidence, four independent sources:
//
//   - The Windows SDK's own shared/evntrace.h names opcode 0x12 (18)
//     EVENT_TRACE_TYPE_COPY_TCP with the inline comment "Copy in PendData".
//     The copy is OUT OF already-pended data — bytes event 11 has already
//     counted. Its sibling 0x13 COPY_ARP ("NDIS_STATUS_RESOURCES Copy") is
//     unambiguously buffer-handling instrumentation, not a byte flow.
//   - The manifest on a live Windows 10.0.26200 host (wevtutil gp
//     Microsoft-Windows-Kernel-Network /ge /gm:true) renders 11 as
//     "TCPv4: %2 bytes received from %4:%6 to %3:%5." and 18 as "TCPv4: %2
//     bytes copied in protocol on behalf of user for connection between
//     %4:%6 and %3:%5." — a copy stage on a connection, not an arrival. The
//     archived manifest names them Datareceived / Protocolcopieddataon-
//     behalfofuser, and 34 reuses 27's template verbatim.
//   - PerfView's KernelTraceEventParser gives 11 ("Recv") and 18 ("TCPCopy")
//     the SAME payload class, and the kernel MOF groups TCPCopy with the ACK
//     accounting events (FullACK/PartACK/DupACK) — stack-internal accounting.
//   - Behaviourally decisive: System Informer / Process Hacker, the reference
//     per-process network-bytes tool on Windows (plugins/ExtendedTools/
//     etwmon.c), switches on 10/11/26/27 and has no case for 18 or 34
//     anywhere. If 18 carried bytes 11 missed, its per-process counters would
//     visibly undercount. No tool, library or article was found that sums
//     11 + 18.
//
// RESIDUAL, stated rather than buried: whether 18 fires for EVERY received
// byte or only for the copy-into-a-posted-user-buffer path is not established
// — which is a further reason to prefer 11, since 18 alone could undercount.
// The definitive test needs elevation: enable the provider, move N known bytes
// over ONE connection, and compare the per-connid seqnum spans of 11 and 18.
// Overlapping spans confirm the double count; disjoint spans would mean 18 has
// to be added after all. A non-elevated attempt on 2026-07-26 could create a
// session but could not enable this kernel-mode provider (Access is denied),
// so the live leg is still owed. Do not delete this paragraph to close it.
//
// Source: the archived provider manifest (event value= / task= / keywords=
// attributes) cross-checked against microsoft/Tx's generated bindings.
const (
	eventIDTCPSendIPv4 uint16 = 10
	eventIDTCPRecvIPv4 uint16 = 11
	eventIDTCPSendIPv6 uint16 = 26
	eventIDTCPRecvIPv6 uint16 = 27
	eventIDUDPSendIPv4 uint16 = 42
	eventIDUDPRecvIPv4 uint16 = 43
	eventIDUDPSendIPv6 uint16 = 58
	eventIDUDPRecvIPv6 uint16 = 59
)

// EventClass says what a Kernel-Network event id carries. Callers branch on it
// to pick a decoder; there is deliberately no single "decode anything" entry
// point that could return a TCP and a UDP event through the same value.
type EventClass uint8

const (
	// ClassIgnored is every event this package does not decode: control-plane
	// events (connect/accept/disconnect/...), failures, and anything unknown.
	ClassIgnored EventClass = iota
	// ClassTCPData is a TCP data-transfer event — decode with DecodeTCPData.
	ClassTCPData
	// ClassUDPDatagram is a UDP datagram event — decode with DecodeUDPDatagram.
	ClassUDPDatagram
)

// String renders the class for logs and test failures.
func (c EventClass) String() string {
	switch c {
	case ClassTCPData:
		return "tcp_data"
	case ClassUDPDatagram:
		return "udp_datagram"
	default:
		return "ignored"
	}
}

// Direction is which way the bytes of an event moved, relative to the local
// process that owns the socket.
type Direction uint8

const (
	// DirectionUnknown is the zero value; a decoded event never carries it.
	DirectionUnknown Direction = iota
	// DirectionSend means the local process transmitted these bytes.
	DirectionSend
	// DirectionReceive means the local process received these bytes.
	DirectionReceive
)

// String renders the direction for logs and test failures.
func (d Direction) String() string {
	switch d {
	case DirectionSend:
		return "send"
	case DirectionReceive:
		return "receive"
	default:
		return "unknown"
	}
}

// Family is the address family of an event's endpoints.
type Family uint8

const (
	// FamilyUnknown is the zero value; a decoded event never carries it.
	FamilyUnknown Family = iota
	// FamilyIPv4 is an IPv4 event (4-byte addresses in the payload).
	FamilyIPv4
	// FamilyIPv6 is an IPv6 event (16-byte addresses in the payload).
	FamilyIPv6
)

// String renders the family for logs and test failures.
func (f Family) String() string {
	switch f {
	case FamilyIPv4:
		return "ipv4"
	case FamilyIPv6:
		return "ipv6"
	default:
		return "unknown"
	}
}

// TCPDataEvent is ONE decoded TCP data-transfer event from
// Microsoft-Windows-Kernel-Network (event ids 10/11 for IPv4, 26/27 for IPv6).
//
// It is structurally unrelated to UDPDatagramEvent — no shared parent, no
// embedding, no common interface — precisely so a UDP byte count cannot end up
// in a TCP total through an assignment nobody reviewed. See the package doc.
type TCPDataEvent struct {
	// PID is the process that owns the socket, taken from the FIRST 4 bytes of
	// the event payload. It is NOT EVENT_HEADER.ProcessId, which for kernel
	// network events is whatever context the CPU was in (commonly 4 = System).
	PID int

	// Bytes is the number of bytes moved by THIS EVENT, not a running total.
	//
	// ETW is per-event; the Linux eBPF backend this reaches parity with is
	// cumulative. W2 owns the per-pid accumulator that converts a stream of
	// these into the monotonic total processobs consumers differentiate. Adding
	// this value straight into a field documented as cumulative is the single
	// highest-risk bug in the arc.
	Bytes int64

	// Direction is send or receive, derived from the event id.
	Direction Direction

	// Family is IPv4 or IPv6, derived from the event id.
	Family Family

	// SourceAddr / SourcePort / DestAddr / DestPort mirror the manifest's own
	// saddr/sport/daddr/dport fields verbatim, deliberately NOT renamed to
	// local/remote — and that stays true because the two available sources
	// DISAGREE.
	//
	// The manifest's format strings read "%2 bytes transmitted from
	// saddr:sport to daddr:dport" for sends and "%2 bytes received from
	// saddr:sport to daddr:dport" for receives, which reads as: source is the
	// local end on a send and the remote end on a receive.
	//
	// A decoded live sample CONTRADICTS that. On a TCPv4 receive (event 11)
	// the field at v4OffDestAddr decoded to 104.244.42.65 — a public address —
	// with dport 443, while the field at v4OffSourceAddr decoded to
	// 192.168.1.3, this host's own RFC1918 address. Under the format-string
	// reading a receive's saddr would be the REMOTE peer, so saddr should have
	// been the public one. It was not.
	//
	// So the orientation is genuinely unresolved and this package refuses to
	// bake either reading into a field name. None of these fields are used for
	// byte accounting; nothing downstream depends on the answer. If a caller
	// ever needs local-vs-remote, it must be settled by a controlled live
	// capture first, not by re-reading the format string.
	SourceAddr netip.Addr
	SourcePort uint16
	DestAddr   netip.Addr
	DestPort   uint16
}

// UDPDatagramEvent is ONE decoded UDP datagram event from
// Microsoft-Windows-Kernel-Network (event ids 42/43 for IPv4, 58/59 for IPv6).
//
// UDP is OUT OF SCOPE for the cross-OS network totals: Linux counts TCP only.
// This type exists so a caller who genuinely wants UDP has to ask for it by
// name, and so the decoder can prove — by test — that a UDP event never
// arrives dressed as a TCPDataEvent. Its fields carry the same semantics as
// TCPDataEvent's, including Bytes being per-event.
type UDPDatagramEvent struct {
	// PID is the socket-owning process from the payload, not the event header.
	PID int

	// Bytes is the number of bytes moved by THIS EVENT, not a running total.
	Bytes int64

	// Direction is send or receive, derived from the event id.
	Direction Direction

	// Family is IPv4 or IPv6, derived from the event id.
	Family Family

	// SourceAddr / SourcePort / DestAddr / DestPort mirror the manifest's
	// saddr/sport/daddr/dport fields verbatim; see TCPDataEvent.
	SourceAddr netip.Addr
	SourcePort uint16
	DestAddr   netip.Addr
	DestPort   uint16
}

// template describes one manifest event id's payload. Decision logic here is a
// data table walked by id rather than a conditional ladder (CLAUDE.md rule 5),
// so adding an event is a row and the tests iterate the same rows.
type template struct {
	class     EventClass
	direction Direction
	family    Family

	// minLen is the event's DOCUMENTED template length in bytes. Payloads may
	// legitimately be longer (trailing fields this package never reads); a
	// shorter one is rejected.
	minLen int
}

// templates is the manifest layout table. Lengths are the sum of the template's
// declared field widths, packed with no padding (ETW user data is packed):
//
//	IPv4 data:  PID u32, size u32, daddr u32, saddr u32, dport u16, sport u16,
//	            [startime u32, endtime u32 — TCP SEND ONLY], seqnum u32, connid u32
//	IPv6 data:  as above but daddr/saddr are 16-byte binary
//
// so 36 (v4 TCP send), 28 (v4 TCP recv + all v4 UDP), 60 (v6 TCP send), 52 (v6
// TCP recv + all v6 UDP). Only the two TCP SEND templates carry
// startime/endtime; because those sit AFTER every field this package reads, the
// extra pair changes only the length, never an offset.
//
// These are FLOORS, deliberately taken from the manifest's win:UInt32 connid.
// The legacy MOF classes declare connid with the WMI "Pointer" qualifier, so a
// build that emitted a pointer-sized connid would produce 40/32/64/56 instead —
// 4 bytes longer, in the trailing field nothing here reads. Taking the smaller
// value means both shapes decode; taking the larger would have dropped every
// event on a box that used the manifest width.
//
// The table is keyed on event id ALONE, which is only safe because every
// shipped Kernel-Network event is version 0. See supportedEventVersion: a
// non-zero version is REFUSED, never decoded against these rows.
var templates = map[uint16]template{
	eventIDTCPSendIPv4: {class: ClassTCPData, direction: DirectionSend, family: FamilyIPv4, minLen: 36},
	eventIDTCPRecvIPv4: {class: ClassTCPData, direction: DirectionReceive, family: FamilyIPv4, minLen: 28},
	eventIDTCPSendIPv6: {class: ClassTCPData, direction: DirectionSend, family: FamilyIPv6, minLen: 60},
	eventIDTCPRecvIPv6: {class: ClassTCPData, direction: DirectionReceive, family: FamilyIPv6, minLen: 52},
	eventIDUDPSendIPv4: {class: ClassUDPDatagram, direction: DirectionSend, family: FamilyIPv4, minLen: 28},
	eventIDUDPRecvIPv4: {class: ClassUDPDatagram, direction: DirectionReceive, family: FamilyIPv4, minLen: 28},
	eventIDUDPSendIPv6: {class: ClassUDPDatagram, direction: DirectionSend, family: FamilyIPv6, minLen: 52},
	eventIDUDPRecvIPv6: {class: ClassUDPDatagram, direction: DirectionReceive, family: FamilyIPv6, minLen: 52},
}

// supportedEventVersion is the ONLY EVENT_DESCRIPTOR.Version this package
// decodes. Every Kernel-Network event shipped by Windows to date is version 0.
//
// This gate exists because `templates` is keyed on event id alone and minLen is
// a FLOOR: without it, a future version bump that moved a field would sail past
// the length check and decode garbage silently — plan §0.1's exact failure
// shape (a wrong number with no error anywhere). The precedent is concrete
// rather than hypothetical: PerfView's KernelTraceEventParser branches on
// Version >= 1 for the classic TcpIp provider because the offsets literally
// moved (daddr 0 -> 8, dport 8 -> 16).
//
// A newer version must therefore be REFUSED and counted
// (ErrUnsupportedEventVersion / Stats.UnsupportedVersion), so the operator sees
// "these events were dropped" instead of a quietly wrong byte total. Adding
// support for a version means adding version-aware rows, not relaxing this.
const supportedEventVersion uint8 = 0

// Payload offsets. Scalars are little-endian (the payload is written by the
// local kernel in host order on every Windows target we ship). Addresses are
// raw network-order bytes, i.e. an in_addr / in6_addr copied verbatim, so they
// are consumed byte-for-byte with no swap. Ports are the one field decoded
// big-endian — see decodePort.
const (
	offPID = 0
	offLen = 4

	v4OffDestAddr   = 8
	v4OffSourceAddr = 12
	v4OffDestPort   = 16
	v4OffSourcePort = 18

	v6OffDestAddr   = 8
	v6OffSourceAddr = 24
	v6OffDestPort   = 40
	v6OffSourcePort = 42
)

// Classify reports what a Kernel-Network event id carries. Unknown ids and
// every non-data event return ClassIgnored, so a provider that grows new events
// degrades to "dropped", never to "decoded as something else".
func Classify(eventID uint16) EventClass {
	t, ok := templates[eventID]
	if !ok {
		return ClassIgnored
	}
	return t.class
}

// DecodeTCPData decodes one TCP data-transfer payload.
//
// version is EVENT_DESCRIPTOR.Version from the event header. Only
// supportedEventVersion (0) is decoded; anything else returns
// ErrUnsupportedEventVersion rather than being decoded against a layout that
// may no longer apply.
//
// It REJECTS any event id that is not a TCP data event, UDP ids included
// (ErrNotTCPEvent). That refusal is load-bearing: together with UDPDatagramEvent
// being an unrelated type, it is what stops UDP bytes from ever being counted
// as TCP.
//
// payload must not be retained: on Windows it aliases an ETW buffer that is
// reused as soon as the callback returns. Every field of the result is a value
// type, so the returned event is safe to keep.
func DecodeTCPData(eventID uint16, version uint8, payload []byte) (TCPDataEvent, error) {
	t, ok := templates[eventID]
	if !ok || t.class != ClassTCPData {
		return TCPDataEvent{}, fmt.Errorf("etw.DecodeTCPData: event id %d: %w", eventID, ErrNotTCPEvent)
	}
	if version != supportedEventVersion {
		return TCPDataEvent{}, fmt.Errorf("etw.DecodeTCPData: event id %d version %d (only version %d is decoded): %w",
			eventID, version, supportedEventVersion, ErrUnsupportedEventVersion)
	}
	c, err := decodeCommon(t, payload)
	if err != nil {
		return TCPDataEvent{}, fmt.Errorf("etw.DecodeTCPData: event id %d: %w", eventID, err)
	}
	return TCPDataEvent{
		PID:        c.pid,
		Bytes:      c.bytes,
		Direction:  t.direction,
		Family:     t.family,
		SourceAddr: c.sourceAddr,
		SourcePort: c.sourcePort,
		DestAddr:   c.destAddr,
		DestPort:   c.destPort,
	}, nil
}

// DecodeUDPDatagram decodes one UDP datagram payload. It rejects any event id
// that is not a UDP datagram event, TCP ids included (ErrNotUDPEvent), and any
// event version other than supportedEventVersion (ErrUnsupportedEventVersion).
//
// UDP is outside the cross-OS network-total scope; this exists so the capture
// side can observe UDP explicitly without a path that folds it into a
// TCP-labelled figure. Same no-retain rule for payload as DecodeTCPData.
func DecodeUDPDatagram(eventID uint16, version uint8, payload []byte) (UDPDatagramEvent, error) {
	t, ok := templates[eventID]
	if !ok || t.class != ClassUDPDatagram {
		return UDPDatagramEvent{}, fmt.Errorf("etw.DecodeUDPDatagram: event id %d: %w", eventID, ErrNotUDPEvent)
	}
	if version != supportedEventVersion {
		return UDPDatagramEvent{}, fmt.Errorf("etw.DecodeUDPDatagram: event id %d version %d (only version %d is decoded): %w",
			eventID, version, supportedEventVersion, ErrUnsupportedEventVersion)
	}
	c, err := decodeCommon(t, payload)
	if err != nil {
		return UDPDatagramEvent{}, fmt.Errorf("etw.DecodeUDPDatagram: event id %d: %w", eventID, err)
	}
	return UDPDatagramEvent{
		PID:        c.pid,
		Bytes:      c.bytes,
		Direction:  t.direction,
		Family:     t.family,
		SourceAddr: c.sourceAddr,
		SourcePort: c.sourcePort,
		DestAddr:   c.destAddr,
		DestPort:   c.destPort,
	}, nil
}

// common is the field set every Kernel-Network data template shares. Both
// decoders build their own distinct result type from it; it is unexported so it
// can never be the type that lets TCP and UDP mix.
type common struct {
	pid        int
	bytes      int64
	sourceAddr netip.Addr
	sourcePort uint16
	destAddr   netip.Addr
	destPort   uint16
}

// decodeCommon reads the shared leading prefix of a data-event payload. The
// length check is against the template's full documented length, not just the
// prefix, so an event whose payload is truncated or whose id was misidentified
// is rejected rather than half-decoded.
func decodeCommon(t template, payload []byte) (common, error) {
	if len(payload) < t.minLen {
		return common{}, fmt.Errorf("have %d bytes, template needs %d: %w", len(payload), t.minLen, ErrShortPayload)
	}

	var c common
	c.pid = int(binary.LittleEndian.Uint32(payload[offPID : offPID+4]))
	c.bytes = int64(binary.LittleEndian.Uint32(payload[offLen : offLen+4]))

	switch t.family {
	case FamilyIPv4:
		c.destAddr = addr4(payload[v4OffDestAddr : v4OffDestAddr+4])
		c.sourceAddr = addr4(payload[v4OffSourceAddr : v4OffSourceAddr+4])
		c.destPort = decodePort(payload[v4OffDestPort : v4OffDestPort+2])
		c.sourcePort = decodePort(payload[v4OffSourcePort : v4OffSourcePort+2])
	case FamilyIPv6:
		c.destAddr = addr16(payload[v6OffDestAddr : v6OffDestAddr+16])
		c.sourceAddr = addr16(payload[v6OffSourceAddr : v6OffSourceAddr+16])
		c.destPort = decodePort(payload[v6OffDestPort : v6OffDestPort+2])
		c.sourcePort = decodePort(payload[v6OffSourcePort : v6OffSourcePort+2])
	case FamilyUnknown:
		// Unreachable: every templates row sets a concrete family, and
		// TestTemplateTableMatchesTheManifestRows pins that. Kept so the switch
		// is exhaustive rather than relying on a default, and given its OWN
		// sentinel: a row with no family is a table bug, not a truncated
		// payload, and reporting it as ErrShortPayload would send whoever hits
		// it hunting the wrong thing.
		return common{}, fmt.Errorf("event template has no address family: %w", ErrUnknownAddressFamily)
	}
	return c, nil
}

// addr4 copies a 4-byte in_addr out of the payload. The bytes are already in
// network order (a verbatim in_addr), so a.b.c.d is read straight off.
func addr4(b []byte) netip.Addr {
	return netip.AddrFrom4([4]byte{b[0], b[1], b[2], b[3]})
}

// addr16 copies a 16-byte in6_addr out of the payload, likewise verbatim.
func addr16(b []byte) netip.Addr {
	var raw [16]byte
	copy(raw[:], b)
	return netip.AddrFrom16(raw)
}

// decodePort reads a port as BIG-endian (network byte order).
//
// This is CONFIRMED, not inferred. Microsoft's own consumer for these event
// classes — PerfView's KernelTraceEventParser — reads dport and sport with an
// explicit ByteSwap16 at exactly the offsets this package uses (16/18 for the
// IPv4 templates, 40/42 for the IPv6 ones). A byte swap on a little-endian
// host is a statement that the raw payload holds the port big-endian, which is
// what this function implements. It also matches TDH's handling of
// outType="win:Port" (its reference decoders call ntohs) and is what makes
// captures of these events read 80/443 rather than 20480/47873.
//
// The residual uncertainty is only about provenance of the manifest attribute
// itself: the archived manifests available for cross-checking were produced by
// TdhEnumerate, which drops outType. The byte order is not in doubt.
//
// Ports are not used for byte accounting; if a live elevated capture ever
// disagrees, this one function is the fix.
func decodePort(b []byte) uint16 {
	return binary.BigEndian.Uint16(b)
}
