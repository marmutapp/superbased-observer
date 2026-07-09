package notify

import (
	"encoding/json"
	"fmt"
)

// Webhook payload templates (guard spec §15.4): generic JSON, Slack,
// Discord, and PagerDuty Events API v2. Builders are pure — the
// composition layer picks qualifying verdicts (per-webhook
// min_severity) and submits the built body through the Egress worker.
// Every field is a label, decision, or BOUNDED reason text; raw file
// contents never enter an alert (and the egress redaction pass runs
// over the built body regardless).

// WebhookKind values accepted by BuildWebhook.
const (
	WebhookGeneric   = "generic"
	WebhookSlack     = "slack"
	WebhookDiscord   = "discord"
	WebhookPagerDuty = "pagerduty"
)

// WebhookAlert is the content-bounded alert shape shared by all
// templates.
type WebhookAlert struct {
	RuleID    string `json:"rule_id"`
	Category  string `json:"category"`
	Severity  string `json:"severity"`
	Decision  string `json:"decision"`
	Tool      string `json:"tool,omitempty"`
	Enforced  bool   `json:"enforced"`
	Reason    string `json:"reason,omitempty"`
	Timestamp string `json:"timestamp"` // RFC3339
	Source    string `json:"source,omitempty"`
}

// BuildWebhook renders one alert for a webhook kind. routingKey is
// PagerDuty's Events-API routing key (ignored by other kinds). The
// reason field is bounded with the same glanceable-summary limit as
// desktop notifications.
func BuildWebhook(kind, routingKey string, a WebhookAlert) ([]byte, error) {
	a.Reason = bound(a.Reason)
	line := fmt.Sprintf("Observer Guard: %s %s (%s/%s)", a.RuleID, a.Decision, a.Category, a.Severity)
	if a.Tool != "" {
		line += " on " + a.Tool
	}
	if a.Reason != "" {
		line += " — " + a.Reason
	}
	switch kind {
	case WebhookGeneric, "":
		return json.Marshal(a)
	case WebhookSlack:
		return json.Marshal(map[string]string{"text": line})
	case WebhookDiscord:
		return json.Marshal(map[string]string{"content": line})
	case WebhookPagerDuty:
		if routingKey == "" {
			return nil, fmt.Errorf("notify.BuildWebhook: pagerduty kind requires routing_key")
		}
		return json.Marshal(map[string]any{
			"routing_key":  routingKey,
			"event_action": "trigger",
			"payload": map[string]any{
				"summary":   line,
				"severity":  pagerDutySeverity(a.Severity),
				"source":    "superbased-observer-guard",
				"timestamp": a.Timestamp,
				"custom_details": map[string]any{
					"rule_id": a.RuleID, "category": a.Category, "decision": a.Decision,
					"tool": a.Tool, "enforced": a.Enforced,
				},
			},
		})
	default:
		return nil, fmt.Errorf("notify.BuildWebhook: unknown webhook kind %q", kind)
	}
}

// pagerDutySeverity maps guard severities onto PagerDuty's fixed
// vocabulary (critical|error|warning|info).
func pagerDutySeverity(s string) string {
	switch s {
	case "critical":
		return "critical"
	case "high":
		return "error"
	case "medium", "warn":
		return "warning"
	default:
		return "info"
	}
}
