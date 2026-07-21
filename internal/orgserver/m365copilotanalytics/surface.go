package m365copilotanalytics

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Surface selects which M365 Copilot rail the poller targets. A tenant may run a
// poller per rail; m365_copilot_analytics_daily.surface keeps them distinct.
type Surface string

const (
	// SurfaceGraph is Rail A — the Graph aiInteractionHistory content rail
	// (getAllEnterpriseInteractions). Carries actual prompt/response content.
	SurfaceGraph Surface = "graph"
	// SurfacePurview is Rail B — the Office 365 Management Activity metadata rail
	// (RecordType CopilotInteraction 261). Metadata only, no content. SCAFFOLDED.
	SurfacePurview Surface = "purview"
)

// Unit is the measurement unit of a metric's value. There is NO cost unit —
// aiInteractionHistory is not metered (re-verify per release; never "free").
type Unit string

const (
	UnitCount        Unit = "count"        // prompts / responses / accessed-resources counts
	UnitInteractions Unit = "interactions" // whole-interaction counts
)

// Actor types stored in actor_type.
const (
	ActorUser       = "user"       // a licensed M365 Copilot developer (Entra user)
	ActorAutomation = "automation" // a service/app identity
	ActorTenant     = "tenant"     // tenant-aggregate row (no per-user attribution)
)

// appClass surface tags (M365 Copilot host attribution). The exact office-app
// strings past BizChat carry a live-tenant residual (proposal §4.1 must-verify);
// classifyAppClass normalizes the raw Graph appClass to one of these buckets.
const (
	AppBizChat    = "bizchat" // IPM.SkypeTeams.Message.Copilot.BizChat (M365 Chat)
	AppTeams      = "teams"
	AppWebChat    = "webchat"
	AppWord       = "word"
	AppOutlook    = "outlook"
	AppPowerPoint = "powerpoint"
	AppExcel      = "excel"
	AppOther      = "other" // an unrecognized appClass (kept, never dropped)
)

// interactionType values on an aiInteraction.
const (
	InteractionUserPrompt = "userPrompt"
	InteractionAIResponse = "aiResponse"
)

// Metric names stored in m365_copilot_analytics_daily. Rail A derives counts from
// the interaction stream (per day × user × appClass); Rail B derives governance
// metadata counts. NONE is a cost — the API is unmetered.
const (
	// Rail A (graph) — unit: interactions | count.
	MetricInteractions = "interactions" // whole aiInteractions (unit: interactions)
	MetricPrompts      = "prompts"      // interactionType=userPrompt (unit: count)
	MetricResponses    = "responses"    // interactionType=aiResponse (unit: count)
	MetricAttachments  = "attachments"  // attachments across interactions (unit: count)

	// Rail B (purview) — unit: count. Scaffolded metrics documented for parity.
	MetricGovInteractions      = "gov_interactions"      // CopilotInteraction records (unit: count)
	MetricAccessedResources    = "accessed_resources"    // AccessedResources entries (unit: count)
	MetricGroundedInteractions = "grounded_interactions" // grounding flag set (unit: count)
)

// window is a closed-open time range [Start, End) the rails format per their own
// timestamp convention (Graph: ISO 8601 createdDateTime $filter).
type window struct {
	Start time.Time
	End   time.Time
}

// DailyMetric is one normalized (day, user, surface, appClass, metric) value bound
// for m365_copilot_analytics_daily. Surface + Unit + AppClass are stamped by the
// rail parser.
type DailyMetric struct {
	Day       string  // YYYY-MM-DD (UTC)
	UserKey   string  // Entra object id | userPrincipalName | email
	ActorType string  // ActorUser | ActorAutomation | ActorTenant
	Surface   Surface // which rail produced this row
	AppClass  string  // M365 surface bucket (AppBizChat | AppWord | …)
	Unit      Unit    // unit of Value
	Metric    string
	Value     float64
}

// ContentRow is one Rail A aiInteraction body bound for m365_copilot_content.
// Hash is content-free and always set; Content is empty in metadata-only mode.
type ContentRow struct {
	InteractionID   string
	SessionID       string
	RequestID       string
	AppClass        string
	InteractionType string
	UserKey         string
	Content         string // scrubbed body; empty → NULL (metadata-only)
	ContentHash     string // sha256-hex of the scrubbed body; always set
	CreatedAt       string // RFC3339
}

// surfaceSpec is the resolved-once strategy for one rail. The poller is
// rail-blind: it only calls poll, which owns that rail's endpoint topology.
// Selecting the spec at construction (not per-call) keeps the hot path free of
// rail conditionals (rule #3).
type surfaceSpec struct {
	surface Surface
	baseURL string
	poll    func(ctx context.Context, p *Poller, win window) ([]DailyMetric, []ContentRow, error)
}

// Graph + Management Activity default hosts (global commercial cloud ONLY —
// sovereign clouds use different hosts and are NOT supported).
const (
	defaultGraphBaseURL   = "https://graph.microsoft.com"
	defaultPurviewBaseURL = "https://manage.office.com"
)

// surfaceRegistry is the table-driven set of supported rails (rule #5: a data
// table, not a conditional ladder). Adding a rail is one entry + its file.
var surfaceRegistry = map[Surface]surfaceSpec{
	SurfaceGraph:   {surface: SurfaceGraph, baseURL: defaultGraphBaseURL, poll: pollGraph},
	SurfacePurview: {surface: SurfacePurview, baseURL: defaultPurviewBaseURL, poll: pollPurview},
}

// resolveSurface returns the spec for a rail name (with an optional baseURL
// override for testing), or an error for an unknown rail.
func resolveSurface(name, baseURLOverride string) (surfaceSpec, error) {
	spec, ok := surfaceRegistry[Surface(strings.TrimSpace(name))]
	if !ok {
		return surfaceSpec{}, fmt.Errorf("m365copilotanalytics: unknown surface %q (want %s|%s)",
			name, SurfaceGraph, SurfacePurview)
	}
	if o := strings.TrimSpace(baseURLOverride); o != "" {
		spec.baseURL = o
	}
	return spec, nil
}

// dayOf returns the YYYY-MM-DD UTC bucket for a time.
func dayOf(t time.Time) string { return t.UTC().Format("2006-01-02") }

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

// emitMetric builds a DailyMetric.
func emitMetric(day, userKey, actorType string, surface Surface, appClass string, unit Unit, metric string, v float64) DailyMetric {
	return DailyMetric{
		Day: day, UserKey: userKey, ActorType: actorType,
		Surface: surface, AppClass: appClass, Unit: unit, Metric: metric, Value: v,
	}
}

// firstNonEmpty returns a if non-empty, else b.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// hashBody returns the sha256-hex of a body — the content-free anchor stored
// alongside (or instead of) the content.
func hashBody(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// classifyAppClass normalizes a raw Graph appClass string to an appClass bucket.
// Unknown values fall to AppOther (kept, never dropped). The office-app strings
// are the documented shapes; the exact suffixes carry a live-tenant residual, so
// the match is substring-based and case-insensitive for resilience.
func classifyAppClass(raw string) string {
	l := strings.ToLower(raw)
	switch {
	case l == "":
		return AppOther
	case strings.Contains(l, "bizchat"):
		return AppBizChat
	case strings.Contains(l, "teams"):
		return AppTeams
	case strings.Contains(l, "webchat"):
		return AppWebChat
	case strings.Contains(l, "word"):
		return AppWord
	case strings.Contains(l, "outlook"):
		return AppOutlook
	case strings.Contains(l, "powerpoint"), strings.Contains(l, "ppt"):
		return AppPowerPoint
	case strings.Contains(l, "excel"):
		return AppExcel
	default:
		return AppOther
	}
}
