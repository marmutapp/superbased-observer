// Package orgclient is the agent-side client for the Teams & Org Visibility
// feature: it manages the enrolment lifecycle, persists the bearer and the
// agent's signing key in the OS keychain, and runs the push loop that ships
// content-free rollup rows to the org server (spec §2.4.2).
//
// # No-op unless enrolled
//
// Nothing in this package runs unless the user has configured [org_client]
// with enabled = true AND completed enrolment (`observer enroll`). The daemon
// only constructs and starts a [Client] in that case; a solo-local install
// imports this package's types but never starts a push loop, makes no network
// call, and writes no org data. This preserves the byte-identical solo-local
// invariant (tests/invariant).
//
// # Secrets
//
// Enrolment generates a fresh Ed25519 keypair on the agent. The public key is
// bound to the user record on the server; the private key never leaves the
// agent and is used to sign each push (a per-push proof that the bearer is
// presented by the agent that enrolled — a defence against bearer theft). The
// bearer and the private signing key are stored via [BearerStore], which uses
// the OS keychain when one is available and falls back to a 0600-mode file
// (with a WARN log) otherwise. Neither secret is ever written to the agent DB;
// org_enrolment.bearer_key_id is only a handle.
//
// # Privacy
//
// The push payload carries only the content-free row shapes defined in
// internal/orgcontract (counts, costs, timings, paths, hashes — never prompt
// text or tool-output bodies). The push loop reads from
// store.SelectUnpushedSince and the orgcontract row types are the single
// source of truth for what may cross the wire; see [Client.PushOnce].
//
// # Failure posture (P1: never break the host tool)
//
// Every error path degrades gracefully. A failing push loop logs at WARN and
// retries with backoff; it never affects ingest, the proxy, or any other
// part of the daemon. An auth failure (401/403) stops the loop and surfaces
// the error on the local dashboard rather than crashing.
package orgclient
