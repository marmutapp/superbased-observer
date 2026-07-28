// Package etw is a pure-Go, CGO-free Event Tracing for Windows consumer that
// decodes per-process network byte counts from the kernel network provider.
//
// It is phases W1 and W2 of the Windows-parity arc
// (docs/plans/process-obs-etw-windows-parity-plan-2026-07-26.md):
//
//   - W1, decode. Session starts a real-time ETW session, receives events, and
//     turns each event payload into a typed Go value (parse.go).
//   - W2, accounting. Accumulator turns those PER-EVENT byte counts into the
//     CUMULATIVE per-pid totals processobs consumers differentiate, and Capture
//     composes the two into a processobs.NetworkSampler.
//
// It still does NOT implement processobs.Backend and does NOT speak any
// transport — the socket the elevated capturer dials is W3.
//
// # Elevation is mandatory, not "often"
//
// Controlling an ETW trace session requires Administrator, membership of the
// Performance Log Users group, or a LocalSystem/LocalService service. The E0
// spike (docs/plans/process-obs-etw-backend-plan-2026-06-17.md, finding 3)
// confirmed that a non-elevated StartTraceW returns ERROR_ACCESS_DENIED (5)
// with a correctly-parsed EVENT_TRACE_PROPERTIES — the kernel accepted the
// request shape and rejected it purely on privilege. NewSession therefore maps
// that specific failure to the typed sentinel ErrNeedsElevation so a caller can
// degrade with a legible reason instead of crashing or reporting silence. Use
// IsElevated for a cheap pre-flight check.
//
// # Provider choice: Microsoft-Windows-Kernel-Network (manifest), not NT Kernel Logger
//
// Both candidates expose per-process network bytes and both require elevation,
// so elevation is not a tiebreaker. This package uses the modern manifest
// provider Microsoft-Windows-Kernel-Network
// ({7dd42a49-5329-4832-8dfd-43d979153a88}) enabled on our OWN named session via
// EnableTraceEx2, for four reasons:
//
//  1. No singleton-session contention. The legacy path requires the session
//     literally named "NT Kernel Logger" keyed by SystemTraceControlGuid; if
//     WPR, xperf, Defender or any other tool already holds it, StartTraceW
//     fails with ERROR_ALREADY_EXISTS and there is nothing we can do that does
//     not break the other tool. The manifest provider is enabled on a session
//     we name ourselves, so it composes with anything else on the box.
//  2. Fixed-width payloads. The legacy TcpIp MOF classes declare connid with
//     the WMI "Pointer" qualifier
//     (https://learn.microsoft.com/en-us/windows/win32/etw/tcpip-sendipv4), so
//     the classic payload width varies with the trace's pointer size. The
//     manifest declares connid as win:UInt32, a fixed 4 bytes. Fewer ways for a
//     hand-written decoder to be silently wrong.
//  3. Keyword filtering. EnableTraceEx2 takes MatchAnyKeyword, so we can ask
//     for exactly KERNEL_NETWORK_KEYWORD_IPV4 (0x10) and/or
//     KERNEL_NETWORK_KEYWORD_IPV6 (0x20). The kernel logger's EnableFlags are
//     far coarser.
//  4. It is the documented modern path; the legacy kernel logger is retained by
//     Microsoft for compatibility.
//
// # Which PID we attribute bytes to — the payload PID, never the header PID
//
// EVENT_RECORD.EventHeader.ProcessId is the process whose context the CPU
// happened to be in when the event was emitted. Network completion frequently
// runs in a DPC or a system worker thread, so the header PID is routinely 4
// (System), 0 (Idle), or an unrelated process. Microsoft states this directly
// for the kernel network classes: "Because some network events are logged by
// separate threads, you may not be able to use the ProcessId and ThreadId
// members of EVENT_TRACE_HEADER to identify the process or thread that
// originated the network activities."
// (https://learn.microsoft.com/en-us/windows/win32/etw/tcpip)
//
// The Kernel-Network templates therefore carry an explicit PID field as the
// FIRST 4 bytes of every data-event payload, documented as "Identifier of the
// process associated with the request". THAT is the socket owner and THAT is
// what this package decodes into TCPDataEvent.PID / UDPDatagramEvent.PID. The
// header PID is never read. Getting this backwards would attribute every AI
// tool's bytes to PID 4.
//
// # Only event version 0 is decoded
//
// The layout table is keyed on event id alone and its lengths are FLOORS, so a
// future version bump that moved a field would sail straight past the length
// check and decode garbage with nothing failing. That is not hypothetical:
// PerfView's KernelTraceEventParser branches on Version >= 1 for the classic
// TcpIp provider because the offsets literally moved. Every Kernel-Network
// event Windows ships today is version 0, so this package decodes version 0 and
// REFUSES anything else with ErrUnsupportedEventVersion, counted separately in
// Stats.UnsupportedVersion. Dropped-and-visible beats decoded-and-wrong.
//
// # Scope: TCP only — but BOTH address families
//
// TCP-vs-UDP and IPv4-vs-IPv6 are opposite decisions here, for the same
// parity reason. Linux counts TCP only, so UDP is excluded. Linux's probes
// (fexit/tcp_sendmsg, fentry/tcp_cleanup_rbuf) are address-family agnostic and
// therefore count IPv4 and IPv6 together, so BOTH keywords are enabled by
// default — capturing IPv4 only would make the Windows total silently omit
// every IPv6 byte against a Linux number that includes them. Options.ExcludeIPv6
// exists as a deliberate escape hatch and is documented as breaking parity.
//
// # UDP cannot be folded into a TCP total by accident
//
// The provider covers TCP and UDP. The Linux eBPF backend this must reach
// parity with counts TCP only, and processobs.MetricSample's network fields are
// documented as TCP payload only. Emitting TCP+UDP into a TCP-labelled field
// would silently widen its meaning with nothing saying so, making a cross-OS
// comparison apples-to-oranges.
//
// The type system enforces the split: TCP data events decode into
// TCPDataEvent, UDP datagram events decode into the structurally unrelated
// UDPDatagramEvent, and DecodeTCPData REFUSES a UDP event id (and vice versa).
// There is no shared parent type and no field either can be assigned into, so
// a UDP byte count cannot reach a TCP total without someone deliberately
// writing the conversion. Session drops UDP entirely unless the caller supplies
// an OnUDP handler.
//
// # Per-event, not cumulative — the arc's highest-risk bug
//
// ETW reports the bytes moved by EACH event. Linux reports a monotonic
// cumulative total per pid. TCPDataEvent.Bytes is therefore documented, named
// and typed as a per-event quantity, and Accumulator is the one thing that
// turns a stream of these into the cumulative total processobs consumers
// differentiate. Forwarding Bytes straight into a cumulative field produces
// charts that are wrong by a factor of the sampling interval with no error
// anywhere — no failing test, no log line, the line just reads low.
// TestAddIsCumulativeNotPerEvent is the regression guard.
//
// Capture.NetworkBytes — NOT Accumulator.NetworkBytes — is the value to wire
// into poll.Options.NetworkBytes. Both signatures fit
// processobs.NetworkBytesFunc, but only Capture implements its ok contract:
// false means accounting is not live (UNMEASURED), while true with (0,0) means
// measured and idle. Accumulator's narrower ok ("I hold a counter for that
// pid") would report every idle process as unmeasured and gap its chart.
//
// # Layout and build constraints
//
// parse.go, errors.go, session.go, handles.go, accumulator.go and capture.go
// carry NO build tag: payload decoding, the sentinels, the Options defaults,
// the session's handle state, the byte accounting and the handler wiring are
// all pure Go, so they compile and are unit-tested on Linux — imports_test.go
// pins that purity rather than leaving it to the build tags. Every syscall and
// unsafe binding lives in a //go:build windows file.
//
// handles.go, accumulator.go and capture.go's captureStatus are untagged ON
// PURPOSE: each is genuinely concurrent state — the consumer handle is written
// by Close while Process reads it, the accumulator is written by the ETW
// callback thread while the metric sampler reads it, and the status is written
// by the pump goroutine while Status reads it. Keeping all three out of the
// Windows-only files is what lets `go test -race` cover them on the platform CI
// actually has.
//
// The same reasoning puts captureOptions in the untagged file: the TCP-only,
// UDP-severed handler wiring is the package's scope guarantee, and a guarantee
// behind //go:build windows is asserted by nobody.
//
// Because CI has no Windows runner and `GOOS=windows go build` does not
// compile _test.go files, each bound struct is pinned by a two-sided
// compile-time size assertion rather than a test — see session_windows.go.
//
// The Windows bindings assume a 64-bit ABI (win32-x64 / arm64 are the shipped
// targets) and say so with a compile-time assertion on pointer size.
package etw
