package proxy

import (
	"bytes"
	"io"
	"math/rand"
	"net/http"
	"time"
)

// Reliability primitives (§R12; gateway parity for proxy-routed
// traffic). Two mechanisms, both running BEFORE the first byte is
// written to the client — the never-retry-after-streamed-bytes rule
// (§R12.2) is structural, not a check:
//
//   - same-target retries (§R12.2): bounded, jittered backoff for the
//     idempotent failure classes — 529 overloaded, 503 with a
//     Retry-After, and transport-level resets (which doWithRetry
//     already retries once; the budget extends it). Per-class config
//     toggles.
//   - fallback chains (§R12.1): on 429 / 5xx / timeout after retries
//     are exhausted, the turn re-forwards against the next same-shape
//     chain entry (the router's verdict supplies the chain — enforce
//     mode only; the adapter returns no chain in advise mode). The
//     served model lands on api_turns; RecordServed lets the router
//     annotate the decision row with availability_fallback.
//
// Circuit breakers are NOT here: passive health lives in the
// store-side snapshot refresher (one owner), feeding the availability
// basis which excludes open-breaker candidates BEFORE selection.

// ReliabilityOptions configures §R12.2 same-target retries. The zero
// value disables everything new (the pre-existing single transport
// retry in doWithRetry is unchanged).
type ReliabilityOptions struct {
	// MaxRetries bounds same-target retries across all classes.
	MaxRetries int
	// Per-class toggles (§R12.2).
	RetryOverloaded      bool // 529
	RetryUnavailable     bool // 503 with Retry-After
	RetryConnectionReset bool // transport resets beyond doWithRetry's one
}

// forwardReliable forwards the request with §R12 semantics. outReq is
// the prepared first attempt; retries and fallbacks rebuild it.
// plainBody is the decoded JSON payload (for fallback model splices);
// reqBody the wire payload. On a successful fallback the shape's Model
// is updated so the turn row records the SERVED model (§R11.1).
func (p *Proxy) forwardReliable(r *http.Request, outReq *http.Request, reqBody, plainBody []byte, chain []string, provider string, shape *requestShape) (*http.Response, error) {
	resp, err := p.doWithRetry(outReq, reqBody)

	// Same-target retries (§R12.2).
	for attempt := 1; attempt <= p.reliability.MaxRetries; attempt++ {
		retryable, wait := p.retrySameTargetClass(resp, err)
		if !retryable {
			break
		}
		drainResponse(resp)
		if !sleepCtx(r.Context(), backoffDelay(attempt, wait)) {
			break
		}
		p.logger.Info("proxy: same-target retry", "attempt", attempt, "model", shape.Model)
		retryReq, rerr := rebuildRequest(r, outReq, reqBody)
		if rerr != nil {
			break
		}
		resp, err = p.doWithRetry(retryReq, reqBody)
	}

	// Key-pool rotation (§R12.4): a rate-limited turn retries with the
	// provider's next configured key BEFORE falling back to another
	// model — same model, fresh quota. Bounded by pool size; the
	// credential-form guard keeps OAuth/JWT requests untouched.
	if pool, hasPool := p.keyPools[provider]; hasPool && err == nil && resp.StatusCode == http.StatusTooManyRequests {
		for i := 1; i < len(pool.keys); i++ {
			rotReq, rerr := rebuildRequest(r, outReq, reqBody)
			if rerr != nil || !p.rotateAuth(rotReq, provider) {
				break
			}
			drainResponse(resp)
			p.logger.Info("proxy: key-pool rotation on 429", "provider", provider, "attempt", i)
			resp, err = p.doWithRetry(rotReq, reqBody)
			if err != nil || resp.StatusCode != http.StatusTooManyRequests {
				break
			}
		}
	}

	// Fallback chain (§R12.1) on 429 / 5xx / timeout.
	if len(chain) > 0 && isFallbackTrigger(resp, err) {
		for _, fb := range chain {
			if fb == "" || fb == shape.Model {
				continue
			}
			mutated, ok := rewriteTopLevelModel(plainBody, fb)
			if !ok {
				break // unsplicable body; keep the original outcome
			}
			fbBody := mutated
			if isZstdEncoded(r.Header.Get("Content-Encoding")) {
				encoded, eerr := encodeZstd(mutated)
				if eerr != nil {
					break
				}
				fbBody = encoded
			}
			drainResponse(resp)
			p.logger.Info("proxy: availability fallback", "from", shape.Model, "to", fb)
			fbReq, rerr := rebuildRequest(r, outReq, fbBody)
			if rerr != nil {
				break
			}
			fbResp, fbErr := p.doWithRetry(fbReq, fbBody)
			resp, err = fbResp, fbErr
			if fbErr == nil && fbResp.StatusCode < 400 {
				shape.Model = fb // the model that actually served (§R11.1)
				return resp, nil
			}
			if !isFallbackTrigger(resp, err) {
				break // a non-retryable failure class (4xx): surface it
			}
		}
	}
	return resp, err
}

// retrySameTargetClass reports whether the outcome is a configured
// idempotent retry class, plus any upstream-suggested wait.
func (p *Proxy) retrySameTargetClass(resp *http.Response, err error) (bool, time.Duration) {
	if err != nil {
		return p.reliability.RetryConnectionReset && isRetryableTransportError(err), 0
	}
	switch resp.StatusCode {
	case 529:
		return p.reliability.RetryOverloaded, 0
	case http.StatusServiceUnavailable:
		if !p.reliability.RetryUnavailable {
			return false, 0
		}
		// §R12.2: 503 retries only WITH a Retry-After (the upstream
		// explicitly said "try again"); a bare 503 surfaces.
		ra := resp.Header.Get("Retry-After")
		if ra == "" {
			return false, 0
		}
		if d, perr := time.ParseDuration(ra + "s"); perr == nil {
			return true, d
		}
		return true, 0
	default:
		return false, 0
	}
}

// isFallbackTrigger matches the §R12.1 chain triggers: rate limits,
// upstream failure classes, and transport-level outcomes (timeouts).
func isFallbackTrigger(resp *http.Response, err error) bool {
	if err != nil {
		return true
	}
	return resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
}

// backoffDelay is the §R12.2 jittered backoff: 200ms·2^(n−1) plus up
// to 100ms jitter, honoring (and capping) an upstream Retry-After.
// Capped at 2s — the proxy is holding the client's connection open.
func backoffDelay(attempt int, upstreamWait time.Duration) time.Duration {
	d := 200 * time.Millisecond << (attempt - 1)
	if upstreamWait > d {
		d = upstreamWait
	}
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	return d + time.Duration(rand.Int63n(int64(100*time.Millisecond))) //nolint:gosec // jitter, not crypto
}

// sleepCtx sleeps unless the client gave up first.
func sleepCtx(ctx interface{ Done() <-chan struct{} }, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// rebuildRequest clones the outbound request with a fresh body reader
// (the previous attempt consumed it).
func rebuildRequest(r *http.Request, outReq *http.Request, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(r.Context(), outReq.Method, outReq.URL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = outReq.Header.Clone()
	req.Host = outReq.Host
	req.ContentLength = int64(len(body))
	return req, nil
}

// drainResponse releases a response we are abandoning so its
// connection returns to the pool.
func drainResponse(resp *http.Response) {
	if resp == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}
