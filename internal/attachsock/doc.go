// Package attachsock implements the AF_UNIX control+stdio protocol that backs
// `observer <tool> --attach` (session-attach design 2026-07-19, Phase 1).
//
// It is a transport-pure package: it speaks net.Conn, encoding/json and io
// only. It imports none of the daemon's subsystems (termsession, termsvc,
// config, store) — both sides are defined against small local interfaces (Host,
// Session, ClientIO) that the cmd layer adapts to the real PTY registry. Types
// never leak past the seam.
//
// # Roles
//
//   - Server (daemon side): Serve accepts connections on the owner-only unix
//     socket, reads a spawn control frame, asks the Host to launch a daemon-owned
//     PTY, then bridges the PTY's output to the client and the client's stdin to
//     the PTY. A dropped client detaches (releases the writer + unsubscribes)
//     WITHOUT killing the child — the child lives on for the dashboard and other
//     viewers.
//   - Client (`observer <tool> --attach`): Attach dials the socket, sends the
//     spawn request, then pumps the operator's terminal stdin/stdout and window
//     resizes across the connection. TTY raw-mode and SIGWINCH handling belong to
//     the cmd layer, which feeds the Resize channel.
//
// # Wire format
//
// Each frame is a 4-byte big-endian payload length, a 1-byte frame type, then
// the payload. Control frames (type 1) carry JSON and are capped at 64 KiB; data
// frames (stdin type 2, output type 3) carry raw bytes and are capped at 32 KiB
// (larger writes are chunked). A malformed or oversized frame fails the
// connection with a protocol error. Control ops: client→server spawn/resize/
// detach; server→client spawned/exit/error.
//
// The socket is never bound to a network interface — AF_UNIX only, mode 0600,
// under the operator's ~/.observer/ — so it is unreachable off-box by
// construction (design §3.3).
package attachsock
