package aggregateclient

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/aggregate"
)

// ErrNotConsented is returned by Authorize when the live consent status is
// anything other than ConsentValid. Submit cannot be reached without a Gate,
// so this is the single choke point that turns "not consented" into "no
// egress".
var ErrNotConsented = errors.New("aggregateclient: rail not enabled or consent invalid — no submission")

// Gate is the proof-of-consent token Submit requires. It has no exported
// fields and no other constructor than Authorize, so a caller cannot fabricate
// one: it is structurally impossible to Submit without a valid consent state
// (design §6.3).
type Gate struct{ authorized bool }

// Authorize mints a Gate iff status is ConsentValid; every other status
// (disabled, missing, revoked, schema/endpoint/registry-changed) yields
// ErrNotConsented and no Gate. Callers compute status via
// aggregate.CheckConsent over the live config + the stored receipt.
func Authorize(status aggregate.ConsentStatus) (Gate, error) {
	if status != aggregate.ConsentValid {
		return Gate{}, fmt.Errorf("%w (status=%s)", ErrNotConsented, status)
	}
	return Gate{authorized: true}, nil
}

// defaultTimeout bounds a submission attempt end to end.
const defaultTimeout = 15 * time.Second

// maxResponseBytes caps how much of the collector's response we read — the
// collector returns 204 with an empty body; anything large is discarded.
const maxResponseBytes = 8 << 10

// Client is the hardened HTTPS POST transport for one collector endpoint.
type Client struct {
	httpClient *http.Client
	endpoint   string
	host       string
}

// New builds a Client for endpoint, validating that it is an absolute HTTPS URL
// with a host and no credentials/query (defense in depth — config.Validate
// already enforced host approval). The underlying http.Client refuses
// redirects, uses a short timeout, and carries no cookie jar.
func New(endpoint string) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return nil, fmt.Errorf("aggregateclient.New: parse endpoint: %w", err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("aggregateclient.New: endpoint must be https, got %q", u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, errors.New("aggregateclient.New: endpoint has no host")
	}
	if u.User != nil || u.RawQuery != "" {
		return nil, errors.New("aggregateclient.New: endpoint must not carry credentials or a query string")
	}
	return &Client{
		httpClient: &http.Client{
			Timeout: defaultTimeout,
			// Refuse to follow redirects: return the redirect response
			// unfollowed so a 3xx becomes a submission failure rather than a
			// silent hop to an unpinned host (finding #22).
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
			// No cookie jar (nil): no cookies are ever sent or stored.
		},
		endpoint: endpoint,
		host:     strings.ToLower(u.Hostname()),
	}, nil
}

// Endpoint returns the configured collector URL (for logging/status).
func (c *Client) Endpoint() string { return c.endpoint }

// SetHTTPClientForTest swaps the underlying http.Client, preserving the
// hardened redirect policy. It exists ONLY so the invariant request-shape test
// can trust an httptest TLS server's self-signed cert; production code never
// calls it.
func (c *Client) SetHTTPClientForTest(hc *http.Client) {
	if hc.CheckRedirect == nil {
		hc.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
	if hc.Timeout == 0 {
		hc.Timeout = defaultTimeout
	}
	c.httpClient = hc
}

// Submit gzips and POSTs sub to the collector. It REQUIRES a Gate (minted only
// by Authorize on ConsentValid), so it cannot be called without a valid
// consent state. On success (2xx) it returns nil; a redirect, a non-2xx
// status, or a transport error is returned as an error for the caller to
// record as a failed attempt.
func (c *Client) Submit(ctx context.Context, gate Gate, sub aggregate.Submission) error {
	if !gate.authorized {
		// Unreachable via Authorize, but a zero-value Gate must never send.
		return ErrNotConsented
	}
	body, err := json.Marshal(sub)
	if err != nil {
		return fmt.Errorf("aggregateclient.Submit: marshal: %w", err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(body); err != nil {
		return fmt.Errorf("aggregateclient.Submit: gzip: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("aggregateclient.Submit: gzip close: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, &gz)
	if err != nil {
		return fmt.Errorf("aggregateclient.Submit: new request: %w", err)
	}
	// Only the minimal, non-identifying headers the collector needs.
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("aggregateclient.Submit: post: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))

	if resp.StatusCode >= 300 {
		return fmt.Errorf("aggregateclient.Submit: collector returned status %d", resp.StatusCode)
	}
	return nil
}
