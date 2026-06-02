package orgcontract

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the wire golden file instead of comparing")

// TestWireEncodingGolden pins the JSON wire encoding of every contract
// type. A change to a json tag, a field, or a type fails this test until
// the golden is intentionally regenerated with:
//
//	go test ./internal/orgcontract -update
//
// This is the tripwire that catches an accidental agent↔server wire
// incompatibility at test time rather than in production.
func TestWireEncodingGolden(t *testing.T) {
	fixtures := map[string]any{
		"enroll_request": EnrollRequest{
			OneTimeToken: "tok_id123.ot_abc123", AgentPublicKey: "MCowBQYDK2VwAyEA...",
		},
		"enroll_response": EnrollResponse{
			Bearer: "eyJ...sig", BearerExpiresAt: "2026-08-23T00:00:00Z",
			OrgID: "org-acme", OrgName: "Acme", UserID: "scim-42", UserEmail: "dev@acme.example",
		},
		"bearer_claims": BearerClaims{
			Iss: "https://org.acme.example", Sub: "scim-42", Aud: "org-acme",
			Exp: 1797206400, Iat: 1789430400, Jti: "jti-7f3a",
		},
		"push_response": PushResponse{AcceptedRows: 118, DedupedRows: 7, NextCursor: 90210},
		"push_envelope": PushEnvelope{
			AgentVersion: "1.6.30",
			CursorFrom:   90000,
			CursorTo:     90210,
			Sessions: []SessionRow{{
				ID: "sess-1", ProjectRoot: "/home/dev/acme-api", GitRemote: "git@github.com:acme/api.git",
				Tool: "claude-code", Model: "claude-opus-4-7", GitBranch: "main",
				StartedAt: "2026-05-25T10:00:00Z", EndedAt: "2026-05-25T10:42:00Z",
				TotalActions: 37, OrgID: "org-acme", UserEmail: "dev@acme.example",
			}},
			Actions: []ActionRow{{
				SessionID: "sess-1", SourceFile: "cc.jsonl", SourceEventID: "evt-9",
				Timestamp: "2026-05-25T10:05:00Z", Tool: "claude-code", ActionType: "edit_file",
				Target: "internal/api/handler.go", TurnIndex: 4, Success: true, DurationMs: 1200,
				IsSidechain: false, OrgID: "org-acme", UserEmail: "dev@acme.example",
			}},
			APITurns: []APITurnRow{{
				SessionID: "sess-1", ProjectRoot: "/home/dev/acme-api",
				Timestamp: "2026-05-25T10:05:01Z", Provider: "anthropic", Model: "claude-opus-4-7",
				RequestID: "req-1", InputTokens: 1200, OutputTokens: 800, CacheReadTokens: 4000,
				CacheCreationTokens: 256, CacheCreation1hTokens: 0, WebSearchRequests: 0,
				CostUSD: 0.0421, MessageCount: 6, ToolUseCount: 3,
				SystemPromptHash: "sha256:aaa", MessagePrefixHash: "sha256:bbb",
				TimeToFirstTokenMS: 410, TotalResponseMS: 5200, StopReason: "end_turn",
				HTTPStatus: 200, ErrorClass: "", OrgID: "org-acme", UserEmail: "dev@acme.example",
			}},
			TokenUsage: []TokenUsageRow{{
				SessionID: "sess-1", ProjectRoot: "/home/dev/acme-api",
				Timestamp: "2026-05-25T10:05:01Z", Tool: "claude-code", Model: "claude-opus-4-7",
				InputTokens: 1200, OutputTokens: 800, CacheReadTokens: 4000, CacheCreationTokens: 256,
				CacheCreation1hTokens: 0, ReasoningTokens: 0, WebSearchRequests: 0,
				EstimatedCostUSD: 0.0421, Source: "proxy", Reliability: "reliable",
				SourceFile: "cc.jsonl", SourceEventID: "tok-9",
				OrgID: "org-acme", UserEmail: "dev@acme.example",
			}},
		},
	}

	got, err := json.MarshalIndent(fixtures, "", "  ")
	if err != nil {
		t.Fatalf("marshal fixtures: %v", err)
	}
	got = append(got, '\n')

	golden := filepath.Join("testdata", "wire_golden.json")
	if *update {
		if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden: %v (run `go test ./internal/orgcontract -update` to create it)", err)
	}
	if string(got) != string(want) {
		t.Errorf("wire encoding changed.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
