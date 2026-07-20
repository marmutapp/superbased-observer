package m365copilotanalytics

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"time"
)

// userAgent is sent on every request (integration hygiene).
const userAgent = "superbased-observer/1.0 (https://superbased.app)"

// maxBodyBytes caps a single page read defensively.
const maxBodyBytes = 64 << 20

// maxRetries bounds the exponential-backoff retry loop on throttling (429/503).
// getAllEnterpriseInteractions inherits the Teams Export API throttling limits.
const maxRetries = 5

// Poller fetches an M365 Copilot rail and upserts normalized metrics + (Rail A)
// content. Token, HTTPClient and the rail baseURL are injectable so tests drive
// it against an httptest server. The rail strategy is fixed at construction.
type Poller struct {
	DB         *sql.DB
	HTTPClient *http.Client
	Token      TokenProvider // Entra app-only bearer source
	TenantID   string
	OrgID      string
	// UserIDs is the set of Graph user ids (Entra object id or userPrincipalName)
	// Rail A iterates — getAllEnterpriseInteractions is a PER-USER endpoint. The
	// scheduler resolves this from org_members (resolver.go) before each poll.
	UserIDs []string
	// StoreContent, when false, runs Rail A in metadata-only mode: content_hash is
	// stored, the body is NULLed. Default true (the point of Rail A is content).
	StoreContent bool
	// Scrub, when non-nil, scrubs an interaction body before hash+store (defence
	// in depth). Injected so the package stays free of the scrub import (rule #1).
	Scrub func(string) string
	Now   func() time.Time

	spec surfaceSpec
}

// TokenProvider yields a valid Entra app-only bearer token. *TokenSource
// implements it; tests inject a static provider.
type TokenProvider interface {
	Token(ctx context.Context) (string, error)
}

// staticToken is a TokenProvider that returns a fixed token (tests).
type staticToken string

// Token returns the fixed token.
func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }

// StaticToken wraps a literal bearer token as a TokenProvider (tests / a
// pre-fetched token). Exported so callers outside the package can inject one.
func StaticToken(tok string) TokenProvider { return staticToken(tok) }

// NewPoller resolves the rail strategy and returns a poller. A baseURL override
// (tests) replaces the rail's default host. StoreContent defaults on.
func NewPoller(db *sql.DB, surface, baseURLOverride string, tp TokenProvider, tenantID, orgID string) (*Poller, error) {
	spec, err := resolveSurface(surface, baseURLOverride)
	if err != nil {
		return nil, err
	}
	if tp == nil {
		return nil, fmt.Errorf("m365copilotanalytics: nil token provider")
	}
	return &Poller{
		DB: db, Token: tp, TenantID: tenantID, OrgID: orgID,
		StoreContent: true, spec: spec,
	}, nil
}

// Surface reports which rail this poller targets.
func (p *Poller) Surface() Surface { return p.spec.surface }

// PollWindow polls [start, end) for the configured rail, upserting every metric
// and (Rail A) content row. Returns the number of daily-metric rows written.
func (p *Poller) PollWindow(ctx context.Context, start, end time.Time) (int, error) {
	metrics, content, err := p.spec.poll(ctx, p, window{Start: start, End: end})
	if err != nil {
		return 0, err
	}
	if err := p.upsertContent(ctx, content); err != nil {
		return 0, err
	}
	return p.upsert(ctx, metrics)
}

// client returns the configured HTTP client or the default.
func (p *Poller) client() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	return http.DefaultClient
}

// nowFn returns the poller's clock (test-injectable).
func (p *Poller) nowFn() func() time.Time {
	if p.Now != nil {
		return p.Now
	}
	return time.Now
}

// get issues an authenticated Graph GET with exponential-backoff retry on
// throttling (429/503), and returns the body, failing on any other non-200.
func (p *Poller) get(ctx context.Context, rawURL string) ([]byte, error) {
	tok, err := p.Token.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("m365copilotanalytics: token: %w", err)
	}
	backoff := 500 * time.Millisecond
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, fmt.Errorf("m365copilotanalytics: new request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", "application/json")

		resp, err := p.client().Do(req)
		if err != nil {
			return nil, fmt.Errorf("m365copilotanalytics: do request: %w", err)
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
		_ = resp.Body.Close()
		if rerr != nil {
			return nil, fmt.Errorf("m365copilotanalytics: read body: %w", rerr)
		}

		switch resp.StatusCode {
		case http.StatusOK:
			return body, nil
		case http.StatusTooManyRequests, http.StatusServiceUnavailable:
			if attempt >= maxRetries {
				return nil, fmt.Errorf("m365copilotanalytics: %s throttled after %d retries (last %d)", p.spec.surface, maxRetries, resp.StatusCode)
			}
			wait := retryAfter(resp.Header, backoff)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
			backoff *= 2
		default:
			return nil, fmt.Errorf("m365copilotanalytics: %s GET returned %d", p.spec.surface, resp.StatusCode)
		}
	}
}

// retryAfter honours a Retry-After header (seconds) when present, else falls back
// to the caller's exponential backoff.
func retryAfter(h http.Header, fallback time.Duration) time.Duration {
	if v := h.Get("Retry-After"); v != "" {
		if d, err := time.ParseDuration(v + "s"); err == nil && d > 0 {
			return d
		}
	}
	return fallback
}

// paginate walks a Graph @odata.nextLink-paged collection: buildFirstURL builds
// the first page URL, parse maps a page body into (metrics, content, nextLink).
// An empty nextLink ends the walk.
func (p *Poller) paginate(
	ctx context.Context,
	firstURL string,
	parse func(body []byte) (metrics []DailyMetric, content []ContentRow, nextLink string, err error),
) ([]DailyMetric, []ContentRow, error) {
	var allM []DailyMetric
	var allC []ContentRow
	next := firstURL
	for next != "" {
		body, err := p.get(ctx, next)
		if err != nil {
			return nil, nil, err
		}
		metrics, content, nl, err := parse(body)
		if err != nil {
			return nil, nil, err
		}
		allM = append(allM, metrics...)
		allC = append(allC, content...)
		next = nl
	}
	return allM, allC, nil
}

// upsert writes metrics to m365_copilot_analytics_daily idempotently
// (UNIQUE day+user_key+surface+app_class+metric). A re-poll overwrites the value
// — correct because a trailing-window re-poll is the convergence mechanism.
func (p *Poller) upsert(ctx context.Context, metrics []DailyMetric) (int, error) {
	if len(metrics) == 0 {
		return 0, nil
	}
	pulledAt := p.nowFn()().UTC().Format(time.RFC3339)

	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("m365copilotanalytics: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var n int
	for _, m := range metrics {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO m365_copilot_analytics_daily
			   (day, user_key, actor_type, surface, app_class, unit, metric, value, org_id, tenant_id, pulled_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(day, user_key, surface, app_class, metric)
			 DO UPDATE SET value = excluded.value, actor_type = excluded.actor_type,
			               unit = excluded.unit, pulled_at = excluded.pulled_at`,
			m.Day, m.UserKey, nullIfEmpty(m.ActorType), string(m.Surface), m.AppClass,
			nullIfEmpty(string(m.Unit)), m.Metric, m.Value,
			nullIfEmpty(p.OrgID), nullIfEmpty(p.TenantID), pulledAt); err != nil {
			return n, fmt.Errorf("m365copilotanalytics: upsert: %w", err)
		}
		n++
	}
	if err := tx.Commit(); err != nil {
		return n, fmt.Errorf("m365copilotanalytics: commit: %w", err)
	}
	return n, nil
}

// upsertContent writes Rail A content bodies to m365_copilot_content idempotently
// (UNIQUE interaction_id). content_hash is always stored; content is NULLed in
// metadata-only mode (StoreContent=false). The body is scrubbed first when a
// Scrub func is injected.
func (p *Poller) upsertContent(ctx context.Context, rows []ContentRow) error {
	if len(rows) == 0 {
		return nil
	}
	pulledAt := p.nowFn()().UTC().Format(time.RFC3339)

	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("m365copilotanalytics: begin content: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, r := range rows {
		var stored any // NULL in metadata-only mode
		if p.StoreContent && r.Content != "" {
			stored = r.Content
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO m365_copilot_content
			   (interaction_id, session_id, request_id, app_class, interaction_type,
			    user_key, org_id, tenant_id, content, content_hash, created_at, pulled_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(interaction_id)
			 DO UPDATE SET content = excluded.content, content_hash = excluded.content_hash,
			               pulled_at = excluded.pulled_at`,
			r.InteractionID, nullIfEmpty(r.SessionID), nullIfEmpty(r.RequestID), r.AppClass,
			nullIfEmpty(r.InteractionType), r.UserKey, nullIfEmpty(p.OrgID), nullIfEmpty(p.TenantID),
			stored, r.ContentHash, nullIfEmpty(r.CreatedAt), pulledAt); err != nil {
			return fmt.Errorf("m365copilotanalytics: upsert content: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("m365copilotanalytics: commit content: %w", err)
	}
	return nil
}

// scrub applies the injected scrubber (or identity) to a body.
func (p *Poller) scrub(s string) string {
	if p.Scrub != nil {
		return p.Scrub(s)
	}
	return s
}

// nullIfEmpty returns nil for "" so the column stores NULL.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
