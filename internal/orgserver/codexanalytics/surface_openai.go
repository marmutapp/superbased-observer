package codexanalytics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// OpenAI org Usage + Cost API paths (Phase-0 rail-c findings; the page/bucket/
// results envelope is confirmed from live cookbook payloads).
const (
	openAIUsagePath = "/v1/organization/usage/completions"
	openAICostsPath = "/v1/organization/costs"
	openAIPageLimit = 180 // usage/cost endpoints page small; cursor follows next_page
)

// orgAggregateKey is the user_key for rows whose user_id is null (not grouped /
// not attributable) — kept distinct from any real user and tagged ActorWorkspace
// so it never joins to org_members and never inflates per-developer rollups.
const orgAggregateKey = "__org_aggregate__"

// pollOpenAIOrg polls the two org endpoints — usage (tokens, per user_id) and
// costs (dollars) — each cursor-paginated over the shared helper, and returns
// the union. Codex is isolated from other OpenAI-API usage by model filtering
// (RESIDUAL Q: confirm the gpt-*-codex model-name patterns against a live org).
func pollOpenAIOrg(ctx context.Context, p *Poller, win window) ([]DailyMetric, error) {
	usage, err := p.paginate(ctx, p.openAIURLBuilder(openAIUsagePath, win, "user_id"), parseOpenAIUsage)
	if err != nil {
		return nil, err
	}
	costs, err := p.paginate(ctx, p.openAIURLBuilder(openAICostsPath, win, "user_id"), parseOpenAICosts)
	if err != nil {
		return nil, err
	}
	return append(usage, costs...), nil
}

// openAIURLBuilder returns a page-cursor URL builder for an org endpoint over the
// window. start_time/end_time are Unix seconds (confirmed); bucket_width=1d.
func (p *Poller) openAIURLBuilder(path string, win window, groupBy string) func(page string) string {
	base := p.spec.baseURL + path
	return func(page string) string {
		q := url.Values{}
		q.Set("start_time", strconv.FormatInt(win.Start.UTC().Unix(), 10))
		q.Set("end_time", strconv.FormatInt(win.End.UTC().Unix(), 10))
		q.Set("bucket_width", "1d")
		// RESIDUAL Q: exact array-param encoding (group_by[] vs repeated). We
		// repeat the key; confirm against a live org and adjust if grouping keys
		// come back null.
		q.Add("group_by", groupBy)
		q.Set("limit", fmt.Sprintf("%d", openAIPageLimit))
		if page != "" {
			q.Set("page", page)
		}
		return base + "?" + q.Encode()
	}
}

// openAIUsagePage is the /usage/completions envelope (object:page → buckets →
// results). This struct + parseOpenAIUsage are the ONLY place that knows the
// usage schema.
type openAIUsagePage struct {
	Data []struct {
		StartTime int64               `json:"start_time"` // Unix seconds
		Results   []openAIUsageResult `json:"results"`
	} `json:"data"`
	HasMore  bool    `json:"has_more"`
	NextPage *string `json:"next_page"`
}

type openAIUsageResult struct {
	InputTokens       int    `json:"input_tokens"`
	OutputTokens      int    `json:"output_tokens"`
	InputCachedTokens int    `json:"input_cached_tokens"`
	NumModelRequests  int    `json:"num_model_requests"`
	UserID            string `json:"user_id"` // populated only when group_by=user_id
}

func parseOpenAIUsage(body []byte) ([]DailyMetric, bool, string, error) {
	var pg openAIUsagePage
	if err := json.Unmarshal(body, &pg); err != nil {
		return nil, false, "", fmt.Errorf("codexanalytics: parse openai usage: %w", err)
	}
	var out []DailyMetric
	for _, b := range pg.Data {
		day := dayOf(time.Unix(b.StartTime, 0))
		for _, r := range b.Results {
			userKey, actor := openAIActor(r.UserID)
			out = append(
				out,
				emitMetric(day, userKey, actor, SurfaceOpenAIOrg, UnitTokens, MetricTokensInput, float64(r.InputTokens)),
				emitMetric(day, userKey, actor, SurfaceOpenAIOrg, UnitTokens, MetricTokensOutput, float64(r.OutputTokens)),
				emitMetric(day, userKey, actor, SurfaceOpenAIOrg, UnitTokens, MetricTokensCached, float64(r.InputCachedTokens)),
				emitMetric(day, userKey, actor, SurfaceOpenAIOrg, UnitCount, MetricModelRequests, float64(r.NumModelRequests)),
			)
		}
	}
	return out, pg.HasMore, derefStr(pg.NextPage), nil
}

// openAICostPage is the /costs envelope. Cost UNIT is DOLLARS (amount.value) —
// not cents (CC), not credits (ChatGPT surface). Do NOT divide by 100.
type openAICostPage struct {
	Data []struct {
		StartTime int64              `json:"start_time"`
		Results   []openAICostResult `json:"results"`
	} `json:"data"`
	HasMore  bool    `json:"has_more"`
	NextPage *string `json:"next_page"`
}

type openAICostResult struct {
	Amount struct {
		Value    float64 `json:"value"`
		Currency string  `json:"currency"`
	} `json:"amount"`
	UserID string `json:"user_id"`
}

func parseOpenAICosts(body []byte) ([]DailyMetric, bool, string, error) {
	var pg openAICostPage
	if err := json.Unmarshal(body, &pg); err != nil {
		return nil, false, "", fmt.Errorf("codexanalytics: parse openai costs: %w", err)
	}
	var out []DailyMetric
	for _, b := range pg.Data {
		day := dayOf(time.Unix(b.StartTime, 0))
		for _, r := range b.Results {
			userKey, actor := openAIActor(r.UserID)
			out = append(out, emitMetric(day, userKey, actor, SurfaceOpenAIOrg, UnitUSD, MetricCost, r.Amount.Value))
		}
	}
	return out, pg.HasMore, derefStr(pg.NextPage), nil
}

// openAIActor maps a (possibly null) user_id to (user_key, actor_type). A null
// user_id is an org/project-aggregate row — bucketed under orgAggregateKey +
// ActorWorkspace so it never joins to a developer.
func openAIActor(userID string) (string, string) {
	if userID == "" {
		return orgAggregateKey, ActorWorkspace
	}
	return userID, ActorUser
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
