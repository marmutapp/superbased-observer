package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/integration"
)

// TestRenderAdapterMatrixCoversEveryAdapter pins that the generated matrix
// has a row for every registered adapter and renders the expected cells for
// representative tools — so the support grid stays generated, not hand-kept.
func TestRenderAdapterMatrixCoversEveryAdapter(t *testing.T) {
	var buf bytes.Buffer
	renderAdapterMatrix(&buf, integration.Capabilities())
	out := buf.String()

	for _, c := range integration.Capabilities() {
		if !strings.Contains(out, c.Tool) {
			t.Errorf("matrix missing row for %q", c.Tool)
		}
	}
	// Representative cells (capability shapes, not tool identity).
	for _, want := range []string{
		"env:ANTHROPIC_BASE_URL",       // claude-code RouteEnvSettings
		"config-file",                  // codex RouteConfigFile
		"launcher",                     // opencode RouteLauncher
		"cline_cli_hooks_jsonl+manual", // cline-cli not-auto-wired
		"claude_settings_json+bridge",  // claude-code cross-OS bridge
		"A/B/C",                        // native rails
		"(gap)",                        // a token gap marker
		"SURFACE",                      // routability column header
		"routable",                     // RouteStatusRoutableNow render
		"after-upstream",               // hermes RouteStatusAfterUpstream
		"after-bridge",                 // gemini-cli RouteStatusAfterBridge
		"probe",                        // RouteStatusProbeRequired render
		"native-exempt",                // RouteStatusNativeExempt render
		"HANDOFF",                      // handoff column header
		"full+seeded",                  // full transcript + seeded launcher (claude-code/codex)
		"full+doc-assisted",            // hermes DocAssisted launch
	} {
		if !strings.Contains(out, want) {
			t.Errorf("matrix missing expected cell %q", want)
		}
	}
}

// TestAdaptersCmdJSON pins the --json path emits the full registry.
func TestAdaptersCmdJSON(t *testing.T) {
	cmd := newAdaptersCmd()
	cmd.SetArgs([]string{"--json"})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	var caps []integration.Capability
	if err := json.Unmarshal(buf.Bytes(), &caps); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if len(caps) != len(integration.Capabilities()) {
		t.Errorf("json rows = %d, want %d", len(caps), len(integration.Capabilities()))
	}
}

func TestProxyAndTokenCells(t *testing.T) {
	if got := proxyCell(nil); got != "—" {
		t.Errorf("proxyCell(nil) = %q, want dash", got)
	}
	if got := tokenCell(integration.TokenTier{Best: "proxy"}); got != "proxy" {
		t.Errorf("tokenCell clean = %q", got)
	}
	if got := tokenCell(integration.TokenTier{Best: "sqlite", Gap: "x"}); got != "sqlite (gap)" {
		t.Errorf("tokenCell gapped = %q", got)
	}
	if got := mcpCell(nil); got != "—" {
		t.Errorf("mcpCell(nil) = %q", got)
	}
}

// TestHandoffCell pins the HANDOFF cell rendering across the four grounded
// handoff shapes: a seeded launchable row, the DocAssisted row, a
// transcript-only (not launchable) row, and the actions-only zero value.
func TestHandoffCell(t *testing.T) {
	cases := []struct {
		name string
		in   integration.HandoffCapability
		want string
	}{
		{
			name: "seeded launchable (codex shape)",
			in: integration.HandoffCapability{
				Transcript: integration.TranscriptFull,
				Inject:     []integration.InjectKind{integration.InjectFile, integration.InjectPrompt},
				Launch:     &integration.LaunchSpec{Subcommand: "codex"},
			},
			want: "full+seeded",
		},
		{
			name: "doc-assisted (hermes shape)",
			in: integration.HandoffCapability{
				Transcript: integration.TranscriptFull,
				Inject:     []integration.InjectKind{integration.InjectFile, integration.InjectMCP},
				Launch:     &integration.LaunchSpec{Subcommand: "hermes", Mode: integration.LaunchDocAssisted},
			},
			want: "full+doc-assisted",
		},
		{
			name: "transcript-only, not launchable (cline shape)",
			in: integration.HandoffCapability{
				Transcript: integration.TranscriptFull,
				Inject:     []integration.InjectKind{integration.InjectFile, integration.InjectMCP},
			},
			want: "full",
		},
		{
			name: "partial transcript + seeded (antigravity-cli shape)",
			in: integration.HandoffCapability{
				Transcript: integration.TranscriptPartial,
				Inject:     []integration.InjectKind{integration.InjectFile, integration.InjectPrompt},
				Launch:     &integration.LaunchSpec{Subcommand: "antigravity-cli"},
			},
			want: "partial+seeded",
		},
		{
			name: "partial transcript-only (antigravity desktop shape)",
			in: integration.HandoffCapability{
				Transcript: integration.TranscriptPartial,
				Inject:     []integration.InjectKind{integration.InjectFile},
			},
			want: "partial",
		},
		{
			name: "zero value (actions-only carry floor)",
			in:   integration.HandoffCapability{},
			want: "—",
		},
	}
	for _, tc := range cases {
		if got := handoffCell(tc.in); got != tc.want {
			t.Errorf("%s: handoffCell() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestRoutabilityCell(t *testing.T) {
	cases := map[integration.RouteStatus]string{
		integration.RouteStatusUnknown:       "—",
		integration.RouteStatusRoutableNow:   "routable",
		integration.RouteStatusAfterUpstream: "after-upstream",
		integration.RouteStatusAfterBridge:   "after-bridge",
		integration.RouteStatusProbeRequired: "probe",
		integration.RouteStatusNativeExempt:  "native-exempt",
	}
	for in, want := range cases {
		if got := routabilityCell(in); got != want {
			t.Errorf("routabilityCell(%q) = %q, want %q", in, got, want)
		}
	}
}
