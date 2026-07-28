package main

import (
	"testing"

	"github.com/marmutapp/superbased-observer/internal/routing"
)

func TestResolveTargetModelViaTier(t *testing.T) {
	table := routing.NewTierResolver().Table()
	tests := []struct {
		name        string
		sourceModel string
		targetTool  string
		want        string
	}{
		{
			// The Anthropic opus-class representative moved
			// claude-opus-4-8 → claude-opus-5 on 2026-07-25 when Opus 5
			// became the flagship (operator-approved; see the
			// seedRepresentatives comment in internal/routing/tiers.go).
			// Handoff target resolution is the SECOND consumer of that
			// cell, alongside the routing engine's pin action — so this
			// expectation tracks the representative by design, and a
			// future representative change is expected to land here too.
			name:        "cross-family opus tier remaps to anthropic representative",
			sourceModel: "gpt-5.5", // OpenAI opus-class
			targetTool:  "claude-code",
			want:        "claude-opus-5",
		},
		{
			name:        "cross-family sonnet tier remaps to openai representative",
			sourceModel: "claude-sonnet-4-6", // Anthropic sonnet-class
			targetTool:  "codex",
			want:        "gpt-5.4",
		},
		{
			name:        "cross-family to gemini representative",
			sourceModel: "claude-opus-4-8", // Anthropic opus-class
			targetTool:  "gemini-cli",
			want:        "gemini-3.1-pro-preview",
		},
		{
			name:        "same shape different tool returns empty (source model correct)",
			sourceModel: "claude-opus-4-8",
			targetTool:  "cline", // Anthropic shape, same as source
			want:        "",
		},
		{
			name:        "unknown target tool returns empty",
			sourceModel: "gpt-5.5",
			targetTool:  "opencode", // not in the shape table
			want:        "",
		},
		{
			name:        "unknown source tier returns empty",
			sourceModel: "some-unlisted-model-xyz",
			targetTool:  "claude-code",
			want:        "",
		},
		{
			name:        "empty source model returns empty",
			sourceModel: "",
			targetTool:  "claude-code",
			want:        "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveTargetModelViaTier(table, handoffTargetShapes, tt.sourceModel, tt.targetTool)
			if got != tt.want {
				t.Errorf("resolveTargetModelViaTier(%q, %q) = %q, want %q",
					tt.sourceModel, tt.targetTool, got, tt.want)
			}
		})
	}
}

func TestNewHandoffTargetResolver(t *testing.T) {
	resolve := newHandoffTargetResolver()
	// Tracks the Anthropic opus-class representative (claude-opus-5 as of
	// 2026-07-25); see the table-driven test above for the rationale.
	if got := resolve("gpt-5.5", "claude-code"); got != "claude-opus-5" {
		t.Errorf("resolver(gpt-5.5, claude-code) = %q, want claude-opus-5", got)
	}
	if got := resolve("claude-opus-4-8", "claude-code"); got != "" {
		t.Errorf("resolver(claude-opus-4-8, claude-code) = %q, want empty (same shape)", got)
	}
}
