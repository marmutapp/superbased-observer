// Settings section specs — mirrors the legacy SECTION_FIELDS map
// (tmp/legacy/index.html:3993-4050) so the React app can render the
// same structured form UI per section instead of a raw TOML dump.
//
// Fields are RENDERED disabled until `PUT /api/config/section/<id>`
// lands on the backend. The point of this module is to bring back
// the legacy's per-field labels + help text + option lists, which
// were lost when the static TomlView replaced the structured form.

export type FieldKind = "text" | "select" | "bool" | "int" | "list";

export type FieldDef = {
  id: string;
  label: string;
  kind: FieldKind;
  help?: string;
  // For `select`: the allowed values.
  options?: string[];
  // For `int`: bounds + step.
  min?: number;
  max?: number;
  step?: number;
};

export type SectionSpec = {
  id: string;
  // Path into the loaded ConfigResponse.config object that holds this
  // section's live values. E.g. ["Observer", "Watch"] for the watcher
  // section. Resolved at render time. May be empty when the spec is
  // purely a parent for grouped sub-paths (see `groups` below).
  path: string[];
  description: string;
  // Flat fields. Pages with sub-objects (e.g. compression's 4 layers)
  // use `groups` instead — the renderer prefers `groups` when present.
  fields?: FieldDef[];
  // Optional nested groups — for sections whose live data lives across
  // multiple sub-objects rather than one flat record (Compression's
  // CodeGraph / Shell / Indexing / Conversation). Each group renders
  // as its own card under the section header.
  groups?: SectionGroup[];
};

export type SectionGroup = {
  id: string;
  label: string;
  // Path into the loaded config relative to the section root (or
  // absolute when the section's path is empty). E.g. ["Compression",
  // "Shell"] when iterating from the config root.
  path: string[];
  description?: string;
  fields: FieldDef[];
};

// Each spec matches a single ConfigViewerSection route in
// Settings.tsx::sectionData(). The path[] resolves to the same object
// today's sectionData() returns.

export const SECTION_SPECS: Record<string, SectionSpec> = {
  observer: {
    id: "observer",
    path: ["Observer"],
    description:
      "Top-level observer settings — database path, log verbosity. Sub-sections (watcher / freshness / retention / hooks / antigravity) live as their own section nav entries.",
    fields: [
      {
        id: "DBPath",
        label: "DB path",
        kind: "text",
        help: "SQLite database path. Created on first run if absent.",
      },
      {
        id: "LogLevel",
        label: "Log level",
        kind: "select",
        options: ["trace", "debug", "info", "warn", "error"],
        help: "Daemon log verbosity. `info` is the recommended baseline.",
      },
    ],
  },
  watcher: {
    id: "watcher",
    path: ["Observer", "Watch"],
    description:
      "Filesystem watcher — JSONL/log file watcher. Defines what observer tails for new session activity.",
    fields: [
      {
        id: "PollIntervalSeconds",
        label: "Poll interval",
        kind: "int",
        min: 1,
        max: 60,
        help: "How often the watcher re-scans for new files (seconds).",
      },
      {
        id: "MaxFileSizeMB",
        label: "Max file size (MB)",
        kind: "int",
        min: 1,
        help: "Files larger than this are skipped to avoid OOM on rogue logs.",
      },
      {
        id: "EnabledAdapters",
        label: "Enabled adapters",
        kind: "list",
        help: "Comma-separated adapter names — claude-code, codex, cursor, cline, copilot, cowork, antigravity, opencode, openclaw, pi, gemini.",
      },
    ],
  },
  freshness: {
    id: "freshness",
    path: ["Observer", "Freshness"],
    description:
      "File-content hash engine. Used by the Discovery tab's stale-read detection.",
    fields: [
      {
        id: "EnableContentHashing",
        label: "Content hashing",
        kind: "bool",
        help: "Hash file contents to detect changes between session reads.",
      },
      {
        id: "MaxHashFileSizeMB",
        label: "Max hash file size (MB)",
        kind: "int",
        min: 1,
        help: "Files larger than this fall back to stat-only freshness.",
      },
      {
        id: "FastPathStatOnly",
        label: "Fast-path stat only",
        kind: "bool",
        help: "Skip hashing entirely; use mtime + size for change detection.",
      },
      {
        id: "IgnorePatterns",
        label: "Ignore patterns",
        kind: "list",
        help: "Globs to skip during freshness scans — node_modules, .git, etc.",
      },
    ],
  },
  retention: {
    id: "retention",
    path: ["Observer", "Retention"],
    description:
      "How long observer keeps each table's data. Pruning runs on a schedule + opportunistically on startup.",
    fields: [
      {
        id: "MaxAgeDays",
        label: "Max age (days)",
        kind: "int",
        min: 1,
        help: "Actions / token rows older than this are vacuumed.",
      },
      {
        id: "MaxDBSizeMB",
        label: "Max DB size (MB)",
        kind: "int",
        min: 1,
        help: "Soft cap on the SQLite database — pruning triggers above.",
      },
      {
        id: "PruneOnStartup",
        label: "Prune on startup",
        kind: "bool",
        help: "Run the prune sweep when the daemon starts.",
      },
      {
        id: "ObserverLogMaxAgeDays",
        label: "Observer log max age (days)",
        kind: "int",
        min: 1,
        help: "Lifetime of the daemon's own log file.",
      },
    ],
  },
  hooks: {
    id: "hooks",
    path: ["Observer", "Hooks"],
    description:
      "Per-tool hook configuration. The daemon installs lightweight stop-hooks into each AI client to capture turn boundaries.",
    fields: [
      {
        id: "TimeoutMS",
        label: "Hook timeout (ms)",
        kind: "int",
        min: 50,
        max: 30_000,
        help: "How long the hook is allowed to run before the client gives up on it. Keep low — the hook is on the user's latency budget.",
      },
    ],
  },
  antigravity: {
    id: "antigravity",
    path: ["Observer", "Antigravity"],
    description:
      "Antigravity (Google) adapter config. The bridge mode controls how the Windows-side .pb files are reached when observer runs under WSL2.",
    fields: [
      {
        id: "NetworkRecovery",
        label: "Network recovery",
        kind: "select",
        options: ["off", "local"],
        help: "off = no recovery, fails on missing files. local = use a Windows-side bridge helper for cross-mount filesystem access. See the helper card below for setup.",
      },
    ],
  },
  compression: {
    id: "compression",
    // Saving POSTs the section root to PUT /api/config/section/compression
    // which decodes the body as config.CompressionConfig. The body keys
    // therefore have to be {CodeGraph, Shell, Indexing, Conversation}
    // at the top level — i.e. spec.path must root at ["Compression"]
    // and the groups' paths must be RELATIVE to that root. An empty
    // spec.path here (the prior shape) made draft = the whole root
    // config and every Save zeroed cfg.Compression server-side.
    path: ["Compression"],
    description:
      "Three live compression layers — shell output filters, FTS5 tool-output indexing, and conversation-level compression (with stash / rolling-summarisation / compaction sub-toggles). Each layer toggles independently; defaults err on the side of safety. (Code-graph integration is configured under Settings → Intelligence per the v1.4.20 audit — the compression.code_graph fields had no Go consumers and were removed.)",
    groups: [
      {
        id: "shell",
        label: "Shell output",
        path: ["Shell"],
        description:
          "RTK-style filters that strip noise (ANSI escapes, progress bars, repetitive output) from captured shell command output.",
        fields: [
          {
            id: "Enabled",
            label: "Enabled",
            kind: "bool",
            help: "Master toggle for shell output filtering.",
          },
          {
            id: "ExcludeCommands",
            label: "Exclude commands",
            kind: "list",
            help: "Comma-separated list of commands whose output is never filtered.",
          },
        ],
      },
      {
        id: "indexing",
        label: "Tool output indexing",
        path: ["Indexing"],
        description:
          "Index large tool-call outputs in an FTS5 table so the MCP search_past_outputs can retrieve them on demand without bloating the conversation.",
        fields: [
          {
            id: "Enabled",
            label: "Enabled",
            kind: "bool",
            help: "Master toggle for FTS5 indexing of tool outputs.",
          },
          {
            id: "MaxExcerptBytes",
            label: "Max excerpt bytes",
            kind: "int",
            min: 256,
            help: "Truncate excerpts beyond this byte count.",
          },
          {
            id: "Embeddings",
            label: "Embeddings (experimental — not yet wired)",
            kind: "bool",
            help: "Reserved for semantic embeddings on indexed excerpts. No Go consumer reads this flag today; toggling has no effect. Kept for forward-compat with the indexing pipeline.",
          },
        ],
      },
      {
        id: "conversation",
        label: "Conversation",
        path: ["Conversation"],
        description:
          "Per-turn conversation-level compression. The mode controls how aggressively prior turns are pruned vs. cached. cache_aware preserves Anthropic's prefix cache for Pro/Max users.",
        fields: [
          {
            id: "Enabled",
            label: "Enabled",
            kind: "bool",
            help: "Master toggle for conversation compression.",
          },
          {
            id: "Mode",
            label: "Mode",
            kind: "select",
            options: ["token", "cache", "cache_aware"],
            help: "token = legacy drop ranking; cache = restrict drops to tail half + inject cache_control; cache_aware = no drops (preserves Anthropic prefix cache).",
          },
          {
            id: "TargetRatio",
            label: "Target ratio",
            kind: "text",
            help: "Compression target as a fraction (0..1). e.g. 0.5 = compress to half input tokens.",
          },
          {
            id: "PreserveLastN",
            label: "Preserve last N",
            kind: "int",
            min: 0,
            help: "Always keep this many of the most-recent messages uncompressed.",
          },
          {
            id: "CompressTypes",
            label: "Compress types",
            kind: "list",
            help: "Role-types eligible for compression. Empty = all.",
          },
        ],
      },
      {
        id: "stash",
        label: "Stash (CCR)",
        path: ["Conversation", "Stash"],
        description:
          "Compressed Content Retrieval — write large tool_result bodies to a content-addressed on-disk stash and replace inline with a SHA marker. The model retrieves originals via the `retrieve_stashed` MCP tool.",
        fields: [
          {
            id: "Enabled",
            label: "Enabled",
            kind: "bool",
            help: "Master toggle for the stash layer.",
          },
          {
            id: "Dir",
            label: "Stash dir",
            kind: "text",
            help: "Where stashed bodies live on disk. Default: ~/.observer/stash.",
          },
          {
            id: "ThresholdBytes",
            label: "Threshold bytes",
            kind: "int",
            min: 512,
            help: "Tool_result bodies above this size get stashed.",
          },
          {
            id: "MaxTotalMB",
            label: "Max total (MB)",
            kind: "int",
            min: 1,
            help: "Soft cap on total stash dir size — older entries evict above this.",
          },
        ],
      },
      {
        id: "rolling",
        label: "Rolling summarisation",
        path: ["Conversation", "Rolling"],
        description:
          "When a session crosses the threshold the proxy summarises older messages inline. Independent summary models per Anthropic + OpenAI traffic.",
        fields: [
          {
            id: "Enabled",
            label: "Enabled",
            kind: "bool",
            help: "Master toggle for rolling summarisation.",
          },
          {
            id: "ThresholdTokens",
            label: "Threshold tokens",
            kind: "int",
            min: 1024,
            help: "Sessions crossing this many tokens trigger rolling summarisation.",
          },
          {
            id: "SummaryModel",
            label: "Anthropic summary model",
            kind: "text",
            help: "Default: claude-haiku-4-5.",
          },
          {
            id: "OpenAISummaryModel",
            label: "OpenAI summary model",
            kind: "text",
            help: "Default: gpt-5-nano.",
          },
        ],
      },
      {
        id: "compaction",
        label: "Compaction recovery",
        path: ["Conversation", "Compaction"],
        description:
          "After a compaction event the proxy can inject a synthetic system block carrying recovery context (recent reads, edits, failures, learned rules) so the model can re-orient.",
        fields: [
          {
            id: "InjectPostCompact",
            label: "Inject post-compact context",
            kind: "bool",
            help: "Surface recovery context after compaction events.",
          },
        ],
      },
    ],
  },
  proxy: {
    id: "proxy",
    path: ["Proxy"],
    description:
      "API proxy listener. The proxy intercepts Anthropic / OpenAI / Google API calls from your AI client, captures the exact request/response bodies, then forwards upstream — giving the dashboard ground-truth token + cost accounting.",
    fields: [
      {
        id: "Enabled",
        label: "Proxy enabled",
        kind: "bool",
        help: "If false, no HTTP listener binds. Hook-only capture continues to work.",
      },
      {
        id: "Port",
        label: "Listen port",
        kind: "int",
        min: 1024,
        max: 65_535,
        help: "TCP port the proxy listens on. Default 8820.",
      },
      {
        id: "AnthropicUpstream",
        label: "Anthropic upstream",
        kind: "text",
        help: "Base URL for Anthropic requests. Default https://api.anthropic.com.",
      },
      {
        id: "OpenAIUpstream",
        label: "OpenAI upstream",
        kind: "text",
        help: "Base URL for OpenAI-compatible requests. Default https://api.openai.com.",
      },
      {
        id: "ChatGPTUpstream",
        label: "ChatGPT upstream",
        kind: "text",
        help: "Base URL for ChatGPT-plan Codex backend requests. Default https://chatgpt.com.",
      },
      {
        id: "ForceChatGPTHTTP",
        label: "Force ChatGPT HTTP",
        kind: "bool",
        help: "Reject ChatGPT Codex websocket upgrades so the client falls back to HTTPS POST requests (which the proxy can compress).",
      },
    ],
  },
};
