package notify

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBuildWebhook_Templates(t *testing.T) {
	alert := WebhookAlert{
		RuleID: "R-110", Category: "boundary", Severity: "high", Decision: "deny",
		Tool: "claude-code", Enforced: true, Reason: "write outside project",
		Timestamp: "2026-06-11T10:00:00Z", Source: "hook",
	}
	cases := []struct {
		name       string
		kind       string
		routingKey string
		wantSub    []string
		wantErr    bool
	}{
		{
			name: "generic_json_carries_all_fields",
			kind: WebhookGeneric,
			wantSub: []string{
				`"rule_id":"R-110"`, `"severity":"high"`, `"decision":"deny"`,
				`"enforced":true`, `"timestamp":"2026-06-11T10:00:00Z"`,
			},
		},
		{
			name:    "empty_kind_defaults_to_generic",
			kind:    "",
			wantSub: []string{`"rule_id":"R-110"`},
		},
		{
			name:    "slack_text_line",
			kind:    WebhookSlack,
			wantSub: []string{`"text":"Observer Guard: R-110 deny (boundary/high) on claude-code`},
		},
		{
			name:    "discord_content_line",
			kind:    WebhookDiscord,
			wantSub: []string{`"content":"Observer Guard: R-110 deny`},
		},
		{
			name:       "pagerduty_events_v2",
			kind:       WebhookPagerDuty,
			routingKey: "rk-1",
			wantSub: []string{
				`"routing_key":"rk-1"`, `"event_action":"trigger"`,
				`"severity":"error"`, `"summary":"Observer Guard: R-110 deny`,
			},
		},
		{
			name:    "pagerduty_without_routing_key_errors",
			kind:    WebhookPagerDuty,
			wantErr: true,
		},
		{
			name:    "unknown_kind_errors",
			kind:    "msteams",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := BuildWebhook(tc.kind, tc.routingKey, alert)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildWebhook: %v", err)
			}
			if !json.Valid(body) {
				t.Fatalf("invalid JSON: %s", body)
			}
			for _, sub := range tc.wantSub {
				if !strings.Contains(string(body), sub) {
					t.Errorf("body %s\nmissing %q", body, sub)
				}
			}
		})
	}
}

func TestPagerDutySeverityMap(t *testing.T) {
	cases := map[string]string{
		"critical": "critical", "high": "error", "medium": "warning",
		"warn": "warning", "info": "info", "": "info", "weird": "info",
	}
	for in, want := range cases {
		if got := pagerDutySeverity(in); got != want {
			t.Errorf("pagerDutySeverity(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildJudgeRequest_BoundsAndShape(t *testing.T) {
	long := strings.Repeat("x", 500)
	req, err := BuildJudgeRequest("https://j/v1/chat/completions", "gpt-x", "", JudgeContext{
		RuleID: "R-180", Decision: "flag", Reason: long, TargetExcerpt: long,
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.Feature != "llm_judge" || req.Endpoint != "https://j/v1/chat/completions" {
		t.Errorf("request meta = %+v", req)
	}
	if _, ok := req.Headers["Authorization"]; ok {
		t.Error("empty api key must not emit an Authorization header")
	}
	var body struct {
		Model    string `json:"model"`
		Messages []struct {
			Role, Content string
		} `json:"messages"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.Model != "gpt-x" || len(body.Messages) != 2 || body.Messages[0].Role != "system" {
		t.Errorf("chat shape = %+v", body)
	}
	if strings.Contains(body.Messages[1].Content, long) {
		t.Error("unbounded reason/excerpt leaked into the judge payload")
	}
}

func TestParseJudgeResponse(t *testing.T) {
	wrap := func(content string) string {
		b, _ := json.Marshal(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": content}}},
		})
		return string(b)
	}
	cases := []struct {
		name    string
		body    string
		want    JudgeVerdict
		wantErr bool
	}{
		{
			name: "strict_json",
			body: wrap(`{"verdict":"allow","confidence":0.9,"rationale":"routine build step"}`),
			want: JudgeVerdict{Verdict: "allow", Confidence: 0.9, Rationale: "routine build step"},
		},
		{
			name: "code_fenced_json_tolerated",
			body: wrap("```json\n{\"verdict\":\"deny\",\"confidence\":0.8,\"rationale\":\"exfil shape\"}\n```"),
			want: JudgeVerdict{Verdict: "deny", Confidence: 0.8, Rationale: "exfil shape"},
		},
		{name: "unknown_verdict_rejected", body: wrap(`{"verdict":"maybe","confidence":0.9}`), wantErr: true},
		{name: "confidence_out_of_range_rejected", body: wrap(`{"verdict":"allow","confidence":1.5}`), wantErr: true},
		{name: "no_json_rejected", body: wrap("looks fine to me"), wantErr: true},
		{name: "no_choices_rejected", body: `{"choices":[]}`, wantErr: true},
		{name: "garbage_rejected", body: "not json", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseJudgeResponse([]byte(tc.body))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseJudgeResponse: %v", err)
			}
			if got != tc.want {
				t.Errorf("verdict = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestBuildNPMLookupAndParse(t *testing.T) {
	req, err := BuildNPMLookup("@scope/pkg")
	if err != nil {
		t.Fatal(err)
	}
	if req.Endpoint != "https://registry.npmjs.org/@scope%2Fpkg" || req.Method != "GET" || req.Feature != "reputation" {
		t.Errorf("lookup = %+v", req)
	}
	if _, err := BuildNPMLookup(""); err == nil {
		t.Error("empty package must error")
	}

	doc := `{
		"name":"@scope/pkg","description":"an MCP server",
		"dist-tags":{"latest":"1.2.3"},
		"versions":{"1.0.0":{},"1.2.3":{}},
		"time":{"created":"2026-06-01T00:00:00Z","modified":"2026-06-10T00:00:00Z"},
		"maintainers":[{"name":"a"},{"name":"b"}]
	}`
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	info, err := ParseNPMMetadata([]byte(doc), now)
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "@scope/pkg" || info.Latest != "1.2.3" || info.VersionCount != 2 ||
		info.AgeDays != 10 || info.Maintainers != 2 {
		t.Errorf("info = %+v", info)
	}
	if _, err := ParseNPMMetadata([]byte(`{"error":"Not found"}`), now); err == nil {
		t.Error("non-package document must error")
	}

	lines := FormatNPMInfo(info)
	joined := strings.Join(lines, "\n")
	for _, sub := range []string{"@scope/pkg", "1.2.3 (2 versions)", "10 days", "maintainers: 2"} {
		if !strings.Contains(joined, sub) {
			t.Errorf("format output missing %q:\n%s", sub, joined)
		}
	}
}
