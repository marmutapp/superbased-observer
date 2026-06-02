package otel

import (
	"go.opentelemetry.io/otel/attribute"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// GenAI semantic-convention attribute keys (spec version v1.41.0). The
// OpenTelemetry contrib repo does not yet publish a Go package for the GenAI
// conventions, so the keys are defined here against the v1.41.0 spec. Values
// follow the convention's well-known enums where one applies (e.g. the
// provider name and operation name).
const (
	keyGenAIProviderName   = "gen_ai.provider.name"
	keyGenAIOperationName  = "gen_ai.operation.name"
	keyGenAIRequestModel   = "gen_ai.request.model"
	keyGenAIResponseModel  = "gen_ai.response.model"
	keyGenAIResponseID     = "gen_ai.response.id"
	keyGenAIFinishReasons  = "gen_ai.response.finish_reasons"
	keyGenAIUsageInputTok  = "gen_ai.usage.input_tokens"
	keyGenAIUsageOutputTok = "gen_ai.usage.output_tokens"

	// errorTypeKey is the general semconv error.type attribute, set on errored
	// turns so backends can slice failures by class.
	errorTypeKey = "error.type"
)

// SuperBased custom-namespace attribute keys. These carry the freshness /
// redundancy / attribution signal that is unique to SuperBased Observer and
// has no GenAI-convention equivalent.
const (
	keySBOProjectRoot       = "sbo.project.root"
	keySBOSessionID         = "sbo.session.id"
	keySBOOrgID             = "sbo.org.id"
	keySBOUserEmail         = "sbo.user.email"
	keySBOCostUSD           = "sbo.cost.usd"
	keySBOCacheReadTokens   = "sbo.cache.read_tokens" //nolint:gosec // G101: OTel attribute-key constant, not a credential.
	keySBOToolAdapter       = "sbo.tool.adapter"
	keySBOActionNormalized  = "sbo.action.normalized_type"
	keySBOFreshnessClass    = "sbo.freshness.classification"
	keySBORedundancyStale   = "sbo.redundancy.is_stale_reread"
	keySBOWebSearchRequests = "sbo.web_search.requests"
)

// operationChat is the GenAI operation.name for a chat-completion turn, which
// is what every api_turns row represents.
const operationChat = "chat"

// SpanName returns the span name for a turn, per the GenAI convention's
// "{operation} {model}" recommendation (e.g. "chat claude-opus-4-7").
func SpanName(turn models.APITurn) string {
	if turn.Model == "" {
		return operationChat
	}
	return operationChat + " " + turn.Model
}

// Attributes maps an api_turns row — and, when supplied, the related action —
// to the OTel attribute set for its span.
//
// The turn carries the GenAI standard attributes plus the turn-intrinsic sbo.*
// (project root, session, org/user attribution, cost, cache, web search). When
// action is non-nil its action-level sbo.* attributes (tool adapter, normalized
// type, freshness classification, stale-reread redundancy) are appended — this
// is the single home for the full attribute vocabulary, exercised as a unit by
// the tests. The production turn tail passes a nil action.
//
// sbo.user.email is emitted only when the agent is enrolled (turn.UserEmail
// non-empty) AND emitUserEmail is true; sbo.org.id only when enrolled. Empty
// optional values are omitted rather than emitted blank.
func Attributes(turn models.APITurn, action *models.Action, projectRoot string, emitUserEmail bool) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 16)

	// --- GenAI standard ---
	if turn.Provider != "" {
		attrs = append(attrs, attribute.String(keyGenAIProviderName, turn.Provider))
	}
	attrs = append(attrs, attribute.String(keyGenAIOperationName, operationChat))
	if turn.Model != "" {
		attrs = append(
			attrs,
			attribute.String(keyGenAIRequestModel, turn.Model),
			attribute.String(keyGenAIResponseModel, turn.Model),
		)
	}
	if turn.RequestID != "" {
		attrs = append(attrs, attribute.String(keyGenAIResponseID, turn.RequestID))
	}
	if turn.StopReason != "" {
		attrs = append(attrs, attribute.StringSlice(keyGenAIFinishReasons, []string{turn.StopReason}))
	}
	attrs = append(
		attrs,
		attribute.Int64(keyGenAIUsageInputTok, turn.InputTokens),
		attribute.Int64(keyGenAIUsageOutputTok, turn.OutputTokens),
	)
	if turn.ErrorClass != "" {
		attrs = append(attrs, attribute.String(errorTypeKey, turn.ErrorClass))
	}

	// --- SuperBased custom (turn-intrinsic) ---
	if projectRoot != "" {
		attrs = append(attrs, attribute.String(keySBOProjectRoot, projectRoot))
	}
	if turn.SessionID != "" {
		attrs = append(attrs, attribute.String(keySBOSessionID, turn.SessionID))
	}
	if turn.OrgID != "" {
		attrs = append(attrs, attribute.String(keySBOOrgID, turn.OrgID))
		if emitUserEmail && turn.UserEmail != "" {
			attrs = append(attrs, attribute.String(keySBOUserEmail, turn.UserEmail))
		}
	}
	attrs = append(attrs, attribute.Float64(keySBOCostUSD, turn.CostUSD))
	if turn.CacheReadTokens > 0 {
		attrs = append(attrs, attribute.Int64(keySBOCacheReadTokens, turn.CacheReadTokens))
	}
	if turn.WebSearchRequests > 0 {
		attrs = append(attrs, attribute.Int64(keySBOWebSearchRequests, turn.WebSearchRequests))
	}

	// --- SuperBased custom (action-level, when an action is in scope) ---
	if action != nil {
		if action.Tool != "" {
			attrs = append(attrs, attribute.String(keySBOToolAdapter, action.Tool))
		}
		if action.ActionType != "" {
			attrs = append(attrs, attribute.String(keySBOActionNormalized, action.ActionType))
		}
		if action.Freshness != "" {
			attrs = append(
				attrs,
				attribute.String(keySBOFreshnessClass, action.Freshness),
				attribute.Bool(keySBORedundancyStale, action.Freshness == models.FreshnessStale),
			)
		}
	}

	return attrs
}
