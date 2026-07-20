package managedsettings

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// CodexMCPServerName is the key Codex uses for Observer's MCP server under
// [mcp_servers.<name>] in config.toml / managed_config.toml. If the enterprise
// requirements.toml restricts which MCP servers users may enable, this name
// must be on the allowlist or Codex refuses to start it.
const CodexMCPServerName = "superbased-observer"

// DefaultCodexProxyBaseURL is the openai_base_url default — Observer's proxy on
// its default port with the /v1 suffix Codex's OpenAI client expects. Codex
// 0.130+ silently drops the launcher's `-c openai_base_url` override, so the
// file-level value is the only one that reaches the inner app-server (V6-2).
const DefaultCodexProxyBaseURL = "http://127.0.0.1:8820/v1"

// CodexOptions configures Codex managed-config artifact generation (Rail B).
//
// Two artifacts mirror Codex's two-layer policy model:
//   - managed_config.toml — DEFAULTS the user may change in-session (reapplied
//     next launch; overrides even CLI --config). The high-confidence artifact.
//   - requirements.toml — ENFORCED rules (a conflicting user value falls back to
//     a compatible one + notifies the user). Conservative in v1: only settings
//     whose exact keys are confirmed from Codex's config-reference are emitted
//     un-commented; keys the Phase-0 findings could not pin down ship as flagged,
//     commented templates rather than invented names.
type CodexOptions struct {
	// OpenAIBaseURL is the proxy route written as managed_config.toml's
	// top-level openai_base_url. Required when IncludeProxyRoute is set.
	OpenAIBaseURL string
	// OTelEndpoint, when non-empty, emits the optional [otel] redirect block
	// (Rail A — points Codex's native OpenTelemetry at Observer's receiver).
	// The exact [otel] nesting needs live confirmation against the
	// config-reference; the block carries a VERIFY note when emitted.
	OTelEndpoint string
	// MCPPackage overrides DefaultMCPPackage (e.g. a pinned version). Optional.
	MCPPackage string
	// IncludeMCP emits the [mcp_servers.superbased-observer] pin (+ the
	// requirements MCP-allowlist template). IncludeProxyRoute emits
	// openai_base_url + [features].hooks. At least one must be set.
	IncludeMCP        bool
	IncludeProxyRoute bool
	// EnforceManagedHooksOnly emits `allow_managed_hooks_only = true` in
	// requirements.toml (ignore user/project/session hooks, still run managed
	// hooks). Off by default — it is a strong enforcement that disables a
	// user's own hooks, so it is opt-in.
	EnforceManagedHooksOnly bool
}

// CodexArtifacts is the generated Codex managed-config output: deploy-ready TOML
// bytes (nil when not requested) plus a README describing the enforced-vs-default
// split, the MCP-pin-≠-telemetry distinction, and the deploy paths.
type CodexArtifacts struct {
	ManagedConfigTOML []byte // managed_config.toml (defaults)
	RequirementsTOML  []byte // requirements.toml (enforced)
	Readme            string
}

// GenerateCodex builds the Codex managed-config artifacts. It validates that a
// proxy base URL is present when the proxy route is requested, and that at least
// one artifact section is selected.
func GenerateCodex(opts CodexOptions) (CodexArtifacts, error) {
	if !opts.IncludeMCP && !opts.IncludeProxyRoute {
		return CodexArtifacts{}, fmt.Errorf("managedsettings: nothing to emit (IncludeMCP and IncludeProxyRoute both false)")
	}
	if opts.IncludeProxyRoute && strings.TrimSpace(opts.OpenAIBaseURL) == "" {
		return CodexArtifacts{}, fmt.Errorf("managedsettings: OpenAIBaseURL is required when IncludeProxyRoute is set")
	}
	pkg := opts.MCPPackage
	if pkg == "" {
		pkg = DefaultMCPPackage
	}

	var out CodexArtifacts
	out.ManagedConfigTOML = []byte(codexManagedConfig(opts, pkg))
	out.RequirementsTOML = []byte(codexRequirements(opts))
	out.Readme = codexReadme(opts, pkg)
	return out, nil
}

// RequirementsTOMLBase64 returns the base64 of the requirements.toml artifact,
// the value an MDM pushes as `requirements_toml_base64`. Returns "" when no
// requirements artifact was generated.
func (a CodexArtifacts) RequirementsTOMLBase64() string {
	if len(a.RequirementsTOML) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(a.RequirementsTOML)
}

// codexManagedConfig builds managed_config.toml (the DEFAULTS layer). Emitted as
// an annotated, stably-ordered string so the file reads cleanly and diffs across
// regenerations — a map encode would lose the comments and the ordering.
func codexManagedConfig(opts CodexOptions, pkg string) string {
	var b strings.Builder
	b.WriteString("# Observer-managed Codex defaults (managed_config.toml).\n")
	b.WriteString("# These are DEFAULTS a user may change in-session; Codex reapplies them on\n")
	b.WriteString("# next launch and they override even CLI --config. Deploy per the README.\n\n")

	if opts.IncludeProxyRoute {
		b.WriteString("# Route Codex's OpenAI traffic through Observer's proxy for accurate token\n")
		b.WriteString("# capture. This MUST live in the file: Codex 0.130+ drops the launcher's\n")
		b.WriteString("# `-c openai_base_url` override before the inner app-server makes the call\n")
		b.WriteString("# (docs/codex-shared-app-server-gotcha.md, V6-2).\n")
		b.WriteString(fmt.Sprintf("openai_base_url = %q\n\n", opts.OpenAIBaseURL))

		b.WriteString("# Enable Codex hooks so Observer captures session/permission/user-prompt rows.\n")
		b.WriteString("[features]\n")
		b.WriteString("hooks = true\n\n")
	}

	if opts.IncludeMCP {
		b.WriteString("# Pin Observer's MCP server for in-session project-knowledge tools. The npx\n")
		b.WriteString("# bootstrap self-installs the matching platform binary per node. NOTE: this\n")
		b.WriteString("# pin delivers in-session TOOLS, NOT telemetry — see the README.\n")
		b.WriteString(fmt.Sprintf("[mcp_servers.%s]\n", CodexMCPServerName))
		b.WriteString("command = \"npx\"\n")
		b.WriteString(fmt.Sprintf("args = [\"-y\", %q, \"serve\"]\n\n", pkg))
	}

	if strings.TrimSpace(opts.OTelEndpoint) != "" {
		// Emitted COMMENTED on purpose: Rail A is gated on a live OTLP capture,
		// and Observer's Phase-0 research confirmed the [otel] surface exists but
		// not the exact nested key names. Shipping an active block risks invalid
		// TOML / a silently-wrong redirect, so the admin uncomments after
		// confirming the shape against Codex's config-reference. THIS block (not
		// the MCP pin) is what would deliver native per-turn telemetry.
		b.WriteString("# (Rail A, optional — UNCOMMENT after confirming key nesting against Codex's\n")
		b.WriteString("# config-reference) Redirect Codex's native OpenTelemetry to Observer's OTLP\n")
		b.WriteString("# receiver. THIS is what delivers per-turn telemetry (the MCP pin above does not).\n")
		b.WriteString("# [otel]\n")
		b.WriteString("# exporter = \"otlp-http\"\n")
		b.WriteString("# [otel.exporter.otlp-http]\n")
		b.WriteString(fmt.Sprintf("# endpoint = %q\n\n", opts.OTelEndpoint))
	}

	return b.String()
}

// codexRequirements builds requirements.toml (the ENFORCED layer). Conservative:
// only confirmed keys are emitted live; unconfirmed enforcement (the MCP
// allowlist key) ships commented with a VERIFY note so an admin opts in
// deliberately rather than relying on an invented key that silently no-ops.
func codexRequirements(opts CodexOptions) string {
	var b strings.Builder
	b.WriteString("# Observer-managed Codex requirements (requirements.toml) — ENFORCED.\n")
	b.WriteString("# When a user's config/profile/CLI conflicts with a rule here, Codex falls\n")
	b.WriteString("# back to a compatible value and notifies the user. Deploy per the README\n")
	b.WriteString("# (system path /etc/codex/requirements.toml or MDM requirements_toml_base64).\n\n")

	if opts.EnforceManagedHooksOnly {
		b.WriteString("# Run ONLY managed hooks; ignore user/project/session hooks. Strong — it\n")
		b.WriteString("# disables a user's own hooks. Enabled because --enforce-managed-hooks was set.\n")
		b.WriteString("allow_managed_hooks_only = true\n\n")
	} else {
		b.WriteString("# Optional: run only managed hooks (ignore user/project/session hooks).\n")
		b.WriteString("# Uncomment to enforce. Strong — it disables a user's own hooks.\n")
		b.WriteString("# allow_managed_hooks_only = true\n\n")
	}

	if opts.IncludeMCP {
		b.WriteString("# Optional MCP allowlist enforcement: restrict which MCP servers users may\n")
		b.WriteString("# enable. Codex's config-reference documents this enforcement, but the exact\n")
		b.WriteString("# key was not pinned in Observer's Phase-0 research — CONFIRM the key name\n")
		b.WriteString("# against the config-reference before relying on it. Until then,\n")
		b.WriteString(fmt.Sprintf("# managed_config.toml's [mcp_servers.%s] ships the server as a DEFAULT pin.\n", CodexMCPServerName))
		b.WriteString(fmt.Sprintf("# allowed_mcp_servers = [%q]\n", CodexMCPServerName))
	}

	return b.String()
}

// codexReadme explains the two-artifact deploy + the MCP-vs-telemetry split.
func codexReadme(opts CodexOptions, pkg string) string {
	var b strings.Builder
	b.WriteString("# Observer managed-config for OpenAI Codex\n\n")
	b.WriteString("Two artifacts, two layers of Codex's policy model:\n\n")
	b.WriteString("- **managed_config.toml** — DEFAULTS the user may change in-session (Codex\n")
	b.WriteString("  reapplies them next launch; they override even CLI `--config`). This is the\n")
	b.WriteString("  artifact that wires Observer up.\n")
	b.WriteString("- **requirements.toml** — ENFORCED rules. A conflicting user value falls back\n")
	b.WriteString("  to a compatible one and the user is notified.\n\n")

	if opts.IncludeMCP {
		b.WriteString(fmt.Sprintf(
			"## MCP pin ≠ telemetry\n\n"+
				"`managed_config.toml`'s `[mcp_servers.%s]` (`npx -y %s serve`) gives\n"+
				"in-session TOOL presence. It does **NOT** deliver telemetry. If your\n"+
				"`requirements.toml` restricts which MCP servers users may enable, add `%s`\n"+
				"to that allowlist or Codex refuses to start it.\n\n",
			CodexMCPServerName, pkg, CodexMCPServerName,
		))
	}
	if opts.IncludeProxyRoute {
		b.WriteString(fmt.Sprintf(
			"## Telemetry delivery\n\n"+
				"Per-turn token capture comes from the **proxy route** (`openai_base_url = %q`)\n"+
				"plus `[features].hooks`. The proxy must be running and reachable. (Codex\n"+
				"0.130+ drops the launcher's `-c openai_base_url`; the file value is what works.)\n\n",
			opts.OpenAIBaseURL,
		))
	}
	if strings.TrimSpace(opts.OTelEndpoint) != "" {
		b.WriteString(fmt.Sprintf(
			"An optional `[otel]` block redirects Codex's **native** OpenTelemetry to\n"+
				"Observer's OTLP receiver (%s) — Rail A. VERIFY the exact `[otel]` key nesting\n"+
				"against Codex's config-reference before fleet deploy.\n\n",
			opts.OTelEndpoint,
		))
	}

	b.WriteString("## Deploy\n\n")
	b.WriteString("- **Single node (no license):** write `requirements.toml` to\n")
	b.WriteString("  `/etc/codex/requirements.toml` and `managed_config.toml` next to the user's\n")
	b.WriteString("  Codex config (or the system config dir). Verify Codex picks them up on launch.\n")
	b.WriteString("- **Fleet (MDM):** push `requirements.toml` as the `requirements_toml_base64`\n")
	b.WriteString("  value — base64 of the file (the CLI prints it with `--print-mdm-base64`).\n")
	b.WriteString("- **Precedence (highest→lowest):** cloud-managed requirements → MDM\n")
	b.WriteString("  `requirements_toml_base64` → system `/etc/codex/requirements.toml`.\n")
	return b.String()
}
