// Package api implements the org server's HTTP handlers: the generated
// agent-protocol ServerInterface (EnrollAgent, PushBatch), the admin
// enrolment-token mint endpoint, and the cross-cutting middleware (request
// ID, logging, rate limiting). It also provides the auth bridges the server
// wiring needs — a BearerVerifier (signature + revocation + subject-active)
// and a UserResolver (SAML identity → user_id).
//
// M1 scope: EnrollAgent runs the full token-burn → pubkey-bind → bearer-mint
// flow; PushBatch authenticates and ACKs with 202 but does not yet ingest
// (M2 wires storage). No row content ever reaches a handler — the push
// envelope's row types are content-free by construction (orgcontract).
package api
