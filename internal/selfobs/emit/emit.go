package emit

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	otelexp "github.com/marmutapp/superbased-observer/internal/exporter/otel"
	"github.com/marmutapp/superbased-observer/internal/selfobs/run"
	"github.com/marmutapp/superbased-observer/internal/selfobs/shape"
)

const (
	defaultServiceName  = "superbased-observer"
	tracerName          = "github.com/marmutapp/superbased-observer/internal/selfobs/emit"
	headerAuthorization = "Authorization"
	authScheme          = "Bearer "
)

// Config configures the self-observability emit sink. The endpoint is used
// EXACTLY as given (no OTEL_* env overrides); the credential is composed from
// Token, or KeyID + "." + Secret.
type Config struct {
	// Endpoint is the OTLP/HTTP gateway endpoint (host:port or a full URL). A
	// bare host:port is given an explicit scheme (https, or http when Insecure)
	// so the scheme — not OTEL_* env — decides plaintext-vs-TLS.
	Endpoint string
	// KeyID is the ingest credential key id.
	KeyID string
	// Secret is the ingest credential secret.
	Secret string
	// Token is an OPTIONAL precomposed "<key_id>.<secret>"; wins over KeyID+Secret.
	Token string
	// Insecure permits a plaintext http:// endpoint. Default requires https://.
	Insecure bool
	// ServiceName is the resource service.name. Default "superbased-observer".
	ServiceName string
	// TLSConfig optionally overrides the client TLS configuration.
	TLSConfig *tls.Config
}

// Sink emits self-observability decision runs. Emit is fire-and-forget; callers
// flush with ForceFlush and release with Shutdown.
type Sink interface {
	Emit(ctx context.Context, r run.DecisionRun)
	ForceFlush(ctx context.Context) error
	Shutdown(ctx context.Context) error
}

// Nop returns a real no-op Sink (never nil).
func Nop() Sink { return nopSink{} }

type nopSink struct{}

func (nopSink) Emit(context.Context, run.DecisionRun) {}
func (nopSink) ForceFlush(context.Context) error      { return nil }
func (nopSink) Shutdown(context.Context) error        { return nil }

// New builds a credential-authenticated OTLP/HTTP sink. It returns Nop(), nil
// (a real no-op, never a nil interface) when the endpoint is empty or the
// resolved credential is empty. It errors only on a malformed/unsupported
// endpoint scheme or a provider-construction failure.
func New(cfg Config) (Sink, error) {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		return Nop(), nil
	}
	token := resolveToken(cfg)
	if token == "" {
		return Nop(), nil
	}

	fullURL, scheme, err := buildEndpointURL(endpoint, cfg.Insecure)
	if err != nil {
		return nil, err
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.TLSConfig != nil {
		transport.TLSClientConfig = cfg.TLSConfig
	}
	hc := &http.Client{Transport: transport, CheckRedirect: noRedirect}

	serviceName := strings.TrimSpace(cfg.ServiceName)
	if serviceName == "" {
		serviceName = defaultServiceName
	}

	otelCfg := otelexp.Config{
		Endpoint: fullURL,
		// The RESOLVED scheme — not cfg.Insecure — is authoritative for
		// plaintext-vs-TLS (R6-B1). Insecure is true ONLY for a resolved http://
		// scheme, so an https:// endpoint is ALWAYS exported over TLS even if the
		// caller passed Insecure:true; a stray Insecure flag can never downgrade
		// the credential-bearing export to plaintext.
		Insecure:    scheme == "http",
		ServiceName: serviceName,
		Headers:     map[string]string{headerAuthorization: authScheme + token},
		HTTPClient:  hc,
	}
	tp, err := otelexp.NewTracerProvider(otelCfg)
	if err != nil {
		return nil, fmt.Errorf("selfobs/emit: build tracer provider: %w", err)
	}
	return &otelSink{tp: tp, tracer: tp.Tracer(tracerName)}, nil
}

// resolveToken composes the compound credential: Config.Token wins; else
// KeyID + "." + Secret when BOTH are present. Returns "" when no usable
// credential is available (drives the Nop fallback).
func resolveToken(cfg Config) string {
	token := strings.TrimSpace(cfg.Token)
	if token != "" {
		return token
	}
	keyID := strings.TrimSpace(cfg.KeyID)
	secret := strings.TrimSpace(cfg.Secret)
	if keyID == "" || secret == "" {
		return ""
	}
	return keyID + "." + secret
}

// buildEndpointURL returns a full URL string with an EXPLICIT scheme AND the
// resolved scheme, so New can make the scheme authoritative over OTEL_* env
// (R5-B1) AND over cfg.Insecure (R6-B1). BOTH a bare host:port and a full URL
// converge on ONE parse+validate path: a bare endpoint is first given an
// explicit scheme (https, or http when insecure), then the constructed
// candidate is parsed and validated identically to a caller-supplied URL. This
// closes the bare-endpoint credential-leak gaps — "trusted@evil.example"
// (userinfo → wrong host), ":4318" (empty host), and "collector:bad" (bad port)
// all previously slipped through unparsed. It rejects any non-http/https
// scheme, a URL carrying userinfo (a credential-leak vector — "trusted@evil"
// would silently target "evil"; R6-SF1), an empty host (checked via
// u.Hostname(), so a port-only ":4318"/"https://:4318" whose u.Host is
// non-empty is still caught; R6-SF1), a URL that fails to parse (e.g. an
// invalid port), and a plaintext http:// endpoint unless insecure is set.
//
// R6-B1 coerce-to-TLS choice: an explicit https:// endpoint ALWAYS resolves to
// the "https" scheme even when insecure is true — the scheme wins, so New sets
// Insecure=false and the credential can never travel in plaintext. We COERCE
// (scheme wins) rather than returning an error because this is a fire-and-forget
// credential sink: silently upgrading a self-contradictory (https + Insecure)
// config to TLS is strictly safer than failing to emit, and an https:// URL
// unambiguously expresses TLS intent.
func buildEndpointURL(endpoint string, insecure bool) (string, string, error) {
	candidate := endpoint
	if !strings.Contains(endpoint, "://") {
		scheme := "https"
		if insecure {
			scheme = "http"
		}
		candidate = scheme + "://" + endpoint
	}
	u, err := url.Parse(candidate)
	if err != nil {
		return "", "", fmt.Errorf("selfobs/emit: parse endpoint %q: %w", endpoint, err)
	}
	if u.User != nil {
		return "", "", fmt.Errorf("selfobs/emit: endpoint %q must not carry userinfo (credential-leak guard)", endpoint)
	}
	if u.Hostname() == "" {
		return "", "", fmt.Errorf("selfobs/emit: endpoint %q has no host (malformed endpoint)", endpoint)
	}
	switch u.Scheme {
	case "https":
		return candidate, "https", nil
	case "http":
		if !insecure {
			return "", "", fmt.Errorf("selfobs/emit: refusing plaintext http endpoint %q without Insecure", endpoint)
		}
		return candidate, "http", nil
	default:
		return "", "", fmt.Errorf("selfobs/emit: unsupported endpoint scheme %q (want http or https)", u.Scheme)
	}
}

// noRedirect makes the sink's HTTP client return a 3xx verbatim instead of
// following it — following a redirect would resend the OTLP body AND the
// credential header to the Location host, a credential leak. Mirrors
// internal/edge/forward/client.go:noRedirect.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// otelSink is the concrete Sink; it owns its OWN TracerProvider + tracer.
type otelSink struct {
	tp     *sdktrace.TracerProvider
	tracer trace.Tracer
}

// Emit starts, decorates, and ENDS one client span per run. span.End is called
// on EVERY path (including the error path) — WithBatcher exports only ended
// spans, so an un-ended span is invisible to ForceFlush (B5).
func (s *otelSink) Emit(ctx context.Context, r run.DecisionRun) {
	_, span := s.tracer.Start(
		ctx, r.Component,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(shape.Attributes(r)...),
	)
	if r.ErrMsg != "" {
		span.SetStatus(codes.Error, r.ErrMsg)
	}
	span.End()
}

// ForceFlush flushes batched spans through the owned provider.
func (s *otelSink) ForceFlush(ctx context.Context) error { return s.tp.ForceFlush(ctx) }

// Shutdown releases the owned provider.
func (s *otelSink) Shutdown(ctx context.Context) error { return s.tp.Shutdown(ctx) }
