// Package browser is a small, loopback-only HTTP receiver for captured-turn
// payloads from the browser extension. It imitates the shape of
// internal/ingest/otlp/receiver.go — loopback-guarded, an injected Handler
// with no schema/store coupling, on its OWN dedicated port (never :8820,
// never the dashboard mux) — but for the browser rail's JSON wire.
//
// It is the HTTP receiver skeleton. The DEFAULT binding for the browser
// extension is the native-messaging bridge (the browser launches `observer
// browser hook` directly, which works even when the daemon is down and needs
// no open port). This listener is the alternative for deployments that
// prefer HTTP, and the receiver pattern future browser-adjacent rails can
// reuse. Both transports funnel into the SAME internal/adapter/browserchat
// normalizer, so the transport choice is a deployment detail, not a schema
// fork.
//
// The Handler is deliberately schema-agnostic: it receives the raw request
// body ([]byte) and is injected by the daemon with a closure that calls
// browserchat.Normalize + store.Ingest. That keeps this package free of any
// dependency on the browserchat schema or the store — exactly the boundary
// discipline internal/ingest/otlp keeps (its Handler takes a decoded proto,
// nothing store-shaped).
package browser
