package cowork

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// writeInstance synthesizes a minimal local-instance layout under root:
//
//	<root>/cowork-<n>/dev-<n>/local_<n>.json       — sidecar
//	<root>/cowork-<n>/dev-<n>/local_<n>/audit.jsonl — audit log
//
// The audit.jsonl body is whatever the caller supplies; the sidecar
// carries a minimal `{}` so loadSidecar succeeds without trying to
// resolve a real workspace.
func writeInstance(t *testing.T, root, name, audit string) string {
	t.Helper()
	dev := filepath.Join(root, "cowork-test", "dev-1")
	instance := filepath.Join(dev, "local_"+name)
	if err := os.MkdirAll(instance, 0o755); err != nil {
		t.Fatalf("mkdir instance: %v", err)
	}
	auditPath := filepath.Join(instance, "audit.jsonl")
	if err := os.WriteFile(auditPath, []byte(audit), 0o644); err != nil {
		t.Fatalf("write audit: %v", err)
	}
	sidecarPath := filepath.Join(dev, "local_"+name+".json")
	if err := os.WriteFile(sidecarPath, []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	return auditPath
}

// TestActionMap_CoworkNewToolNamesMappedCorrectly pins the v1.6.15
// audit B1 fix — every Cowork tool name observed in the live corpus
// roster (system.init.tools[]) that previously fell through to
// ActionUnknown now lands on the right action_type.
//
// The 9 names below cover both the MCP-flavored Cowork extensions
// (mcp__workspace__*, mcp__cowork__*, mcp__visualize__*) and the
// platform-level helpers (Skill, ToolSearch).
func TestActionMap_CoworkNewToolNamesMappedCorrectly(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		want string
	}{
		{"mcp__workspace__bash", models.ActionRunCommand},
		{"mcp__workspace__web_fetch", models.ActionWebFetch},
		{"Skill", models.ActionMCPCall},
		{"ToolSearch", models.ActionMCPCall},
		{"mcp__cowork__request_cowork_directory", models.ActionMCPCall},
		{"mcp__cowork__present_files", models.ActionMCPCall},
		{"mcp__cowork__allow_cowork_file_delete", models.ActionMCPCall},
		{"mcp__visualize__show_widget", models.ActionMCPCall},
		{"mcp__visualize__read_me", models.ActionMCPCall},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := actionMap[tc.name]
			if !ok {
				t.Fatalf("actionMap[%q] missing — must map to %q", tc.name, tc.want)
			}
			if got != tc.want {
				t.Fatalf("actionMap[%q]=%q want %q", tc.name, got, tc.want)
			}
		})
	}
}

// TestParseSessionFile_ImageOnlyUserMessageEmitsMarkerRow pins the
// v1.6.15 audit B2 fix — a user message whose content is a single
// image block (no text, no tool_result) must NOT be silently dropped.
// The adapter emits a marker user_prompt row so the dashboard reflects
// the user activity. Image cost lands on the next result.modelUsage
// bucket; this row is observability-only.
func TestParseSessionFile_ImageOnlyUserMessageEmitsMarkerRow(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	body := `{"type":"system","subtype":"init","cwd":"/sessions/synthetic-init","session_id":"S","tools":[],"_audit_timestamp":"2026-05-19T10:00:00.000Z"}
{"type":"user","uuid":"u-img-only","session_id":"S","message":{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]},"_audit_timestamp":"2026-05-19T10:00:01.000Z"}
{"type":"user","uuid":"u-two-imgs","session_id":"S","message":{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"BBBB"}}]},"_audit_timestamp":"2026-05-19T10:00:02.000Z"}
{"type":"user","uuid":"u-text-only","session_id":"S","message":{"role":"user","content":"hello"},"_audit_timestamp":"2026-05-19T10:00:03.000Z"}
`
	auditPath := writeInstance(t, root, "img", body)
	a := NewWithOptions(nil, root)
	res, err := a.ParseSessionFile(context.Background(), auditPath, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Three user_prompt rows: one image-only marker, one two-images
	// marker, one plain text — all action_type=user_prompt.
	var prompts []models.ToolEvent
	for _, ev := range res.ToolEvents {
		if ev.ActionType == models.ActionUserPrompt {
			prompts = append(prompts, ev)
		}
	}
	if len(prompts) != 3 {
		t.Fatalf("user_prompt rows=%d want 3 (events: %v)", len(prompts), summarizeEvents(res.ToolEvents))
	}
	wantTargets := map[string]string{
		"u-img-only":  "[user sent 1 image attachment(s)]",
		"u-two-imgs":  "[user sent 2 image attachment(s)]",
		"u-text-only": "hello",
	}
	for _, ev := range prompts {
		want, ok := wantTargets[ev.SourceEventID]
		if !ok {
			t.Errorf("unexpected user_prompt SourceEventID=%q target=%q", ev.SourceEventID, ev.Target)
			continue
		}
		if !strings.HasPrefix(ev.Target, want[:min(len(want), 30)]) && ev.Target != want {
			t.Errorf("%s: Target=%q want %q", ev.SourceEventID, ev.Target, want)
		}
	}
}

// TestParseSessionFile_PermissionEventsCaptured pins the v1.6.15 audit
// G1 fix — permission_request becomes an ActionPermissionRequest row,
// the matching permission_response patches Success + ApprovalKind,
// permission_denied lands as a separate ActionPermissionDenied row
// with the platform's reason text in ErrorMessage.
func TestParseSessionFile_PermissionEventsCaptured(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	body := `{"type":"system","subtype":"init","cwd":"/sessions/x","session_id":"S","tools":[],"_audit_timestamp":"2026-05-19T10:00:00.000Z"}
{"type":"system","subtype":"permission_request","uuid":"perm-req-1","session_id":"S","tool_name":"AskUserQuestion","tool_input":{"questions":[{"question":"approve?"}]},"_audit_timestamp":"2026-05-19T10:00:01.000Z"}
{"type":"system","subtype":"permission_response","uuid":"perm-req-1","session_id":"S","tool_name":"AskUserQuestion","decision":"once","granted":true,"_audit_timestamp":"2026-05-19T10:00:02.000Z"}
{"type":"system","subtype":"permission_request","uuid":"perm-req-2","session_id":"S","tool_name":"Write","tool_input":{"file_path":"/etc/passwd"},"_audit_timestamp":"2026-05-19T10:00:03.000Z"}
{"type":"system","subtype":"permission_response","uuid":"perm-req-2","session_id":"S","tool_name":"Write","decision":"once","granted":false,"_audit_timestamp":"2026-05-19T10:00:04.000Z"}
{"type":"system","subtype":"permission_denied","uuid":"perm-deny-1","session_id":"S","tool_name":"Read","tool_use_id":"toolu_xyz","agent_id":"agent-abc","decision_reason_type":"mode","message":"Read denied in dont-ask mode","_audit_timestamp":"2026-05-19T10:00:05.000Z"}
`
	auditPath := writeInstance(t, root, "perm", body)
	a := NewWithOptions(nil, root)
	res, err := a.ParseSessionFile(context.Background(), auditPath, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var requests, denieds []models.ToolEvent
	for _, ev := range res.ToolEvents {
		switch ev.ActionType {
		case models.ActionPermissionRequest:
			requests = append(requests, ev)
		case models.ActionPermissionDenied:
			denieds = append(denieds, ev)
		}
	}
	if len(requests) != 2 {
		t.Fatalf("permission_request rows=%d want 2 (events: %v)", len(requests), summarizeEvents(res.ToolEvents))
	}
	if len(denieds) != 1 {
		t.Fatalf("permission_denied rows=%d want 1", len(denieds))
	}

	// First request (perm-req-1): granted=true → Success=true,
	// ApprovalKind="once".
	r1 := requests[0]
	if r1.SourceEventID != "perm-req-1" || !r1.Success {
		t.Errorf("perm-req-1: SourceEventID=%q Success=%v — want id=perm-req-1, Success=true", r1.SourceEventID, r1.Success)
	}
	if r1.Metadata == nil || r1.Metadata.PermissionApprovalKind != "once" {
		t.Errorf("perm-req-1: metadata.PermissionApprovalKind=%v want %q", r1.Metadata, "once")
	}
	if r1.Target != "AskUserQuestion" {
		t.Errorf("perm-req-1: Target=%q want %q", r1.Target, "AskUserQuestion")
	}

	// Second request (perm-req-2): granted=false → Success=false,
	// still ApprovalKind="once".
	r2 := requests[1]
	if r2.SourceEventID != "perm-req-2" || r2.Success {
		t.Errorf("perm-req-2: SourceEventID=%q Success=%v — want id=perm-req-2, Success=false", r2.SourceEventID, r2.Success)
	}

	// Denied: ErrorMessage carries the reason text, RawToolName is
	// the decision_reason_type.
	d := denieds[0]
	if d.Target != "Read" {
		t.Errorf("denied: Target=%q want %q", d.Target, "Read")
	}
	if !strings.Contains(d.ErrorMessage, "denied in dont-ask mode") {
		t.Errorf("denied: ErrorMessage=%q missing reason text", d.ErrorMessage)
	}
	if d.RawToolName != "mode" {
		t.Errorf("denied: RawToolName=%q want %q (decision_reason_type)", d.RawToolName, "mode")
	}
	if d.Success {
		t.Errorf("denied: Success=true — want false")
	}
}

// TestParseSessionFile_CompactBoundaryEmitsContextCompacted pins the
// v1.6.15 audit G2 fix — system.compact_boundary becomes an
// ActionContextCompacted row mirroring claudecode's shape: Target is
// "<trigger>: ~<preTokens> tokens reclaimed", RawToolInput is the
// full metadata JSON, DurationMs is populated.
func TestParseSessionFile_CompactBoundaryEmitsContextCompacted(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	body := `{"type":"system","subtype":"init","cwd":"/sessions/x","session_id":"S","tools":[],"_audit_timestamp":"2026-05-19T10:00:00.000Z"}
{"type":"system","subtype":"compact_boundary","session_id":"S","uuid":"compact-1","compact_metadata":{"trigger":"auto","pre_tokens":167123,"post_tokens":6978,"duration_ms":105992},"_audit_timestamp":"2026-05-19T10:00:01.000Z"}
`
	auditPath := writeInstance(t, root, "compact", body)
	a := NewWithOptions(nil, root)
	res, err := a.ParseSessionFile(context.Background(), auditPath, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var compacts []models.ToolEvent
	for _, ev := range res.ToolEvents {
		if ev.ActionType == models.ActionContextCompacted {
			compacts = append(compacts, ev)
		}
	}
	if len(compacts) != 1 {
		t.Fatalf("context_compacted rows=%d want 1 (events: %v)", len(compacts), summarizeEvents(res.ToolEvents))
	}
	c := compacts[0]
	if c.SourceEventID != "compact:compact-1" {
		t.Errorf("SourceEventID=%q want %q", c.SourceEventID, "compact:compact-1")
	}
	if c.DurationMs != 105992 {
		t.Errorf("DurationMs=%d want 105992", c.DurationMs)
	}
	if c.RawToolName != "compact_boundary" {
		t.Errorf("RawToolName=%q want %q", c.RawToolName, "compact_boundary")
	}
	if !strings.Contains(c.Target, "auto") || !strings.Contains(c.Target, "167123") {
		t.Errorf("Target=%q want substring 'auto' and '167123'", c.Target)
	}
	if !strings.Contains(c.RawToolInput, "pre_tokens") {
		t.Errorf("RawToolInput=%q want JSON containing pre_tokens", c.RawToolInput)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
