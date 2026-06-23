// Action-type → category mapping. The dashboard's brief assigns
// distinct colors per category (not per type), so the 28 known
// action types collapse into 9 visual buckets keyed off the
// --act-* CSS variables.

export type ActionCategory =
  | "file"
  | "cmd"
  | "search"
  | "web"
  | "meta"
  | "fail"
  | "agent"
  | "mcp"
  | "user";

export type ActionMeta = {
  category: ActionCategory;
  label: string;
  colorVar: string;
};

const CATEGORY_COLOR: Record<ActionCategory, string> = {
  file: "var(--act-file)",
  cmd: "var(--act-cmd)",
  search: "var(--act-search)",
  web: "var(--act-web)",
  meta: "var(--act-meta)",
  fail: "var(--act-fail)",
  agent: "var(--act-agent)",
  mcp: "var(--act-mcp)",
  user: "var(--act-user)",
};

const ACTION_REGISTRY: Record<string, { category: ActionCategory; label: string }> = {
  // file ops
  read_file: { category: "file", label: "Read file" },
  write_file: { category: "file", label: "Write file" },
  edit_file: { category: "file", label: "Edit file" },
  // commands
  run_command: { category: "cmd", label: "Run command" },
  // search
  search_text: { category: "search", label: "Search text" },
  search_files: { category: "search", label: "Search files" },
  // web
  web_search: { category: "web", label: "Web search" },
  web_fetch: { category: "web", label: "Web fetch" },
  // sub-agents
  spawn_subagent: { category: "agent", label: "Spawn subagent" },
  subagent_start: { category: "agent", label: "Subagent start" },
  subagent_stop: { category: "agent", label: "Subagent stop" },
  // mcp
  mcp_call: { category: "mcp", label: "MCP call" },
  // user
  user_prompt: { category: "user", label: "User prompt" },
  user_prompt_expansion: { category: "user", label: "Prompt expansion" },
  ask_user: { category: "user", label: "Ask user" },
  // prompt scaffolding
  system_prompt: { category: "meta", label: "System prompt" },
  // prompt_context: a non-content prompt-budget component (tool defs,
  // rules, skills, subagents) whose token count is recorded but whose
  // content the source tool doesn't persist. Distinct from system_prompt
  // so the table doesn't mislabel a "Rules" row as "System prompt".
  prompt_context: { category: "meta", label: "Prompt context" },
  // meta / session
  task_complete: { category: "meta", label: "Task complete" },
  permission_request: { category: "meta", label: "Permission request" },
  permission_denied: { category: "fail", label: "Permission denied" },
  post_tool_batch: { category: "meta", label: "Post-tool batch" },
  setup: { category: "meta", label: "Setup" },
  instructions_loaded: { category: "meta", label: "Instructions loaded" },
  config_change: { category: "meta", label: "Config change" },
  session_start: { category: "meta", label: "Session start" },
  session_end: { category: "meta", label: "Session end" },
  notification: { category: "meta", label: "Notification" },
  cwd_change: { category: "meta", label: "CWD change" },
  todo_update: { category: "meta", label: "Todo update" },
  context_compacted: { category: "meta", label: "Context compacted" },
  rate_limit: { category: "meta", label: "Rate limit" },
  turn_aborted: { category: "meta", label: "Turn aborted" },
  // failures
  tool_failure: { category: "fail", label: "Tool failure" },
  api_error: { category: "fail", label: "API error" },
};

const FALLBACK: ActionMeta = {
  category: "meta",
  label: "Unknown",
  colorVar: CATEGORY_COLOR.meta,
};

export function actionMeta(type: string | null | undefined): ActionMeta {
  if (!type) return FALLBACK;
  const reg = ACTION_REGISTRY[type];
  if (reg) {
    return {
      category: reg.category,
      label: reg.label,
      colorVar: CATEGORY_COLOR[reg.category],
    };
  }
  // Unknown action_type — humanize the key and fall back to "meta" gray.
  return {
    category: "meta",
    label: type.replace(/_/g, " ").replace(/\b\w/g, (c) => c.toUpperCase()),
    colorVar: CATEGORY_COLOR.meta,
  };
}

// Known action-type keys for filter dropdowns. Sorted alphabetically.
export const KNOWN_ACTION_TYPES: string[] = Object.keys(ACTION_REGISTRY).sort();

export const KNOWN_EFFORT_LEVELS = [
  "minimal",
  "low",
  "medium",
  "high",
  "xhigh",
  "max",
] as const;

export const KNOWN_PERMISSION_MODES = [
  "default",
  "plan",
  "acceptEdits",
  "auto",
  "dontAsk",
  "bypassPermissions",
] as const;
