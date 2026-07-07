// Package bridge is the cross-OS process-capture transport for Process
// Observability (docs/process-observability.md §5.5). It carries normalized
// process events from a Windows-native capturer to the WSL daemon over the
// WSL-interop stdout pipe — the mirror image of the existing wsl.exe hook
// bridge (which runs the Linux binary from Windows; this runs the Windows
// binary from WSL).
//
// This package splits into two layers:
//
//   - wire.go — the pure, both-OS NDJSON codec: a versioned Frame envelope
//     (one JSON object per line) reusing processobs.RawEvent as the event
//     payload, with a streaming Encoder (capturer side) and Decoder (WSL
//     backend side). No OS-specific code, no I/O beyond the io.Writer/Reader
//     it is handed; compiles and is unit-tested on every host.
//   - backend.go — the WSL-side processobs.Backend (P-B3): resolves the
//     Windows observer.exe, execs it via interop, and decodes its stdout into
//     the Observer's RawEvent channel.
//
// Privacy: the wire carries RAW RawEvents (full argv/cwd), exactly as the
// local poll backend hands the Observer raw /proc data. All scrub/cap/hash
// runs downstream in the WSL daemon at the existing FieldScrubber boundary
// (§5.5); the transport adds no new exposure — both processes are local and
// owned by the same user, and there is no network hop.
package bridge
