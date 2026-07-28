// Package bridge is the cross-OS process-capture transport for Process
// Observability (docs/process-observability.md §5.5). It carries normalized
// process events from a Windows-native capturer to the WSL daemon over the
// WSL-interop stdout pipe — the mirror image of the existing wsl.exe hook
// bridge (which runs the Linux binary from Windows; this runs the Windows
// binary from WSL).
//
// This package splits into a wire, a shared stream, and TWO transports:
//
//   - wire.go — the pure, both-OS NDJSON codec: a versioned Frame envelope
//     (one JSON object per line) reusing processobs.RawEvent as the event
//     payload, with a streaming Encoder (capturer side) and Decoder (WSL
//     backend side). No OS-specific code, no I/O beyond the io.Writer/Reader
//     it is handed; compiles and is unit-tested on every host.
//   - stream.go — the transport-agnostic consumption of that wire: one decode
//     loop over a plain io.Reader (consumeFrames) plus the network-accounting
//     claim rule (netClaim), shared by both transports so they cannot drift.
//   - backend.go — SPAWN transport. The WSL-side processobs.Backend (P-B3):
//     resolves the Windows observer.exe, execs it via interop, and decodes its
//     stdout into the Observer's RawEvent channel. Respawns a dead capturer,
//     and gives up after maxConsecutiveFailures fruitless runs.
//   - listener.go — ACCEPT transport. A loopback-bound processobs.Backend the
//     capturer dials INTO, for the elevated ETW feed the daemon cannot spawn
//     itself. Direction-inverted for a measured reason: WSL cannot reach a
//     Windows-bound listener, but Windows reaches a WSL-bound one via
//     localhostForwarding (plan §0.2). Because that also makes the listener
//     reachable from any process on the Windows host, a constant-time shared
//     token is MANDATORY — there is no unauthenticated mode. It has NO
//     give-up cap: a capturer that has not connected yet is the normal boot
//     state, not a failure.
//
// Health: the capturer's hello frame also carries its OWN per-process
// network-byte accounting status (Hello.NetworkAccountingMode/Reason), which
// the backend forwards to the shared processobs.NetworkAccounting handle. On a
// Windows deployment the only host that knows whether byte counting is live —
// and why it is not — is the Windows capturer, so that answer has to travel
// rather than be guessed daemon-side. Those fields are optional: an older
// capturer omits them, which means UNKNOWN and leaves the daemon's own view
// untouched, never a fabricated "off".
//
// Privacy: the wire carries RAW RawEvents (full argv/cwd), exactly as the
// local poll backend hands the Observer raw /proc data. All scrub/cap/hash
// runs downstream in the WSL daemon at the existing FieldScrubber boundary
// (§5.5); the transport adds no new exposure — both processes are local and
// owned by the same user, and there is no network hop.
package bridge
