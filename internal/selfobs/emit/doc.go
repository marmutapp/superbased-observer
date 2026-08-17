// Package emit is the self-observability gateway CLIENT: it turns a
// run.DecisionRun into a credential-authenticated OTLP/HTTP span exported to the
// org gateway.
//
// The endpoint is EXACTLY emit.Config.Endpoint — emit has NO OTEL_* env
// overrides. New validates the scheme first (only http/https; plaintext http
// only when Insecure), then builds a full explicit-scheme URL so the exporter's
// WithEndpointURL makes the scheme authoritative and an OTEL_..._INSECURE env
// var cannot downgrade the credential-bearing path (R5-B1). The sink's HTTP
// client refuses redirects (so the credential is never resent to a redirect
// target) and applies any Config.TLSConfig.
//
// New returns a real no-op Sink (never a nil interface) on an empty endpoint or
// an empty resolved credential. It never imports internal/obs.
package emit
