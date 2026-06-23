package codexanalytics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"
)

// chatgptUsagePath is the ChatGPT-Enterprise Codex usage analytics endpoint
// (Phase-0 findings §3). Workspace-scoped; %s is the workspace id.
const chatgptUsagePath = "/v1/analytics/codex/workspaces/%s/usage"

// chatgptPageLimit is the max rows/page requested.
const chatgptPageLimit = 1000

// pollChatGPTEnterprise polls the single /usage endpoint with cursor pagination.
// `group` is omitted so rows are PER-USER (group=workspace would aggregate).
func pollChatGPTEnterprise(ctx context.Context, p *Poller, win window) ([]DailyMetric, error) {
	base := p.spec.baseURL + fmt.Sprintf(chatgptUsagePath, url.PathEscape(p.WorkspaceID))
	buildURL := func(page string) string {
		q := url.Values{}
		// RESIDUAL (findings Q-C2): sources disagree on ISO-8601 vs Unix seconds
		// for start_time/end_time. We send RFC3339 UTC; confirm against a live
		// payload and switch to Unix here if rejected.
		q.Set("start_time", win.Start.UTC().Format(time.RFC3339))
		q.Set("end_time", win.End.UTC().Format(time.RFC3339))
		q.Set("group_by", "day")
		q.Set("limit", fmt.Sprintf("%d", chatgptPageLimit))
		if page != "" {
			q.Set("page", page)
		}
		return base + "?" + q.Encode()
	}
	return p.paginate(ctx, buildURL, parseChatGPTUsage)
}

// chatgptUsageResponse is the ChatGPT-Enterprise usage shape. Aligned to the
// richer reconstructed sample (docs/plans/codex-analytics-sample-response.json):
// rows carry `bucket_start` + an `actor` object (`user_email` nullable when the
// workspace withholds it, + `user_id`), and TOKENS live per-model under
// `by_model[].tokens` (text_input/cached_input/output), summed into the user-day
// total (the grain the spend merge needs). The legacy flat field names
// (`start_time`, `user.email/id`, a top-level `tokens`) are kept as a FALLBACK so
// either provisional shape parses. STILL provisional — both shapes await a true
// live capture (the sample self-labels illustrative); the capture script locks it.
// This struct + parseChatGPTUsage are the ONLY place that knows this surface's schema.
type chatgptUsageResponse struct {
	Data     []chatgptUsageRow `json:"data"`
	HasMore  bool              `json:"has_more"`
	NextPage *string           `json:"next_page"`
}

// chatgptTokenBucket is the per-bucket token split (per-model under by_model, or
// the legacy top-level `tokens`).
type chatgptTokenBucket struct {
	TextInput   int `json:"text_input"`
	CachedInput int `json:"cached_input"`
	Output      int `json:"output"`
}

type chatgptUsageRow struct {
	// Day bucket: sample uses bucket_start; legacy used start_time.
	BucketStart string `json:"bucket_start"`
	StartTime   string `json:"start_time"`
	// Identity: sample uses actor.{user_email,user_id} (email nullable);
	// legacy used user.{email,id}.
	Actor struct {
		UserEmail *string `json:"user_email"`
		UserID    string  `json:"user_id"`
	} `json:"actor"`
	User struct {
		Email string `json:"email"`
		ID    string `json:"id"`
	} `json:"user"`
	Credits float64 `json:"credits"`
	Threads int     `json:"threads"`
	Turns   int     `json:"turns"`
	// Tokens: sample nests them per-model; legacy had a single top-level bucket.
	ByModel []struct {
		Model  string             `json:"model"`
		Tokens chatgptTokenBucket `json:"tokens"`
	} `json:"by_model"`
	Tokens chatgptTokenBucket `json:"tokens"`
}

// parseChatGPTUsage flattens one page into normalized metrics + the pagination
// signal. Cost stays in CREDITS (unit-tagged); the spendCTE merge converts.
func parseChatGPTUsage(body []byte) ([]DailyMetric, bool, string, error) {
	var r chatgptUsageResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, false, "", fmt.Errorf("codexanalytics: parse chatgpt usage: %w", err)
	}
	var out []DailyMetric
	for _, row := range r.Data {
		out = append(out, chatgptRowMetrics(row)...)
	}
	next := ""
	if r.NextPage != nil {
		next = *r.NextPage
	}
	return out, r.HasMore, next, nil
}

func chatgptRowMetrics(row chatgptUsageRow) []DailyMetric {
	// Identity: actor.user_email (where the workspace permits) → user.email →
	// the user_id fallback (a non-email id stays unenrolled at the merge).
	userKey := ""
	if row.Actor.UserEmail != nil {
		userKey = *row.Actor.UserEmail
	}
	if userKey == "" {
		userKey = row.User.Email
	}
	if userKey == "" {
		userKey = firstNonEmpty(row.Actor.UserID, row.User.ID)
	}
	day := utcDayFromTimestamp(firstNonEmpty(row.BucketStart, row.StartTime))
	if userKey == "" || day == "" {
		return nil
	}

	// Tokens: sum the per-model breakdown (the user-day grain); fall back to a
	// legacy top-level bucket when by_model is absent.
	tok := row.Tokens
	if len(row.ByModel) > 0 {
		tok = chatgptTokenBucket{}
		for _, m := range row.ByModel {
			tok.TextInput += m.Tokens.TextInput
			tok.CachedInput += m.Tokens.CachedInput
			tok.Output += m.Tokens.Output
		}
	}

	const s = SurfaceChatGPTEnterprise
	return []DailyMetric{
		emitMetric(day, userKey, ActorUser, s, UnitCredits, MetricCost, row.Credits),
		emitMetric(day, userKey, ActorUser, s, UnitCount, MetricThreads, float64(row.Threads)),
		emitMetric(day, userKey, ActorUser, s, UnitCount, MetricTurns, float64(row.Turns)),
		emitMetric(day, userKey, ActorUser, s, UnitTokens, MetricTokensInput, float64(tok.TextInput)),
		emitMetric(day, userKey, ActorUser, s, UnitTokens, MetricTokensCached, float64(tok.CachedInput)),
		emitMetric(day, userKey, ActorUser, s, UnitTokens, MetricTokensOutput, float64(tok.Output)),
	}
}

// firstNonEmpty returns a if non-empty, else b.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// utcDayFromTimestamp extracts YYYY-MM-DD (UTC) from an RFC3339 timestamp,
// falling back to the leading 10 chars if it is already date-only.
func utcDayFromTimestamp(ts string) string {
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return dayOf(t)
	}
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ""
}
