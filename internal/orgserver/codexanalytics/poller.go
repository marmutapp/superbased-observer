package codexanalytics

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"time"
)

// userAgent is sent on every analytics request (integration hygiene; neither
// surface requires a vendor version header).
const userAgent = "superbased-observer/1.0 (https://superbased.app)"

// maxBodyBytes caps a single page read defensively (daily volume is tiny).
const maxBodyBytes = 64 << 20

// Poller fetches a Codex analytics surface and upserts normalized metrics. The
// HTTPClient + the surface baseURL are injectable so tests drive it against an
// httptest server. The surface strategy is fixed at construction.
type Poller struct {
	DB          *sql.DB
	HTTPClient  *http.Client
	APIKey      string
	WorkspaceID string // required for SurfaceChatGPTEnterprise
	OrgID       string
	Now         func() time.Time

	spec surfaceSpec
}

// NewPoller resolves the surface strategy and returns a poller. A baseURL
// override (tests) replaces the surface's default host.
func NewPoller(db *sql.DB, surface, baseURLOverride, apiKey, workspaceID, orgID string) (*Poller, error) {
	spec, err := resolveSurface(surface, baseURLOverride)
	if err != nil {
		return nil, err
	}
	if spec.surface == SurfaceChatGPTEnterprise && workspaceID == "" {
		return nil, fmt.Errorf("codexanalytics: surface %s requires workspace_id", SurfaceChatGPTEnterprise)
	}
	return &Poller{
		DB: db, APIKey: apiKey, WorkspaceID: workspaceID, OrgID: orgID, spec: spec,
	}, nil
}

// Surface reports which surface this poller targets.
func (p *Poller) Surface() Surface { return p.spec.surface }

// PollWindow polls [start, end) for the configured surface and upserts every
// metric. Returns the number of rows written.
func (p *Poller) PollWindow(ctx context.Context, start, end time.Time) (int, error) {
	metrics, err := p.spec.poll(ctx, p, window{Start: start, End: end})
	if err != nil {
		return 0, err
	}
	return p.upsert(ctx, metrics)
}

// get issues an authenticated GET and returns the body, failing on non-200.
func (p *Poller) get(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := newGet(ctx, rawURL, p.APIKey)
	if err != nil {
		return nil, err
	}
	client := p.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codexanalytics: do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("codexanalytics: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codexanalytics: %s returned %d", p.spec.surface, resp.StatusCode)
	}
	return body, nil
}

// paginate walks a cursor-paginated endpoint: buildURL(page) builds the request
// URL for a page cursor, parse maps a page body into metrics + the next cursor.
// Both surfaces use the same {has_more, next_page} pagination family, so this is
// shared; only buildURL + parse are surface-specific.
func (p *Poller) paginate(
	ctx context.Context,
	buildURL func(page string) string,
	parse func(body []byte) (metrics []DailyMetric, hasMore bool, nextPage string, err error),
) ([]DailyMetric, error) {
	var all []DailyMetric
	page := ""
	for {
		body, err := p.get(ctx, buildURL(page))
		if err != nil {
			return nil, err
		}
		metrics, hasMore, next, err := parse(body)
		if err != nil {
			return nil, err
		}
		all = append(all, metrics...)
		if !hasMore || next == "" {
			break
		}
		page = next
	}
	return all, nil
}

// upsert writes metrics to codex_analytics_daily idempotently
// (UNIQUE day+user_key+surface+metric). A re-poll overwrites the value — correct
// because the APIs restate a day's running total once it is past their freshness
// boundary, and a trailing-window re-poll is the convergence mechanism.
func (p *Poller) upsert(ctx context.Context, metrics []DailyMetric) (int, error) {
	if len(metrics) == 0 {
		return 0, nil
	}
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	pulledAt := now().UTC().Format(time.RFC3339)

	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("codexanalytics: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var n int
	for _, m := range metrics {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO codex_analytics_daily
			   (day, user_key, actor_type, surface, unit, metric, value, org_id, workspace_id, pulled_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(day, user_key, surface, metric)
			 DO UPDATE SET value = excluded.value, actor_type = excluded.actor_type,
			               unit = excluded.unit, pulled_at = excluded.pulled_at`,
			m.Day, m.UserKey, nullIfEmpty(m.ActorType), string(m.Surface), nullIfEmpty(string(m.Unit)),
			m.Metric, m.Value, nullIfEmpty(p.OrgID), nullIfEmpty(p.WorkspaceID), pulledAt); err != nil {
			return n, fmt.Errorf("codexanalytics: upsert: %w", err)
		}
		n++
	}
	if err := tx.Commit(); err != nil {
		return n, fmt.Errorf("codexanalytics: commit: %w", err)
	}
	return n, nil
}

// nullIfEmpty returns nil for "" so the column stores NULL.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
