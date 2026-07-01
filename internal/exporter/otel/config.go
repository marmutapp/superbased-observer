package otel

import (
	"os"
	"strings"
	"time"
)

// Package-local defaults. These mirror the [exporter.otel] defaults in
// internal/config but are duplicated here so the exporter package is
// self-contained and independently testable — it never imports internal/config
// (start.go maps the TOML values onto Config).
const (
	defaultEndpoint         = "localhost:4318"
	defaultPollInterval     = time.Second
	defaultSemconvStability = "gen_ai_latest_experimental"
	defaultServiceName      = "superbased-observer"
)

// OTel configuration environment variables honored as overrides over the
// file-supplied Config values (OTel configuration spec). File values are the
// defaults; a set env var wins.
const (
	envEndpoint        = "OTEL_EXPORTER_OTLP_ENDPOINT"
	envTracesEndpoint  = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	envInsecure        = "OTEL_EXPORTER_OTLP_INSECURE"
	envTracesInsecure  = "OTEL_EXPORTER_OTLP_TRACES_INSECURE"
	envSemconvOptIn    = "OTEL_SEMCONV_STABILITY_OPT_IN"
	envServiceNameAttr = "OTEL_SERVICE_NAME"
)

// Config is the exporter's runtime configuration. It is constructed by
// cmd/observer/start.go from the [exporter.otel] section and handed to New,
// which applies environment-variable overrides and fills defaults.
type Config struct {
	// Endpoint is the OTLP/HTTP collector endpoint as host:port (no scheme,
	// no path). The SDK appends /v1/traces. A full URL (with scheme/path) is
	// accepted too and passed through to the SDK as the traces endpoint.
	Endpoint string
	// Insecure sends over plain HTTP instead of HTTPS.
	Insecure bool
	// PollInterval is the api_turns row-tail poll cadence. Zero means default.
	PollInterval time.Duration
	// EmitPromptContent attaches prompt/completion bodies as the
	// gen_ai.client.inference.operation.details event. Default false.
	EmitPromptContent bool
	// EmitUserEmail attaches sbo.user.email when the agent is enrolled.
	EmitUserEmail bool
	// SemconvStability is advertised for OTEL_SEMCONV_STABILITY_OPT_IN.
	SemconvStability string
	// ServiceName is the resource service.name. Default "superbased-observer".
	ServiceName string
}

// withEnvOverrides returns a copy of c with OTEL_* environment variables
// applied over the file-supplied values, then defaults filled for any field
// still empty. Precedence is: env var > Config field > package default.
func (c Config) withEnvOverrides() Config {
	out := c

	// Traces-specific endpoint wins over the generic one, which wins over the
	// file value. A full URL is accepted; the SDK distinguishes by scheme.
	if v := strings.TrimSpace(os.Getenv(envTracesEndpoint)); v != "" {
		out.Endpoint = v
	} else if v := strings.TrimSpace(os.Getenv(envEndpoint)); v != "" {
		out.Endpoint = v
	}

	// OTel spec: the *_INSECURE vars are "true"/"false". Traces-specific wins.
	if v, ok := lookupBool(envTracesInsecure); ok {
		out.Insecure = v
	} else if v, ok := lookupBool(envInsecure); ok {
		out.Insecure = v
	}

	if v := strings.TrimSpace(os.Getenv(envSemconvOptIn)); v != "" {
		out.SemconvStability = v
	}
	if v := strings.TrimSpace(os.Getenv(envServiceNameAttr)); v != "" {
		out.ServiceName = v
	}

	if out.Endpoint == "" {
		out.Endpoint = defaultEndpoint
	}
	if out.PollInterval <= 0 {
		out.PollInterval = defaultPollInterval
	}
	if out.SemconvStability == "" {
		out.SemconvStability = defaultSemconvStability
	}
	if out.ServiceName == "" {
		out.ServiceName = defaultServiceName
	}
	return out
}

// lookupBool reads an OTel "true"/"false" boolean env var. The bool result is
// only meaningful when ok is true (the variable was set to a parseable value).
func lookupBool(key string) (val bool, ok bool) {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	switch v {
	case "true", "1", "yes":
		return true, true
	case "false", "0", "no":
		return false, true
	default:
		return false, false
	}
}
