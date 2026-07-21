package m365copilotanalytics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// graphInteractionsPath is the per-user Graph aiInteractionHistory endpoint
// (v1.0, GA). %s is the Graph user id (Entra object id or userPrincipalName).
// Application permission AiEnterpriseInteraction.Read.All (app-only, admin
// consent). GLOBAL COMMERCIAL CLOUD ONLY.
const graphInteractionsPath = "/v1.0/copilot/users/%s/interactionHistory/getAllEnterpriseInteractions"

// graphPageSize is $top per page. getAllEnterpriseInteractions inherits the Teams
// Export API limit of 100.
const graphPageSize = 100

// pollGraph (Rail A) iterates the configured user set, calling
// getAllEnterpriseInteractions per user over the window and flattening the
// aiInteraction stream into per-(day, user, appClass) metrics + content rows.
// A user with no interactions contributes nothing. Delta is unsupported, so each
// poll re-reads the trailing window and the upserts converge.
func pollGraph(ctx context.Context, p *Poller, win window) ([]DailyMetric, []ContentRow, error) {
	var allM []DailyMetric
	var allC []ContentRow
	for _, uid := range p.UserIDs {
		if strings.TrimSpace(uid) == "" {
			continue
		}
		first := graphFirstURL(p.spec.baseURL, uid, win)
		metrics, content, err := p.paginate(ctx, first, func(body []byte) ([]DailyMetric, []ContentRow, string, error) {
			return parseGraphPage(p, uid, body)
		})
		if err != nil {
			return nil, nil, err
		}
		allM = append(allM, metrics...)
		allC = append(allC, content...)
	}
	return allM, allC, nil
}

// graphFirstURL builds the first-page URL for a user over the window. The
// createdDateTime $filter bounds the window [Start, End); $top caps the page.
func graphFirstURL(baseURL, userID string, win window) string {
	base := baseURL + fmt.Sprintf(graphInteractionsPath, url.PathEscape(userID))
	q := url.Values{}
	// Graph $filter over createdDateTime (ISO 8601). Closed-open window.
	q.Set("$filter", fmt.Sprintf("createdDateTime ge %s and createdDateTime lt %s",
		win.Start.UTC().Format(time.RFC3339), win.End.UTC().Format(time.RFC3339)))
	q.Set("$top", fmt.Sprintf("%d", graphPageSize))
	return base + "?" + q.Encode()
}

// ProbeGraph issues ONE bounded getAllEnterpriseInteractions request for a single
// user ($top=1) over [start, end) and returns the number of interactions visible
// on that first page. It is the `observer-org m365 doctor` reachability probe: a
// non-error return proves the app authenticated to Entra (get resolves a token
// first) AND reached Graph with the granted application permission, without
// walking pagination or writing anything. A per-user 4xx (unlicensed user,
// missing consent, tenant mismatch) surfaces as the wrapped Graph error so doctor
// can map it to a fix hint. It never touches the DB.
func (p *Poller) ProbeGraph(ctx context.Context, userID string, start, end time.Time) (int, error) {
	if strings.TrimSpace(userID) == "" {
		return 0, fmt.Errorf("m365copilotanalytics: probe requires a user id")
	}
	base := p.spec.baseURL + fmt.Sprintf(graphInteractionsPath, url.PathEscape(strings.TrimSpace(userID)))
	q := url.Values{}
	q.Set("$filter", fmt.Sprintf("createdDateTime ge %s and createdDateTime lt %s",
		start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339)))
	q.Set("$top", "1")
	body, err := p.get(ctx, base+"?"+q.Encode())
	if err != nil {
		return 0, err
	}
	var page graphInteractionPage
	if err := json.Unmarshal(body, &page); err != nil {
		return 0, fmt.Errorf("m365copilotanalytics: parse probe page: %w", err)
	}
	return len(page.Value), nil
}

// graphInteractionPage is the getAllEnterpriseInteractions collection envelope.
// This struct + parseGraphPage are the ONLY place that knows this rail's schema.
type graphInteractionPage struct {
	NextLink string          `json:"@odata.nextLink"`
	Value    []aiInteraction `json:"value"`
}

// aiInteraction is one Graph aiInteraction object (proposal §4.1 / Graph docs).
type aiInteraction struct {
	ID              string `json:"id"`
	SessionID       string `json:"sessionId"`
	RequestID       string `json:"requestId"`
	AppClass        string `json:"appClass"`
	InteractionType string `json:"interactionType"` // userPrompt | aiResponse
	CreatedDateTime string `json:"createdDateTime"`
	From            struct {
		User struct {
			ID               string `json:"id"`
			DisplayName      string `json:"displayName"`
			UserIdentityType string `json:"userIdentityType"` // aadUser | …
		} `json:"user"`
	} `json:"from"`
	Body struct {
		ContentType string `json:"contentType"`
		Content     string `json:"content"`
	} `json:"body"`
	Attachments []json.RawMessage `json:"attachments"`
	Links       []json.RawMessage `json:"links"`
	Mentions    []json.RawMessage `json:"mentions"`
	Contexts    []json.RawMessage `json:"contexts"`
}

// parseGraphPage flattens one page into metrics + content + the nextLink. Metrics
// aggregate per (day, appClass): whole interactions, prompts, responses,
// attachments. Each interaction also yields a content row (hashed always; body
// scrubbed then stored unless metadata-only). userKey is the Graph user id passed
// in (the resolver maps it to an org member later); actor is inferred from the
// identity type.
func parseGraphPage(p *Poller, userID string, body []byte) ([]DailyMetric, []ContentRow, string, error) {
	var page graphInteractionPage
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, nil, "", fmt.Errorf("m365copilotanalytics: parse graph page: %w", err)
	}

	// agg[day][appClass] → counters.
	type counters struct {
		interactions int
		prompts      int
		responses    int
		attachments  int
		actor        string
	}
	agg := map[string]map[string]*counters{}
	var content []ContentRow

	for _, it := range page.Value {
		day := utcDayFromTimestamp(it.CreatedDateTime)
		if day == "" || it.ID == "" {
			continue
		}
		appClass := classifyAppClass(it.AppClass)
		actor := actorForIdentity(it.From.User.UserIdentityType)

		byApp := agg[day]
		if byApp == nil {
			byApp = map[string]*counters{}
			agg[day] = byApp
		}
		c := byApp[appClass]
		if c == nil {
			c = &counters{actor: actor}
			byApp[appClass] = c
		}
		c.interactions++
		switch it.InteractionType {
		case InteractionUserPrompt:
			c.prompts++
		case InteractionAIResponse:
			c.responses++
		}
		c.attachments += len(it.Attachments)

		scrubbed := p.scrub(it.Body.Content)
		content = append(content, ContentRow{
			InteractionID:   it.ID,
			SessionID:       it.SessionID,
			RequestID:       it.RequestID,
			AppClass:        appClass,
			InteractionType: it.InteractionType,
			UserKey:         userID,
			Content:         scrubbed,
			ContentHash:     hashBody(scrubbed),
			CreatedAt:       it.CreatedDateTime,
		})
	}

	var metrics []DailyMetric
	for day, byApp := range agg {
		for appClass, c := range byApp {
			metrics = append(
				metrics,
				emitMetric(day, userID, c.actor, SurfaceGraph, appClass, UnitInteractions, MetricInteractions, float64(c.interactions)),
				emitMetric(day, userID, c.actor, SurfaceGraph, appClass, UnitCount, MetricPrompts, float64(c.prompts)),
				emitMetric(day, userID, c.actor, SurfaceGraph, appClass, UnitCount, MetricResponses, float64(c.responses)),
				emitMetric(day, userID, c.actor, SurfaceGraph, appClass, UnitCount, MetricAttachments, float64(c.attachments)),
			)
		}
	}
	return metrics, content, page.NextLink, nil
}

// actorForIdentity maps a Graph userIdentityType to an actor_type. A non-user
// identity (application / device) is bucketed as automation so it never inflates
// per-developer rollups.
func actorForIdentity(identityType string) string {
	switch strings.ToLower(identityType) {
	case "", "aaduser", "user":
		return ActorUser
	case "application", "device", "serviceprincipal":
		return ActorAutomation
	default:
		return ActorUser
	}
}
