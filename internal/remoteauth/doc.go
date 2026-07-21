// Package remoteauth is the PURE-LOGIC security substrate for remote dashboard
// access (remote-dashboard-access plan §4). It provides, with no I/O:
//
//   - argon2id hash/verify with a constant-time comparison (HashSecret /
//     VerifySecret), for the pairing secret stored hashed at rest.
//   - a device-session model (SessionStore): generation, expiry, idle timeout,
//     max concurrent sessions, revocation, rotation-invalidates-all, and a
//     per-session revocation channel so an open privileged socket can be torn
//     down on rotate/revoke (plan §4.3).
//   - single-use, session- AND action-bound execute capabilities
//     (CapabilityStore) minted after a local approval step — never a reusable
//     execute token (plan §4.2 / codex P0 #6).
//   - a token-bucket rate limiter for auth attempts (plan §4.8).
//   - 128-bit pairing-secret generation (GenerateSecret).
//
// It imports NO net/http, database/sql, or fsnotify — pinned by imports_test.go
// (CLAUDE.md module rule #1). The HTTP glue (cookies, CSRF, pairing routes)
// lives in the dashboard package, which injects these primitives; the at-rest
// secret file and audit persistence live in the store/cmd layers.
//
// # argon2id cost parameters (operator Q1, resolved)
//
// HashSecret uses the OWASP-recommended argon2id defaults: memory = 19 MiB
// (19456 KiB), iterations (time) = 2, parallelism = 1, 16-byte salt, 32-byte
// key. This is the "second recommended option" from the OWASP Password Storage
// Cheat Sheet (m=19456,t=2,p=1) — a low-memory profile appropriate for a
// single-user desktop daemon where the hash is computed once at pairing, not
// per request. (The org server's per-request SCIM token stays a plain
// constant-time compare precisely because argon2 per request would be a DoS
// vector; this hash is paid once at enrol time.)
package remoteauth
