package handoffsvc

import (
	"context"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/integration"
)

// TestBuild_PromptDeliverySupported pins the inject_prompt lane end to end:
// codex declares InjectPrompt, so a prompt-delivery handoff succeeds and the
// rendered doc (which the launcher injects as the first prompt) carries the
// short-id marker the P3 linker greps for.
func TestBuild_PromptDeliverySupported(t *testing.T) {
	deps, fs := testDeps(t, t.TempDir())
	res, err := Build(context.Background(), deps, Request{
		SessionID:  "s1",
		TargetTool: "codex",
		Delivery:   integration.InjectPrompt,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.Delivery != integration.InjectPrompt {
		t.Errorf("delivery = %q, want prompt", res.Delivery)
	}
	if !strings.Contains(res.Doc, "superbased-handoff "+res.ShortID) {
		t.Error("prompt-delivered doc must carry the marker for the linker")
	}
	if len(fs.inserted) != 1 || fs.inserted[0].Delivery != "prompt" {
		t.Fatalf("want one row with delivery=prompt, got %+v", fs.inserted)
	}
}

// TestBuild_PromptDeliveryUnsupportedLaneErrors pins the capability check:
// requesting --continue-from against a tool that does not declare
// InjectPrompt (cline: file+mcp only) errors honestly, naming the gap —
// the same validateDeliveryLane dispatch the launcher relies on.
func TestBuild_PromptDeliveryUnsupportedLaneErrors(t *testing.T) {
	deps, _ := testDeps(t, t.TempDir())
	_, err := Build(context.Background(), deps, Request{
		SessionID:  "s1",
		TargetTool: "cline",
		Delivery:   integration.InjectPrompt,
	})
	if err == nil || !strings.Contains(err.Error(), "does not support") {
		t.Fatalf("unsupported prompt lane must error naming the gap, got %v", err)
	}
	// Sanity: cline genuinely lacks the prompt lane in the registry.
	cap, _ := integration.For("cline")
	for _, k := range cap.Handoff.Lanes() {
		if k == integration.InjectPrompt {
			t.Fatal("test premise broken: cline now declares InjectPrompt")
		}
	}
}
