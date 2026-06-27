package ccanalytics

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is Anthropic's API base; overridable via config for testing.
const DefaultBaseURL = "https://api.anthropic.com"

// analyticsPath is the Claude Code Analytics Admin API report endpoint
// (confirmed 2026-06-16, docs/plans/native-console-phase5-research-findings).
const analyticsPath = "/v1/organizations/usage_report/claude_code"

// pageLimit is the max rows/page the endpoint allows (default 20, max 1000).
const pageLimit = 1000

// userAgent is sent per the docs' integration recommendation.
const userAgent = "superbased-observer/1.0 (https://superbased.app)"

// Metric names stored in cc_analytics_daily. Token + cost metrics are summed
// across the per-row model_breakdown into user-day totals (the granularity the
// spend merge + overview need); per-tool accept/reject are kept raw so consumers
// derive acceptance rate (the API does not return a rate field).
const (
	MetricSessions      = "sessions"
	MetricLinesAdded    = "lines_added"
	MetricLinesRemoved  = "lines_removed"
	MetricCommits       = "commits"
	MetricPullRequests  = "pull_requests"
	MetricCostUSD       = "cost_usd" // dollars (cents/100), summed across models
	MetricTokensInput   = "tokens_input"
	MetricTokensOutput  = "tokens_output"
	MetricTokensCacheRd = "tokens_cache_read"
	MetricTokensCacheCr = "tokens_cache_creation"
	// Per-tool metrics are "tool_<tool>_accepted" / "tool_<tool>_rejected".
	toolAcceptedSuffix = "_accepted"
	toolRejectedSuffix = "_rejected"
)

// Actor types as returned by the API.
const (
	ActorUser = "user_actor" // carries email_address — the org-member join key
	ActorAPI  = "api_actor"  // carries api_key_name — service/CI, no org email
)

// DailyMetric is one normalized (day, user, metric) value for
// cc_analytics_daily. UserKey is the email (user_actor) or api_key_name
// (api_actor); ActorType lets the identity resolver skip api_actor rows.
type DailyMetric struct {
	Day       string // YYYY-MM-DD (UTC)
	UserKey   string
	ActorType string
	Metric    string
	Value     float64
}

// Poller fetches the analytics report a day at a time and upserts it. The
// HTTPClient + BaseURL are injectable so tests drive it against an httptest
// server.
type Poller struct {
	DB         *sql.DB
	HTTPClient *http.Client
	BaseURL    string
	APIKey     string
	OrgID      string
	Now        func() time.Time
}

// analyticsResponse is the documented Claude Code Analytics report shape
// (research findings §2). This struct + parseAnalyticsResponse are the ONLY
// place that knows the vendor schema.
type analyticsResponse struct {
	Data     []analyticsRow `json:"data"`
	HasMore  bool           `json:"has_more"`
	NextPage *string        `json:"next_page"`
}

type analyticsRow struct {
	Date  string `json:"date"` // RFC3339 UTC, e.g. "2025-09-01T00:00:00Z"
	Actor struct {
		Type         string `json:"type"`
		EmailAddress string `json:"email_address"`
		APIKeyName   string `json:"api_key_name"`
	} `json:"actor"`
	OrganizationID string `json:"organization_id"`
	CustomerType   string `json:"customer_type"`
	TerminalType   string `json:"terminal_type"`
	CoreMetrics    struct {
		NumSessions int `json:"num_sessions"`
		LinesOfCode struct {
			Added   int `json:"added"`
			Removed int `json:"removed"`
		} `json:"lines_of_code"`
		Commits      int `json:"commits_by_claude_code"`
		PullRequests int `json:"pull_requests_by_claude_code"`
	} `json:"core_metrics"`
	ToolActions map[string]struct {
		Accepted int `json:"accepted"`
		Rejected int `json:"rejected"`
	} `json:"tool_actions"`
	ModelBreakdown []struct {
		Model  string `json:"model"`
		Tokens struct {
			Input         int `json:"input"`
			Output        int `json:"output"`
			CacheRead     int `json:"cache_read"`
			CacheCreation int `json:"cache_creation"`
		} `json:"tokens"`
		EstimatedCost struct {
			Currency string `json:"currency"`
			Amount   int    `json:"amount"` // CENTS USD (1025 = $10.25)
		} `json:"estimated_cost"`
	} `json:"model_breakdown"`
}

// PollDay fetches one UTC day (YYYY-MM-DD), following cursor pagination, and
// upserts every metric. Returns the number of metric rows written.
func (p *Poller) PollDay(ctx context.Context, day string) (int, error) {
	var all []DailyMetric
	page := ""
	for {
		body, err := p.fetchPage(ctx, day, page)
		if err != nil {
			return 0, err
		}
		metrics, hasMore, next, err := parseAnalyticsResponse(body)
		if err != nil {
			return 0, err
		}
		all = append(all, metrics...)
		if !hasMore || next == "" {
			break
		}
		page = next
	}
	return p.upsert(ctx, all)
}

// fetchPage issues one GET for a single day + optional page cursor.
func (p *Poller) fetchPage(ctx context.Context, day, page string) ([]byte, error) {
	base := p.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	q := url.Values{}
	q.Set("starting_at", day) // single UTC day; no window param exists
	q.Set("limit", fmt.Sprintf("%d", pageLimit))
	if page != "" {
		q.Set("page", page)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+analyticsPath+"?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("ccanalytics: new request: %w", err)
	}
	hk, hv := authHeader(p.APIKey)
	req.Header.Set(hk, hv)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("User-Agent", userAgent)

	client := p.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ccanalytics: do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("ccanalytics: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ccanalytics: analytics API returned %d", resp.StatusCode)
	}
	return body, nil
}

// parseAnalyticsResponse maps one page into normalized metrics + the pagination
// signal. Cost is converted from CENTS to dollars here (the 100× trap).
func parseAnalyticsResponse(body []byte) (metrics []DailyMetric, hasMore bool, nextPage string, err error) {
	var ar analyticsResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		return nil, false, "", fmt.Errorf("ccanalytics: parse response: %w", err)
	}
	for _, row := range ar.Data {
		metrics = append(metrics, rowToMetrics(row)...)
	}
	if ar.NextPage != nil {
		nextPage = *ar.NextPage
	}
	return metrics, ar.HasMore, nextPage, nil
}

// rowToMetrics flattens one actor-day row into DailyMetrics. Rows with no usable
// user key are dropped.
func rowToMetrics(row analyticsRow) []DailyMetric {
	userKey := row.Actor.EmailAddress
	if userKey == "" {
		userKey = row.Actor.APIKeyName
	}
	day := utcDay(row.Date)
	if userKey == "" || day == "" {
		return nil
	}
	at := row.Actor.Type
	emit := func(metric string, v float64) DailyMetric {
		return DailyMetric{Day: day, UserKey: userKey, ActorType: at, Metric: metric, Value: v}
	}

	out := []DailyMetric{
		emit(MetricSessions, float64(row.CoreMetrics.NumSessions)),
		emit(MetricLinesAdded, float64(row.CoreMetrics.LinesOfCode.Added)),
		emit(MetricLinesRemoved, float64(row.CoreMetrics.LinesOfCode.Removed)),
		emit(MetricCommits, float64(row.CoreMetrics.Commits)),
		emit(MetricPullRequests, float64(row.CoreMetrics.PullRequests)),
	}
	for tool, ta := range row.ToolActions {
		out = append(
			out,
			emit("tool_"+tool+toolAcceptedSuffix, float64(ta.Accepted)),
			emit("tool_"+tool+toolRejectedSuffix, float64(ta.Rejected)),
		)
	}

	// Sum the per-model breakdown into user-day totals. Cost: cents → dollars.
	var in, outTok, cr, cc int
	var costCents int
	for _, m := range row.ModelBreakdown {
		in += m.Tokens.Input
		outTok += m.Tokens.Output
		cr += m.Tokens.CacheRead
		cc += m.Tokens.CacheCreation
		costCents += m.EstimatedCost.Amount
	}
	out = append(
		out,
		emit(MetricTokensInput, float64(in)),
		emit(MetricTokensOutput, float64(outTok)),
		emit(MetricTokensCacheRd, float64(cr)),
		emit(MetricTokensCacheCr, float64(cc)),
		emit(MetricCostUSD, float64(costCents)/100.0),
	)
	return out
}

// utcDay extracts the YYYY-MM-DD UTC day from the RFC3339 date the API returns;
// falls back to the leading 10 chars if it's already date-only.
func utcDay(rfc3339 string) string {
	if t, err := time.Parse(time.RFC3339, rfc3339); err == nil {
		return t.UTC().Format("2006-01-02")
	}
	if len(rfc3339) >= 10 {
		return rfc3339[:10]
	}
	return ""
}

// upsert writes metrics to cc_analytics_daily idempotently (UNIQUE on
// day+user_key+metric). A re-poll overwrites the value — correct because the
// API restates the running daily total once a day is past the ~1h freshness
// boundary.
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
		return 0, fmt.Errorf("ccanalytics: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var n int
	for _, m := range metrics {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO cc_analytics_daily (day, user_key, actor_type, metric, value, org_id, pulled_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(day, user_key, metric)
			 DO UPDATE SET value = excluded.value, actor_type = excluded.actor_type, pulled_at = excluded.pulled_at`,
			m.Day, m.UserKey, nullIfEmpty(m.ActorType), m.Metric, m.Value, nullIfEmpty(p.OrgID), pulledAt); err != nil {
			return n, fmt.Errorf("ccanalytics: upsert: %w", err)
		}
		n++
	}
	if err := tx.Commit(); err != nil {
		return n, fmt.Errorf("ccanalytics: commit: %w", err)
	}
	return n, nil
}

// ResolveOrgUserID maps an analytics actor email to an org member's user_id
// (B3: case-insensitive email join against org_members.email). It returns
// ok=false for an api_actor (no email) or an unmatched email — the caller
// buckets those as automation / unenrolled rather than dropping them.
func ResolveOrgUserID(ctx context.Context, db *sql.DB, actorType, userKey string) (string, bool) {
	if actorType != ActorUser || userKey == "" {
		return "", false
	}
	var userID string
	err := db.QueryRowContext(ctx,
		`SELECT user_id FROM org_members WHERE lower(email) = lower(?) LIMIT 1`,
		strings.TrimSpace(userKey)).Scan(&userID)
	if err != nil {
		return "", false
	}
	return userID, true
}

// nullIfEmpty returns nil for "" so the column stores NULL.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
