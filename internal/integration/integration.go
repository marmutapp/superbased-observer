package integration

import "sort"

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
	// CrossOSBridge mirrors HookSpec.CrossOSBridge for the proxy route: this
	// persisted route's config file can ALSO be written into a foreign
	// Windows home from a WSL daemon — the `<tool>-windows` virtual-target
	// convention. When true, the `<tool>-windows` target resolves the
	// Windows-side config path through crossmount and writes the base-URL
	// route (env settings / config file) pointing at the WSL proxy's
	// localhost-forwarded port, so a Windows-installed claude/codex routes
	// through the WSL proxy. A capability FLAG, not a tool branch
	// (CLAUDE.md #3); meaningful only for the persisted RouteKinds
	// (RouteEnvSettings / RouteConfigFile).
	CrossOSBridge bool
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
	// ProxyProbe records the config-lane WRITER BINDING (a RouteKind, plus
	// the writer's Note) for a tool whose GUARDED, ADDITIVE proxy-route
	// writer EXISTS in internal/proxyroute. It names HOW `observer init`
	// writes the route: init dispatches its probe-route step on
	// ProxyProbe.Kind (never a tool-name branch) and derives the eligible
	// SET from this field, not a hand-list.
	//
	// ProxyProbe PERSISTS after promotion. Proxy and ProxyProbe answer two
	// different questions: Proxy records the live-verified route observer
	// DRIVES; ProxyProbe records how init WRITES it. The writer remains
	// init's apply mechanism on any machine where the tool's config is not
	// yet routed — a live-verified route on the operator's box does not
	// pre-write the config on a fresh install, so init still needs the
	// binding to lay it down. So a promoted tool carries BOTH: a non-nil
	// Proxy (verified) and a non-nil ProxyProbe (the writer that applies it).
	//
	// An UN-promoted probe (Proxy nil, Routability probe_required) is the
	// "writer ready, promotion pending a live api_turns confirmation" state
	// the docs mandate ("flip the matrix cell ONLY after live verification").
	// A nil ProxyProbe means "no config-lane writer" (the common case). Never
	// fabricate a verified Proxy from a ProxyProbe.
	ProxyProbe *ProxyRoute
	// Routability is the surface-specific bucket (RouteStatus): whether the
	// tool is routable at all, independent of whether observer drives a route
	// today (Proxy). A row can have Proxy==nil yet Routability != exempt — the
	// surface exists but observer's writer/upstream support is still pending.
	Routability RouteStatus
	Hook        HookSpec
	MCP         *MCPTarget
	Native      NativeRails
	TokenTier   TokenTier
	// Handoff is the session-handoff row: transcript readability (Phase 0
	// P0.1, live-grounded 2026-07-03 on a 328-session corpus) + grounded
	// delivery lanes. Zero value = actions-only carry + file delivery, the
	// honest floor.
	Handoff HandoffCapability
	// Attach, when non-nil, declares the tool is attachable: `observer
	// <Subcommand> --attach` hands its PTY to the daemon so the dashboard
	// can join the live session as a second seat (session-attach design
	// §2.3, T2). Nil = not attachable (bare launch only). Populated only
	// for tools with a wired attachable launcher; the row must also be
	// Launchable (pinned by registry_coverage_test.go).
	Attach *AttachSpec
	// Resume declares how a CLOSED session is reopened (session-attach
	// design §2.3, T3). Zero value (ResumeNone) = no grounded native
	// resume; the dashboard offers the shipped handoff-fork resume when the
	// tool is launchable, else a disabled affordance. A ResumeNative row is
	// declared only after the native-resume argv is verified live.
	Resume ResumeSpec
	// Binary, when non-nil, declares how the `observer <x>` launcher resolves
	// the tool's executable across OSes plus the grounded one-click install
	// hints (BinaryResolveSpec). Nil = no grounded resolution row (the honest
	// floor). Populated for every launchable tool (Handoff.Launch != nil),
	// pinned by registry_coverage_test.go. Consumers (the toolresolve ladder,
	// the dashboard install endpoint, the doctor) dispatch on the SHAPE of
	// this field, never on tool name (CLAUDE.md #3).
	Binary *BinaryResolveSpec
	// AuthEnv lists the environment-variable NAMES (never values) a tool reads
	// its provider credentials from at runtime. The attach client forwards the
	// caller's values for these keys claude-style — presence-forwarded (an
	// absent key is skipped, a present-but-empty `KEY=` forwarded verbatim),
	// layered last so launchChildEnv's inherited daemon env loses to them
	// (last-wins) — so a shell-exported-only key (never written to any config
	// file) reaches the daemon-spawned child exactly as it would a bare launch,
	// which inherits the caller's os.Environ() directly. A zero value means "no
	// grounded credential-env" — the tool authenticates via a config file,
	// OAuth, or a keychain, OR its env surface is unverified — never a
	// fabricated key. NAMES ONLY: TestAuthEnvWellFormed enforces that no value
	// leaks in (no `=`, no whitespace, not OBSERVER_-prefixed, not a launcher-
	// closure-owned routing/profile key, no intra-row duplicate), and
	// TestAuthEnvImpliesAttachable pins that only an attachable row carries keys
	// (a bare-only tool has no attach socket to forward across).
	AuthEnv []string
	// Vocabulary declares whether this adapter's NATIVE TOOL NAMES live in
	// the canonical cross-adapter taxonomy table (internal/tooltax) — the
	// WP-T3 teeth of the tool-taxonomy plan. A zero value is "not declared"
	// and fails registry_coverage_test.go's
	// TestVocabularyDeclaredForEveryAdapter, so a NEW adapter cannot ship a
	// private action-type map that drifts from the table. InTaxonomy false
	// is legal ONLY with a Note explaining the honest zero (the five
	// browser-chat `*-web` rows capture chat turns, not tool calls).
	Vocabulary Vocabulary
}

// registry is the capability table, keyed by the adapter's canonical tool
// name (matching adapter.Adapter.Name() and the EnabledAdapters list). It
// covers every registered adapter (pinned by registry_coverage_test.go); the doctor (and the matrix)
// render from it. Every cell is code-grounded — a zero value means "no
// grounded capability" (genuinely unsupported OR pending a discovery spike
// against a live install), distinguished by the per-row comment, never a
// fabricated capability. An absent tool resolves via For to a zero-value
// Capability that still echoes the Tool name.
var registry = map[string]Capability{
	// Full-capability flagships: proxy + hook + MCP + all native rails.
	"claude-code": {
		Tool:        "claude-code",
		Vocabulary:  Vocabulary{InTaxonomy: true},
		Proxy:       &ProxyRoute{Kind: RouteEnvSettings, EnvVar: "ANTHROPIC_BASE_URL", Suffix: "", Launcher: "observer claude", CrossOSBridge: true},
		Routability: RouteStatusRoutableNow,
		Hook:        HookSpec{Mechanism: HookClaudeSettings, CrossOSBridge: true, AutoWired: true},
		MCP:         &MCPTarget{Format: MCPServersJSON, PathHint: ".claude.json", Implemented: true},
		Native:      NativeRails{A: true, B: true, C: true},
		TokenTier:   TokenTier{Best: "proxy"},
		// P0.1 FULL: ~/.claude/projects/<slug>/<sid>.jsonl; reader derives
		// the path by session-id glob (hook-fed rows carry a sentinel).
		Handoff: HandoffCapability{Transcript: TranscriptFull, Inject: []InjectKind{InjectFile, InjectMCP, InjectHook, InjectPrompt}, Launch: &LaunchSpec{Subcommand: "claude"}},
		// Binary resolution + grounded installs. Unix launcher resolves
		// "claude"; the npm shim + the official curl installer are both
		// grounded (npm @anthropic-ai/claude-code; claude.ai/install.sh).
		Binary: &BinaryResolveSpec{
			Names: BinaryNames{
				Unix:    []string{"claude"},
				Windows: []string{"claude.exe", "claude.cmd", "claude"},
			},
			ProbeDirs: []ProbeDir{
				{OS: ProbeUnix, Rel: ".claude/local"},
				{OS: ProbeUnix, Rel: ".local/bin"},
			},
			Installs: []InstallHint{
				{OS: "", Channel: "npm", Argv: []string{"npm", "install", "-g", "@anthropic-ai/claude-code"}, Display: "npm install -g @anthropic-ai/claude-code"},
				{OS: "linux", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://claude.ai/install.sh | bash"}, Display: "curl -fsSL https://claude.ai/install.sh | bash"},
				{OS: "darwin", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://claude.ai/install.sh | bash"}, Display: "curl -fsSL https://claude.ai/install.sh | bash"},
			},
		},
		// Attachable: `observer claude --attach` hands the PTY to the daemon
		// (session-attach v1 scope).
		Attach: &AttachSpec{Subcommand: "claude"},
		// Credential env forwarded across the attach socket. Grounded:
		// cmd/observer/claude.go prepareClaudeEnv honors a preset
		// ANTHROPIC_AUTH_TOKEN (docs/proxy-wrappers.md); ANTHROPIC_API_KEY is
		// claude-code's standard key env. NAMES only — the caller's values ride
		// the socket so a shell-exported key reaches the daemon-spawned child.
		AuthEnv: []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"},
		// Native resume GROUNDED (session-attach design Phase 3, decision #6:
		// claude-code first). Verified live 2026-07-19 against the installed
		// CLI: `claude --help` lists `-r, --resume [value]` — "Resume a
		// conversation by session ID" (also `-c, --continue` for the most
		// recent). Observer's sessions.id IS claude's native JSONL sessionId
		// (adapter recon), so `claude --resume <sessions.id>` reattaches the
		// REAL transcript. IDMechanism "flag:--resume"; the `observer claude`
		// launcher exposes a uniform `--resume <id>` flag that maps to it.
		Resume: ResumeSpec{Kind: ResumeNative, Subcommand: "claude", IDMechanism: "flag:--resume"},
	},
	"codex": {
		Tool:        "codex",
		Vocabulary:  Vocabulary{InTaxonomy: true},
		Proxy:       &ProxyRoute{Kind: RouteConfigFile, EnvVar: "", Launcher: "observer codex", Note: "codex routes through ~/.codex/config.toml openai_base_url (not an env var)", CrossOSBridge: true},
		Routability: RouteStatusRoutableNow,
		Hook:        HookSpec{Mechanism: HookCodexConfig, AutoWired: true},
		MCP:         &MCPTarget{Format: MCPCodexTOML, PathHint: ".codex/config.toml", Implemented: true},
		Native:      NativeRails{A: true, B: true, C: true, Note: "Rail A (usage-export) config-gated on live keys"},
		TokenTier:   TokenTier{Best: "proxy"},
		// P0.1 FULL: rollout JSONL (event_msg text lane + function_call
		// pairing); reader derives the path by session-id glob.
		Handoff: HandoffCapability{Transcript: TranscriptFull, Inject: []InjectKind{InjectFile, InjectMCP, InjectPrompt}, Launch: &LaunchSpec{Subcommand: "codex"}},
		// Binary resolution + grounded installs. Unix launcher resolves
		// "codex"; npm @openai/codex (any OS) + brew on macOS.
		Binary: &BinaryResolveSpec{
			Names: BinaryNames{
				Unix:    []string{"codex"},
				Windows: []string{"codex.exe", "codex.cmd", "codex"},
			},
			Installs: []InstallHint{
				{OS: "", Channel: "npm", Argv: []string{"npm", "install", "-g", "@openai/codex"}, Display: "npm install -g @openai/codex"},
				{OS: "darwin", Channel: "brew", Argv: []string{"brew", "install", "codex"}, Display: "brew install codex"},
			},
		},
		// Attachable: `observer codex --attach` hands the PTY to the daemon
		// (session-attach v1 scope).
		Attach: &AttachSpec{Subcommand: "codex"},
		// Credential env forwarded across the attach socket. Grounded:
		// cmd/observer/codex.go's sk-/JWT auth paths read OPENAI_API_KEY.
		AuthEnv: []string{"OPENAI_API_KEY"},
		// Native resume GROUNDED (session-attach design Phase 3, decision #6:
		// codex first). Verified live 2026-07-19 against the installed CLI:
		// `codex resume [SESSION_ID] [PROMPT]` — "Resume a previous interactive
		// session … Session id (UUID) or session name … use --last to pick the
		// most recent". Observer's sessions.id IS codex's native session_id
		// (adapter recon), so `codex resume <sessions.id>` reattaches the REAL
		// session. The global `-c openai_base_url` override is honored BEFORE
		// the `resume` subcommand (verified: `codex -c … resume --help` parses
		// clean), so proxy routing composes. IDMechanism "subcommand:resume";
		// the `observer codex` launcher exposes a uniform `--resume <id>` flag
		// that maps to the `resume <id>` subcommand.
		Resume: ResumeSpec{Kind: ResumeNative, Subcommand: "codex", IDMechanism: "subcommand:resume"},
	},

	// Proxy-routable CLI (OpenAI-compatible base URL via launcher).
	// LIVE-VERIFIED 2026-06-27: `observer opencode -- run …` routed a
	// gpt-5.4-nano turn through the proxy (api_turns grew).
	"opencode": {
		Tool:        "opencode",
		Vocabulary:  Vocabulary{InTaxonomy: true},
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
		// P0.1 FULL: opencode.db message+part tables (reader = P2 tranche).
		Handoff: HandoffCapability{Transcript: TranscriptFull, Inject: []InjectKind{InjectFile, InjectMCP, InjectPrompt}, Launch: &LaunchSpec{Subcommand: "opencode"}},
		// Attach grounded 2026-07-24 (attach-all-launchers); PTY handoff only
		// — no prompt seeding, token capture path unchanged.
		Attach: &AttachSpec{Subcommand: "opencode"},
		// Native resume GROUNDED, live-verified 2026-07-24: `opencode --session
		// <id>` reattaches the real session; id is the raw observer SessionID
		// (`ses_…`). The `observer opencode` launcher maps `--resume <id>` to it.
		Resume: ResumeSpec{Kind: ResumeNative, Subcommand: "opencode", IDMechanism: "flag:--session"},
		// AuthEnv zero: opencode's primary auth is its `opencode auth` file
		// store; per-provider env keys (OPENAI_API_KEY / ANTHROPIC_API_KEY / …)
		// are candidates but unverified as the effective runtime source here.
		// Binary resolution + grounded installs. Unix launcher resolves
		// "opencode"; npm opencode-ai@latest (any OS) + the official curl
		// installer on linux/darwin. Its own bin dir (.opencode/bin) is a
		// per-tool extra on both OSes.
		Binary: &BinaryResolveSpec{
			// npm JS bin: Windows install lays down a `.cmd` shim (+
			// .ps1/POSIX-shell forms), never an `.exe` — see the
			// command-code row's Binary comment for the long-form
			// rationale.
			Names: BinaryNames{
				Unix:    []string{"opencode"},
				Windows: []string{"opencode.cmd", "opencode"},
			},
			ProbeDirs: []ProbeDir{
				{OS: ProbeUnix, Rel: ".opencode/bin"},
				{OS: ProbeWindows, Rel: ".opencode/bin"},
			},
			Installs: []InstallHint{
				{OS: "", Channel: "npm", Argv: []string{"npm", "install", "-g", "opencode-ai@latest"}, Display: "npm install -g opencode-ai@latest"},
				{OS: "linux", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://opencode.ai/install | bash"}, Display: "curl -fsSL https://opencode.ai/install | bash"},
				{OS: "darwin", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://opencode.ai/install | bash"}, Display: "curl -fsSL https://opencode.ai/install | bash"},
			},
		},
	},

	// IDE/extension adapters that talk only to their own backend → no proxy
	// route (Proxy=nil is DATA, not a missing feature). Hooks/MCP per tool.
	"cursor": {
		Tool:       "cursor",
		Vocabulary: Vocabulary{InTaxonomy: true},
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
		// P0.1 FULL (CLI): ~/.cursor/projects/<slug>/agent-transcripts/
		// <sid>/<sid>.jsonl, Anthropic-shaped; NOT referenced by DB
		// source_file (sentinel) — derive by session id. IDE state.vscdb
		// unmeasured on this corpus. Reader = P2 tranche.
		Handoff: HandoffCapability{Transcript: TranscriptFull, Inject: []InjectKind{InjectFile, InjectMCP, InjectPrompt}, Launch: &LaunchSpec{Subcommand: "cursor"}, Note: "CLI grounded; IDE surface unmeasured"},
		// Attach grounded 2026-07-24 (attach-all-launchers); PTY handoff only
		// — no prompt seeding, token capture path unchanged.
		Attach: &AttachSpec{Subcommand: "cursor"},
		// Resume LIVE-CONFIRMED 2026-07-25 (the 2026-07-24 auth block is gone —
		// operator logged in, `cursor-agent status` → "Logged in as …").
		// `cursor-agent --help` lists `--resume [chatId]  Select a session to
		// resume`, and the chatId is our stored SessionID VERBATIM: the on-disk
		// chat dirs ~/.cursor/chats/<hash>/<chatId>/ are named with the exact
		// sessions.id values our adapter stores, so NO id transform is needed.
		// SPELLING (operator-corrected 2026-07-25): pass it JOINED as
		// `--resume=<chatId>`, not space-separated. The flag takes an OPTIONAL
		// value (`[chatId]`, help reports `default: false`), so the `=` form is
		// the unambiguous spelling; the space form relies on the parser electing
		// to consume the next token instead of leaving the flag bare and letting
		// the id fall through as a positional. resume_launcher.go's "cursor" row
		// therefore sets joined:true.
		// CONTENT-LEVEL PROOF: `cursor-agent --resume=<id> -p …` run against a
		// chat this session never wrote to, asked to quote the conversation's
		// first user message, answered with that message verbatim — the prior
		// transcript really is loaded, not merely a chat re-opened. Structural
		// proof alongside it: createdAtMs + title preserved, store.db grew,
		// no new chat dir; CONTROL — the same command WITHOUT --resume created
		// a new chat dir. Caveat (honest, non-blocking): the call is flaky over
		// this network — `RetriableError: WritableIterable is closed` against
		// agentn.global.api5.cursor.sh, typically clearing within 1–3 attempts,
		// on BOTH argv forms and on non-resume calls too. That is TRANSPORT
		// flakiness, not a broken resume mechanism; retry before concluding.
		Resume: ResumeSpec{Kind: ResumeNative, Subcommand: "cursor", IDMechanism: "flag:--resume"},
		// AuthEnv zero: cursor-agent authenticates via OAuth by default;
		// CURSOR_API_KEY is upstream-plausible but ungrounded as the runtime
		// key env, so no key is declared (never fabricate one).
		// Binary resolution + grounded install. Unix launcher resolves
		// "cursor-agent"; the installer drops versioned binaries under
		// .local/share/cursor-agent/versions/*. Official installer script
		// (cursor.com/docs/cli/installation); Windows hint is display-only
		// (no Windows binary spelling grounded yet).
		Binary: &BinaryResolveSpec{
			Names: BinaryNames{Unix: []string{"cursor-agent"}},
			ProbeDirs: []ProbeDir{
				{OS: ProbeUnix, Rel: ".local/bin"},
				{OS: ProbeUnix, Rel: ".local/share/cursor-agent/versions/*"},
			},
			Installs: []InstallHint{
				{OS: "linux", Channel: "script", Argv: []string{"bash", "-lc", "curl https://cursor.com/install -fsS | bash"}, Display: "curl https://cursor.com/install -fsS | bash"},
				{OS: "darwin", Channel: "script", Argv: []string{"bash", "-lc", "curl https://cursor.com/install -fsS | bash"}, Display: "curl https://cursor.com/install -fsS | bash"},
				{OS: "windows", Channel: "script", Argv: []string{"powershell", "-Command", "irm 'https://cursor.com/install?win32=true' | iex"}, Display: "irm 'https://cursor.com/install?win32=true' | iex"},
			},
		},
	},
	"cline": {
		Tool:       "cline",
		Vocabulary: Vocabulary{InTaxonomy: true},
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
		// P0.1 FULL: tasks/<id>/api_conversation_history.json, Anthropic-
		// shaped (reader = P2 tranche).
		Handoff: HandoffCapability{Transcript: TranscriptFull, Inject: []InjectKind{InjectFile, InjectMCP}},
	},
	"copilot": {
		Tool:       "copilot",
		Vocabulary: Vocabulary{InTaxonomy: true},
		// VS Code extension → GitHub-hosted backend (native traffic exempt),
		// but VS Code's custom-endpoint / BYOK model support MAY route — probe
		// before flipping. Native hosted + inline completions stay exempt.
		Proxy:       nil,
		Routability: RouteStatusProbeRequired,
		Hook:        HookSpec{Mechanism: HookNone},
		MCP:         nil,
		Native:      NativeRails{A: true, B: true, C: true, Note: "rails partial; identity = GitHub login, cost seat/account-level"},
		TokenTier:   TokenTier{Best: "events_jsonl", Gap: "no cache tier"},
		// P0.1 PARTIAL: chatSessions/<id>.jsonl is a key-path PATCH LOG
		// (kind:0 init + kind:1/2 patches) — content present but needs a
		// replay reader (deferred tranche).
		Handoff: HandoffCapability{Transcript: TranscriptPartial, Inject: []InjectKind{InjectFile}, Note: "patch-log replay reader not built"},
	},
	"copilot-cli": {
		Tool:       "copilot-cli",
		Vocabulary: Vocabulary{InTaxonomy: true},
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
		// P0.1 FULL: ~/.copilot/session-store.db turns(user_message,
		// assistant_response) (reader = P2 tranche).
		Handoff: HandoffCapability{Transcript: TranscriptFull, Inject: []InjectKind{InjectFile, InjectPrompt}, Launch: &LaunchSpec{Subcommand: "copilot-cli"}},
		// Attach grounded 2026-07-24 (attach-all-launchers); PTY handoff only
		// — no prompt seeding, token capture path unchanged.
		Attach: &AttachSpec{Subcommand: "copilot-cli"},
		// Native resume GROUNDED, live-verified 2026-07-24: `copilot
		// --session-id <id>` reattaches the real session; id is a raw uuid. The
		// `observer copilot-cli` launcher maps `--resume <id>` to it.
		Resume: ResumeSpec{Kind: ResumeNative, Subcommand: "copilot-cli", IDMechanism: "flag:--session-id"},
		// Credential env forwarded across the attach socket. COPILOT_PROVIDER_API_KEY
		// is the BYOK model-provider key (repo-grounded in copilotcli.go — the
		// COPILOT_PROVIDER_* BYOK lane). The other three are the upstream-
		// documented GitHub-auth precedence chain (docs.github.com,
		// "Authenticating GitHub Copilot CLI"): COPILOT_GITHUB_TOKEN > GH_TOKEN
		// > GITHUB_TOKEN, and an env value silently overrides stored OAuth — so
		// forwarding the caller's value preserves the profile a bare launch
		// would use. NAMES only.
		AuthEnv: []string{"COPILOT_PROVIDER_API_KEY", "COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"},
		// Binary resolution + grounded installs. Unix launcher resolves
		// "copilot"; npm @github/copilot (any OS) + the cask/script/winget
		// channels (docs.github.com copilot-cli install).
		Binary: &BinaryResolveSpec{
			// npm JS bin: Windows install lays down a `.cmd` shim (+
			// .ps1/POSIX-shell forms), never an `.exe` — see the
			// command-code row's Binary comment for the long-form
			// rationale.
			Names: BinaryNames{
				Unix:    []string{"copilot"},
				Windows: []string{"copilot.cmd", "copilot"},
			},
			Installs: []InstallHint{
				{OS: "", Channel: "npm", Argv: []string{"npm", "install", "-g", "@github/copilot"}, Display: "npm install -g @github/copilot"},
				{OS: "darwin", Channel: "brew", Argv: []string{"brew", "install", "--cask", "copilot-cli"}, Display: "brew install --cask copilot-cli"},
				{OS: "linux", Channel: "brew", Argv: []string{"brew", "install", "--cask", "copilot-cli"}, Display: "brew install --cask copilot-cli"},
				{OS: "linux", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://gh.io/copilot-install | bash"}, Display: "curl -fsSL https://gh.io/copilot-install | bash"},
				{OS: "darwin", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://gh.io/copilot-install | bash"}, Display: "curl -fsSL https://gh.io/copilot-install | bash"},
				{OS: "windows", Channel: "winget", Argv: []string{"winget", "install", "GitHub.Copilot"}, Display: "winget install GitHub.Copilot"},
			},
		},
	},
	"kilo-code": {
		Tool:       "kilo-code",
		Vocabulary: Vocabulary{InTaxonomy: true},
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
		// Handoff zero value: no kilo-code (legacy) sessions on this node
		// during Phase 0 — expected FULL via the cline task-dir format, but
		// unmeasured, so the row stays the honest actions-only floor until
		// live data grounds it.
		Handoff: HandoffCapability{},
	},
	"kilo-code-cli": {
		Tool:       "kilo-code-cli",
		Vocabulary: Vocabulary{InTaxonomy: true},
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
		// P0.1 FULL: kilo.db message+part tables (reader = P2 tranche).
		Handoff: HandoffCapability{Transcript: TranscriptFull, Inject: []InjectKind{InjectFile, InjectPrompt}, Launch: &LaunchSpec{Subcommand: "kilo"}},
		// Attach grounded 2026-07-24 (attach-all-launchers); PTY handoff only
		// — no prompt seeding, token capture path unchanged.
		Attach: &AttachSpec{Subcommand: "kilo"},
		// Native resume GROUNDED, live-verified 2026-07-24: `kilo --session <id>`
		// (OpenCode-fork surface) reattaches the real session; id is raw. The
		// `observer kilo` launcher maps `--resume <id>` to it.
		Resume: ResumeSpec{Kind: ResumeNative, Subcommand: "kilo", IDMechanism: "flag:--session"},
		// AuthEnv zero: KILO_API_KEY appears in Kilo docs, but the CLI's
		// env-read of it is ungrounded (it talks to the api.kilo.ai gateway
		// directly); no key declared until a live read is grounded.
		// Binary resolution + grounded installs. Unix launcher resolves
		// "kilo"; npm @kilocode/cli (any OS) + the script/brew channels
		// (kilo.ai/docs/cli). Windows shim grounded at
		// %APPDATA%\npm\kilo.cmd (docs/kilo-code-adapter.md).
		Binary: &BinaryResolveSpec{
			// npm JS bin: Windows install lays down a `.cmd` shim (+
			// .ps1/POSIX-shell forms), never an `.exe` — see the
			// command-code row's Binary comment for the long-form
			// rationale.
			Names: BinaryNames{
				Unix:    []string{"kilo"},
				Windows: []string{"kilo.cmd", "kilo"},
			},
			Installs: []InstallHint{
				{OS: "", Channel: "npm", Argv: []string{"npm", "install", "-g", "@kilocode/cli"}, Display: "npm install -g @kilocode/cli"},
				{OS: "linux", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://kilo.ai/cli/install | bash"}, Display: "curl -fsSL https://kilo.ai/cli/install | bash"},
				{OS: "darwin", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://kilo.ai/cli/install | bash"}, Display: "curl -fsSL https://kilo.ai/cli/install | bash"},
				{OS: "darwin", Channel: "brew", Argv: []string{"brew", "install", "Kilo-Org/tap/kilo"}, Display: "brew install Kilo-Org/tap/kilo"},
			},
		},
	},

	// CLI adapters captured via watcher/SQLite (+ opt-in receivers).
	"cline-cli": {
		Tool:       "cline-cli",
		Vocabulary: Vocabulary{InTaxonomy: true},
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
		// MCP: nil here means "no WRITER", not "not capable" — re-grounded
		// 2026-08-01 against cline 3.0.48. There is still NO
		// cline_mcp_settings.json on the live install (the settings dir holds
		// only providers.json + cli-notices.json), but Cline CLI IS
		// MCP-capable via a command: `cline mcp install|add <name>
		// [targetArgs...] --transport stdio --yes`. MCPTarget cannot express
		// that today — every MCPFormat names a FILE and PathHint assumes one —
		// so wiring it needs a command-mediated MCPFormat plus a writer in
		// internal/mcp, not a row edit. Left nil deliberately rather than
		// fabricating a target we cannot write (docs/clinecli-adapter.md).
		MCP:       nil,
		Native:    NativeRails{},
		TokenTier: TokenTier{Best: "sqlite"}, // sessions.db + per-session messages.json; full.
		// P0.1 FULL: <id>.messages.json, Anthropic-shaped (reader = P2
		// tranche).
		Handoff: HandoffCapability{Transcript: TranscriptFull, Inject: []InjectKind{InjectFile, InjectPrompt}, Launch: &LaunchSpec{Subcommand: "cline-cli"}},
		// Attach grounded 2026-07-24 (attach-all-launchers); PTY handoff only
		// — no prompt seeding, token capture path unchanged.
		Attach: &AttachSpec{Subcommand: "cline-cli"},
		// Native resume GROUNDED, live-verified 2026-07-24: `cline --id <id>`
		// reattaches the real session; id is raw (e.g. `1782548283719_prf8j`).
		// The `observer cline-cli` launcher maps `--resume <id>` to it.
		Resume: ResumeSpec{Kind: ResumeNative, Subcommand: "cline-cli", IDMechanism: "flag:--id"},
		// AuthEnv zero: cline-cli reads provider keys from its providers.json
		// file store (`cline auth … -k`), not an env var — file auth, no
		// grounded credential-env to forward.
		// Binary resolution + grounded install. Unix launcher resolves
		// "cline"; npm-distributed `cline` 3.x (docs/clinecli-adapter.md).
		Binary: &BinaryResolveSpec{
			// npm JS bin: Windows install lays down a `.cmd` shim (+
			// .ps1/POSIX-shell forms), never an `.exe` — see the
			// command-code row's Binary comment for the long-form
			// rationale.
			Names: BinaryNames{
				Unix:    []string{"cline"},
				Windows: []string{"cline.cmd", "cline"},
			},
			Installs: []InstallHint{
				{OS: "", Channel: "npm", Argv: []string{"npm", "install", "-g", "cline"}, Display: "npm install -g cline"},
			},
		},
	},
	"hermes": {
		Tool:       "hermes",
		Vocabulary: Vocabulary{InTaxonomy: true},
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
		// P0.1 FULL: state.db messages(role, content, tool_calls) active=1
		// (reader = P2 tranche).
		// No InjectPrompt lane: hermes' TUI (`--tui` / HERMES_TUI=1) takes NO
		// initial-message flag (its only prompt entry points — `-z/--oneshot`,
		// `chat -q` — are headless one-shots that answer and exit; upstream
		// gap, NousResearch/hermes-agent Issue #19675). So it is launchable
		// only in DocAssisted mode: write the doc + open `hermes --tui`.
		Handoff: HandoffCapability{Transcript: TranscriptFull, Inject: []InjectKind{InjectFile, InjectMCP}, Launch: &LaunchSpec{Subcommand: "hermes", Mode: LaunchDocAssisted}, Note: "TUI has no initial-prompt seed (upstream gap); launch writes the handover doc + opens hermes --tui"},
		// Attach grounded 2026-07-24 (attach-all-launchers); PTY handoff only
		// — no prompt seeding, token capture path unchanged. DocAssisted only
		// gates --continue-from seeding (incompatible with attach); plain attach
		// opens the TUI seedless.
		Attach: &AttachSpec{Subcommand: "hermes"},
		// Native resume GROUNDED, live-verified 2026-07-24: `hermes --resume
		// <id>` reattaches the real session; id is raw (`20260627_132748_325fea`
		// shape). Composes with the config.yaml provider route (the launcher
		// prepends `--provider observer`). The `observer hermes` launcher maps
		// `--resume <id>` to it.
		Resume: ResumeSpec{Kind: ResumeNative, Subcommand: "hermes", IDMechanism: "flag:--resume"},
		// Credential env forwarded across the attach socket. Grounded:
		// hermes.go hermesDefaultKeyEnv (OPENROUTER_API_KEY) — the default
		// key_env the observer provider resolves at runtime. A NON-default
		// `--key-env NAME` is dynamic and handled at the launcher call site
		// (hermes.go sets authEnvExtra to that NAME); the registry row carries
		// only the static default.
		AuthEnv: []string{"OPENROUTER_API_KEY"},
		// Binary resolution + grounded install. Unix launcher resolves
		// "hermes"; its bundled node prefix (.hermes/node/bin) + .hermes/bin
		// are per-tool extras (the off-PATH hermes-bundled npm prefix from
		// the opencode-WSL incident). Official install script
		// (github.com/NousResearch/hermes-agent README; no official pip
		// path). Windows hint is display-only (no Windows binary spelling
		// grounded yet).
		Binary: &BinaryResolveSpec{
			Names: BinaryNames{Unix: []string{"hermes"}},
			ProbeDirs: []ProbeDir{
				{OS: ProbeUnix, Rel: ".hermes/bin"},
				{OS: ProbeUnix, Rel: ".hermes/node/bin"},
			},
			Installs: []InstallHint{
				{OS: "linux", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://hermes-agent.nousresearch.com/install.sh | bash"}, Display: "curl -fsSL https://hermes-agent.nousresearch.com/install.sh | bash"},
				{OS: "darwin", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://hermes-agent.nousresearch.com/install.sh | bash"}, Display: "curl -fsSL https://hermes-agent.nousresearch.com/install.sh | bash"},
				{OS: "windows", Channel: "script", Argv: []string{"powershell", "-Command", "iex (irm https://hermes-agent.nousresearch.com/install.ps1)"}, Display: "iex (irm https://hermes-agent.nousresearch.com/install.ps1)"},
			},
		},
	},
	"cowork": {
		Tool:       "cowork",
		Vocabulary: Vocabulary{InTaxonomy: true},
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
		// P0.1 FULL: audit.jsonl user/assistant records (Windows
		// cross-mount; reader = P2 tranche).
		Handoff: HandoffCapability{Transcript: TranscriptFull, Inject: []InjectKind{InjectFile}},
	},
	"gemini-cli": {
		Tool:       "gemini-cli",
		Vocabulary: Vocabulary{InTaxonomy: true},
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
		// P0.1 FULL: ~/.gemini/tmp/<proj>/chats/session-*.jsonl user/gemini
		// records (reader = P2 tranche).
		Handoff: HandoffCapability{Transcript: TranscriptFull, Inject: []InjectKind{InjectFile, InjectPrompt}, Launch: &LaunchSpec{Subcommand: "gemini"}},
		// Attach grounded 2026-07-24 (attach-all-launchers); PTY handoff only
		// — no prompt seeding, token capture path unchanged.
		Attach: &AttachSpec{Subcommand: "gemini"},
		// Native resume GROUNDED, live-verified 2026-07-24 (v0.49.0): `gemini
		// --resume <uuid>` honors a full session UUID (help documents only
		// index/latest; older-UUID disambiguation confirmed live). Each resume
		// writes a continuation .jsonl carrying the SAME sessionId (native
		// checkpointing — not a new logical session). The `observer gemini`
		// launcher maps `--resume <id>` to it.
		Resume: ResumeSpec{Kind: ResumeNative, Subcommand: "gemini", IDMechanism: "flag:--resume"},
		// Credential env forwarded across the attach socket. Upstream-verified
		// at geminicli.com/docs/get-started/authentication: GEMINI_API_KEY =
		// AI-Studio mode key, GOOGLE_API_KEY = Vertex express-mode key (mode-
		// specific, NOT a precedence pair); GOOGLE_APPLICATION_CREDENTIALS =
		// Vertex service-account JSON path; the project var is checked
		// GOOGLE_CLOUD_PROJECT then GOOGLE_CLOUD_PROJECT_ID; GOOGLE_CLOUD_LOCATION
		// is required for Vertex. NAMES only.
		AuthEnv: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_PROJECT_ID", "GOOGLE_CLOUD_LOCATION"},
		// Binary resolution + grounded install. Unix launcher resolves
		// "gemini"; npm @google/gemini-cli (any OS).
		Binary: &BinaryResolveSpec{
			// npm JS bin: Windows install lays down a `.cmd` shim (+
			// .ps1/POSIX-shell forms), never an `.exe` — see the
			// command-code row's Binary comment for the long-form
			// rationale.
			Names: BinaryNames{
				Unix:    []string{"gemini"},
				Windows: []string{"gemini.cmd", "gemini"},
			},
			Installs: []InstallHint{
				{OS: "", Channel: "npm", Argv: []string{"npm", "install", "-g", "@google/gemini-cli"}, Display: "npm install -g @google/gemini-cli"},
			},
		},
	},
	"openclaw": {
		Tool:       "openclaw",
		Vocabulary: Vocabulary{InTaxonomy: true},
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
		TokenTier:   TokenTier{Best: "transcript"}, // <id>.jsonl message.usage covers EVERY call (accurate; re-grounded 2026-07-31 — byte-identical to the trajectory's lastCallUsage where they overlap); *.trajectory.jsonl model.completed is a one-row-per-run SUBSET that only fills gateway-injected usage-zero turns. The runs.sqlite task path genuinely has no token columns.
		// P0.1 FULL: agents/main/sessions/<sid>.jsonl message records
		// (content present despite gateway-zeroed tokens; reader = P2
		// tranche).
		// Seeded via `openclaw chat --message "<handover>"` (chat ≡ tui
		// --local). The `--continue-from` launch runs NON-PROXIED to sidestep
		// the known `--local` proxy-routing stall (project_openclaw_runtime_
		// block); token capture stays on the trajectory adapter, so seeding is
		// orthogonal to capture.
		Handoff: HandoffCapability{Transcript: TranscriptFull, Inject: []InjectKind{InjectFile, InjectPrompt}, Launch: &LaunchSpec{Subcommand: "openclaw"}, Note: "seeded via chat --message; --continue-from launches non-proxied to avoid the --local proxy stall"},
		// Attach grounded 2026-07-24 (attach-all-launchers); PTY handoff only
		// — no prompt seeding, token capture path unchanged.
		Attach: &AttachSpec{Subcommand: "openclaw"},
		// Resume stays ResumeNone (2026-07-24): no non-interactive resume surface
		// — the only entry is the picker-only `sessions` command, and the
		// documented runtime-block (project_openclaw_runtime_block) prevents
		// probing a resume argv. The dashboard offers the handoff-fork resume.
		// AuthEnv zero: openclaw authenticates via OAuth tokens in its
		// agent-dir auth store (auth-profiles.json); no grounded runtime key
		// env to forward.
		// Binary resolution + grounded installs. Unix launcher resolves
		// "openclaw"; the vendor script is primary (docs.openclaw.ai/install),
		// npm is an alternate (needs `openclaw onboard --install-daemon`
		// after, so script leads).
		Binary: &BinaryResolveSpec{
			// npm JS bin: Windows install lays down a `.cmd` shim (+
			// .ps1/POSIX-shell forms), never an `.exe` — see the
			// command-code row's Binary comment for the long-form
			// rationale.
			Names: BinaryNames{
				Unix:    []string{"openclaw"},
				Windows: []string{"openclaw.cmd", "openclaw"},
			},
			Installs: []InstallHint{
				{OS: "linux", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://openclaw.ai/install.sh | bash"}, Display: "curl -fsSL https://openclaw.ai/install.sh | bash"},
				{OS: "darwin", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://openclaw.ai/install.sh | bash"}, Display: "curl -fsSL https://openclaw.ai/install.sh | bash"},
				{OS: "", Channel: "npm", Argv: []string{"npm", "install", "-g", "openclaw@latest"}, Display: "npm install -g openclaw@latest"},
			},
		},
	},
	"pi": {
		Tool:       "pi",
		Vocabulary: Vocabulary{InTaxonomy: true},
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
		// P0.1 FULL: sessions/<slug>/<ts>_<id>.jsonl message records
		// (reader = P2 tranche).
		Handoff: HandoffCapability{Transcript: TranscriptFull, Inject: []InjectKind{InjectFile, InjectPrompt}, Launch: &LaunchSpec{Subcommand: "pi"}},
		// Attach grounded 2026-07-24 (attach-all-launchers); PTY handoff only
		// — no prompt seeding, token capture path unchanged.
		Attach: &AttachSpec{Subcommand: "pi"},
		// Native resume GROUNDED, live-verified 2026-07-24: `pi --session <id>`
		// reattaches the real session (accepts full or partial uuid; the
		// launcher passes the full id). The `observer pi` launcher maps
		// `--resume <id>` to it.
		Resume: ResumeSpec{Kind: ResumeNative, Subcommand: "pi", IDMechanism: "flag:--session"},
		// Credential env forwarded across the attach socket. Grounded: pi.go
		// writes the "observer" provider with apiKey = the env-var NAME
		// OPENAI_API_KEY (no secret on disk — pi resolves it from the env at
		// runtime), so that IS the key env the caller exports. NAMES only.
		AuthEnv: []string{"OPENAI_API_KEY"},
		// Binary resolution + grounded installs. Unix launcher resolves
		// "pi"; npm @earendil-works/pi-coding-agent (earendil-works/pi,
		// pi.dev — NOT Inflection) + the official install script.
		Binary: &BinaryResolveSpec{
			// npm JS bin: Windows install lays down a `.cmd` shim (+
			// .ps1/POSIX-shell forms), never an `.exe` — see the
			// command-code row's Binary comment for the long-form
			// rationale.
			Names: BinaryNames{
				Unix:    []string{"pi"},
				Windows: []string{"pi.cmd", "pi"},
			},
			Installs: []InstallHint{
				{OS: "", Channel: "npm", Argv: []string{"npm", "install", "-g", "--ignore-scripts", "@earendil-works/pi-coding-agent"}, Display: "npm install -g --ignore-scripts @earendil-works/pi-coding-agent"},
				{OS: "linux", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://pi.dev/install.sh | sh"}, Display: "curl -fsSL https://pi.dev/install.sh | sh"},
				{OS: "darwin", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://pi.dev/install.sh | sh"}, Display: "curl -fsSL https://pi.dev/install.sh | sh"},
			},
		},
	},
	"antigravity": {
		Tool:       "antigravity",
		Vocabulary: Vocabulary{InTaxonomy: true},
		// No base-URL / custom-provider knob found (decrypt-gated, own
		// backend). A grounded negative — reclassify if Google documents a
		// gateway knob.
		Proxy:       nil,
		Routability: RouteStatusNativeExempt,
		Hook:        HookSpec{Mechanism: HookNone},
		MCP:         nil,
		Native:      NativeRails{}, // Google Cloud usage API not yet investigated (Phase-4 ledger).
		// Desktop + older-CLI .pb remain OSCrypt/gRPC-gated (Windows cipher unknown).
		TokenTier: TokenTier{Best: "sqlite", Gap: "desktop/.pb path still decrypt-gated"},
		// Desktop .pb OSCrypt-encrypted (Windows cipher unknown) → actions_only/partial.
		Handoff: HandoffCapability{Transcript: TranscriptPartial, Inject: []InjectKind{InjectFile}, Note: "desktop .pb decrypt-gated"},
	},
	"antigravity-cli": {
		Tool:       "antigravity-cli",
		Vocabulary: Vocabulary{InTaxonomy: true},
		// agy CLI itself is non-proxied (runs locally).
		Proxy:       nil,
		Routability: RouteStatusNativeExempt,
		Hook:        HookSpec{Mechanism: HookNone},
		MCP:         nil,
		Native:      NativeRails{},
		// CLI (agy) writes plaintext-protobuf SQLite .db — parsed directly (clidb.go).
		TokenTier: TokenTier{Best: "sqlite"},
		// CLI (agy) plaintext .db readable; seeds agy -i.
		Handoff: HandoffCapability{
			Transcript: TranscriptPartial,
			Inject:     []InjectKind{InjectFile, InjectPrompt},
			Launch:     &LaunchSpec{Subcommand: "antigravity-cli"},
			Note:       "CLI (agy) .db readable + -i seed",
		},
		// Attach grounded 2026-07-24 (attach-all-launchers); PTY handoff only
		// — no prompt seeding, token capture path unchanged.
		Attach: &AttachSpec{Subcommand: "antigravity-cli"},
		// Native resume GROUNDED, live-verified 2026-07-24 (structural: id echo +
		// no new .db; space form accepted): `agy --conversation <UUID>`
		// reattaches the real session; id is a raw uuid. The `observer
		// antigravity-cli` launcher maps `--resume <id>` to it.
		Resume: ResumeSpec{Kind: ResumeNative, Subcommand: "antigravity-cli", IDMechanism: "flag:--conversation"},
		// AuthEnv zero: the agy CLI authenticates via Google OAuth, not a key
		// env — no grounded credential-env to forward.
		// Binary resolution + grounded install. Unix launcher resolves "agy"
		// (the agy CLI); official install script (antigravity.google/docs/
		// cli/install). Windows hint is display-only (no Windows binary
		// spelling grounded yet).
		Binary: &BinaryResolveSpec{
			Names: BinaryNames{Unix: []string{"agy"}},
			Installs: []InstallHint{
				{OS: "linux", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://antigravity.google/cli/install.sh | bash"}, Display: "curl -fsSL https://antigravity.google/cli/install.sh | bash"},
				{OS: "darwin", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://antigravity.google/cli/install.sh | bash"}, Display: "curl -fsSL https://antigravity.google/cli/install.sh | bash"},
				{OS: "windows", Channel: "script", Argv: []string{"powershell", "-Command", "irm https://antigravity.google/cli/install.ps1 | iex"}, Display: "irm https://antigravity.google/cli/install.ps1 | iex"},
			},
		},
	},
	"qwen-code": {
		Tool:       "qwen-code",
		Vocabulary: Vocabulary{InTaxonomy: true},
		// Live-captured 2026-07-09 (WSL + Windows): CC-shaped JSONL under
		// ~/.qwen/projects/<slug>/chats/.
		//
		// ROUTE LANE — PROMOTED, LIVE-VERIFIED 2026-07-09 (operator-approved
		// probe). Qwen Code resolves the active model by the (id, baseUrl) pair,
		// so rewriting model.baseUrl ALONE was insufficient: the first probe
		// (2026-07-09) set model.baseUrl → the proxy but left no matching
		// modelProviders entry, and `qwen -p` warned "no longer matches any
		// provider for model 'gpt-4o' … using the first id match
		// ('https://api.openai.com/v1')" and went DIRECT, no api_turns row.
		// The follow-up writer now ALSO retargets every openai-lane
		// modelProviders entry on the known default host to the proxy URL
		// (carrying each entry's id + envKey untouched — the operator's real
		// OpenAI key still forwards). With that, model.name=gpt-4o resolves to
		// (id=gpt-4o, baseUrl=http://127.0.0.1:8820/v1) and `qwen -p "reply with
		// the word ok"` routed through the proxy and landed api_turns rows
		// (id 23728-23730, provider=openai, model gpt-4o-2024-08-06, HTTP 200).
		// The OPENAI_BASE_URL env knob stays inert; the config lane is the
		// working route. Proxy now drives it; init applies it via the
		// RegisterQwenCode writer (dispatched on Kind).
		Proxy: &ProxyRoute{
			Kind:     RouteConfigFile,
			EnvVar:   "",
			Suffix:   "/v1",
			Launcher: "observer qwen",
			Note:     "routes via model.baseUrl + a matching modelProviders openai entry in ~/.qwen/settings.json (proxyroute.RegisterQwenCode, retargets the openai-default provider, keeps id + envKey); implicit host was api.openai.com so the fixed OpenAI upstream applies. Live-verified 2026-07-09 (api_turns 23728-23730, gpt-4o-2024-08-06)",
		},
		// ProxyProbe PERSISTS after promotion: it is the config-lane WRITER
		// BINDING init uses to apply the (now verified) route on a machine whose
		// ~/.qwen/settings.json is not yet routed. Proxy above is the verified
		// route; this is how init writes it.
		ProxyProbe: &ProxyRoute{
			Kind:     RouteConfigFile,
			Launcher: "observer qwen",
			Note:     "proxyroute.RegisterQwenCode rewrites model.baseUrl AND retargets the matching openai-lane modelProviders entry in ~/.qwen/settings.json (from the known default only; keeps id + envKey; .bak)",
		},
		Routability: RouteStatusRoutableNow,
		// Upstream ships a full CC-shaped lifecycle hook system
		// (PreToolUse/PostToolUse/… in settings.json) — a future receiver
		// lane; no observer receiver wired, so the honest value is none.
		Hook: HookSpec{Mechanism: HookNone},
		// MCP client exists (Gemini lineage, mcpServers in settings.json),
		// but settings.json embeds plaintext provider keys — an MCP writer
		// needs a guarded additive write path before this can be grounded.
		MCP:    nil,
		Native: NativeRails{},
		// ui_telemetry in-transcript records: gross input netted against
		// cached (OpenAI convention, live-evidenced), thoughts → Reasoning.
		TokenTier: TokenTier{Best: "transcript", Gap: "no cache-creation tier; counts approximate (self-reported telemetry records)"},
		// Records carry complete prompts/responses/tool bodies; `qwen -i`
		// seed verified live 2026-07-09; launched non-proxied by
		// `observer qwen` (the base-URL lane stays unprobed).
		Handoff: HandoffCapability{
			Transcript: TranscriptFull,
			Inject:     []InjectKind{InjectFile, InjectPrompt},
			Launch:     &LaunchSpec{Subcommand: "qwen"},
			Note:       "-i/--prompt-interactive seed verified live",
		},
		// Attach grounded 2026-07-24 (attach-all-launchers); PTY handoff only
		// — no prompt seeding, token capture path unchanged.
		Attach: &AttachSpec{Subcommand: "qwen"},
		// Native resume GROUNDED, live-verified 2026-07-24: `qwen --resume <id>`
		// reattaches the real session; id is raw. The `observer qwen` launcher
		// maps `--resume <id>` to it.
		Resume: ResumeSpec{Kind: ResumeNative, Subcommand: "qwen", IDMechanism: "flag:--resume"},
		// AuthEnv zero: qwen-code keys live inside settings.json
		// (modelProviders[].apiKey / envKey), a file store — no grounded
		// top-level runtime key env to forward.
		// Binary resolution + grounded installs. Unix launcher resolves
		// "qwen"; npm @qwen-code/qwen-code@latest (any OS) + the standalone
		// install script + brew (QwenLM/qwen-code + official docs).
		Binary: &BinaryResolveSpec{
			// npm JS bin: Windows install lays down a `.cmd` shim (+
			// .ps1/POSIX-shell forms), never an `.exe` — see the
			// command-code row's Binary comment for the long-form
			// rationale.
			Names: BinaryNames{
				Unix:    []string{"qwen"},
				Windows: []string{"qwen.cmd", "qwen"},
			},
			Installs: []InstallHint{
				{OS: "", Channel: "npm", Argv: []string{"npm", "install", "-g", "@qwen-code/qwen-code@latest"}, Display: "npm install -g @qwen-code/qwen-code@latest"},
				{OS: "linux", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://qwen-code-assets.oss-cn-hangzhou.aliyuncs.com/installation/install-qwen-standalone.sh | bash"}, Display: "curl -fsSL https://qwen-code-assets.oss-cn-hangzhou.aliyuncs.com/installation/install-qwen-standalone.sh | bash"},
				{OS: "darwin", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://qwen-code-assets.oss-cn-hangzhou.aliyuncs.com/installation/install-qwen-standalone.sh | bash"}, Display: "curl -fsSL https://qwen-code-assets.oss-cn-hangzhou.aliyuncs.com/installation/install-qwen-standalone.sh | bash"},
				{OS: "darwin", Channel: "brew", Argv: []string{"brew", "install", "qwen-code"}, Display: "brew install qwen-code"},
			},
		},
	},
	"kiro-cli": {
		Tool:       "kiro-cli",
		Vocabulary: Vocabulary{InTaxonomy: true},
		// SigV4-signed AWS endpoints (CodeWhisperer lineage); no base-URL /
		// BYOK surface exists on a live install — grounded negative.
		Proxy:       nil,
		Routability: RouteStatusNativeExempt,
		// Kiro has first-class agent hooks (~/.kiro/hooks) upstream, but no
		// observer receiver is wired — honest none.
		Hook: HookSpec{Mechanism: HookNone},
		// ~/.kiro/settings/mcp.json is the documented MCP surface; absent on
		// this install, shape unconfirmed — no writer until grounded.
		MCP:    nil,
		Native: NativeRails{},
		// Mode-dependent dual store (flat bundles + conversations_v2 sqlite);
		// local token counts were structurally 0/null in every capture.
		TokenTier: TokenTier{Best: "transcript", Gap: "no proxy tier (SigV4); local token counts 0/null in practice — credit metering only, not stored as tokens"},
		// Both layouts re-readable (adapter implements ReadTranscript);
		// `kiro-cli chat "<seed>"` positional seed verified live 2026-07-09;
		// launched non-proxied by `observer kiro` (SigV4 — nothing to route).
		Handoff: HandoffCapability{
			Transcript: TranscriptFull,
			Inject:     []InjectKind{InjectFile, InjectPrompt},
			Launch:     &LaunchSpec{Subcommand: "kiro"},
			Note:       "chat positional seed verified live; dual-store reader",
		},
		// Attach grounded 2026-07-24 (attach-all-launchers); PTY handoff only
		// — no prompt seeding, token capture path unchanged.
		Attach: &AttachSpec{Subcommand: "kiro"},
		// Native resume GROUNDED, live-verified 2026-07-24: `kiro-cli chat
		// --resume-id <id>` — the resume flag lives on the `chat` SUBCOMMAND. The
		// `observer kiro` launcher maps `--resume <id>` to it (composes the chat
		// subcommand + --resume-id).
		Resume: ResumeSpec{Kind: ResumeNative, Subcommand: "kiro", IDMechanism: "flag:--resume-id"},
		// AuthEnv zero — DELIBERATE exclusion: kiro-cli uses the AWS credential
		// chain (SigV4). Forwarding the AWS_* family (AWS_ACCESS_KEY_ID /
		// AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN / AWS_PROFILE / …) is a much
		// larger blast radius than a single provider key, so it is deferred, not
		// declared here.
		// Binary resolution + grounded install. Unix launcher resolves
		// "kiro-cli"; official install script (kiro.dev/docs/cli/
		// installation). Homebrew is explicitly NOT supported per vendor
		// docs. Windows hint is display-only (no Windows binary spelling
		// grounded yet).
		Binary: &BinaryResolveSpec{
			Names: BinaryNames{Unix: []string{"kiro-cli"}},
			Installs: []InstallHint{
				{OS: "linux", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://cli.kiro.dev/install | bash"}, Display: "curl -fsSL https://cli.kiro.dev/install | bash"},
				{OS: "darwin", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://cli.kiro.dev/install | bash"}, Display: "curl -fsSL https://cli.kiro.dev/install | bash"},
				{OS: "windows", Channel: "script", Argv: []string{"powershell", "-Command", "irm 'https://cli.kiro.dev/install.ps1' | iex"}, Display: "irm 'https://cli.kiro.dev/install.ps1' | iex"},
			},
		},
	},
	"grok": {
		Tool:       "grok",
		Vocabulary: Vocabulary{InTaxonomy: true},
		// LIVE-VERIFIED 2026-07-09: grok's CLI chat proxy base URL is
		// overridable via the GROK_CLI_CHAT_PROXY_BASE_URL env var (the env
		// form of the `grok agent --cli-chat-proxy-base-url` flag; the
		// top-level `grok` rejects that flag as an argument, but DOES honor the
		// env var with `-p/--single`). Setting
		// GROK_CLI_CHAT_PROXY_BASE_URL=http://127.0.0.1:8820/up/grok/v1 routed a
		// live `grok -p` turn through the /up/grok upstream ([proxy.upstreams]
		// grok = https://cli-chat-proxy.grok.com): grok fetched models from
		// /up/grok/v1/models and its OpenAI-Responses turn landed an api_turns
		// row (id 23025, provider=openai, model grok-4.5, 11394/29 tokens,
		// HTTP 200). The per-model base_url is https://cli-chat-proxy.grok.com/v1
		// (api_backend:"responses") — so the /v1 belongs in the override.
		// Routable now, but observer drives no route today — the `observer grok`
		// launcher is non-proxied and init writes no route — so Proxy stays nil
		// while the writer is pending (the cline/aider/goose "routable_now,
		// writer-pending" pattern).
		Proxy:       nil,
		Routability: RouteStatusRoutableNow,
		// updates.jsonl shows hook_execution records (ACP pre_tool_use fired
		// live) — a real upstream hook system, but no observer receiver is
		// wired; honest none.
		Hook: HookSpec{Mechanism: HookNone},
		// events.jsonl shows an MCP client connecting (mcp_server_connected,
		// 25 tools), but no grounded config-writer surface — nil until then.
		MCP:    nil,
		Native: NativeRails{},
		// Global ~/.grok/logs/unified.jsonl inference_done lines carry
		// {prompt, cached_prompt, completion, reasoning} tokens WITH a `sid`
		// correlation key (no timestamp heuristics). prompt is GROSS
		// (cached ⊂ prompt, live-proven) — the adapter nets it.
		TokenTier: TokenTier{Best: "transcript", Gap: "no cache-creation split; session-bundle counts are cumulative-only (unified.jsonl is the split source)"},
		// chat_history.jsonl re-readable (ReadTranscript); positional seed
		// (`grok "<seed>"`) verified live 2026-07-09 → `observer grok`.
		Handoff: HandoffCapability{
			Transcript: TranscriptFull,
			Inject:     []InjectKind{InjectFile, InjectPrompt},
			Launch:     &LaunchSpec{Subcommand: "grok"},
			Note:       "positional seed verified live; tool-exec capture still owed (plan-agent default)",
		},
		// Attach grounded 2026-07-24 (attach-all-launchers); PTY handoff only
		// — no prompt seeding, token capture path unchanged.
		Attach: &AttachSpec{Subcommand: "grok"},
		// Native resume GROUNDED, live-verified 2026-07-24: `grok --resume <id>`
		// reattaches the real session; id is raw. The `observer grok` launcher
		// maps `--resume <id>` to it.
		Resume: ResumeSpec{Kind: ResumeNative, Subcommand: "grok", IDMechanism: "flag:--resume"},
		// Credential env forwarded across the attach socket. Upstream-verified
		// (docs.x.ai/build/overview): XAI_API_KEY is grok's documented
		// headless / non-browser auth key. NAMES only.
		AuthEnv: []string{"XAI_API_KEY"},
		// Binary resolution + grounded installs. Unix launcher resolves
		// "grok"; npm @xai-official/grok (any OS) + the official install
		// script (docs.x.ai/build/overview).
		Binary: &BinaryResolveSpec{
			// npm JS bin: Windows install lays down a `.cmd` shim (+
			// .ps1/POSIX-shell forms), never an `.exe` — see the
			// command-code row's Binary comment for the long-form
			// rationale.
			Names: BinaryNames{
				Unix:    []string{"grok"},
				Windows: []string{"grok.cmd", "grok"},
			},
			Installs: []InstallHint{
				{OS: "", Channel: "npm", Argv: []string{"npm", "install", "-g", "@xai-official/grok"}, Display: "npm install -g @xai-official/grok"},
				{OS: "linux", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://x.ai/cli/install.sh | bash"}, Display: "curl -fsSL https://x.ai/cli/install.sh | bash"},
				{OS: "darwin", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://x.ai/cli/install.sh | bash"}, Display: "curl -fsSL https://x.ai/cli/install.sh | bash"},
				{OS: "windows", Channel: "script", Argv: []string{"powershell", "-Command", "irm https://x.ai/cli/install.ps1 | iex"}, Display: "irm https://x.ai/cli/install.ps1 | iex"},
			},
		},
	},
	"kimi-code": {
		Tool:       "kimi-code",
		Vocabulary: Vocabulary{InTaxonomy: true},
		// Live wire traces show an openai-compat endpoint (provider:openai,
		// gpt-4o) configured in ~/.kimi-code/config.toml — a NEVER-READ file
		// (plaintext API key). The [providers.openai] block carries an
		// OpenAI-shaped sk-proj key and NO base_url, so its implicit default
		// host is api.openai.com — the fixed OpenAI upstream the proxy routes
		// to.
		// PROMOTED — LIVE-VERIFIED 2026-07-09 (operator-approved probe): the
		// guarded, ADDITIVE writer proxyroute.RegisterKimiCode added base_url =
		// http://127.0.0.1:8820/v1 under [providers.openai] (.bak +
		// refuse-foreign + never a key), then `kimi -p "…"` (default_model
		// openai/gpt-4o) routed through the proxy and landed an api_turns row
		// (id 23075, provider=openai, model gpt-4o-2024-08-06, 11181/2 tokens,
		// HTTP 200). Proxy now drives the config-lane route; init applies it via
		// the RegisterKimiCode writer (dispatched on Kind).
		Proxy: &ProxyRoute{
			Kind:     RouteConfigFile,
			EnvVar:   "",
			Suffix:   "/v1",
			Launcher: "observer kimi",
			Note:     "routes via base_url under [providers.openai] in ~/.kimi-code/config.toml (proxyroute.RegisterKimiCode, additive, never a key); implicit host was api.openai.com so the fixed OpenAI upstream applies. Live-verified 2026-07-09 (api_turns 23075, gpt-4o-2024-08-06)",
		},
		// ProxyProbe PERSISTS after promotion: it is the config-lane WRITER
		// BINDING init uses to apply the (now verified) route on a machine
		// whose ~/.kimi-code/config.toml is not yet routed. Proxy above is
		// the verified route; this is how init writes it.
		ProxyProbe: &ProxyRoute{
			Kind:     RouteConfigFile,
			Launcher: "observer kimi",
			Note:     "proxyroute.RegisterKimiCode adds base_url under [providers.openai] in ~/.kimi-code/config.toml (additive, never a key)",
		},
		Routability: RouteStatusRoutableNow,
		// No hook surface observed on the live install — honest none.
		Hook: HookSpec{Mechanism: HookNone},
		// MCP client exists (mcp__* names in tools.set_active_tools), but the
		// only config surface is the never-read config.toml — no writer until
		// a guarded additive path is grounded.
		MCP:    nil,
		Native: NativeRails{},
		// wire.jsonl usage.record events: {inputOther (already NET of cache),
		// output, inputCacheRead, inputCacheCreation}. No reasoning split.
		TokenTier: TokenTier{Best: "transcript", Gap: "no reasoning split; counts self-reported (wire-trace usage.record)"},
		// wire.jsonl folds prompts/assistant text/tool bodies (ReadTranscript
		// on both lanes), but there is NO seed lane — `-p` prints and exits,
		// the TUI takes no initial-prompt flag (live-verified 2026-07-09) —
		// so the launch is DocAssisted (hermes precedent): `observer kimi
		// --continue-from` writes the doc + opens the TUI.
		Handoff: HandoffCapability{
			Transcript: TranscriptFull,
			Inject:     []InjectKind{InjectFile},
			Launch:     &LaunchSpec{Subcommand: "kimi", Mode: LaunchDocAssisted},
			Note:       "no seed lane (-p prints+exits; TUI seedless) — doc-assisted launch",
		},
		// Attach grounded 2026-07-24 (attach-all-launchers); PTY handoff only
		// — no prompt seeding, token capture path unchanged. DocAssisted only
		// gates --continue-from seeding (incompatible with attach); plain attach
		// opens the TUI seedless.
		Attach: &AttachSpec{Subcommand: "kimi"},
		// Native resume GROUNDED, live-verified 2026-07-24: `kimi --session <id>`
		// (short `-S`) reattaches the real session, but the id MUST be the
		// PREFIXED form `session_<uuid>` — a bare uuid HARD-FAILS. Our adapter
		// already stores the SessionID in exactly that prefixed form (the
		// `session_<uuid>` directory component — internal/adapter/kimicode/
		// paths.go::sessionIDFromPath), so the `observer kimi` launcher's
		// ensure-prefix transform is idempotent (a stored id passes through). The
		// launcher maps `--resume <id>` to it.
		Resume: ResumeSpec{Kind: ResumeNative, Subcommand: "kimi", IDMechanism: "flag:--session"},
		// AuthEnv zero: kimi-code reads its key from [providers.*] in
		// ~/.kimi-code/config.toml — file auth, no grounded runtime key env.
		// Binary resolution. Unix launcher resolves "kimi". No grounded
		// install channel yet (research pending) → Installs nil, Windows nil.
		Binary: &BinaryResolveSpec{Names: BinaryNames{Unix: []string{"kimi"}}},
	},
	"crush": {
		Tool:       "crush",
		Vocabulary: Vocabulary{InTaxonomy: true},
		// Crush providers support custom base_url incl. an `anthropic` type —
		// a real proxy lane. The provider config lives in crush.json alongside
		// literal API keys (never-read/never-write file); providers.openai
		// carried an OpenAI-shaped sk-proj key and NO base_url, so its implicit
		// default host is api.openai.com — the fixed OpenAI upstream.
		// PROMOTED — LIVE-VERIFIED 2026-07-09 (operator-approved probe): the
		// guarded, ADDITIVE writer proxyroute.RegisterCrush set
		// providers.openai.base_url = http://127.0.0.1:8820/v1 (.bak +
		// refuse-foreign + never a key) in the live Windows-side config
		// (/mnt/c/Users/.../AppData/Local/crush/crush.json, via CRUSH_CONFIG —
		// the Linux crush binary ignores CRUSH_CONFIG and has no config here).
		// A `crush run` turn driven from the Windows crush.cmd reached WSL's
		// :8820 over localhost forwarding and landed api_turns rows (id 23081
		// gpt-5.4-mini 6613/20 + id 23080 gpt-5.4-nano title call,
		// provider=openai, HTTP 200). Proxy now drives the config-lane route;
		// init applies it via the RegisterCrush writer (dispatched on Kind).
		Proxy: &ProxyRoute{
			Kind:   RouteProviderJSON,
			EnvVar: "",
			Suffix: "/v1",
			Note:   "routes via providers.openai.base_url in crush.json (proxyroute.RegisterCrush, additive, never a key); implicit host was api.openai.com so the fixed OpenAI upstream applies; no observer launcher (init/writer applies it). Live-verified 2026-07-09 via the Windows crush.cmd → WSL :8820 (api_turns 23081/23080)",
		},
		// ProxyProbe PERSISTS after promotion: it is the config-lane WRITER
		// BINDING init uses to apply the (now verified) route on a machine
		// whose crush.json is not yet routed. Proxy above is the verified
		// route; this is how init writes it.
		ProxyProbe: &ProxyRoute{
			Kind: RouteProviderJSON,
			Note: "proxyroute.RegisterCrush sets providers.openai.base_url in crush.json (additive, never a key)",
		},
		Routability: RouteStatusRoutableNow,
		Hook:        HookSpec{Mechanism: HookNone},
		// MCP client exists, but its config also lives in crush.json — same
		// guarded-write constraint as the proxy lane.
		MCP:    nil,
		Native: NativeRails{},
		// Project-local .crush/crush.db; tokens + pre-computed cost are
		// session-cumulative (no per-message split).
		TokenTier: TokenTier{Best: "sqlite", Gap: "session-cumulative counts only (no per-message split, no cache/reasoning breakdown)"},
		// messages.parts carry full text/reasoning/tool bodies; NO seed lane
		// (upstream charmbracelet/crush#1791) — file-lane carry only.
		Handoff: HandoffCapability{
			Transcript: TranscriptFull,
			Inject:     []InjectKind{InjectFile},
			Note:       "no interactive-seed lane (upstream #1791)",
		},
	},
	"devin": {
		Tool:       "devin",
		Vocabulary: Vocabulary{InTaxonomy: true},
		// No base-URL override exists — the CLI talks to Cognition's own
		// Windsurf backend (only an HTTP-proxy setting). No observer-routed
		// turn is possible today.
		Proxy:       nil,
		Routability: RouteStatusNativeExempt,
		// `.devin/hooks.v1.json` is explicitly Claude-Code-compatible but
		// UNWIRED in the shipped CLI (live 3000.1.27) — honest none until a
		// firing hook is grounded.
		Hook: HookSpec{Mechanism: HookNone},
		// MCP client only (reads .devin/ config). The registration format IS
		// now grounded (`.devin-plugin/plugin.json` + root `mcp_config.json`,
		// coverage wave A 2026-07-31 — see plugins/devin/); distribution is
		// the plugins channel, and no `observer init` writer is built, so the
		// capability stays nil.
		MCP:    nil,
		Native: NativeRails{},
		// Per-message metadata.metrics in sessions.db (input/output/cache
		// fields + ttft). Cache read/creation were NULL in every captured
		// row, so gross-vs-net is unverified until a cached row appears.
		TokenTier: TokenTier{Best: "sqlite", Gap: "cache splits null in all captured rows (gross-vs-net unverified); no reasoning-token split (thinking folded into output)"},
		// ReadTranscript re-walks the message_nodes main chain (DB lane).
		// Positional seed contract operator-verified on a real TTY 2026-07-09
		// (`devin -- "<prompt>"`, clap last-only positional after the `--`
		// separator) → `observer devin`.
		Handoff: HandoffCapability{
			Transcript: TranscriptFull,
			Inject:     []InjectKind{InjectFile, InjectPrompt},
			Launch:     &LaunchSpec{Subcommand: "devin"},
			Note:       "positional seed contract operator-verified on a real TTY 2026-07-09 (`devin -- \"<prompt>\"`, clap last-only positional after the `--` separator); launcher `observer devin` seed-only/non-proxied (native_exempt, no base-URL knob)",
		},
		// Attach grounded 2026-07-24 (attach-all-launchers); PTY handoff only
		// — no prompt seeding, token capture path unchanged.
		Attach: &AttachSpec{Subcommand: "devin"},
		// Native resume GROUNDED, live-verified 2026-07-24: `devin --resume <id>`
		// reattaches the real session; id is a human MNEMONIC (e.g.
		// `noon-quince`) — the sessions.db primary key == our SessionID. The
		// `observer devin` launcher maps `--resume <id>` to it.
		Resume: ResumeSpec{Kind: ResumeNative, Subcommand: "devin", IDMechanism: "flag:--resume"},
		// AuthEnv zero: devin talks to Cognition's own backend with no grounded
		// key env (file/OAuth auth) — no credential-env to forward.
		// Binary resolution + grounded install. Unix launcher resolves
		// "devin"; official install script (devin.ai/cli). No official
		// Windows path — the winget listing is third-party, not shipped.
		Binary: &BinaryResolveSpec{
			Names: BinaryNames{Unix: []string{"devin"}},
			Installs: []InstallHint{
				{OS: "linux", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://cli.devin.ai/install.sh | bash"}, Display: "curl -fsSL https://cli.devin.ai/install.sh | bash"},
				{OS: "darwin", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://cli.devin.ai/install.sh | bash"}, Display: "curl -fsSL https://cli.devin.ai/install.sh | bash"},
			},
		},
	},
	"qoder": {
		Tool:       "qoder",
		Vocabulary: Vocabulary{InTaxonomy: true},
		// Hardcoded api.qoder.com, PAT auth, NO base-URL knob — token
		// capture is proxy-tier or nothing, and there is no proxy lane.
		Proxy:       nil,
		Routability: RouteStatusNativeExempt,
		// `qodercli hooks` manage-command exists (CC lineage) but no firing
		// hook envelope has been grounded — honest none.
		Hook: HookSpec{Mechanism: HookNone},
		// MCP client exists (`qodercli mcp`, --mcp-config). The bundling
		// format IS now grounded (`.qoder-plugin/plugin.json` + dotted
		// `.mcp.json`, validated by `qodercli plugins validate` — coverage
		// wave A 2026-07-31, see plugins/qoder/); distribution is the plugins
		// channel, and no `observer init` writer is built, so the capability
		// stays nil.
		MCP:    nil,
		Native: NativeRails{},
		// Local stores carry NEITHER model (empty string) NOR tokens
		// (structurally zero even with healthy auth — live-disproven
		// auth-refresh hypothesis 2026-07-09); usage is server-side only.
		// The segment parse is zero-guarded so future counts would flow.
		TokenTier: TokenTier{Best: "none", Gap: "usage server-side only — local logs carry zero tokens and no model; no base-URL knob"},
		// CC-shaped transcript folds prompts/text/tool bodies (ReadTranscript
		// both lanes); `-i/--prompt-interactive <text>` seed verified live →
		// `observer qoder --continue-from` (non-proxied).
		Handoff: HandoffCapability{
			Transcript: TranscriptFull,
			Inject:     []InjectKind{InjectFile, InjectPrompt},
			Launch:     &LaunchSpec{Subcommand: "qoder"},
			Note:       "seeds via -i flag value (binary is `qodercli`)",
		},
		// Attach grounded 2026-07-24 (attach-all-launchers); PTY handoff only
		// — no prompt seeding, token capture path unchanged.
		Attach: &AttachSpec{Subcommand: "qoder"},
		// Native resume GROUNDED, live-verified 2026-07-24: `qodercli --resume
		// <id>` reattaches the real session; id is a raw uuid (binary is
		// `qodercli`). The `observer qoder` launcher maps `--resume <id>` to it.
		Resume: ResumeSpec{Kind: ResumeNative, Subcommand: "qoder", IDMechanism: "flag:--resume"},
		// AuthEnv zero: qoder uses PAT auth to api.qoder.com (file store), no
		// grounded runtime key env to forward.
		// Binary resolution + grounded installs. Unix launcher resolves
		// "qodercli" (the binary is qodercli, not qoder); npm
		// @qoder-ai/qodercli (any OS) + the official install script
		// (qoder.com/cli + npm).
		Binary: &BinaryResolveSpec{
			// npm JS bin: Windows install lays down a `.cmd` shim (+
			// .ps1/POSIX-shell forms), never an `.exe` — see the
			// command-code row's Binary comment for the long-form
			// rationale.
			Names: BinaryNames{
				Unix:    []string{"qodercli"},
				Windows: []string{"qodercli.cmd", "qodercli"},
			},
			Installs: []InstallHint{
				{OS: "", Channel: "npm", Argv: []string{"npm", "install", "-g", "@qoder-ai/qodercli"}, Display: "npm install -g @qoder-ai/qodercli"},
				{OS: "linux", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://qoder.com/install | bash"}, Display: "curl -fsSL https://qoder.com/install | bash"},
				{OS: "darwin", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://qoder.com/install | bash"}, Display: "curl -fsSL https://qoder.com/install | bash"},
			},
		},
	},
	"aider": {
		Tool:       "aider",
		Vocabulary: Vocabulary{InTaxonomy: true},
		// Aider honors OPENAI_API_BASE (LiteLLM-shaped) — a real base-URL
		// surface, LIVE-GROUNDED 2026-07-09: a probe with
		// OPENAI_API_BASE=http://127.0.0.1:8820/v1 (aider --message …
		// --model gpt-4o-mini) landed an api_turns row (provider openai,
		// model gpt-4o-mini-2024-07-18). The knob is proven routable, but
		// observer drives no route today — there is no `observer aider`
		// launcher and no init writer (aider takes the base URL from the
		// operator's own env / .aider.conf.yml) — so Proxy stays nil while
		// the writer is pending (the cline/kilo VS-Code "routable_now,
		// writer-pending" pattern).
		Proxy:       nil,
		Routability: RouteStatusRoutableNow,
		// No pre/post-tool hook surface exists.
		Hook: HookSpec{Mechanism: HookNone},
		// Not an MCP client host observer writes into.
		MCP:    nil,
		Native: NativeRails{},
		// Tokens exist only as prose lines in the Markdown transcript
		// ("Tokens: 10.0k sent, ..."), format_tokens-ROUNDED; `sent` is
		// GROSS → the adapter nets it against the cache-hit clause. Aider's
		// own per-message Cost is carried as EstimatedCostUSD.
		TokenTier: TokenTier{Best: "transcript", Gap: "prose-only rounded counts (unreliable precision); no reasoning split; no per-turn timestamps"},
		// The per-repo .aider.chat.history.md is fully re-readable, but
		// there is NO seed lane: --message runs one turn and exits, the
		// REPL takes no preload flag — file-lane carry only, no launcher.
		Handoff: HandoffCapability{
			Transcript: TranscriptFull,
			Inject:     []InjectKind{InjectFile},
			Note:       "no interactive-seed lane (--message exits after the turn)",
		},
	},
	"goose": {
		Tool:       "goose",
		Vocabulary: Vocabulary{InTaxonomy: true},
		// Goose reads OPENAI_HOST (NOT OPENAI_BASE_URL; host ROOT — goose
		// appends /v1) plus per-provider host settings in config.yaml — a
		// real override surface, LIVE-GROUNDED 2026-07-09: a probe on the
		// openai provider (--provider openai + OPENAI_HOST=http://127.0.0.1:8820,
		// --model gpt-4o-mini, keyring disabled so the env key is used)
		// landed api_turns rows (provider openai, model gpt-4o-mini-2024-07-18,
		// real usage). The knob is proven routable, but the `observer goose`
		// launcher is deliberately NON-PROXIED — setting OPENAI_HOST would
		// redirect an operator's already-configured provider (the live config
		// here defaulted to openrouter) — and init writes no route, so
		// observer drives no route today. Proxy stays nil while the writer is
		// pending (the cline/kilo VS-Code "routable_now, writer-pending"
		// pattern).
		Proxy:       nil,
		Routability: RouteStatusRoutableNow,
		// No pre/post-tool hook surface exists (extensions are MCP servers,
		// not hooks).
		Hook: HookSpec{Mechanism: HookNone},
		// Goose IS an MCP client — extensions in config.yaml are MCP
		// servers, a future additive-registration candidate — but no
		// guarded write path into config.yaml is grounded; no writer.
		MCP:    nil,
		Native: NativeRails{},
		// sessions.db carries the richest session-level token columns of
		// the 2026-07 wave (incl. accumulated_* + accumulated_cost), but
		// messages.tokens was NULL in every 1.41.0 capture — no per-message
		// split. input_tokens is GROSS (cache_read ⊂ input, single-turn
		// proof) → the adapter nets it. Token-EMPTY sessions persist on
		// provider errors.
		TokenTier: TokenTier{Best: "sqlite", Gap: "session-level counts only (messages.tokens null in all captures); no reasoning split; cache_write null on OpenAI"},
		// messages.content_json re-readable (ReadTranscript, DB lane);
		// `goose run -t "<seed>" -s` seed-then-interactive verified live
		// 2026-07-09 (keyed run) → `observer goose`.
		Handoff: HandoffCapability{
			Transcript: TranscriptFull,
			Inject:     []InjectKind{InjectFile, InjectPrompt},
			Launch:     &LaunchSpec{Subcommand: "goose"},
			Note:       "seeds via `run -t <seed> -s` (seed-then-interactive verified live)",
		},
		// Attach grounded 2026-07-24 (attach-all-launchers); PTY handoff only
		// — no prompt seeding, token capture path unchanged.
		Attach: &AttachSpec{Subcommand: "goose"},
		// Native resume GROUNDED, live-verified 2026-07-24: `goose session
		// --resume --session-id <RAW>` reattaches the real session under the
		// `session` SUBCOMMAND. Observer stores the SCOPED id `<id>@<hash8>`
		// (scopedSessionID, internal/adapter/goose/parse.go), so the `observer
		// goose` launcher STRIPS everything from the first `@` before composing
		// the native argv. It maps `--resume <id>` to it.
		Resume: ResumeSpec{Kind: ResumeNative, Subcommand: "goose", IDMechanism: "flag:--session-id"},
		// AuthEnv zero: goose resolves provider keys from its keyring / config.yaml
		// by default (per-provider env keys exist but are not the grounded runtime
		// source here) — file auth, no key declared.
		// Binary resolution + grounded installs. Unix launcher resolves
		// "goose"; its installer drops the binary under .local/bin (a
		// per-tool extra). Official install script + brew (repo moved
		// block/goose → aaif-goose/goose, Linux Foundation AAIF, Dec 2025 —
		// goose-docs.ai installation page).
		Binary: &BinaryResolveSpec{
			Names:     BinaryNames{Unix: []string{"goose"}},
			ProbeDirs: []ProbeDir{{OS: ProbeUnix, Rel: ".local/bin"}},
			Installs: []InstallHint{
				{OS: "linux", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://github.com/aaif-goose/goose/releases/download/stable/download_cli.sh | bash"}, Display: "curl -fsSL https://github.com/aaif-goose/goose/releases/download/stable/download_cli.sh | bash"},
				{OS: "darwin", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://github.com/aaif-goose/goose/releases/download/stable/download_cli.sh | bash"}, Display: "curl -fsSL https://github.com/aaif-goose/goose/releases/download/stable/download_cli.sh | bash"},
				{OS: "darwin", Channel: "brew", Argv: []string{"brew", "install", "block-goose-cli"}, Display: "brew install block-goose-cli"},
				{OS: "linux", Channel: "brew", Argv: []string{"brew", "install", "block-goose-cli"}, Display: "brew install block-goose-cli"},
			},
		},
	},

	// Browser-chatbot rail (Phase 1 = ChatGPT only). Captured by the opt-in
	// MV3 browser extension's native-messaging bridge, NOT a coding CLI or a
	// watcher/SQLite adapter — so there is no local session file, no proxy
	// leg, and no server-side usage field. The other planned *-web sites
	// (claude-web / perplexity-web / gemini-web / copilot-web) join in
	// Phase 2. See docs/plans/browser-extension-and-m365-copilot-proposal-
	// 2026-07-10.md + internal/adapter/browserchat.
	"chatgpt-web": {
		Tool:       "chatgpt-web",
		Vocabulary: Vocabulary{Note: "no native tool vocabulary: the MV3 browser-capture lane records ChatGPT prompt/answer turns from the chat UI, never tool calls, so tooltax carries no rows for it"},
		// The real browser makes the real request; observer only OBSERVES a
		// captured turn relayed by the extension. There is no base-URL knob
		// to route (routing would re-originate TLS and risk flagging the
		// user's own account) — natively exempt, a grounded negative.
		Proxy:       nil,
		Routability: RouteStatusNativeExempt,
		// The extension attaches via the native-messaging bridge (the
		// browser launches `observer browser hook`), not an AI-tool config
		// file. AutoWired is true as of Phase 2: `observer init`'s 4th
		// consent step writes the per-browser native-messaging host manifest
		// (extensionSupported → browserhost.Registrar), so the doctor
		// honestly reports the rail as auto-wired.
		Hook:   HookSpec{Mechanism: HookBrowserExtension, AutoWired: true},
		MCP:    nil,
		Native: NativeRails{}, // no vendor admin/usage API for the consumer web app.
		// Tokens are ALWAYS estimated: no target UI returns authoritative
		// counts, so the extension estimates client-side (gpt-tokenizer) and
		// the server may recompute a chars/4 heuristic — either way
		// TokenSourceEstimated + ReliabilityUnreliable.
		TokenTier: TokenTier{Best: "browser_extension", Gap: "no server-side usage field; estimated only"},
		// Handoff zero value: the browser rail has no re-readable on-disk
		// transcript (the turn lives only in the browser), so it stays the
		// honest actions-only floor with the universal file lane. Not
		// launchable in-terminal.
		Handoff: HandoffCapability{},
	},
	// Claude.ai browser web app (SSE content_block_delta). Same rail shape
	// as chatgpt-web — the only differences are DATA (host, default model,
	// tokenizer family) in internal/adapter/browserchat.siteRules.
	"claude-web": {
		Tool:        "claude-web",
		Vocabulary:  Vocabulary{Note: "no native tool vocabulary: browser-captured Claude.ai chat turns only (no tool-call surface in the DOM/WS capture)"},
		Proxy:       nil,
		Routability: RouteStatusNativeExempt,
		Hook:        HookSpec{Mechanism: HookBrowserExtension, AutoWired: true},
		MCP:         nil,
		Native:      NativeRails{},
		TokenTier:   TokenTier{Best: "browser_extension", Gap: "no server-side usage field; estimated only (Anthropic tokenizer is documented-inaccurate for Claude 3+)"},
		Handoff:     HandoffCapability{},
	},
	// Perplexity browser web app (SSE /rest/sse/perplexity_ask — NOT the
	// Comet automation WebSocket).
	"perplexity-web": {
		Tool:        "perplexity-web",
		Vocabulary:  Vocabulary{Note: "no native tool vocabulary: browser-captured Perplexity chat turns only"},
		Proxy:       nil,
		Routability: RouteStatusNativeExempt,
		Hook:        HookSpec{Mechanism: HookBrowserExtension, AutoWired: true},
		MCP:         nil,
		Native:      NativeRails{},
		TokenTier:   TokenTier{Best: "browser_extension", Gap: "no server-side usage field; estimated only (no light client tokenizer — chars/4 heuristic)"},
		Handoff:     HandoffCapability{},
	},
	// Gemini browser web app (BatchExecute RPC — the hardest transport). The
	// extension parser is BEST-EFFORT / incomplete (proposal §3.4); the Gap
	// says so plainly so the honesty is visible in the doctor.
	"gemini-web": {
		Tool:        "gemini-web",
		Vocabulary:  Vocabulary{Note: "no native tool vocabulary: browser-captured Gemini chat turns only (BatchExecute best-effort)"},
		Proxy:       nil,
		Routability: RouteStatusNativeExempt,
		Hook:        HookSpec{Mechanism: HookBrowserExtension, AutoWired: true},
		MCP:         nil,
		Native:      NativeRails{},
		TokenTier:   TokenTier{Best: "browser_extension", Gap: "no server-side usage field; estimated only; BatchExecute RPC parser is best-effort/incomplete (highest-maintenance site)"},
		Handoff:     HandoffCapability{},
	},
	// Consumer Copilot browser web app (copilot.microsoft.com — WebSocket
	// frames). NOT GitHub Copilot (see "copilot"/"copilot-cli") and NOT
	// enterprise M365 Copilot (a separate org-tier connector, out of the
	// browser rail).
	"copilot-web": {
		Tool:        "copilot-web",
		Vocabulary:  Vocabulary{Note: "no native tool vocabulary: browser-captured consumer-Copilot chat turns only (WebSocket capture, cf_clearance-gated)"},
		Proxy:       nil,
		Routability: RouteStatusNativeExempt,
		Hook:        HookSpec{Mechanism: HookBrowserExtension, AutoWired: true},
		MCP:         nil,
		Native:      NativeRails{},
		TokenTier:   TokenTier{Best: "browser_extension", Gap: "no server-side usage field; estimated only; WebSocket-frame parser (cf_clearance-gated)"},
		Handoff:     HandoffCapability{},
	},
	// Factory AI's "droid" CLI (docs/plans/factory-droid-adapter-plan-2026-07-29.md).
	// Phase-0 research only — no adapter package yet (Phase A wiring row).
	"droid": {
		Tool:       "droid",
		Vocabulary: Vocabulary{InTaxonomy: true},
		// No live-verified route today; BYOK custom models call the
		// underlying provider directly (the only near-routable_now
		// candidate) but that's unverified — no live turn through :8820.
		Proxy:       nil,
		Routability: RouteStatusProbeRequired,
		// No hook subcommand found in `droid --help`.
		Hook: HookSpec{Mechanism: HookNone},
		// ~/.factory/mcp.json reuses claude-code/cursor's {"mcpServers":{}}
		// shape (format confirmed live via a zero-cost `droid mcp add`/
		// `remove` probe). A writer now exists (internal/mcp/register.go's
		// generic registerJSONMCP, via the internal/mcp/locate row) so this
		// is Implemented — parking §3.4.
		MCP:    &MCPTarget{Format: MCPServersJSON, PathHint: ".factory/mcp.json", Implemented: true},
		Native: NativeRails{},
		// Sidecar <uuid>.settings.json carries session-level cumulative
		// tokens only — no per-message token field in the JSONL itself.
		TokenTier: TokenTier{Best: "jsonl", Gap: "no per-message tokens; no proxy path verified; Factory-hosted built-in-model wire shape entirely unconfirmed (no active subscription in this corpus)"},
		Handoff: HandoffCapability{
			Transcript: TranscriptFull,
			Inject:     []InjectKind{InjectFile, InjectPrompt},
			// Seed contract grounded 2026-07-29 against `droid --help`
			// (v0.181.0): `Usage: droid [options] [command] [prompt...]`
			// with the worked example `droid "review app.tsx"   Start with
			// an initial prompt` — the initial prompt is a bare positional
			// on the DEFAULT (TUI) command, so the launcher appends the
			// handover as the TRAILING positional after any forwarded
			// flags. `droid exec [prompt]` is the headless one-shot ("Run
			// non-interactively (for scripts/automation)") and is rejected
			// under --continue-from rather than silently seeded.
			Launch: &LaunchSpec{Subcommand: "droid", Mode: LaunchSeeded},
		},
		// Attachable: `observer droid --attach` hands the PTY to the daemon.
		Attach: &AttachSpec{Subcommand: "droid"},
		// Native resume: `droid --resume=<sessionId>`. Grounded 2026-07-29
		// zero-spend on the live install: `-r, --resume [sessionId]` is a
		// commander.js OPTIONAL-value option, so the JOINED `=` spelling is
		// the unambiguous form (the cursor precedent); a REAL uuid from
		// ~/.factory/sessions reopened that session's TUI (no model call,
		// transcript mtime unchanged) while a bogus uuid exited silently —
		// the two outcomes discriminate, so the flag really consumes the
		// value. The id IS our stored SessionID verbatim: the transcript is
		// `<uuid>.jsonl` and its `session_start` line carries the same
		// `"id"` (internal/adapter/droid/adapter.go), so no transform.
		Resume: ResumeSpec{Kind: ResumeNative, Subcommand: "droid", IDMechanism: "flag:--resume"},
		Binary: &BinaryResolveSpec{
			Names: BinaryNames{
				Unix:    []string{"droid"},
				Windows: []string{"droid.exe", "droid.cmd", "droid"},
			},
			// ~/.local/bin is where the live install landed (2026-07-29);
			// Factory's docs describe the installer dropping the binary
			// under ~/.factory/bin, which exists on this install too.
			ProbeDirs: []ProbeDir{
				{OS: ProbeUnix, Rel: ".local/bin"},
				{OS: ProbeUnix, Rel: ".factory/bin"},
				{OS: ProbeWindows, Rel: ".factory/bin"},
			},
			// Officially documented channels only. The macOS/Linux line is
			// the installer script's OWN documented usage (fetched
			// 2026-07-29, its first comment reads
			// `# Usage: curl -fsSL https://app.factory.ai/cli | sh`). The
			// npm package name `droid` is registry-confirmed as Factory's
			// own (repo github.com/Factory-AI/factory, directory apps/cli,
			// bin {"droid":"bin/droid"}); it carries OS "" so it is also
			// the Windows answer.
			//
			// NO Windows script hint on purpose: app.factory.ai/cli/windows
			// documents a THREE-step, optionally cookie-authenticated flow
			// (`curl.exe -b 'session=…' … -o install.ps1` → `powershell
			// -ExecutionPolicy Bypass -File install.ps1` → `del`), not a
			// one-liner — so a piped `irm | iex` hint would be ours, not
			// theirs.
			Installs: []InstallHint{
				{OS: "linux", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://app.factory.ai/cli | sh"}, Display: "curl -fsSL https://app.factory.ai/cli | sh"},
				{OS: "darwin", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://app.factory.ai/cli | sh"}, Display: "curl -fsSL https://app.factory.ai/cli | sh"},
				{OS: "", Channel: "npm", Argv: []string{"npm", "install", "-g", "droid"}, Display: "npm install -g droid"},
			},
		},
	},
	// Rebadged OpenAI Codex CLI Rust build, installed under
	// ~/.openinterpreter (docs/plans/openinterpreter-adapter-plan-2026-07-29.md).
	// Phase-0 research only — no adapter package yet (Phase A wiring row);
	// the eventual implementation is expected to retag internal/adapter/codex
	// (antigravity/antigravity-cli pattern), not a fresh package.
	"open-interpreter": {
		Tool:       "open-interpreter",
		Vocabulary: Vocabulary{InTaxonomy: true},
		// config schema present (base_url/wire_api strings confirmed in
		// the binary) but not live-verified on this fork.
		Proxy:       nil,
		Routability: RouteStatusProbeRequired,
		// Codex's hook mechanism (HookCodexConfig) may port, but this
		// fork's config.toml has no [hooks] observed live — unverified.
		Hook: HookSpec{Mechanism: HookNone},
		// [mcp_servers] TOML table almost certainly matches codex's format,
		// but MCP must be nil here: no writer exists yet (TestMCPTargets
		// requires nil for every tool without a grounded implemented
		// writer) — the format finding is preserved in the plan doc for
		// the Phase-B/C adapter build.
		MCP:    nil,
		Native: NativeRails{},
		// Rollout JSONL byte-identical to codex's; token_count event GROSS
		// input, nets the same way — Tier 2 until proxy routability
		// confirmed.
		TokenTier: TokenTier{Best: "jsonl", Gap: "no proxy path verified on this fork; hook mechanism unconfirmed"},
		Handoff: HandoffCapability{
			Transcript: TranscriptFull,
			Inject:     []InjectKind{InjectFile, InjectPrompt},
			// Seed contract grounded 2026-07-29 against THIS fork's own
			// help (not codex's): `interpreter --help` prints
			// `Usage: interpreter [OPTIONS] [PROMPT]` and
			// `Arguments: [PROMPT]  Optional user prompt to start the
			// session` — the codex trailing-positional shape. A live
			// zero-spend parse smoke confirmed the argv is accepted
			// (`interpreter "seed text"` reaches the TTY gate — "stdin is
			// not a terminal" — while an unknown token is rejected by clap
			// with "unexpected argument", so acceptance discriminates).
			Launch: &LaunchSpec{Subcommand: "open-interpreter", Mode: LaunchSeeded},
		},
		// Attachable: `observer open-interpreter --attach`.
		Attach: &AttachSpec{Subcommand: "open-interpreter"},
		// Native resume: `interpreter resume <SESSION_ID>` — the codex
		// subcommand shape, grounded 2026-07-29 on `interpreter resume
		// --help`: `Usage: interpreter resume [OPTIONS] [SESSION_ID]
		// [PROMPT]`, `[SESSION_ID]  Session id (UUID) or session name`. A
		// live zero-spend smoke accepted the argv (reached the TTY gate)
		// while clap rejects an unknown token, so acceptance is real. The
		// id is our stored SessionID verbatim: the rollout's `session_meta`
		// carries the same UUID the codex parser adopts as SessionID.
		Resume: ResumeSpec{Kind: ResumeNative, Subcommand: "open-interpreter", IDMechanism: "subcommand:resume"},
		Binary: &BinaryResolveSpec{
			Names: BinaryNames{Unix: []string{"interpreter"}, Windows: []string{"interpreter.exe"}},
			// Live install layout (2026-07-29): ~/.local/bin/interpreter is
			// a symlink into the standalone package's own bin dir.
			ProbeDirs: []ProbeDir{
				{OS: ProbeUnix, Rel: ".local/bin"},
				{OS: ProbeUnix, Rel: ".openinterpreter/packages/standalone/current/bin"},
			},
			// Officially documented channels (openinterpreter.com
			// /docs/terminal/install): the shell installer only — the docs
			// list no npm or Homebrew channel. NOTE: `pip install
			// open-interpreter` is the UNRELATED Python project of the same
			// name and must never be offered here.
			Installs: []InstallHint{
				{OS: "linux", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://www.openinterpreter.com/install | sh"}, Display: "curl -fsSL https://www.openinterpreter.com/install | sh"},
				{OS: "darwin", Channel: "script", Argv: []string{"bash", "-lc", "curl -fsSL https://www.openinterpreter.com/install | sh"}, Display: "curl -fsSL https://www.openinterpreter.com/install | sh"},
				{OS: "windows", Channel: "script", Argv: []string{"powershell", "-Command", "irm https://www.openinterpreter.com/install.ps1 | iex"}, Display: "irm https://www.openinterpreter.com/install.ps1 | iex"},
			},
		},
	},
	// commandcode.ai's npm CLI (docs/plans/commandcode-adapter-plan-2026-07-29.md).
	// Phase-0 research only — no adapter package yet (Phase A wiring row).
	"command-code": {
		Tool:       "command-code",
		Vocabulary: Vocabulary{InTaxonomy: true},
		// COMMANDCODE_API_URL / COMMAND_CODE_API_KEY / COMMANDCODE_API_ENV
		// point at Command Code's OWN closed gateway (not a BYOK
		// Anthropic/OpenAI-shaped endpoint) — a knob exists but it isn't
		// routable_now-shaped, so after_bridge rather than native_exempt.
		Proxy:       nil,
		Routability: RouteStatusAfterBridge,
		// No hook mechanism grounded.
		Hook: HookSpec{Mechanism: HookNone},
		// ~/.commandcode/mcp.json reuses claude-code/cursor/droid's
		// {"mcpServers":{}} shape, grounded 2026-07-29 from the npm
		// package's cli.mjs getUserMcpConfigPath + bundled reference/mcp.md.
		// A writer already exists (internal/mcp/register.go's generic
		// registerJSONMCP, via the internal/mcp/locate row) so this is
		// Implemented, not a new writer.
		MCP:    &MCPTarget{Format: MCPServersJSON, PathHint: ".commandcode/mcp.json", Implemented: true},
		Native: NativeRails{},
		// Per-assistant-message usage envelope (inputTokens/outputTokens/
		// cacheReadTokens/cacheWriteTokens/costUsd); inputTokens almost
		// certainly GROSS (high confidence, not proxy-confirmed).
		TokenTier: TokenTier{Best: "jsonl", Gap: "no Tier-1 proxy path; costUsd trusted as-is for open-weight models with no observer pricing table"},
		Handoff: HandoffCapability{
			Transcript: TranscriptFull,
			Inject:     []InjectKind{InjectFile, InjectPrompt},
			// Seed contract grounded 2026-07-29 against `commandcode
			// --help` (v1.4.5), whose Options block states the two default-
			// command forms verbatim: `commandcode   Start interactive
			// session` and `commandcode "message"   Start with initial
			// message` — a bare positional on the default TUI command, so
			// the launcher appends the handover as the TRAILING positional.
			// `-p, --print [query]` is the headless one-shot ("Run in
			// non-interactive mode, output response and exit") and is a
			// declared conflict.
			Launch: &LaunchSpec{Subcommand: "command-code", Mode: LaunchSeeded},
		},
		// Attachable: `observer command-code --attach`.
		Attach: &AttachSpec{Subcommand: "command-code"},
		// Native resume: `commandcode --session <id>`. Grounded 2026-07-29
		// on `commandcode --help`: `--session <path|id>   Resume a session
		// by transcript path (.jsonl) or a unique session-id prefix`. This
		// is the REQUIRED-value spelling, so the plain two-token form is
		// unambiguous — deliberately preferred over `-r, --resume [name]`,
		// which is an OPTIONAL-value option (the shape that forced cursor's
		// joined `=` workaround) and resolves names as well as ids. The id
		// is our stored SessionID verbatim (sessionIDFromPath: the
		// `<uuid>.jsonl` basename under ~/.commandcode/projects/<enc-cwd>/).
		Resume: ResumeSpec{Kind: ResumeNative, Subcommand: "command-code", IDMechanism: "flag:--session"},
		Binary: &BinaryResolveSpec{
			// The npm package installs FOUR bin aliases for one binary
			// (`cmd`, `cmdc`, `command-code`, `commandcode` — registry-
			// confirmed). Only the two unambiguous spellings are listed:
			// `cmd`/`cmdc` are omitted deliberately (on Windows `cmd`
			// resolves to the system shell — a shadowing hazard, the same
			// exclusion internal/processobs already makes).
			//
			// Windows spellings are the npm SHIM forms, not `.exe`. The
			// package's bins are JavaScript entry points, so a global npm
			// install writes `<name>.cmd` (+ a `.ps1` and a bare POSIX-shell
			// shim for Git Bash/MSYS) into the npm prefix — it never
			// produces a `<name>.exe`. Both aliases get the same pair, which
			// also fixes an asymmetry: `command-code` previously claimed
			// only a `.cmd` while `commandcode` claimed three forms.
			// `.ps1` is left out on purpose: toolresolve stats a candidate
			// and then EXECS it, and a PowerShell script is not directly
			// executable. Resolution order is not load-bearing here —
			// toolresolve.orderNamesByPathExt re-sorts Windows candidates by
			// the operator's own PATHEXT.
			Names: BinaryNames{
				Unix:    []string{"commandcode", "command-code"},
				Windows: []string{"commandcode.cmd", "commandcode", "command-code.cmd", "command-code"},
			},
			// Official channel: the npm package `command-code` (registry-
			// confirmed name/description/bins). No script or brew channel
			// is documented.
			Installs: []InstallHint{
				{OS: "", Channel: "npm", Argv: []string{"npm", "install", "-g", "command-code"}, Display: "npm install -g command-code"},
			},
		},
	},
	// Meta's Muse Code CLI (docs/muse-adapter.md). Phase-0 grounded
	// 2026-08-06 against a live `Muse Code 0.1.0 (0.1.0-R708.1)` install on
	// Linux x86_64 (session log + config files + the shipped binary's own
	// string table). Every cell below is either observed in that install or
	// an explicit zero.
	"muse": {
		Tool: "muse",
		// 8 grounded native tool names in internal/tooltax (4 from the live
		// capture: bash / read_file / write_file / edit_file; 4 named
		// verbatim in the binary: web_search / web_fetch / read_skill /
		// search) plus the conventional defensive vocabulary.
		Vocabulary: Vocabulary{
			InTaxonomy: true,
			Note: "27 tools were active in the Phase-0 run but the log's " +
				"model_request_configured trace elides the names " +
				"(active_tools:[\"<string>\"], #len=27), so the tail of the " +
				"surface is defensive rather than observed",
		},
		// Model traffic goes to https://api.meta.ai/v1, whose base URL is
		// MINTED BY THE LOGIN FLOW ("mint response missing api_key or
		// base_url" / "unrecognized Model API base URL from login; using the
		// default") and authenticated with a Model API key. TBH_AUTH_BASE_URL
		// and TBH_MINT_BASE_URL steer only the auth + mint endpoints, NOT
		// model traffic, so neither is a route knob. settings.json does carry
		// an endpoint/transport block with `proxy` and mTLS fields plus a
		// `destination` discriminator ("destination=external is direct-only:
		// proxy/mTLS fields do not apply") — a real surface whose schema is
		// NOT grounded and which has never been driven live. That is exactly
		// probe_required: a documented-in-binary BYOK-ish path, unconfirmed on
		// a live install. Proxy stays nil until a live turn lands an api_turns
		// row (checklist §10.1f).
		Proxy:       nil,
		Routability: RouteStatusProbeRequired,
		// The binary carries a Claude-Code-derived hook vocabulary in
		// settings.json — user_prompt_submit / pre_tool_use /
		// permission_request / post_tool_use / pre_llm_call / post_llm_call /
		// pre_compact / post_compact / subagent_start / subagent_stop, with
		// matcher-group diagnostics ("hook matcher group must declare
		// `hooks`") and explicit Claude-compat rejections (D99 "Claude
		// `command` plus `args` argv execution is not implemented yet"). But
		// the on-disk matcher schema is not grounded and observer ships no
		// receiver, so the honest value is None: a mechanism named here would
		// make init claim a registration it cannot write.
		Hook: HookSpec{Mechanism: HookNone},
		// Muse is an MCP client (settings.json carries a server block with
		// transport/command/env/framing/url/headers + stdio |
		// streamable_http, and the binary reports "MCP tool name collision
		// for `…`"), but that shape is its OWN, not the shared
		// {"mcpServers":{…}} object any existing writer emits. No writer, no
		// grounded PathHint → nil rather than a fabricated target.
		MCP:    nil,
		Native: NativeRails{},
		// Tier-2 transcript capture only. model_completed.usage carries
		// input/output/cache_read/cache_write/cached/reasoning; both gross
		// fields are netted at emit time. No proxy lane ⇒ no Tier-1, and
		// observer ships no pricing entry for Muse models because Meta
		// publishes no per-token rate card for the subscription.
		TokenTier: TokenTier{
			Best: "transcript",
			Gap: "no Tier-1 proxy path (login-minted base URL); no pricing " +
				"entry for muse-* models, so cost rows resolve as unknown",
		},
		// The log re-reads in full (prompts, assistant text, tool bodies) so
		// a completed session is a usable handoff source. No Launch: the
		// interactive-seed contract has NOT been grounded — the checklist
		// requires reading the tool's own --help, and this session was
		// forbidden from invoking the CLI at all. InjectFile is the
		// universal floor and is all that is claimed.
		// Launcher GROUNDED 2026-08-06 against a live `muse --help` /
		// `muse resume --help` / `muse exec --help` read (this session was
		// permitted to invoke the CLI, unlike Phase 0). `Usage: muse
		// [OPTIONS] [PROMPT]` seeds the interactive session as a bare
		// trailing positional (LaunchSeeded); `cmd/observer/muse.go` is the
		// wired launcher.
		Handoff: HandoffCapability{
			Transcript: TranscriptFull,
			Inject:     []InjectKind{InjectFile, InjectPrompt},
			Launch:     &LaunchSpec{Subcommand: "muse"},
		},
		// Attach grounded 2026-08-06 (attach-all-launchers); PTY handoff
		// only — no prompt seeding, token capture path unchanged. muse is
		// launched non-proxied (Proxy stays nil above), so the attach spec
		// forwards no proxy env.
		Attach: &AttachSpec{Subcommand: "muse"},
		// Native resume GROUNDED off `muse resume --help`: `Usage: muse
		// resume` / `muse resume --last` / `muse resume <session-uuid>` —
		// a SUBCOMMAND whose positional argument is the session uuid. The
		// id is our stored SessionID verbatim (muse's own directory-name
		// uuid — internal/adapter/muse's sessionIDFromPath), so no
		// transform. Argv construction verified via
		// cmd/observer/resume_launcher_test.go, not a live resume (no paid
		// turn was run).
		Resume: ResumeSpec{Kind: ResumeNative, Subcommand: "muse", IDMechanism: "subcommand:resume"},
		// Binary: Names.Unix populated (required for a launchable tool);
		// Installs stays an EMPTY slice per the honesty rule — muse ships
		// from lookaside.facebook.com behind an auth.meta.com device
		// login, and no safe scriptable install command has been grounded
		// for that channel.
		Binary: &BinaryResolveSpec{Names: BinaryNames{Unix: []string{"muse"}}},
	},
	"prime-agent": {
		Tool: "prime-agent",
		// Prime Agent is deliberately a ONE-TOOL agent: "Available
		// built-in tools: `ipython`" (README) — the model drives a
		// persistent Python kernel for everything. `bash` and `edit` are
		// the two other built-in names docs/extensions.md says an
		// extension may override. All three carry tooltax rows; the rest
		// of the classifier is the conventional defensive vocabulary.
		Vocabulary: Vocabulary{
			InTaxonomy: true,
			Note: "the native surface is a single built-in tool (`ipython`); " +
				"skills are Python-backed and run INSIDE that kernel, and MCP " +
				"servers are reached from Python as `integration.<tool>(…)`, so " +
				"neither adds an LLM-visible tool name",
		},
		// ROUTABLE surface, route NOT DRIVEN YET. `~/.prime/agent/
		// models.json` accepts a custom provider with an explicit `baseUrl`
		// and `"api":"openai-completions"` (vendor docs/models.md: "Add
		// custom providers and models (Ollama, vLLM, LM Studio, proxies)
		// via ~/.prime/agent/models.json"), and `prime-agent --provider
		// <name>` selects it (confirmed in --help). That is structurally
		// the SAME RouteProviderJSON route cmd/observer/pi.go already
		// drives for pi — unsurprising, since prime-agent is a hard fork of
		// the same pi-mono upstream and inherited the file schema.
		//
		// `cmd/observer/prime-agent.go` (added 2026-08-06, modelled 1:1 on
		// pi.go) now writes that provider and execs
		// `prime-agent --provider observer`. Proxy still stays nil: no
		// live turn has landed an api_turns row through it yet, and
		// checklist §10.1f forbids flipping the PROXY cell before that —
		// this row is the grounded structural half, not the verified half.
		Proxy:       nil,
		Routability: RouteStatusRoutableNow,
		// Prime Agent's extension points are TypeScript EXTENSIONS
		// (-e/--extension, pi.registerTool / lifecycle callbacks), not a
		// settings.json hook-command vocabulary observer can register into.
		// Observer ships no receiver → None rather than a mechanism init
		// cannot write.
		Hook: HookSpec{Mechanism: HookNone},
		// Prime Agent IS an MCP client, but its servers are configured
		// through the TUI's /login → "MCP Connections" flow with
		// credentials in auth.json, and are called from inside the Python
		// kernel. That is not the shared {"mcpServers":{…}} object any
		// existing writer emits, and no grounded on-disk path was
		// established → nil rather than a fabricated target.
		MCP:    nil,
		Native: NativeRails{},
		// Tier-2 transcript only (no proxy lane driven yet). usage carries
		// input/output/cacheRead/cacheWrite + a provider-reported cost
		// breakdown; `input` is already NET (totalTokens == input + output
		// + cacheRead + cacheWrite holds exactly on every observed row), so
		// nothing is re-netted.
		TokenTier: TokenTier{
			Best: "transcript",
			Gap: "no reasoning-token count in the usage envelope on either " +
				"API lane (thinking TEXT is captured, the count is not " +
				"published); no pricing entries for the prime-inference / " +
				"openrouter model ids seen, so cost rows resolve as unknown " +
				"apart from the provider-reported usage.cost.total",
		},
		// The log re-reads in full — prompts, assistant text, thinking
		// blocks, tool bodies and shell output — so a completed session is
		// a usable handoff source.
		//
		// Launcher GROUNDED 2026-08-06 against a live `prime-agent --help`
		// read (this session was permitted to invoke the CLI). `Usage:
		// prime-agent [options] [@files...] [message...]` seeds the
		// interactive session as a trailing positional (LaunchSeeded);
		// `cmd/observer/prime-agent.go` is the wired launcher (also drives
		// the RouteProviderJSON route structurally — see Proxy above,
		// which stays nil pending a live-verified turn).
		Handoff: HandoffCapability{
			Transcript: TranscriptFull,
			Inject:     []InjectKind{InjectFile, InjectPrompt},
			Launch:     &LaunchSpec{Subcommand: "prime-agent"},
		},
		// Attach grounded 2026-08-06 (attach-all-launchers); PTY handoff
		// only — no prompt seeding, token capture path unchanged.
		Attach: &AttachSpec{Subcommand: "prime-agent"},
		// Native resume GROUNDED off `prime-agent --help`: `-r, --resume
		// <path|id>` is a REQUIRED-value flag (angle brackets) taking the
		// session UUID this adapter already keys on (the `<uuid>.jsonl`
		// filename stem), so no transform. Argv construction verified via
		// cmd/observer/resume_launcher_test.go, not a live resume (no
		// paid turn was run).
		Resume: ResumeSpec{Kind: ResumeNative, Subcommand: "prime-agent", IDMechanism: "flag:--resume"},
		// Binary: Names.Unix populated (required for a launchable tool);
		// npm package `prime-agent` is a grounded install channel
		// (`npm ls -g` confirmed the live install as `prime-agent@0.7.0`).
		Binary: &BinaryResolveSpec{
			Names: BinaryNames{Unix: []string{"prime-agent"}},
			Installs: []InstallHint{
				{OS: "", Channel: "npm", Argv: []string{"npm", "install", "-g", "prime-agent"}, Display: "npm install -g prime-agent"},
			},
		},
	},
}

// RegistryVersion is the version of the adapter capability registry's
// closed tool vocabulary. It is bumped whenever a tool is ADDED or REMOVED
// (a change to the set Tools() returns), so a consumer that ships the tool
// vocabulary on a wire — e.g. the G25 aggregate rail — can stamp which
// vocabulary it was built against (design §3.2, finding #24). It versions
// the tool NAME set only, not the content of every capability cell.
const RegistryVersion = 1

// Tools returns every registered tool name, sorted. It is the canonical,
// closed tool vocabulary (NOT config.EnabledAdapters, which is a
// user-configured watch list — finding #24). Consumers that need a stable
// allow-list of known tool names source it here.
func Tools() []string {
	out := make([]string, 0, len(registry))
	for tool := range registry {
		out = append(out, tool)
	}
	sort.Strings(out)
	return out
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

// ToolForLaunchSubcommand resolves an `observer <verb>` launcher verb (the
// Handoff.Launch.Subcommand a dashboard-handoff terminal_run row stores as its
// tool label) back to the canonical registry tool key (== sessions.tool).
// ok=false for an unknown or empty verb.
func ToolForLaunchSubcommand(sub string) (string, bool) {
	if sub == "" {
		return "", false
	}
	// Walk tool keys in sorted order so that IF a future row ever collides
	// on verb (registry_coverage_test.go pins that none do today), the
	// resolution is still deterministic — first match by tool-key order —
	// rather than depending on Go's randomized map iteration.
	for _, tool := range Tools() {
		if launch := registry[tool].Handoff.Launch; launch != nil && launch.Subcommand == sub {
			return tool, true
		}
	}
	return "", false
}
