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
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

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

// SessionAwareCompressor is the v1.4.42 extension: when the proxy has
// already extracted the request's session_id (Anthropic Pro/Max OAuth
// path) it passes it through so session-scoped features can fire
// (D20 rolling summaries, C16 read-cache auto-substitution). The base
// Compressor interface stays for backward compat with simpler callers
// and tests.
type SessionAwareCompressor interface {
	Compressor
	CompressInSession(ctx context.Context, provider string, body []byte, sessionID string) CompressionResult
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
	BodyHash        string  // sha256-hex of pre-compression body (V7-9)
}

// CostComputer turns a (model, token-bundle) pair into a USD cost for
// the api_turns.cost_usd column. Optional — nil means the proxy
// records turns without a cost (downstream readers compute it on the
// fly via the cost engine). When set, the proxy populates cost_usd at
// insert time so `observer cost` and CLI scripts that sum the column
// directly (scripts/ab-claude-report.sh, etc.) see real numbers
// without re-implementing pricing in shell. In production this is a
// thin adapter over (*cost.Engine).Compute.
type CostComputer interface {
	Compute(model string, tokens CostTokens) (usd float64, ok bool)
}

// CostTokens mirrors cost.TokenBundle but is duplicated here so the
// proxy contract stays free of an import cycle into the intelligence
// package. Field names and meaning match TokenBundle exactly.
type CostTokens struct {
	Input           int64
	Output          int64
	CacheRead       int64
	CacheCreation   int64
	CacheCreation1h int64
}

// Options configures a Proxy.
type Options struct {
	// AnthropicUpstream is the base URL for Anthropic requests. Must be an
	// absolute URL with a scheme. Typical: https://api.anthropic.com.
	AnthropicUpstream string
	// OpenAIUpstream is the base URL for OpenAI-compatible requests. Typical:
	// https://api.openai.com.
	OpenAIUpstream string
	// ChatGPTUpstream is the base URL for ChatGPT-plan Codex backend requests.
	// Typical: https://chatgpt.com.
	ChatGPTUpstream string
	// ForceChatGPTHTTP rejects ChatGPT backend websocket upgrades so Codex falls
	// back to HTTPS POST, which is the path Observer can currently compress.
	ForceChatGPTHTTP bool
	// Sink receives one APITurn per request. Required.
	Sink Sink
	// Compressor is optional. When non-nil, it runs before the request
	// body is forwarded upstream. Must preserve the request's JSON
	// envelope. See [Compressor].
	Compressor Compressor
	// CostComputer is optional. When non-nil, the proxy populates
	// APITurn.CostUSD before insertion using this computer. See
	// [CostComputer].
	CostComputer CostComputer
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
	// PostCompactInjector, when non-nil, gates the v1.4.43+ / Tier 3 /
	// D23 compaction-survival feature: on Anthropic requests whose
	// session_id has a recent compaction event in the observer DB, the
	// proxy prepends a synthetic system block carrying recovery context
	// (last reads, last edits, recent failures, learned rules) so the
	// model can re-orient without paying for a re-Read of every file.
	// Cross-turn invariance: the injected content is byte-stable per
	// compaction event, so Anthropic's prefix cache hits hold across
	// every turn of the post-compact conversation.
	PostCompactInjector PostCompactInjector
	// AuthCache, when non-nil, captures the Authorization /
	// x-api-key headers of every Anthropic request before the
	// compressor runs (D20 / Tier 2 — live API wire-up). The
	// rolling-summ summariser pulls credentials from this cache so
	// the per-session summary call rides the same auth as the user's
	// regular request. Optional — without it, rolling summarisation
	// no-ops (the framework's safety contract).
	AuthCache AuthCache
	// PrewarmTargets are URLs the proxy fires a no-op GET against at
	// startup to populate the http.Transport connection pool with
	// warm TLS sessions. V6-3 mitigation: the first real proxy
	// request reuses the pooled connection, saving the 500ms-1.5s
	// TLS handshake delay that pushed codex's inner-pipe TTFB past
	// its ~15s timeout. Empty list disables pre-warm. Defaults are
	// set by cmd/observer/start.go from [proxy].prewarm_targets in
	// config.toml. See docs/observer-platform-issues-v6.md §V6-3.
	PrewarmTargets []string
	// CompressTypes mirrors `[compression.conversation] compress_types`.
	// Used by the V7-2 codex-variant warning: when the proxy sees an
	// OpenAI request whose model matches the codex-variant family
	// (anything matching the `(?i)(?:^codex-|-codex(?:-|$))` regex)
	// AND CompressTypes is non-empty, observer emits one stderr
	// warning per session pointing the operator at
	// `docs/codex-compression-recipe.md`. Empty / nil disables the
	// warning. Optional — leaving it unset is the test-only path.
	CompressTypes []string
}

// AuthCache is the proxy-side write interface for
// (messagesummary.AuthCache). The interface lets proxy_test.go stub
// it without importing the messagesummary package.
type AuthCache interface {
	Set(sessionID string, creds AuthCredentials)
}

// AuthCredentials mirrors messagesummary.AuthCredentials so the proxy
// contract stays import-cycle-free. Field names match exactly.
type AuthCredentials struct {
	Authorization string
	APIKey        string
}

// Proxy is the API reverse proxy. Safe for concurrent use.
type Proxy struct {
	anthropicURL     *url.URL
	openaiURL        *url.URL
	chatgptURL       *url.URL
	forceChatGPTHTTP bool
	sink             Sink
	compressor       Compressor
	cost             CostComputer
	obsLog           ObserverLogSink
	sessions         SessionResolver
	logger           *slog.Logger
	client           *http.Client
	now              func() time.Time
	postCompact      PostCompactInjector
	authCache        AuthCache
	prewarmTargets   []string
	// compressTypes holds the configured `compress_types` slice so the
	// per-request V7-2 codex-variant warning can decide whether to
	// fire. See Options.CompressTypes.
	compressTypes []string
	// codexVariantWarned is the per-session dedup map for the V7-2
	// warning. Keyed by session_id (codex's prompt_cache_key); value
	// is unused. Bounded via codexVariantWarnedCap to prevent memory
	// growth on long-running daemons; eviction is simple oldest-first
	// (any-key delete) when the cap is hit. The map's read/write
	// contention is low — one Load + one Store per session, lifetime
	// of the daemon.
	codexVariantWarned sync.Map
}

// codexVariantRe matches OpenAI-shape model identifiers that belong to
// the codex-variant family — i.e. names where `codex` appears as a
// dash-delimited token. Verified against the v4 batch's observed
// model strings: gpt-5.3-codex, gpt-5.3-codex-low, gpt-5.3-codex-high,
// gpt-5.3-codex-xhigh, gpt-5-codex-agent, codex-internal. Does NOT
// match gpt-5.4-mini, gpt-5.5, claude-* etc. Case-insensitive — codex
// emits lowercase in practice but the model field is operator-supplied
// on some paths.
//
// The empirical v4-codex-compression session (2026-05-29) showed that
// this family treats compressed tool_result bodies as missing data and
// re-derives via shell tools, inflating cost ~2-3×. V7-2 mitigation
// emits a one-line warning per session pointing the operator at the
// codex-variant recipe.
var codexVariantRe = regexp.MustCompile(`(?i)(?:^codex-|-codex(?:-|$))`)

// codexVariantWarnedCap caps the per-daemon dedup map so a long-running
// observer with thousands of distinct sessions doesn't grow unbounded.
// 10k is plenty for realistic operator deployments — well past the
// session count of any single workday.
const codexVariantWarnedCap = 10_000

// maybeWarnCodexVariantModel emits at most one stderr warning per
// session_id when an OpenAI-shape request's model matches a
// codex-variant identifier AND the proxy was configured with non-empty
// `compress_types`. See Options.CompressTypes for the contract.
// Pure no-op when sid is empty, compressTypes is empty, or the model
// is not a codex variant. Safe for concurrent use.
func (p *Proxy) maybeWarnCodexVariantModel(sid, model string) {
	if sid == "" || model == "" || len(p.compressTypes) == 0 {
		return
	}
	if !codexVariantRe.MatchString(model) {
		return
	}
	if _, loaded := p.codexVariantWarned.LoadOrStore(sid, struct{}{}); loaded {
		return
	}
	// Bound the map. Drop one arbitrary entry — eviction order is best-
	// effort, not strictly oldest-first. The cap prevents pathological
	// growth, not warning re-emission for the previously-evicted key
	// (a re-emission for an old session that just happens to send a
	// request after eviction is acceptable, given how rare it is).
	if mapLen(&p.codexVariantWarned) > codexVariantWarnedCap {
		p.codexVariantWarned.Range(func(k, _ any) bool {
			p.codexVariantWarned.Delete(k)
			return false // delete one and stop
		})
	}
	p.logger.Warn(
		"proxy: codex-variant model paired with non-empty compress_types — "+
			"the codex-variant family re-derives data on compressed tool_results, "+
			"inflating cost; recommend `[compression.conversation] compress_types = []` "+
			"or `observer start --recipe codex-variant`; see docs/codex-compression-recipe.md",
		"model", model,
		"session_id", sid,
		"compress_types", strings.Join(p.compressTypes, ","),
	)
}

// mapLen returns the number of entries in m. sync.Map has no Len
// method; we walk the map once. Called only on the warning-emit path
// (rare), so the cost is amortized away.
func mapLen(m *sync.Map) int {
	n := 0
	m.Range(func(_, _ any) bool { n++; return true })
	return n
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
	chatgptURL, err := parseUpstream("chatgpt_upstream", opts.ChatGPTUpstream, "https://chatgpt.com")
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
	prewarm := opts.PrewarmTargets
	if prewarm == nil {
		// nil → use defaults. Empty slice (non-nil) → operator explicitly
		// disabled pre-warm (don't fire any).
		prewarm = []string{"https://chatgpt.com/", "https://api.openai.com/"}
	}
	return &Proxy{
		anthropicURL:     anthropicURL,
		openaiURL:        openaiURL,
		chatgptURL:       chatgptURL,
		forceChatGPTHTTP: opts.ForceChatGPTHTTP,
		sink:             opts.Sink,
		compressor:       opts.Compressor,
		cost:             opts.CostComputer,
		obsLog:           opts.ObserverLog,
		sessions:         opts.SessionResolver,
		logger:           logger,
		client:           client,
		now:              now,
		postCompact:      opts.PostCompactInjector,
		authCache:        opts.AuthCache,
		prewarmTargets:   prewarm,
		compressTypes:    opts.CompressTypes,
	}, nil
}

// Prewarm fires a no-op HEAD against each target URL using the
// proxy's upstream http.Client. The goal is to populate the
// http.Transport's connection pool with warm TLS sessions so the
// first real proxy request reuses a pooled connection instead of
// paying a cold-handshake. V6-3 mitigation (see
// docs/observer-platform-issues-v6.md).
//
// Cheap: HEAD against a public endpoint, 5s timeout, non-fatal on
// any error (connectivity, DNS, 4xx/5xx — all logged at info, never
// returned). Safe to call multiple times; each call refreshes the
// pool.
//
// Exported so cmd/observer can fire it at start (current callsite:
// ListenAndServe) and so the test suite can drive it directly.
func (p *Proxy) Prewarm(ctx context.Context, targets []string) {
	if len(targets) == 0 {
		return
	}
	for _, raw := range targets {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodHead, raw, nil)
		if err != nil {
			cancel()
			p.logger.Info("proxy: prewarm skipped (bad URL)", "target", raw, "err", err)
			continue
		}
		resp, err := p.client.Do(req)
		if err != nil {
			cancel()
			p.logger.Info("proxy: prewarm failed", "target", raw, "err", err)
			continue
		}
		_ = resp.Body.Close()
		cancel()
		p.logger.Info("proxy: prewarm ok", "target", raw, "status", resp.StatusCode)
	}
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
	// V6-3 mitigation: pre-warm the http.Transport connection pool
	// against configured upstream targets so the first real request
	// doesn't pay the 500ms-1.5s TLS handshake cost that pushed
	// codex's inner-pipe TTFB past its ~15s timeout. Non-blocking;
	// failures are logged but never abort startup.
	go p.Prewarm(ctx, p.prewarmTargets)
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
	// Codex 0.128.0+ with `requires_openai_auth = true` on a custom
	// model_provider hits canonical /v1/responses paths with a ChatGPT
	// JWT — the proxy can't differentiate by path alone, so detect via
	// the Authorization header. When detected, route the request to
	// chatgpt.com and rewrite the path to the codex backend equivalent.
	chatgptAuth := provider == models.ProviderOpenAI && isChatGPTAuthRequest(r)
	upstream := p.upstreamForPath(r.URL.Path, provider, chatgptAuth)
	upstreamPath := r.URL.Path
	if chatgptAuth {
		upstreamPath = translateChatGPTPath(r.URL.Path)
	}

	// Short-circuit /v1/models for ChatGPT-auth: chatgpt.com doesn't
	// expose a /backend-api/codex/models endpoint, so codex's model-
	// list refresher hammers it and logs 401s on every interactive
	// turn. Codex tolerates an empty model list, so return a synthetic
	// 200 and skip the upstream trip entirely. No api_turn is recorded
	// — this isn't an LLM call. Codex 0.128.0+ expects the chatgpt-
	// shaped `{"models":[]}` body, not OpenAI's
	// `{"object":"list","data":[]}` — the latter trips its decoder with
	// "missing field `models`".
	if chatgptAuth && r.Method == http.MethodGet && isModelsPath(r.URL.Path) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":[]}`))
		return
	}

	if isWebSocketUpgrade(r) {
		if p.forceChatGPTHTTP && (isChatGPTBackendPath(r.URL.Path) || chatgptAuth) {
			http.Error(w, "observer: ChatGPT websocket disabled; use HTTP fallback", http.StatusUpgradeRequired)
			return
		}
		p.serveUpgradePassthrough(w, r, upstream)
		return
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
	reqShapeBody := reqBody
	if p.compressor != nil {
		compressionInput := reqBody
		needsReencode := false
		if isZstdEncoded(r.Header.Get("Content-Encoding")) {
			if decoded, err := decodeZstd(reqBody); err != nil {
				p.logger.Debug("proxy: skip compressed request body", "encoding", r.Header.Get("Content-Encoding"), "err", err)
			} else {
				compressionInput = decoded
				reqShapeBody = decoded
				needsReencode = true
			}
		}
		// v1.4.43+ / Tier 3 / D23: pre-compression post-compact context
		// injection. When the session has a recent compaction event in
		// the observer DB, prepend a synthetic system block carrying
		// recovery context (last reads, last edits, recent failures,
		// learned rules) so the model can re-orient without re-Reading
		// every file. Runs after zstd-decode so injectAnthropicSystemBlock
		// sees plain JSON; runs before compression so the compressor
		// sees the injected body. The injector caches per compaction
		// event — the prepended bytes are stable turn-over-turn and
		// Anthropic's prefix cache hits hold across the post-compact
		// conversation.
		injected := false
		if p.postCompact != nil && provider == models.ProviderAnthropic {
			sid := extractAnthropicSessionID(compressionInput)
			if sid != "" {
				if content, err := p.postCompact.Get(r.Context(), sid); err == nil && content != "" {
					if mutated, ierr := injectAnthropicSystemBlock(compressionInput, content); ierr == nil && len(mutated) > 0 {
						compressionInput = mutated
						reqShapeBody = mutated
						injected = true
					} else if ierr != nil {
						p.logger.Debug("proxy: post-compact inject failed", "session_id", sid, "err", ierr)
					}
				}
			}
		}
		// v1.4.42+: when the compressor implements SessionAwareCompressor
		// and the body carries a session_id (Claude Code SDK's metadata
		// blob, or codex's prompt_cache_key), thread it through so
		// session-scoped features (rolling-summ, read-cache) can fire on
		// the right session. Falls back to the base Compress() path when
		// neither side supports it OR when the provider has no extractor.
		//
		// Codex parity: extractOpenAISessionID reads prompt_cache_key
		// from the Responses API body. Header fallback reads `session_id`
		// HTTP header (same value in observed traffic). Auth-cache
		// stores Bearer tokens for both API-key (sk-...) and ChatGPT-Plus
		// JWT (eyJ...) shapes — the OpenAI summariser picks the right
		// one when calling the Responses API.
		if sac, ok := p.compressor.(SessionAwareCompressor); ok && (provider == models.ProviderAnthropic || provider == models.ProviderOpenAI) {
			var sid string
			switch provider {
			case models.ProviderAnthropic:
				sid = extractAnthropicSessionID(compressionInput)
			case models.ProviderOpenAI:
				sid = extractOpenAISessionID(compressionInput)
				if sid == "" {
					// Header fallback: codex puts the same UUID on
					// `session_id` / `thread_id` / `x-client-request-id`.
					// Body extract should normally hit; this catches edge
					// cases where the body lost the prompt_cache_key
					// (older codex builds, custom upstreams, or a turn
					// where the SDK omits it).
					sid = r.Header.Get("session_id")
				}
			}
			// D20: capture per-session Authorization / x-api-key so the
			// rolling-summ Summarizer can call upstream with the same
			// auth the proxy is forwarding. Done before invoking the
			// compressor so an in-flight summary call sees fresh
			// credentials. No-op when AuthCache is unset.
			if p.authCache != nil && sid != "" {
				p.authCache.Set(sid, AuthCredentials{
					Authorization: r.Header.Get("Authorization"),
					APIKey:        r.Header.Get("x-api-key"),
				})
			}
			// V7-2: once-per-session warning when an OpenAI-shape request
			// targets a codex-variant model (gpt-5.3-codex family) while
			// `compress_types` is non-empty. Logs are pinned at
			// docs/codex-compression-recipe.md; no-op for Anthropic
			// requests (no codex-variant Anthropic models) and for
			// requests that don't carry a session_id.
			if provider == models.ProviderOpenAI {
				p.maybeWarnCodexVariantModel(sid, extractOpenAIModel(compressionInput))
			}
			compression = sac.CompressInSession(r.Context(), provider, compressionInput, sid)
		} else {
			compression = p.compressor.Compress(r.Context(), provider, compressionInput)
		}
		if !compression.Skipped && len(compression.Body) > 0 {
			reqShapeBody = compression.Body
			if needsReencode {
				encoded, err := encodeZstd(compression.Body)
				if err != nil {
					p.logger.Debug("proxy: zstd re-encode compressed request body", "err", err)
				} else {
					reqBody = encoded
				}
			} else {
				reqBody = compression.Body
			}
			p.recordCompression(r.Context(), provider, compression)
		} else if injected {
			// Compression skipped (zero events fired) but we did mutate
			// the body via post-compact injection. Forward the injected
			// bytes upstream rather than the pre-injection original.
			if needsReencode {
				if encoded, err := encodeZstd(compressionInput); err == nil {
					reqBody = encoded
				} else {
					p.logger.Debug("proxy: zstd re-encode post-inject body", "err", err)
				}
			} else {
				reqBody = compressionInput
			}
		}
	}

	// Build the upstream request.
	outURL := *upstream
	outURL.Path = joinPath(upstream.Path, upstreamPath)
	outURL.RawQuery = r.URL.RawQuery

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, outURL.String(), bytes.NewReader(reqBody))
	if err != nil {
		p.logger.Warn("proxy: build upstream request", "err", err)
		http.Error(w, "proxy: build upstream request", http.StatusBadGateway)
		return
	}
	copyRequestHeaders(outReq.Header, r.Header)
	// Force identity response encoding on upstream so we can parse the
	// SSE stream and the non-streaming JSON body. Without this, claude
	// (and most modern HTTP clients) send Accept-Encoding: gzip, br —
	// api.anthropic.com responds with Content-Encoding: gzip — and the
	// streaming parser tees gzip-encoded bytes through `data:`-line
	// pattern matching, finding zero usage fields. The bug is invisible
	// for API-key-auth requests when ANTHROPIC_BASE_URL is hit by
	// scripts/curl that don't request compression, but surfaces hard on
	// the OAuth path where claude's own request always negotiates gzip.
	// Identity-forcing trades a small bandwidth cost (the proxy runs on
	// loopback so it's effectively free) for accurate usage capture.
	outReq.Header.Set("Accept-Encoding", "identity")
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
	// chatgpt.com/backend-api/codex/responses returns SSE bodies with an
	// empty/missing Content-Type header (verified against codex 0.129.0+;
	// fixture: testdata/chatgpt_codex_responses_sse.bin). The looksLikeSSE
	// fallback below recovers the api_turn metadata after-the-fact, but
	// without forcing the streaming path here, the response is buffered
	// in io.ReadAll → codex's app-server inner-pipe "wait for first byte"
	// timeout (~15 s) trips, codex emits `Reconnecting... N/5 (timeout
	// waiting for child process to exit)`, and each turn takes ~2 min
	// wall instead of ~10 s. See docs/observer-platform-issues-v4.md V4-1.
	isStream := strings.HasPrefix(contentType, "text/event-stream") || chatgptAuth
	// V6-3 defense-in-depth: on the streaming path only, flush
	// upstream status+headers immediately so codex's inner pipe has
	// SOMETHING to consume before the first SSE event arrives. Gated
	// on isStream because flushing in the non-stream path would let
	// the client read the response before the synchronous
	// insertTurnDetached completes — racing tests that check sink
	// state immediately after http.Post returns.
	if isStream {
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}

	reqShape := parseRequest(reqShapeBody)
	if isChatGPTBackendPath(r.URL.Path) {
		if items := summarizeResponsesInput(reqShapeBody); len(items) > 0 {
			p.logger.Debug("proxy: chatgpt responses input", "items", items)
		}
	}
	// Resolve the session_id that lands on the api_turn row. Lookup
	// order:
	//   - Anthropic: extractAnthropicSessionID (metadata.user_id from
	//     Claude Code SDK) → X-Session-Id header → SessionResolver.
	//   - OpenAI: extractOpenAISessionID (prompt_cache_key) →
	//     `session_id` / `thread_id` / `x-client-request-id` header
	//     fallback (codex emits the same UUID on each) → X-Session-Id
	//     header → SessionResolver. Pre-V4-4, OpenAI requests skipped
	//     the body+header fallback at this insert site entirely, so
	//     ChatGPT-Plus auth turns (which drop prompt_cache_key from
	//     the body) always landed with `session_id = ""` and collapsed
	//     to `<unattributed>` in `observer cost --group-by session`.
	sessionID := p.resolveAPITurnSessionID(r, provider, reqShapeBody)

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
			p.applyCost(&turn)
			p.insertTurnDetached(turn, "proxy: insert error api_turn (stream)")
			return
		}
		turn := p.buildStreamTurn(provider, reqShape, captured, resp.Header, start, sessionID)
		if turn.Model == "" {
			return
		}
		if isEmptyUsage(turn) && !hasCompressionEvidence(compression) && !chatgptAuth {
			// Stream delivered headers + message_start (model is set) but
			// never produced a usage-bearing delta — likely a cancelled
			// request or short-circuited upstream. Recording a zero-token
			// turn pollutes averages and inflates turn counts. See audit
			// item B2. Exceptions: (1) when Observer itself compressed the
			// request, keep the turn so compression telemetry remains
			// visible even if the upstream omitted usage; (2) when the
			// request transited the ChatGPT-auth path, keep the turn
			// regardless — chatgpt.com's SSE usage shape can differ from
			// api.openai.com's and we'd rather log a partial turn than
			// silently drop the A/B baseline.
			p.logger.Debug("proxy: dropping zero-usage stream turn", "model", turn.Model)
			return
		}
		applyCompressionMeta(&turn, compression)
		p.applyCost(&turn)
		p.insertTurnDetached(turn, "proxy: insert api_turn (stream)")
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
	// SSE content-sniffing: chatgpt.com/backend-api/codex/responses
	// (observed 2026-05-08 against codex 0.129.0) returns SSE bodies
	// with an empty/missing Content-Type header, which means the
	// `Content-Type: text/event-stream` check above mis-classified the
	// stream as non-streaming. Sniff the body prefix as a fallback so
	// the SSE parser actually runs — without this, parseOpenAIResponse
	// fails to JSON-unmarshal the SSE body, returns Model="", and the
	// proxy silently drops the turn with no api_turns row.
	if !isStream && looksLikeSSE(respBody) {
		// Non-2xx error already handled above; skip to the success path.
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			turn := p.buildStreamTurn(provider, reqShape, respBody, resp.Header, start, sessionID)
			if turn.Model == "" {
				return
			}
			if isEmptyUsage(turn) && !hasCompressionEvidence(compression) && !chatgptAuth {
				p.logger.Debug("proxy: dropping zero-usage sniffed-SSE turn", "model", turn.Model)
				return
			}
			applyCompressionMeta(&turn, compression)
			p.applyCost(&turn)
			p.insertTurnDetached(turn, "proxy: insert api_turn (sniffed SSE)")
			return
		}
	}

	// Non-2xx — record a zero-token error turn so the failure is
	// visible. Pre-v1.4.20 the proxy returned early here, silently
	// dropping rate-limit / overloaded / invalid-request errors. Now
	// the parsed error envelope (Anthropic / OpenAI standard shape)
	// lands on api_turns.{http_status, error_class, error_message}.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		turn := buildErrorTurn(provider, reqShape, respBody, resp.Header, resp.StatusCode, start, sessionID)
		p.applyCost(&turn)
		p.insertTurnDetached(turn, "proxy: insert error api_turn")
		return
	}

	turn := p.buildTurn(provider, reqShape, respBody, resp.Header, start, sessionID)
	if turn.Model == "" {
		return
	}
	if isEmptyUsage(turn) && !hasCompressionEvidence(compression) && !chatgptAuth {
		p.logger.Debug("proxy: dropping zero-usage turn", "model", turn.Model)
		return
	}
	applyCompressionMeta(&turn, compression)
	p.applyCost(&turn)
	p.insertTurnDetached(turn, "proxy: insert api_turn")
}

// insertTurnDetached writes turn to the sink on a context derived from
// context.Background() with a 10 s timeout, so a client that closes
// its read side promptly after the stream ends doesn't cancel the SQL
// insert mid-flight. Mirrors the proxy.go shutdownCtx idiom — when
// persistence MUST complete even though the request-scoped context
// has died, the call has to ride a detached context.
//
// The race the detached context closes: serve() finishes teeStream
// once the upstream body is fully consumed, then constructs the
// api_turn and calls the sink. SQLite's BeginTx + write under WAL
// contention takes a handful of ms — long enough for a fast client
// (codex 0.130+ closes its connection the instant the final SSE event
// lands) to win the race and cancel r.Context(). Before this helper,
// every codex turn dropped silently with `proxy: insert api_turn
// (stream): store.InsertAPITurn: context canceled`.
//
// Errors are logged at WARN under the supplied label and never
// propagate — the upstream response is already delivered; the
// api_turn is a best-effort persistence side effect.
func (p *Proxy) insertTurnDetached(turn models.APITurn, label string) {
	insertCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := p.sink.InsertAPITurn(insertCtx, turn); err != nil {
		p.logger.Warn(label, "err", err)
	}
}

// applyCost populates turn.CostUSD using the configured CostComputer.
// No-op when the proxy was constructed without one, when the turn has
// no model, or when the model isn't in the pricing table — in those
// cases the column stays NULL and downstream readers (dashboard
// /api/cost, observer cost) compute it on the fly. The helper exists
// so the four insert sites stay consistent without a wrapping
// `insertTurn` method that would also have to thread compression and
// the audit-B2 drop filter.
func (p *Proxy) applyCost(t *models.APITurn) {
	if p.cost == nil || t.Model == "" {
		return
	}
	if usd, ok := p.cost.Compute(t.Model, CostTokens{
		Input:           t.InputTokens,
		Output:          t.OutputTokens,
		CacheRead:       t.CacheReadTokens,
		CacheCreation:   t.CacheCreationTokens,
		CacheCreation1h: t.CacheCreation1hTokens,
	}); ok {
		t.CostUSD = usd
	}
}

// resolveAPITurnSessionID picks the session_id that lands on the
// api_turn row, with provider-aware extraction.
//
// Anthropic: body (metadata.user_id) → X-Session-Id → SessionResolver.
//
// OpenAI: body (prompt_cache_key) → codex header fan-out → X-Session-Id
// → SessionResolver. The codex header fan-out closes the V4-4 gap:
// codex's ChatGPT-Plus auth path drops prompt_cache_key from the
// request body, so the pre-fix code returned "" and every chatgpt-auth
// turn collapsed to `<unattributed>` in `observer cost --group-by
// session`.
//
// Header names are HYPHEN-SEPARATED canonical (codex 0.130+ emits
// `Session-Id`, `Thread-Id`, `X-Client-Request-Id` — verified via
// 2026-05-28 wire capture against codex 0.133). Go's
// textproto.CanonicalMIMEHeaderKey treats `Session-Id` and
// `Session_Id` as distinct keys (underscore vs hyphen is NOT
// normalized), so an `r.Header.Get("session_id")` against a real
// codex request returns "" — the fallback must use the exact
// hyphenated form. Listed in codex's apparent precedence; first
// non-empty hit wins.
func (p *Proxy) resolveAPITurnSessionID(r *http.Request, provider string, reqBody []byte) string {
	var sid string
	switch provider {
	case models.ProviderAnthropic:
		sid = extractAnthropicSessionID(reqBody)
	case models.ProviderOpenAI:
		sid = extractOpenAISessionID(reqBody)
		if sid == "" {
			for _, h := range []string{"Session-Id", "Thread-Id", "X-Client-Request-Id"} {
				if v := r.Header.Get(h); v != "" {
					sid = v
					break
				}
			}
		}
	}
	if sid == "" {
		sid = r.Header.Get("X-Session-Id")
	}
	if sid == "" && p.sessions != nil {
		if resolved, ok, err := p.sessions.Resolve(r.Context(), r.RemoteAddr); err != nil {
			p.logger.Debug("proxy: session resolve", "addr", r.RemoteAddr, "err", err)
		} else if ok {
			sid = resolved
		}
	}
	return sid
}

func (p *Proxy) upstreamForPath(path, provider string, chatgptAuth bool) *url.URL {
	if isChatGPTBackendPath(path) || chatgptAuth {
		return p.chatgptURL
	}
	if provider == models.ProviderOpenAI {
		return p.openaiURL
	}
	return p.anthropicURL
}

// isEmptyUsage reports whether a parsed turn carries no usable token
// counts at all. Such turns happen when the upstream response stream
// ended before any usage event arrived (cancellation, mid-flight error,
// or an envelope with model + id but no usage block). Recording them
// distorts averages and turn counts without adding analytical value.
// looksLikeSSE returns true when body's first non-whitespace bytes match
// the SSE wire format ("event:" or "data:" line prefix). Used as a
// content-sniffing fallback when the upstream omits the Content-Type
// header — chatgpt.com/backend-api/codex/responses returns SSE with
// empty Content-Type as of codex 0.129.0 / observation 2026-05-08.
//
// Conservative — looks at the first 64 bytes only; mismatched bytes
// fall through to the JSON branch as before. Cross-checked against the
// Anthropic and OpenAI standard SSE preludes (both start with one of
// these two prefixes after any leading whitespace).
func looksLikeSSE(body []byte) bool {
	for i, b := range body {
		if b == ' ' || b == '\r' || b == '\n' || b == '\t' {
			continue
		}
		if i > 64 {
			return false
		}
		rest := body[i:]
		return bytes.HasPrefix(rest, []byte("event:")) ||
			bytes.HasPrefix(rest, []byte("event: ")) ||
			bytes.HasPrefix(rest, []byte("data:")) ||
			bytes.HasPrefix(rest, []byte("data: "))
	}
	return false
}

func isEmptyUsage(t models.APITurn) bool {
	return t.InputTokens == 0 &&
		t.OutputTokens == 0 &&
		t.CacheReadTokens == 0 &&
		t.CacheCreationTokens == 0
}

func hasCompressionEvidence(c CompressionResult) bool {
	if c.Skipped {
		return false
	}
	return c.OriginalBytes > 0 ||
		c.CompressedBytes > 0 ||
		c.CompressedCount > 0 ||
		c.DroppedCount > 0 ||
		c.MarkerCount > 0 ||
		c.MessagePrefixHash != "" ||
		len(c.Events) > 0
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
				BodyHash:        e.BodyHash,
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

func (p *Proxy) serveUpgradePassthrough(w http.ResponseWriter, r *http.Request, upstream *url.URL) {
	target := *upstream
	proxy := httputil.NewSingleHostReverseProxy(&target)
	if p.client != nil {
		proxy.Transport = p.client.Transport
	}
	originalDirector := proxy.Director
	proxy.Director = func(outReq *http.Request) {
		originalDirector(outReq)
		outReq.URL.Path = joinPath(upstream.Path, r.URL.Path)
		outReq.URL.RawQuery = r.URL.RawQuery
		outReq.Host = upstream.Host
		outReq.Header.Del("X-Session-Id")
	}
	proxy.ErrorLog = slog.NewLogLogger(p.logger.Handler(), slog.LevelWarn)
	proxy.ServeHTTP(w, r)
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		headerHasToken(r.Header.Get("Connection"), "upgrade")
}

func headerHasToken(value, token string) bool {
	for _, part := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

func isZstdEncoded(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), "zstd")
}

func decodeZstd(body []byte) ([]byte, error) {
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer decoder.Close()
	return decoder.DecodeAll(body, nil)
}

func encodeZstd(body []byte) ([]byte, error) {
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, err
	}
	defer encoder.Close()
	return encoder.EncodeAll(body, nil), nil
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
