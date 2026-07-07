package copilotanalytics

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// userAgent is sent on every request (GitHub requires a User-Agent).
const userAgent = "superbased-observer/1.0 (https://superbased.app)"

// maxBodyBytes caps a single page/report read defensively (daily volume is tiny).
const maxBodyBytes = 64 << 20

// OwnerType selects the GitHub scope: an organization or an enterprise. It picks
// the path prefix (/orgs/{owner} vs /enterprises/{owner}); the enhanced-billing
// surface always uses /organizations/{owner}.
type OwnerType string

const (
	OwnerOrg        OwnerType = "org"
	OwnerEnterprise OwnerType = "enterprise"
)

// Poller fetches a Copilot analytics surface and upserts normalized metrics. The
// HTTPClient + BaseURL are injectable so tests drive it against an httptest
// server. The surface strategy is fixed at construction.
type Poller struct {
	DB         *sql.DB
	HTTPClient *http.Client
	BaseURL    string // default https://api.github.com; test override
	APIKey     string
	Owner      string    // org login or enterprise slug
	OwnerType  OwnerType // OwnerOrg | OwnerEnterprise
	Report     string    // engagement report name (default users-1-day)
	OrgID      string    // observer org id (stamped on rows)
	Now        func() time.Time

	spec surfaceSpec
}

// defaultBaseURL is GitHub's REST host.
const defaultBaseURL = "https://api.github.com"

// defaultReport is the per-user 1-day engagement report.
const defaultReport = "users-1-day"

// NewPoller resolves the surface strategy and returns a poller. An empty BaseURL
// defaults to GitHub's host; an empty OwnerType defaults to org.
func NewPoller(db *sql.DB, surface, baseURL, apiKey, owner string, ownerType OwnerType, report, orgID string) (*Poller, error) {
	spec, err := resolveSurface(surface)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(owner) == "" {
		return nil, fmt.Errorf("copilotanalytics: surface %s requires an owner (org or enterprise)", spec.surface)
	}
	if ownerType == "" {
		ownerType = OwnerOrg
	}
	if ownerType != OwnerOrg && ownerType != OwnerEnterprise {
		return nil, fmt.Errorf("copilotanalytics: bad owner_type %q (want org|enterprise)", ownerType)
	}
	// The enhanced-billing surface is org-scoped only (/organizations/{org}); the
	// per-seat billing summary likewise has no enterprise variant.
	if ownerType == OwnerEnterprise && spec.surface == SurfaceBilling {
		return nil, fmt.Errorf("copilotanalytics: surface %s is org-scoped only (no enterprise billing endpoint)", SurfaceBilling)
	}
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = defaultBaseURL
	}
	rep := strings.TrimSpace(report)
	if rep == "" {
		rep = defaultReport
	}
	return &Poller{
		DB: db, BaseURL: base, APIKey: apiKey, Owner: owner, OwnerType: ownerType,
		Report: rep, OrgID: orgID, spec: spec,
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

// ownerPrefix returns the path prefix for the owner scope: /orgs/{owner} or
// /enterprises/{owner}.
func (p *Poller) ownerPrefix() string {
	seg := "orgs"
	if p.OwnerType == OwnerEnterprise {
		seg = "enterprises"
	}
	return "/" + seg + "/" + url.PathEscape(p.Owner)
}

// billingPrefix returns /organizations/{owner} — the enhanced-billing scope (note
// the full word "organizations", distinct from "orgs").
func (p *Poller) billingPrefix() string {
	return "/organizations/" + url.PathEscape(p.Owner)
}

// client returns the configured HTTP client or the default.
func (p *Poller) client() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	return http.DefaultClient
}

// get issues an AUTHENTICATED GitHub GET (Bearer + GitHub headers) and returns
// the body + response headers, failing on non-200.
func (p *Poller) get(ctx context.Context, rawURL string) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("copilotanalytics: new request: %w", err)
	}
	ghHeaders(req, p.APIKey)
	resp, err := p.client().Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("copilotanalytics: do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, nil, fmt.Errorf("copilotanalytics: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("copilotanalytics: %s GET %s returned %d", p.spec.surface, rawURL, resp.StatusCode)
	}
	return body, resp.Header, nil
}

// getRaw fetches a signed download URL with NO Authorization header and NO GitHub
// headers. The engagement report's download_links point at object storage
// (objects.githubusercontent.com) and are pre-authenticated by the signature —
// attaching our admin token would LEAK it to a different host. Only a User-Agent
// is sent. This is a deliberate security boundary, not an oversight.
func (p *Poller) getRaw(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("copilotanalytics: new raw request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := p.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("copilotanalytics: do raw request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("copilotanalytics: read raw body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("copilotanalytics: download link returned %d", resp.StatusCode)
	}
	return body, nil
}

// upsert writes metrics to copilot_analytics_daily idempotently
// (UNIQUE day+user_key+surface+metric). A re-poll overwrites the value — correct
// because the metrics reports restate a day until ~2 days past, and a
// trailing-window re-poll is the convergence mechanism.
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
		return 0, fmt.Errorf("copilotanalytics: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var n int
	for _, m := range metrics {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO copilot_analytics_daily
			   (day, user_key, actor_type, surface, unit, metric, value, org_id, owner, pulled_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(day, user_key, surface, metric)
			 DO UPDATE SET value = excluded.value, actor_type = excluded.actor_type,
			               unit = excluded.unit, pulled_at = excluded.pulled_at`,
			m.Day, m.UserKey, nullIfEmpty(m.ActorType), string(m.Surface), nullIfEmpty(string(m.Unit)),
			m.Metric, m.Value, nullIfEmpty(p.OrgID), nullIfEmpty(p.Owner), pulledAt); err != nil {
			return n, fmt.Errorf("copilotanalytics: upsert: %w", err)
		}
		n++
	}
	if err := tx.Commit(); err != nil {
		return n, fmt.Errorf("copilotanalytics: commit: %w", err)
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
