package integration

// RouteKind names HOW a proxy route is applied — the capability shape the
// init proxy-route step dispatches on (CLAUDE.md #3), distinguishing a
// persisted config write from an ephemeral launcher env var.
type RouteKind string

const (
	// RouteLauncher: the base-URL env var is exported at exec time by the
	// `observer <x>` launcher; there is NO persisted per-tool config file to
	// write (opencode). routeSupported is false for these — the launcher,
	// not init, applies the route.
	RouteLauncher RouteKind = "launcher"
	// RouteEnvSettings: the base-URL env var is persisted into the client's
	// own settings file (claude-code → ~/.claude/settings.json "env").
	RouteEnvSettings RouteKind = "env_settings"
	// RouteConfigFile: the base URL is written into the client's config file
	// under a vendor-specific key (codex → ~/.codex/config.toml
	// openai_base_url).
	RouteConfigFile RouteKind = "config_file"
	// RouteProviderJSON: the base URL is written into a provider entry in a
	// JSON config the tool reads (openclaw/pi models.json baseUrl). Declared
	// in Phase 0; its writer lands in Phase B (gated on a live probe).
	RouteProviderJSON RouteKind = "provider_json"
	// RouteVSCodeSettings: the base URL is written into a VS Code extension's
	// settings (cline/roo/kilo "OpenAI Compatible" Base URL). Declared in
	// Phase 0; writer lands in Phase B.
	RouteVSCodeSettings RouteKind = "vscode_settings"
	// RouteManual: observer cannot safely auto-edit the client, so the
	// `observer <tool> --setup` launcher PRINTS the base-URL settings for the
	// operator to paste (cursor/copilot VS Code BYOK). The ProxyRoute carries
	// the URL to print; init never auto-writes these (routeSupported excludes
	// them). Declared in Phase 0; consumed in Phase B.
	RouteManual RouteKind = "manual_instructions"
)

// RouteStatus is the surface-specific routability bucket for an adapter —
// the honest answer to "is this tool's model traffic routable through the
// observer proxy?", INDEPENDENT of whether observer auto-applies the route
// today (that is the Proxy field: non-nil ⇒ observer drives a route now).
//
// It retires the old "permanently impossible" framing (operator directive
// 2026-06-26): native hosted/proprietary traffic is exempt, but a tool's
// BYOK / custom-base-URL surface is frequently routable. Buckets mirror
// docs/audits/notes-on-proxy.md and are grounded against adapter code +
// the live 2026-06-26 8-tool run. The zero value (RouteStatusUnknown) means
// "not yet classified", never "impossible".
type RouteStatus string

const (
	// RouteStatusUnknown: not yet classified against a surface.
	RouteStatusUnknown RouteStatus = ""
	// RouteStatusRoutableNow: an OpenAI/Anthropic-shaped base-URL knob exists
	// that observer can drive. The Proxy field shows whether observer applies
	// it today (claude-code/codex/opencode) or whether the writer is still
	// pending (cline/kilo VS Code surface — knob exists, Phase-B writer).
	RouteStatusRoutableNow RouteStatus = "routable_now"
	// RouteStatusAfterUpstream: routable only after the proxy gains
	// per-provider upstream selection (hermes → OpenRouter; Phase C).
	RouteStatusAfterUpstream RouteStatus = "after_upstream"
	// RouteStatusAfterBridge: routable only after a request/response protocol
	// bridge (gemini-cli generateContent; Phase E).
	RouteStatusAfterBridge RouteStatus = "after_bridge"
	// RouteStatusProbeRequired: a BYOK / custom-base-URL path is documented
	// but unconfirmed on a live install — confirm with a live turn before
	// flipping the Proxy field on (cline-cli, openclaw, pi, cursor-BYOK,
	// copilot-cli/VS Code BYOK, cowork third-party gateway).
	RouteStatusProbeRequired RouteStatus = "probe_required"
	// RouteStatusNativeExempt: no routable surface found — the tool talks
	// only to its own backend with no base-URL knob (antigravity, kilo-cli
	// per the live 2026-06-26 finding, cowork's local microVM path). A
	// grounded negative, not "permanently impossible": a future version or a
	// new BYOK surface can reclassify it.
	RouteStatusNativeExempt RouteStatus = "native_exempt"
)

// ProxyRoute describes how a proxy-routable adapter is pointed at the
// observer proxy. Most adapters are captured by the watcher/hooks and are
// NOT proxy-routable (they talk only to their vendor's own backend, with
// no base-URL knob) — those carry a nil Proxy on their Capability, which
// is data, not a missing feature.
type ProxyRoute struct {
	// Kind is how the route is applied (persisted config write vs launcher
	// env var). init only writes config for the persisted kinds.
	Kind RouteKind
	// EnvVar is the base-URL env var that routes the tool at the proxy
	// (e.g. ANTHROPIC_BASE_URL, OPENAI_BASE_URL), or "" when the tool
	// routes via a config file instead (see Note).
	EnvVar string
	// Suffix is appended to the proxy URL: "/v1" for OpenAI-compatible
	// endpoints, "" for Anthropic's ANTHROPIC_BASE_URL.
	Suffix string
	// Launcher is the `observer <x>` command that wires the routing.
	Launcher string
	// Note documents config-file-routed tools (EnvVar == ""), where the
	// base URL lives in a config file rather than an env var.
	Note string
}

// Capability is one adapter's row in the registry: everything observer
// knows, as DATA, about how this tool can integrate with the proxy / hooks
// / MCP / native-console / token capture. Consumers gate on the SHAPE of
// the field they need (Proxy != nil, Hook.Mechanism != HookNone, MCP != nil
// …), never on Tool name (CLAUDE.md rule #3). A zero-value Capability is
// safe to pass around: every field reads as "no grounded capability".
//
// Population status (2026-06-26): Proxy seeded Phase 0; Hook / MCP / Native
// / TokenTier filled by the capability-discovery spike, code-grounded with
// honest zero values where a cell could not be grounded (see capability.go).
// Consumers (init, register, doctor) are wired phase-by-phase; the fields
// are inert data until then.
type Capability struct {
	Tool  string
	Proxy *ProxyRoute
	// Routability is the surface-specific bucket (RouteStatus): whether the
	// tool is routable at all, independent of whether observer drives a route
	// today (Proxy). A row can have Proxy==nil yet Routability != exempt — the
	// surface exists but observer's writer/upstream support is still pending.
	Routability RouteStatus
	Hook        HookSpec
	MCP         *MCPTarget
	Native      NativeRails
	TokenTier   TokenTier
}

// registry is the capability table, keyed by the adapter's canonical tool
// name (matching adapter.Adapter.Name() and the EnabledAdapters list). It
// covers all 16 registered adapters; the doctor (and, later, the matrix)
// render from it. Every cell is code-grounded — a zero value means "no
// grounded capability" (genuinely unsupported OR pending a discovery spike
// against a live install), distinguished by the per-row comment, never a
// fabricated capability. An absent tool resolves via For to a zero-value
// Capability that still echoes the Tool name.
var registry = map[string]Capability{
	// Full-capability flagships: proxy + hook + MCP + all native rails.
	"claude-code": {
		Tool:        "claude-code",
		Proxy:       &ProxyRoute{Kind: RouteEnvSettings, EnvVar: "ANTHROPIC_BASE_URL", Suffix: "", Launcher: "observer claude"},
		Routability: RouteStatusRoutableNow,
		Hook:        HookSpec{Mechanism: HookClaudeSettings, CrossOSBridge: true, AutoWired: true},
		MCP:         &MCPTarget{Format: MCPServersJSON, PathHint: ".claude.json", Implemented: true},
		Native:      NativeRails{A: true, B: true, C: true},
		TokenTier:   TokenTier{Best: "proxy"},
	},
	"codex": {
		Tool:        "codex",
		Proxy:       &ProxyRoute{Kind: RouteConfigFile, EnvVar: "", Launcher: "observer codex", Note: "codex routes through ~/.codex/config.toml openai_base_url (not an env var)"},
		Routability: RouteStatusRoutableNow,
		Hook:        HookSpec{Mechanism: HookCodexConfig, AutoWired: true},
		MCP:         &MCPTarget{Format: MCPCodexTOML, PathHint: ".codex/config.toml", Implemented: true},
		Native:      NativeRails{A: true, B: true, C: true, Note: "Rail A (usage-export) config-gated on live keys"},
		TokenTier:   TokenTier{Best: "proxy"},
	},

	// Proxy-routable CLI (OpenAI-compatible base URL via launcher).
	// LIVE-VERIFIED 2026-06-27: `observer opencode -- run …` routed a
	// gpt-5.4-nano turn through the proxy (api_turns grew).
	"opencode": {
		Tool:        "opencode",
		Proxy:       &ProxyRoute{Kind: RouteLauncher, EnvVar: "OPENAI_BASE_URL", Suffix: "/v1", Launcher: "observer opencode"},
		Routability: RouteStatusRoutableNow,
		Hook:        HookSpec{Mechanism: HookNone},
		// OpenCode hosts MCP under its own "mcp" object in
		// ~/.config/opencode/opencode.json (live-grounded 2026-06-26 against
		// the operator's install: {"type":"local","command":[…],"enabled"}),
		// written globally by registerOpenCodeJSON.
		MCP:       &MCPTarget{Format: MCPOpenCodeJSON, PathHint: ".config/opencode/opencode.json", Implemented: true},
		Native:    NativeRails{},
		TokenTier: TokenTier{Best: "sqlite"},
	},

	// IDE/extension adapters that talk only to their own backend → no proxy
	// route (Proxy=nil is DATA, not a missing feature). Hooks/MCP per tool.
	"cursor": {
		Tool: "cursor",
		// Surface-split (2026-06-26): the NATIVE Cursor backend has no base-URL
		// knob (exempt), but Cursor's custom "OpenAI Base URL" / BYOK model
		// setting MAY route through observer — unconfirmed on a live install,
		// so probe before flipping Proxy on. Proxy stays nil (observer drives
		// no route today); the surface is recorded in Routability, not faked.
		Proxy:       nil,
		Routability: RouteStatusProbeRequired,
		Hook:        HookSpec{Mechanism: HookCursor, CrossOSBridge: true, AutoWired: true},
		MCP:         &MCPTarget{Format: MCPServersJSON, PathHint: ".cursor/mcp.json", Implemented: true},
		Native:      NativeRails{}, // business admin/usage API not yet investigated (Phase-4 ledger).
		// Auto-mode "default" model is now resolved from store.db turn blobs
		// (providerOptions.cursor.modelName) at hook time — see
		// cursor.ResolveModelFromStore. Tokens still depend on the stop hook
		// firing (the transcript carries no usage).
		TokenTier: TokenTier{Best: "sqlite", Gap: "tokens require the stop hook (transcript has none)"},
	},
	"cline": {
		Tool: "cline",
		// VS Code extension → backend. The "OpenAI Compatible" Base URL surface
		// is routable, but it is a MANUAL-PASTE route, not an auto-writer:
		// live-grounded 2026-06-27, Cline stores its provider/base-URL config
		// in VS Code's globalState (state.vscdb) + SecretStorage, NOT a JSON
		// file (the globalStorage settings dir holds only cline_mcp_settings
		// .json; the claude-dev globalState key held only welcomeViewCompleted,
		// no openAiBaseUrl anywhere). Writing state.vscdb while VS Code runs is
		// unsafe — its in-memory cache overwrites external writes on exit. So
		// the route is `observer`-printed instructions to paste into the
		// extension UI (RouteManual), surfaced via proxyroute.VSCodeBaseURLHint.
		Proxy:       nil,
		Routability: RouteStatusRoutableNow,
		Hook:        HookSpec{Mechanism: HookNone},
		// Cline (saoudrizwan.claude-dev) hosts MCP in VS Code globalStorage
		// cline_mcp_settings.json (standard {"mcpServers":{…}} shape, live-
		// confirmed 2026-06-26: {"mcpServers":{}}). Written natively on the
		// daemon OS (locate "cline") AND cross-OS into a Windows VS Code from
		// a WSL daemon via the cline-windows wsl.exe bridge (CrossOSBridge).
		MCP:       &MCPTarget{Format: MCPServersJSON, PathHint: "<vscode>/User/globalStorage/saoudrizwan.claude-dev/settings/cline_mcp_settings.json", Implemented: true, CrossOSBridge: true},
		Native:    NativeRails{},
		TokenTier: TokenTier{Best: "transcript"}, // per-message metrics + modelInfo; full.
	},
	"copilot": {
		Tool: "copilot",
		// VS Code extension → GitHub-hosted backend (native traffic exempt),
		// but VS Code's custom-endpoint / BYOK model support MAY route — probe
		// before flipping. Native hosted + inline completions stay exempt.
		Proxy:       nil,
		Routability: RouteStatusProbeRequired,
		Hook:        HookSpec{Mechanism: HookNone},
		MCP:         nil,
		Native:      NativeRails{A: true, B: true, C: true, Note: "rails partial; identity = GitHub login, cost seat/account-level"},
		TokenTier:   TokenTier{Best: "events_jsonl", Gap: "no cache tier"},
	},
	"copilot-cli": {
		Tool: "copilot-cli",
		// BYOK path: COPILOT_PROVIDER_BASE_URL/_TYPE/_API_KEY + COPILOT_MODEL →
		// OpenAI-compatible endpoint (GitHub Docs); native GitHub-hosted
		// routing stays exempt.
		// LIVE-VERIFIED 2026-06-27: `observer copilot-cli` (launcher sets
		// COPILOT_PROVIDER_BASE_URL=<proxy>/v1 + _TYPE=openai) with the
		// operator's COPILOT_PROVIDER_API_KEY + --model gpt-4o routed a real
		// turn through the proxy (api_turns provider=openai, gpt-4o-2024-08-06)
		// AND was compressed (4 tools-trim compression_events). The launcher
		// NEVER sets the key — that's the operator's BYOK env. Proxy is the
		// launcher route (mirrors opencode); init does not auto-write it.
		Proxy:       &ProxyRoute{Kind: RouteLauncher, EnvVar: "COPILOT_PROVIDER_BASE_URL", Suffix: "/v1", Launcher: "observer copilot-cli"},
		Routability: RouteStatusRoutableNow,
		Hook:        HookSpec{Mechanism: HookNone},
		MCP:         nil,
		Native:      NativeRails{}, // shares Copilot's GitHub governance but no separate node rail grounded.
		// Captures cache read + creation and nets input (log.go). Live
		// 2026-06-26 grounding: session.shutdown carries the FULL
		// session-aggregate input/cache/cost WITHOUT --log-level debug
		// (modelMetrics.<model>.usage); debug only adds PER-TURN input/cache
		// attribution (plain turns are output-only). The "no cache tier" the
		// audit attributed here was VS Code copilot's, not the CLI's.
		TokenTier: TokenTier{Best: "events_jsonl", Gap: "per-turn input/cache attribution needs --log-level debug (session-aggregate captured without it)"},
	},
	"kilo-code": {
		Tool: "kilo-code",
		// Legacy IDE extension (wraps cline). Same "OpenAI Compatible" Base URL
		// surface as cline VS Code, and the same MANUAL-PASTE reality: the
		// base URL lives in live VS Code globalState (state.vscdb), not a
		// writable JSON, so the route is operator-pasted instructions
		// (RouteManual / proxyroute.VSCodeBaseURLHint), not an auto-writer.
		Proxy:       nil,
		Routability: RouteStatusRoutableNow,
		Hook:        HookSpec{Mechanism: HookNone},
		MCP:         nil,
		Native:      NativeRails{},
		TokenTier:   TokenTier{Best: "sqlite"}, // inherits cline's full per-message capture.
	},
	"kilo-code-cli": {
		Tool: "kilo-code-cli",
		// Native-exempt per the live 2026-06-26 finding: @kilocode/cli has no
		// base-URL env handling and talks to the api.kilo.ai gateway directly
		// (docs/kilo-code-adapter.md). A grounded negative, not "permanently
		// impossible" — a future @kilocode/cli custom-provider knob would
		// reclassify it; a re-probe is optional/low-priority.
		Proxy:       nil,
		Routability: RouteStatusNativeExempt,
		Hook:        HookSpec{Mechanism: HookNone},
		MCP:         nil,
		Native:      NativeRails{},
		// Per-message tokens NET (Anthropic-shape, verified). kilo-auto/* has
		// explicit pricing aliases; stealth/* gateway models price via the
		// cost engine's provider-segment strip (stealth/claude-sonnet-4.6 →
		// claude-sonnet-4.6). cachetrack shape rule now covers stealth/claude
		// explicitly (Anthropic-shape) alongside kilo-auto.
		TokenTier: TokenTier{Best: "sqlite"},
	},

	// CLI adapters captured via watcher/SQLite (+ opt-in receivers).
	"cline-cli": {
		Tool: "cline-cli",
		// ROUTABLE via the openai-compatible provider's persisted baseUrl —
		// VERIFIED LIVE 2026-06-27. The NATIVE `openai` provider hardcodes
		// api.openai.com and ignores OPENAI_BASE_URL (confirmed: `-P openai -k …`
		// succeeded but bypassed the proxy), so the old env launcher was inert.
		// But cline's `openai-compatible` provider reads an explicit
		// `settings.baseUrl` from ~/.cline/data/settings/providers.json (cline
		// auth `-b/--baseurl`). The `observer cline-cli` launcher now writes that
		// baseUrl → the proxy (preserving the api key, NEVER writing one) and
		// execs `cline -P openai-compatible`. A live turn landed a real api_turn
		// (provider=openai, gpt-4o-2024-08-06, HTTP 200). RouteProviderJSON; the
		// operator supplies the key once via `cline auth openai-compatible -k …`.
		// (docs/proxy-routing-blockers.md)
		Proxy:       &ProxyRoute{Kind: RouteProviderJSON, EnvVar: "", Suffix: "/v1", Launcher: "observer cline-cli", Note: "routes via the openai-compatible provider's settings.baseUrl in ~/.cline/data/settings/providers.json; launcher writes baseUrl, never a key"},
		Routability: RouteStatusRoutableNow,
		Hook:        HookSpec{Mechanism: HookClineCLIJSONL, AutoWired: false}, // receiver exists (clinecli/hook.go); the live hooks.jsonl is lifecycle-only (no token payload) → stays a tailer, not auto-wired.
		// MCP: NO cline_mcp_settings.json on the live install (the audit's
		// assumption was wrong); the settings dir holds only providers.json +
		// cli-notices.json. No grounded MCP target.
		MCP:       nil,
		Native:    NativeRails{},
		TokenTier: TokenTier{Best: "sqlite"}, // sessions.db + per-session messages.json; full.
	},
	"hermes": {
		Tool: "hermes",
		// Proxy BLOCKED at the proxy-upstream layer, not a writer gap (live-
		// grounded 2026-06-26). Hermes' only base-URL knob is model.base_url
		// in ~/.hermes/config.yaml, live-set to https://openrouter.ai/api/v1
		// (OpenAI-shaped via OpenRouter). The observer proxy forwards ALL
		// OpenAI-shaped traffic to a single fixed upstream (proxy.go
		// upstreamForPath → openaiURL, default api.openai.com) with no
		// OpenRouter target and no per-request upstream selection. Pointing
		// hermes at the proxy would misroute its OpenRouter-bound traffic to
		// api.openai.com (wrong host/key/models) and break the session. The
		// YAML writer is trivial; making hermes routable needs proxy
		// per-provider upstream routing (a hot-path change), so this stays
		// nil until that lands. (docs/hermes-adapter.md)
		//
		// UPDATE — Phase C shipped + LIVE-VERIFIED 2026-06-27: the /up/<id>
		// seam + [proxy.upstreams] openrouter route hermes' OpenRouter traffic;
		// proxyroute.RegisterHermes rewrites model.base_url →
		// http://127.0.0.1:<port>/up/openrouter/api/v1. A live `hermes -z` turn
		// routed to OpenRouter (confirmed via OpenRouter-specific responses)
		// and landed an api_turns row as provider=openai with the OpenRouter
		// model name (tokens were 0 only because the free tier was rate-limited
		// — error responses carry no usage; the parse path is covered by the
		// proxy e2e test). Proxy stays nil because init does not auto-write it
		// (the RegisterHermes writer + a node [proxy.upstreams] entry apply it),
		// but the surface is verified-routable.
		// ROUTING MECHANISM VERIFIED LIVE 2026-06-27. hermes' NAMED providers
		// (openrouter, nous) hardcode their endpoint via `base_url = base_url or
		// CONST` and IGNORE model.base_url — so `-z`/`chat` under provider:
		// openrouter bypass the proxy. BUT the built-in `custom` provider DOES
		// honor model.base_url (loopback-trusted). Setting model.provider: custom
		// + model.base_url: <proxy>/up/openrouter/api/v1 + an OpenRouter key
		// routed a live `hermes chat` turn through the proxy (api_turn
		// provider=openai, nvidia/nemotron-…:free, HTTP 200).
		// SECRET-FREE AUTO-WRITER SHIPPED + LIVE-VERIFIED 2026-06-27 (Approach
		// B): hermes' top-level `--provider` flag accepts a name from the
		// config's `providers:` section, and a user-config provider entry
		// resolves its key via `key_env` (the env var NAME — providers.py
		// resolve_user_provider). The `observer hermes` launcher
		// (cmd/observer/hermes.go) ADDITIVELY writes a `providers.observer`
		// entry {base_url: <proxy>/up/<upstream>/api/v1, key_env:
		// OPENROUTER_API_KEY, transport: openai_chat} — touching ONLY that
		// entry, so the operator's top-level model block is preserved — then
		// execs `hermes --provider observer`. NEVER writes a key (key_env is
		// the env-var name; the operator exports the credential). Two live
		// turns confirmed it: the key_env config probe AND the launcher's own
		// write each landed an api_turns row (provider=openai,
		// nvidia/nemotron-…:free, ~16.7k input). RouteProviderJSON; init does
		// NOT auto-write it (the launcher does), and routing needs a matching
		// [proxy.upstreams] entry (default `openrouter`). NB: hermes'
		// auxiliary/moa providers (provider: auto) make separate calls that
		// don't follow the override. (docs/proxy-routing-blockers.md)
		Proxy:       &ProxyRoute{Kind: RouteProviderJSON, EnvVar: "", Suffix: "/up/openrouter/api/v1", Launcher: "observer hermes", Note: "routes via a user-config `observer` provider (providers: section) in ~/.hermes/config.yaml with key_env (secret-free); launcher writes the provider additively, never a key; needs a matching [proxy.upstreams] upstream (default openrouter)"},
		Routability: RouteStatusRoutableNow,
		Hook:        HookSpec{Mechanism: HookHermesPlugin, AutoWired: true}, // embedded plugin via `observer init --hermes`.
		MCP:         &MCPTarget{Format: MCPHermesYAML, PathHint: ".hermes/config.yaml", Implemented: true},
		Native:      NativeRails{},
		TokenTier:   TokenTier{Best: "sqlite"}, // post_api_request token rows; full.
	},
	"cowork": {
		Tool: "cowork",
		// The LOCAL-observer path is native-exempt: the microVM sandbox can't
		// reach 127.0.0.1:8820, app JS is ACL-locked, and the only base-URL
		// levers are machine-wide (docs/cowork-adapter.md). A third-party
		// remote inference gateway is a SEPARATE (non-local) surface that may
		// be configurable — out of observer's local-proxy scope, so probe it
		// before any claim. Bucketed probe-required to reflect that surface.
		Proxy:       nil,
		Routability: RouteStatusProbeRequired,
		Hook:        HookSpec{Mechanism: HookNone},
		MCP:         nil,
		Native:      NativeRails{},
		TokenTier:   TokenTier{Best: "transcript"}, // un-audited depth.
	},
	"gemini-cli": {
		Tool: "gemini-cli",
		// Phase E SHIPPED + LIVE-VERIFIED 2026-06-27: the proxy bridges Google
		// generateContent (providerForPath → ProviderGoogle, the
		// generativelanguage upstream, parseGeminiResponse/parseGeminiStream
		// usageMetadata) and `observer gemini` sets GOOGLE_GEMINI_BASE_URL (no
		// /v1 suffix — the CLI appends the /v1beta path). A live `observer
		// gemini -- -p …` turn produced google api_turns rows (gemini-3.5-flash
		// 11092/132) with accurate token capture.
		Proxy:       &ProxyRoute{Kind: RouteLauncher, EnvVar: "GOOGLE_GEMINI_BASE_URL", Suffix: "", Launcher: "observer gemini"},
		Routability: RouteStatusRoutableNow,
		Hook:        HookSpec{Mechanism: HookNone},
		MCP:         nil,
		Native:      NativeRails{},                   // Google Cloud usage API not yet investigated (Phase-4 ledger).
		TokenTier:   TokenTier{Best: "events_jsonl"}, // gross-input netting fixed (tokenEventFor nets cached); no known gap.
	},
	"openclaw": {
		Tool: "openclaw",
		// Live-grounded 2026-06-26 (this WSL install): OpenClaw's bundled
		// `openai` plugin reads OPENAI_BASE_URL / OPENAI_API_BASE
		// (plugin-runtime-deps/.../extensions/openai), and the operator's
		// default model is on the `openai` provider — so the `observer
		// openclaw` launcher's env redirect routes real traffic. NOT a
		// models.json writer (no such file/key exists here; openclaw.json has
		// no provider baseUrl). The `openai-codex` provider is OAuth, env-
		// immune. Proxy stays nil until a live turn confirms api_turns (the
		// app also fronts calls with its own local gateway on :18789).
		// Verification attempt 2026-06-27: the `observer openclaw` launcher
		// correctly injects OPENAI_BASE_URL, but this install's `openai`
		// provider has no API key (only openai-codex OAuth is configured), so
		// a routed openai/* turn can't authenticate here — the launcher is
		// sound; the install lacks OpenAI-compatible credentials to confirm.
		// CONFIG MECHANISM FOUND but RUNTIME STALLS (re-tested 2026-06-27). The
		// correct route is a config provider, NOT env: add
		// models.providers.<id> {baseUrl: <proxy>/v1, api: "openai-completions",
		// models:[…]} AND allow-list "<id>/<model>" in agents.defaults.models
		// (the "not allowed for agent main" blocker). That config is now
		// schema-valid (drop the unrecognized `timeoutSeconds`). HOWEVER a
		// routed `--local` turn STILL STALLS with no api_turn. Source review
		// (2026-06-27, openclaw v2026.4.24) CORRECTED the cause: it is NOT the
		// model-catalog load (offline — pi-SDK ModelRegistry, no network), it's
		// the openai-codex provider's UNBOUNDED fetch (chatgpt.com backend, 0
		// AbortSignal), which fires even when codex isn't primary because the
		// pi-SDK harness discovers a live codex OAuth token in the AGENT-DIR
		// auth store (~/.openclaw/agents/main/agent/auth-profiles.json), distinct
		// from openclaw.json. BOTH fallbacks were TESTED 2026-06-27: config-only
		// (observer primary, codex left in place) AND the operator-authorized
		// credential-step (codex dropped from config + the agent-dir auth store
		// moved aside so no live token is discoverable; restored byte-identical
		// after). BOTH STILL STALLED with no api_turn — so neutralizing codex is
		// necessary-but-NOT-sufficient; an unidentified eager call in openclaw's
		// `--local` startup hangs BEFORE the inference reaches the proxy. A
		// confirmed RUNTIME-BLOCK, closed as a grounded negative; observer drives
		// no route and must not auto-disable a user's OAuth credential to force
		// one. (docs/proxy-routing-blockers.md)
		Proxy:       nil,
		Routability: RouteStatusProbeRequired,
		Hook:        HookSpec{Mechanism: HookNone},
		MCP:         nil,
		Native:      NativeRails{},
		TokenTier:   TokenTier{Best: "trajectory"}, // *.trajectory.jsonl model.completed lastCallUsage (accurate per-call); the runs.sqlite task path genuinely has no token columns.
	},
	"pi": {
		Tool: "pi",
		// ROUTABLE via a custom provider in ~/.pi/agent/models.json — VERIFIED
		// LIVE 2026-06-27. pi's BUILT-IN providers ignore OPENAI_BASE_URL
		// (a dead-port base URL still reached api.openai.com; both env and the
		// default provider bypass the proxy), but pi's documented custom-
		// provider mechanism (docs/models.md) accepts an explicit `baseUrl`.
		// The `observer pi` launcher (cmd/observer/pi.go) idempotently writes an
		// "observer" provider {baseUrl: <proxy>/v1, api: openai-completions,
		// apiKey: "OPENAI_API_KEY" (the env-var NAME — no secret on disk)} and
		// execs `pi --provider observer`. A live turn landed real api_turns
		// rows (provider=openai, gpt-4o-2024-08-06, HTTP 200) — routing
		// confirmed. RouteProviderJSON because the route is a JSON config write,
		// not an env var; init does NOT auto-write it (the launcher does).
		// COMPRESSION CAVEAT: pi is on the proxy's OpenAI compression path, but
		// pi pre-processes files LOCALLY and sends minimal tool_results (an 87KB
		// read produced a 239-token request), so conversation compression rarely
		// has a large tool-output to compress on a typical pi turn — no
		// compression_event captured despite genuine attempts. Routing works;
		// the compression benefit is small by pi's architecture, not a gate.
		// (docs/proxy-routing-blockers.md)
		Proxy:       &ProxyRoute{Kind: RouteProviderJSON, EnvVar: "", Suffix: "/v1", Launcher: "observer pi", Note: "routes via ~/.pi/agent/models.json custom 'observer' provider baseUrl (not an env var); launcher writes it, never a key"},
		Routability: RouteStatusRoutableNow,
		Hook:        HookSpec{Mechanism: HookNone},
		MCP:         nil,
		Native:      NativeRails{},
		TokenTier:   TokenTier{Best: "transcript"}, // un-audited depth.
	},
	"antigravity": {
		Tool: "antigravity",
		// No base-URL / custom-provider knob found (decrypt-gated, own
		// backend). A grounded negative — reclassify if Google documents a
		// gateway knob.
		Proxy:       nil,
		Routability: RouteStatusNativeExempt,
		Hook:        HookSpec{Mechanism: HookNone},
		MCP:         nil,
		Native:      NativeRails{}, // Google Cloud usage API not yet investigated (Phase-4 ledger).
		// Newer CLI (agy) writes plaintext-protobuf SQLite .db — parsed
		// directly (clidb.go), no decrypt/gRPC. Desktop + older-CLI .pb
		// remain OSCrypt/gRPC-gated (Windows cipher unknown).
		TokenTier: TokenTier{Best: "sqlite", Gap: "desktop/.pb path still decrypt-gated"},
	},
}

// For returns the registered Capability for a tool. ok is false when the
// tool has no registry row yet (resolve as "no known integration
// capabilities"); the returned Capability still carries the Tool name so
// callers can use it safely.
func For(tool string) (Capability, bool) {
	c, ok := registry[tool]
	if !ok {
		return Capability{Tool: tool}, false
	}
	return c, true
}

// Capabilities returns every registered Capability. Order is not
// guaranteed; callers that need determinism should sort by Tool.
func Capabilities() []Capability {
	out := make([]Capability, 0, len(registry))
	for _, c := range registry {
		out = append(out, c)
	}
	return out
}
