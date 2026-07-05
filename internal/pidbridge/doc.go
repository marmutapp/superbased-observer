// Package pidbridge maps a host-tool session_id onto the pid that claims
// it, so the local reverse proxy can attribute incoming TCP requests to a
// session even when the host tool (Claude Code, Codex) does not attach an
// X-Session-Id header.
//
// The bridge has two halves:
//
//   - Writers. When Claude Code fires its SessionStart hook,
//     observer-hook captures os.Getppid() (the Claude Code pid that
//     spawned the hook), parses session_id + cwd from stdin, and calls
//     [Store.Write]. One row per Claude Code session ends up in
//     session_pid_bridge (migration 004).
//
//   - Readers. On Linux, [ProcResolver] reads /proc/net/tcp, finds the
//     socket inode matching the proxy's client remote addr, locates the
//     owning pid via /proc/[pid]/fd, and walks the ancestor chain (PPid
//     in /proc/[pid]/status) until it finds a pid registered in the
//     bridge table. On other platforms the resolver is a no-op stub.
//
// Spec references: §9 (proxy), §14 (hook protocol).
package pidbridge
