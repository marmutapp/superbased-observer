package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// LLM-judge review (guard spec §15.2): for ask/flag-class ambiguity —
// never for clear denies — the composition layer ships a BOUNDED
// event context to the operator's own OpenAI-chat-compatible endpoint
// (bring-your-own: a local gateway, litellm, or the user's proxied
// provider session; no new data processor by default). The judge's
// verdict is recorded as a guard event with source="llm_judge" and
// may pre-approve the rule for the session (resolving the ask) or
// flag-upgrade, but it can NEVER be the sole source of a deny on an
// event no deterministic rule touched (F6) — the composition layer
// only ever submits events that already carry a rule verdict.
//
// Payload posture: the context carries the rule, decision, kind, tool
// and the ALREADY-BOUNDED reason/target excerpt from the audit row
// (commands and paths, never file contents — §15.2), and the egress
// redaction pass runs over the built body like every other cloud call.

// JudgeConfidenceFloor is the minimum self-reported confidence at
// which a judge verdict is ACTED on (approval write / flag upgrade).
// Below it the verdict is still recorded — visibility without
// authority.
const JudgeConfidenceFloor = 0.7

// JudgeContext is the bounded event context shipped for review.
type JudgeContext struct {
	RuleID        string `json:"rule_id"`
	Category      string `json:"category"`
	Severity      string `json:"severity"`
	Decision      string `json:"decision"`
	EventKind     string `json:"event_kind"`
	Tool          string `json:"tool,omitempty"`
	Reason        string `json:"reason,omitempty"`
	TargetExcerpt string `json:"target_excerpt,omitempty"`
	TaintOrigin   string `json:"taint_origin,omitempty"`
}

// JudgeVerdict is the parsed review outcome.
type JudgeVerdict struct {
	// Verdict is "allow", "deny" or "flag".
	Verdict string `json:"verdict"`
	// Confidence is the judge's self-reported 0..1 confidence.
	Confidence float64 `json:"confidence"`
	// Rationale is the judge's bounded explanation.
	Rationale string `json:"rationale"`
}

// judgeSystemPrompt instructs the model to return strict JSON.
const judgeSystemPrompt = `You are a security reviewer for an AI coding agent's guard layer. ` +
	`You receive ONE recorded guard event (a rule verdict on an agent action) as JSON. ` +
	`Decide whether the action looks safe in context. Reply with STRICT JSON only, no prose: ` +
	`{"verdict":"allow"|"deny"|"flag","confidence":0.0-1.0,"rationale":"one short sentence"}. ` +
	`"allow" means the flagged/asked action looks legitimate; "deny" means it looks dangerous; ` +
	`"flag" means leave the deterministic verdict as-is.`

// BuildJudgeRequest renders the chat-completions call for one event
// review. apiKey may be empty (locally-authenticated endpoints).
func BuildJudgeRequest(endpoint, model, apiKey string, jc JudgeContext) (Request, error) {
	jc.Reason = bound(jc.Reason)
	jc.TargetExcerpt = bound(jc.TargetExcerpt)
	user, err := json.Marshal(jc)
	if err != nil {
		return Request{}, fmt.Errorf("notify.BuildJudgeRequest: context: %w", err)
	}
	body, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": judgeSystemPrompt},
			{"role": "user", "content": string(user)},
		},
		"temperature": 0,
	})
	if err != nil {
		return Request{}, fmt.Errorf("notify.BuildJudgeRequest: body: %w", err)
	}
	headers := map[string]string{}
	if apiKey != "" {
		headers["Authorization"] = "Bearer " + apiKey
	}
	return Request{Feature: "llm_judge", Endpoint: endpoint, Headers: headers, Body: body}, nil
}

// ParseJudgeResponse extracts the strict-JSON verdict from a
// chat-completions response body. It tolerates code fences and
// leading/trailing prose around the JSON object (models drift), but
// an unrecognisable verdict value is an error — a judge that can't
// follow the contract gets no authority.
func ParseJudgeResponse(body []byte) (JudgeVerdict, error) {
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return JudgeVerdict{}, fmt.Errorf("notify.ParseJudgeResponse: response: %w", err)
	}
	if len(resp.Choices) == 0 {
		return JudgeVerdict{}, fmt.Errorf("notify.ParseJudgeResponse: no choices")
	}
	content := resp.Choices[0].Message.Content
	start := strings.IndexByte(content, '{')
	end := strings.LastIndexByte(content, '}')
	if start < 0 || end <= start {
		return JudgeVerdict{}, fmt.Errorf("notify.ParseJudgeResponse: no JSON object in content")
	}
	var v JudgeVerdict
	if err := json.Unmarshal([]byte(content[start:end+1]), &v); err != nil {
		return JudgeVerdict{}, fmt.Errorf("notify.ParseJudgeResponse: verdict: %w", err)
	}
	switch v.Verdict {
	case "allow", "deny", "flag":
	default:
		return JudgeVerdict{}, fmt.Errorf("notify.ParseJudgeResponse: unknown verdict %q", v.Verdict)
	}
	if v.Confidence < 0 || v.Confidence > 1 {
		return JudgeVerdict{}, fmt.Errorf("notify.ParseJudgeResponse: confidence %v outside [0,1]", v.Confidence)
	}
	v.Rationale = bound(v.Rationale)
	return v, nil
}
