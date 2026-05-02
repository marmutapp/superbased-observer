package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// Sink is the write side of the proxy — the store layer implements this so
// the proxy package doesn't depend on *sql.DB. In production this is
// (*store.Store).InsertAPITurn.
type Sink interface {
	InsertAPITurn(ctx context.Context, t models.APITurn) (int64, error)
}

// SessionResolver maps an incoming TCP connection's remote address
// ("127.0.0.1:54321") onto a host-tool session_id. The proxy calls it
// only when the request did not carry an X-Session-Id header — useful
// for Claude Code and Codex which don't set one. A clean miss
// (`ok=false`, nil error) is not an error; the proxy just stores a NULL
// session_id the way it always has. In production this is
// (*pidbridge.ProcResolver).Resolve.
type SessionResolver interface {
	Resolve(ctx context.Context, remoteAddr string) (sessionID string, ok bool, err error)
}

// Compressor is the optional pre-forward hook the proxy runs on a request
// body (spec §10 Layer 3). Implementations MUST return a body that's safe
// to forward upstream — scrubbed, provider-shape-preserving, and no larger
// than the input. The stats land on models.APITurn and observer_log.
//
// In production the implementation is
// (*conversation.Pipeline).Compress (wrapped to adapt the method set).
// Interface-typed so proxy tests can stub compression without importing
// the conversation package.
type Compressor interface {
	Compress(ctx context.Context, provider string, body []byte) CompressionResult
}

// CompressionResult is the output of [Compressor.Compress].
type CompressionResult struct {
	// Body is the body to forward. When Skipped, callers MUST forward the
	// original body — a nil/empty Body with Skipped=false is treated as
	// "compression failed; fall back to original".
	Body []byte
	// Skipped is true when the compressor did not modify the input.
	Skipped bool
	// MessagePrefixHash is the cache-aligned prefix identifier landed on
	// [models.APITurn.MessagePrefixHash].
	MessagePrefixHash string
	// OriginalBytes / CompressedBytes are the before/after sizes used by
	// observer_log and the dashboard.
	OriginalBytes   int
	CompressedBytes int
	// CompressedCount / DroppedCount / MarkerCount expose per-turn
	// counters for Step 11 savings metrics.
	CompressedCount int
	DroppedCount    int
	MarkerCount     int
	// Events is the per-decision compression detail (one record per
	// compress or drop). Persisted alongside the api_turn into the
	// compression_events table by the store layer; empty when the
	// pipeline skipped.
	Events []CompressionEvent
}

// CompressionEvent is one mechanism-tagged compression decision.
// Mirrors conversation.Event but defined here so the proxy contract
// stays import-cycle-free.
type CompressionEvent struct {
	Mechanism       string // 'json' | 'code' | 'logs' | 'text' | 'diff' | 'html' | 'drop'
	OriginalBytes   int
	CompressedBytes int
	MsgIndex        int
	ImportanceScore float64 // set only for 'drop'
}

// Options configures a Proxy.
type Options struct {
	// AnthropicUpstream is the base URL for Anthropic requests. Must be an
	// absolute URL with a scheme. Typical: https://api.anthropic.com.
	AnthropicUpstream string
	// OpenAIUpstream is the base URL for OpenAI-compatible requests. Typical:
	// https://api.openai.com.
	OpenAIUpstream string
	// Sink receives one APITurn per request. Required.
	Sink Sink
	// Compressor is optional. When non-nil, it runs before the request
	// body is forwarded upstream. Must preserve the request's JSON
	// envelope. See [Compressor].
	Compressor Compressor
	// ObserverLog, when non-nil, receives one entry per compressed
	// request describing the savings. Optional. In production this is
	// adapted from (*store.Store).InsertObserverLog.
	ObserverLog ObserverLogSink
	// SessionResolver, when non-nil, is consulted to fill in session_id
	// on requests that don't send an X-Session-Id header. Optional.
	SessionResolver SessionResolver
	// Logger receives operational messages. Defaults to a discard logger.
	Logger *slog.Logger
	// Client overrides the upstream HTTP client. Defaults to a client with
	// no response timeout (SSE streams can run for minutes) but a short dial
	// timeout so `proxy start` fails fast when the network is down.
	Client *http.Client
	// Clock overrides time.Now for tests. Defaults to time.Now.
	Clock func() time.Time
}

// Proxy is the API reverse proxy. Safe for concurrent use.
type Proxy struct {
	anthropicURL *url.URL
	openaiURL    *url.URL
	sink         Sink
	compressor   Compressor
	obsLog       ObserverLogSink
	sessions     SessionResolver
	logger       *slog.Logger
	client       *http.Client
	now          func() time.Time
}

// ObserverLogSink is the write side of the observer_log telemetry
// channel — same pattern as [Sink]. The zero value is a no-op when
// assigned as nil.
type ObserverLogSink interface {
	InsertObserverLog(ctx context.Context, level, component, message, details string) error
}

// New validates opts and constructs a Proxy.
func New(opts Options) (*Proxy, error) {
	if opts.Sink == nil {
		return nil, errors.New("proxy.New: Sink is required")
	}
	anthropicURL, err := parseUpstream("anthropic_upstream", opts.AnthropicUpstream, "https://api.anthropic.com")
	if err != nil {
		return nil, err
	}
	openaiURL, err := parseUpstream("openai_upstream", opts.OpenAIUpstream, "https://api.openai.com")
	if err != nil {
		return nil, err
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 16,
				// Idle connections are held for at most 30s before
				// recycling. Pre-v1.4.24 this was 90s — too long for
				// environments where a NAT layer (WSL2, corporate
				// firewalls, mobile hotspots) closes idle TCP streams
				// faster than that. The user-visible symptom was
				// "write tcp ...: connection reset by peer" on a
				// reused dead connection. 30s is short enough to evict
				// stale entries before typical NAT idle-kill, long
				// enough to amortize TLS for back-to-back requests in
				// the same conversation.
				IdleConnTimeout:       30 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
				// Disable response buffering so SSE events flow through
				// immediately.
				DisableCompression: true,
			},
			// No Timeout — SSE streams can run for minutes.
		}
	}
	now := opts.Clock
	if now == nil {
		now = time.Now
	}
	return &Proxy{
		anthropicURL: anthropicURL,
		openaiURL:    openaiURL,
		sink:         opts.Sink,
		compressor:   opts.Compressor,
		obsLog:       opts.ObserverLog,
		sessions:     opts.SessionResolver,
		logger:       logger,
		client:       client,
		now:          now,
	}, nil
}

func parseUpstream(name, raw, fallback string) (*url.URL, error) {
	if raw == "" {
		raw = fallback
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("proxy.New: %s: %w", name, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("proxy.New: %s %q must include scheme and host", name, raw)
	}
	return u, nil
}

// Handler returns the proxy's http.Handler. Mount it on a net/http server
// or use ListenAndServe.
func (p *Proxy) Handler() http.Handler { return http.HandlerFunc(p.serve) }

// ListenAndServe runs the proxy on addr until ctx is cancelled, then shuts
// down with a 5s grace period.
func (p *Proxy) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           p.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("proxy shutdown: %w", err)
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// serve is the top-level request handler.
func (p *Proxy) serve(w http.ResponseWriter, r *http.Request) {
	provider := providerForPath(r.URL.Path)
	upstream := p.anthropicURL
	if provider == models.ProviderOpenAI {
		upstream = p.openaiURL
	}

	// Read client request body so we can both forward it and inspect it.
	// Bodies are typically < 100KB (prompts) so a full buffer is fine.
	reqBody, err := io.ReadAll(r.Body)
	if err != nil {
		p.logger.Warn("proxy: read request body", "err", err)
		http.Error(w, "proxy: read request body", http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	// Optional pre-forward compression (spec §10 Layer 3). Must preserve
	// the request's JSON envelope and scrub any values inside before
	// returning a body to forward. When the compressor returns Skipped or
	// an empty Body, we keep the original request untouched.
	var compression CompressionResult
	if p.compressor != nil {
		compression = p.compressor.Compress(r.Context(), provider, reqBody)
		if !compression.Skipped && len(compression.Body) > 0 {
			reqBody = compression.Body
			p.recordCompression(r.Context(), provider, compression)
		}
	}

	// Build the upstream request.
	outURL := *upstream
	outURL.Path = joinPath(upstream.Path, r.URL.Path)
	outURL.RawQuery = r.URL.RawQuery

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, outURL.String(), bytes.NewReader(reqBody))
	if err != nil {
		p.logger.Warn("proxy: build upstream request", "err", err)
		http.Error(w, "proxy: build upstream request", http.StatusBadGateway)
		return
	}
	copyRequestHeaders(outReq.Header, r.Header)
	outReq.Host = upstream.Host
	outReq.ContentLength = int64(len(reqBody))

	start := p.now()
	resp, err := p.doWithRetry(outReq, reqBody)
	if err != nil {
		// Spec §17: proxy upstream failure — forward an error. Don't swallow.
		p.logger.Warn("proxy: upstream error", "provider", provider, "err", err)
		http.Error(w, fmt.Sprintf("proxy: upstream: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy upstream headers and status to the client.
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	contentType := resp.Header.Get("Content-Type")
	isStream := strings.HasPrefix(contentType, "text/event-stream")

	reqShape := parseRequest(reqBody)
	sessionID := r.Header.Get("X-Session-Id")
	if sessionID == "" && p.sessions != nil {
		if resolved, ok, err := p.sessions.Resolve(r.Context(), r.RemoteAddr); err != nil {
			p.logger.Debug("proxy: session resolve", "addr", r.RemoteAddr, "err", err)
		} else if ok {
			sessionID = resolved
		}
	}

	if isStream {
		captured := p.teeStream(r.Context(), w, resp.Body)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			// Upstream returned an error status. The captured body might
			// be either a plain JSON error (when the upstream returned
			// non-200 immediately, before any SSE preamble) or an SSE
			// `event: error` envelope. Try the SSE shape first; fall
			// back to direct parse.
			errBody := extractStreamErrorBody(captured)
			if errBody == nil {
				errBody = captured
			}
			turn := buildErrorTurn(provider, reqShape, errBody, resp.Header, resp.StatusCode, start, sessionID)
			if _, err := p.sink.InsertAPITurn(r.Context(), turn); err != nil {
				p.logger.Warn("proxy: insert error api_turn (stream)", "err", err)
			}
			return
		}
		turn := p.buildStreamTurn(provider, reqShape, captured, resp.Header, start, sessionID)
		if turn.Model == "" {
			return
		}
		if isEmptyUsage(turn) {
			// Stream delivered headers + message_start (model is set) but
			// never produced a usage-bearing delta — likely a cancelled
			// request or short-circuited upstream. Recording a zero-token
			// turn pollutes averages and inflates turn counts. See audit
			// item B2.
			p.logger.Debug("proxy: dropping zero-usage stream turn", "model", turn.Model)
			return
		}
		applyCompressionMeta(&turn, compression)
		if _, err := p.sink.InsertAPITurn(r.Context(), turn); err != nil {
			p.logger.Warn("proxy: insert api_turn (stream)", "err", err)
		}
		return
	}

	// Non-streaming: buffer the full body so we can inspect + forward.
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		p.logger.Warn("proxy: read upstream body", "err", err)
		return
	}
	if _, err := w.Write(respBody); err != nil {
		p.logger.Warn("proxy: write client body", "err", err)
		return
	}

	// Non-2xx — record a zero-token error turn so the failure is
	// visible. Pre-v1.4.20 the proxy returned early here, silently
	// dropping rate-limit / overloaded / invalid-request errors. Now
	// the parsed error envelope (Anthropic / OpenAI standard shape)
	// lands on api_turns.{http_status, error_class, error_message}.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		turn := buildErrorTurn(provider, reqShape, respBody, resp.Header, resp.StatusCode, start, sessionID)
		if _, err := p.sink.InsertAPITurn(r.Context(), turn); err != nil {
			p.logger.Warn("proxy: insert error api_turn", "err", err)
		}
		return
	}

	turn := p.buildTurn(provider, reqShape, respBody, resp.Header, start, sessionID)
	if turn.Model == "" {
		return
	}
	if isEmptyUsage(turn) {
		p.logger.Debug("proxy: dropping zero-usage turn", "model", turn.Model)
		return
	}
	applyCompressionMeta(&turn, compression)
	if _, err := p.sink.InsertAPITurn(r.Context(), turn); err != nil {
		p.logger.Warn("proxy: insert api_turn", "err", err)
	}
}

// isEmptyUsage reports whether a parsed turn carries no usable token
// counts at all. Such turns happen when the upstream response stream
// ended before any usage event arrived (cancellation, mid-flight error,
// or an envelope with model + id but no usage block). Recording them
// distorts averages and turn counts without adding analytical value.
func isEmptyUsage(t models.APITurn) bool {
	return t.InputTokens == 0 &&
		t.OutputTokens == 0 &&
		t.CacheReadTokens == 0 &&
		t.CacheCreationTokens == 0
}

// applyCompressionMeta copies compression fields from the pre-forward
// CompressionResult onto the APITurn so the cost engine and dashboard
// can aggregate savings per model/session/day.
func applyCompressionMeta(t *models.APITurn, c CompressionResult) {
	if c.Skipped {
		return
	}
	t.MessagePrefixHash = c.MessagePrefixHash
	t.CompressionOriginalBytes = int64(c.OriginalBytes)
	t.CompressionCompressedBytes = int64(c.CompressedBytes)
	t.CompressionCount = int64(c.CompressedCount)
	t.CompressionDroppedCount = int64(c.DroppedCount)
	t.CompressionMarkerCount = int64(c.MarkerCount)
	if len(c.Events) > 0 {
		t.CompressionEvents = make([]models.CompressionEvent, 0, len(c.Events))
		for _, e := range c.Events {
			t.CompressionEvents = append(t.CompressionEvents, models.CompressionEvent{
				Timestamp:       t.Timestamp, // share the turn timestamp
				Mechanism:       e.Mechanism,
				OriginalBytes:   int64(e.OriginalBytes),
				CompressedBytes: int64(e.CompressedBytes),
				MsgIndex:        e.MsgIndex,
				ImportanceScore: e.ImportanceScore,
			})
		}
	}
}

// recordCompression logs a per-turn compression summary to the
// observer_log telemetry channel so the dashboard and `observer cost`
// can aggregate savings (spec §10 Layer 3 step 11). Silent no-op when no
// ObserverLogSink is wired.
func (p *Proxy) recordCompression(ctx context.Context, provider string, c CompressionResult) {
	if p.obsLog == nil {
		return
	}
	details := fmt.Sprintf(
		`{"provider":%q,"original_bytes":%d,"compressed_bytes":%d,"compressed_count":%d,"dropped_count":%d,"marker_count":%d,"prefix_hash":%q}`,
		provider,
		c.OriginalBytes,
		c.CompressedBytes,
		c.CompressedCount,
		c.DroppedCount,
		c.MarkerCount,
		c.MessagePrefixHash,
	)
	msg := fmt.Sprintf("conversation compression: %d → %d bytes (%d%% saved)",
		c.OriginalBytes, c.CompressedBytes, savingsPercent(c.OriginalBytes, c.CompressedBytes))
	if err := p.obsLog.InsertObserverLog(ctx, "info", "compress", msg, details); err != nil {
		p.logger.Warn("proxy: observer_log write", "err", err)
	}
}

// savingsPercent is a small helper returning 0..100. Used only for the
// human-readable observer_log message; stats callers should compute
// ratios themselves.
func savingsPercent(before, after int) int {
	if before <= 0 {
		return 0
	}
	saved := before - after
	if saved <= 0 {
		return 0
	}
	return saved * 100 / before
}

// buildTurn constructs an APITurn from a completed non-streaming exchange.
// It returns the turn even when usage fields are zero so the caller can
// decide whether to store it; the serve() path drops turns with an empty
// Model since those almost always indicate an error body we couldn't parse.
func (p *Proxy) buildTurn(
	provider string,
	req requestShape,
	respBody []byte,
	respHeader http.Header,
	start time.Time,
	sessionID string,
) models.APITurn {
	var resp responseShape
	if provider == models.ProviderAnthropic {
		resp = parseAnthropicResponse(respBody)
	} else {
		resp = parseOpenAIResponse(respBody)
	}
	model := resp.Model
	if model == "" {
		model = req.Model
	}
	requestID := resp.RequestID
	if requestID == "" {
		requestID = respHeader.Get("X-Request-Id")
	}
	return models.APITurn{
		SessionID:             sessionID,
		Timestamp:             start.UTC(),
		Provider:              provider,
		Model:                 model,
		RequestID:             requestID,
		InputTokens:           resp.InputTokens,
		OutputTokens:          resp.OutputTokens,
		CacheReadTokens:       resp.CacheReadTokens,
		CacheCreationTokens:   resp.CacheCreationTokens,
		CacheCreation1hTokens: resp.CacheCreation1hTokens,
		MessageCount:          req.MessageCount,
		ToolUseCount:          req.ToolUseCount,
		SystemPromptHash:      req.SystemPromptHash,
		TotalResponseMS:       p.now().Sub(start).Milliseconds(),
		StopReason:            resp.StopReason,
	}
}

// buildStreamTurn constructs an APITurn from a captured SSE exchange. The
// request shape carries message/tool counts; the captured body carries the
// accurate token counts and stop_reason from the stream's terminal events.
func (p *Proxy) buildStreamTurn(
	provider string,
	req requestShape,
	captured []byte,
	respHeader http.Header,
	start time.Time,
	sessionID string,
) models.APITurn {
	result := parseSSEStream(captured, provider)
	model := result.Model
	if model == "" {
		model = req.Model
	}
	requestID := result.RequestID
	if requestID == "" {
		requestID = respHeader.Get("X-Request-Id")
	}
	return models.APITurn{
		SessionID:             sessionID,
		Timestamp:             start.UTC(),
		Provider:              provider,
		Model:                 model,
		RequestID:             requestID,
		InputTokens:           result.InputTokens,
		OutputTokens:          result.OutputTokens,
		CacheReadTokens:       result.CacheReadTokens,
		CacheCreationTokens:   result.CacheCreationTokens,
		CacheCreation1hTokens: result.CacheCreation1hTokens,
		MessageCount:          req.MessageCount,
		ToolUseCount:          req.ToolUseCount,
		SystemPromptHash:      req.SystemPromptHash,
		TotalResponseMS:       p.now().Sub(start).Milliseconds(),
		StopReason:            result.StopReason,
	}
}

// doWithRetry wraps p.client.Do with a single retry on transient
// transport-layer errors. The Go http client auto-retries when the
// request body hasn't been written yet AND the connection failure
// happens before the request goes on the wire — but if the failure
// surfaces mid-write (the typical "write tcp ...: connection reset by
// peer" symptom on a stale keep-alive entry that NAT closed), the
// auto-retry is suppressed and the error bubbles to the caller.
//
// We retry once for that specific class of error: connection reset,
// EOF, or broken pipe surfacing as a net.OpError on Write or as a
// url.Error wrapping one. Exactly one retry — never more — so a
// genuine upstream outage doesn't get masked. The retry rebuilds the
// request because http.NewRequestWithContext + bytes.NewReader gives
// us a body that's already been consumed once on the failed attempt.
//
// We do NOT retry on context cancellation, on non-transient transport
// errors (TLS handshake failure, dial failure mid-handshake), or on
// any HTTP-level response (a 5xx is the upstream's job to surface).
func (p *Proxy) doWithRetry(req *http.Request, body []byte) (*http.Response, error) {
	resp, err := p.client.Do(req)
	if err == nil {
		return resp, nil
	}
	if !isRetryableTransportError(err) || req.Context().Err() != nil {
		return nil, err
	}
	// Transport-level transient. Drain whatever pooled idle connection
	// might still be poisoned, then rebuild and retry once.
	p.logger.Info("proxy: upstream transport hiccup, retrying once", "err", err)
	if rt, ok := p.client.Transport.(*http.Transport); ok {
		rt.CloseIdleConnections()
	}
	retryReq, rerr := http.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), bytes.NewReader(body))
	if rerr != nil {
		return nil, err
	}
	retryReq.Header = req.Header.Clone()
	retryReq.Host = req.Host
	retryReq.ContentLength = int64(len(body))
	resp, retryErr := p.client.Do(retryReq)
	if retryErr != nil {
		// Surface the retry error, not the original — it's the most
		// recent signal of upstream state.
		return nil, retryErr
	}
	return resp, nil
}

// isRetryableTransportError matches the transport-level transient
// errors worth a single retry. The Go stdlib doesn't expose stable
// types for these (most are syscall.Errno wrapped through several
// layers of net.OpError / url.Error), so we string-match on the
// canonical messages. Conservative: only matches the exact phrases
// our deployment has observed, not a broad "any net error" sweep.
func isRetryableTransportError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "connection reset by peer"):
		return true
	case strings.Contains(s, "broken pipe"):
		return true
	case strings.Contains(s, "use of closed network connection"):
		return true
	case strings.HasSuffix(s, ": EOF"):
		return true
	}
	return false
}

// joinPath appends r onto base, preserving the base path's trailing segment
// semantics. url.JoinPath isn't used because it collapses empty base paths
// to /, which breaks against hosts like https://api.anthropic.com with no
// configured path prefix.
func joinPath(base, path string) string {
	if base == "" || base == "/" {
		return path
	}
	base = strings.TrimRight(base, "/")
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path
}

// hopByHopHeaders are stripped from forwarded requests and responses.
// RFC 7230 §6.1.
var hopByHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Proxy-Connection":    {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

// copyRequestHeaders copies client headers onto the upstream request,
// stripping hop-by-hop headers and the X-Session-Id metadata header which
// belongs to the proxy, not the upstream API.
func copyRequestHeaders(dst, src http.Header) {
	for k, vs := range src {
		if _, hop := hopByHopHeaders[k]; hop {
			continue
		}
		if k == "X-Session-Id" {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// copyResponseHeaders copies upstream headers onto the client response,
// stripping hop-by-hop headers.
func copyResponseHeaders(dst, src http.Header) {
	for k, vs := range src {
		if _, hop := hopByHopHeaders[k]; hop {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// sha256Hex returns the lowercase hex SHA-256 of b. The proxy uses it for
// the system_prompt_hash column — stable across runs so an analyst can tell
// when the same prompt prefix is being reused.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
