// Tool registry — color + label + provider class for every AI tool
// the observer can capture. Mirrors `design/tokens.css` --tool-*
// custom properties and the brief's tool-identity-everywhere rule.

export type ToolKey =
  | "claude-code"
  | "codex"
  | "cursor"
  | "cline"
  | "copilot"
  | "copilot-cli"
  | "cowork"
  | "antigravity"
  | "opencode"
  | "openclaw"
  | "pi"
  | "gemini"
  | "gemini-cli";

export type ToolMeta = {
  key: ToolKey | string;
  label: string;
  // CSS variable reference — components apply this via inline style
  // so theme switches stay automatic.
  colorVar: string;
  provider: "anthropic" | "openai" | "google" | "github" | "agnostic";
};

const TOOLS: Record<string, ToolMeta> = {
  "claude-code": {
    key: "claude-code",
    label: "Claude Code",
    colorVar: "var(--tool-claude-code)",
    provider: "anthropic",
  },
  codex: {
    key: "codex",
    label: "Codex",
    colorVar: "var(--tool-codex)",
    provider: "openai",
  },
  cursor: {
    key: "cursor",
    label: "Cursor",
    colorVar: "var(--tool-cursor)",
    provider: "anthropic",
  },
  cline: {
    key: "cline",
    label: "Cline",
    colorVar: "var(--tool-cline)",
    provider: "anthropic",
  },
  copilot: {
    key: "copilot",
    label: "Copilot",
    colorVar: "var(--tool-copilot)",
    provider: "github",
  },
  "copilot-cli": {
    key: "copilot-cli",
    label: "Copilot CLI",
    colorVar: "var(--tool-copilot-cli)",
    provider: "github",
  },
  cowork: {
    key: "cowork",
    label: "Cowork",
    colorVar: "var(--tool-cowork)",
    provider: "anthropic",
  },
  antigravity: {
    key: "antigravity",
    label: "Antigravity",
    colorVar: "var(--tool-antigravity)",
    provider: "google",
  },
  opencode: {
    key: "opencode",
    label: "OpenCode",
    colorVar: "var(--tool-opencode)",
    provider: "agnostic",
  },
  openclaw: {
    key: "openclaw",
    label: "OpenClaw",
    colorVar: "var(--tool-openclaw)",
    provider: "anthropic",
  },
  pi: {
    key: "pi",
    label: "Pi",
    colorVar: "var(--tool-pi)",
    provider: "anthropic",
  },
  gemini: {
    key: "gemini",
    label: "Gemini",
    colorVar: "var(--tool-gemini)",
    provider: "google",
  },
  "gemini-cli": {
    key: "gemini-cli",
    label: "Gemini CLI",
    colorVar: "var(--tool-gemini)",
    provider: "google",
  },
};

const FALLBACK: ToolMeta = {
  key: "other",
  label: "Other",
  colorVar: "var(--tool-other)",
  provider: "agnostic",
};

export function toolMeta(key: string | null | undefined): ToolMeta {
  if (!key) return FALLBACK;
  return TOOLS[key] ?? { ...FALLBACK, key, label: key };
}
