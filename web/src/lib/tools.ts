// Tool registry — color + label + provider class for every AI tool
// the observer can capture. Mirrors `design/tokens.css` --tool-*
// custom properties and the brief's tool-identity-everywhere rule.

export type ToolKey =
  | "claude-code"
  | "codex"
  | "cursor"
  | "cline"
  | "cline-cli"
  | "copilot"
  | "copilot-cli"
  | "cowork"
  | "antigravity"
  | "antigravity-cli"
  | "opencode"
  | "openclaw"
  | "pi"
  | "gemini"
  | "gemini-cli"
  | "hermes"
  | "kilo-code"
  | "kilo-code-cli"
  | "qwen-code"
  | "kiro-cli"
  | "crush"
  | "kimi-code"
  | "grok"
  | "devin"
  | "aider"
  | "qoder"
  | "goose";

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
  "cline-cli": {
    key: "cline-cli",
    label: "Cline CLI",
    colorVar: "var(--tool-cline-cli)",
    provider: "anthropic",
  },
  "antigravity-cli": {
    key: "antigravity-cli",
    label: "Antigravity CLI",
    // Same vendor + product family — shares antigravity's identity
    // color (the gemini/gemini-cli precedent).
    colorVar: "var(--tool-antigravity)",
    provider: "google",
  },
  hermes: {
    key: "hermes",
    label: "Hermes",
    colorVar: "var(--tool-hermes)",
    provider: "agnostic",
  },
  "kilo-code": {
    key: "kilo-code",
    label: "Kilo Code",
    colorVar: "var(--tool-kilo-code)",
    provider: "agnostic",
  },
  "kilo-code-cli": {
    key: "kilo-code-cli",
    label: "Kilo CLI",
    // One product family, one identity color (gemini-cli precedent).
    colorVar: "var(--tool-kilo-code)",
    provider: "agnostic",
  },
  // 2026-07 adapter wave. Per-turn providers are operator-configured
  // (openai-compat multi-backend observed live), so provider stays
  // "agnostic" where no single backend is true.
  "qwen-code": {
    key: "qwen-code",
    label: "Qwen Code",
    colorVar: "var(--tool-qwen-code)",
    provider: "agnostic",
  },
  "kiro-cli": {
    key: "kiro-cli",
    label: "Kiro CLI",
    colorVar: "var(--tool-kiro-cli)",
    provider: "agnostic",
  },
  crush: {
    key: "crush",
    label: "Crush",
    colorVar: "var(--tool-crush)",
    provider: "agnostic",
  },
  "kimi-code": {
    key: "kimi-code",
    label: "Kimi Code",
    colorVar: "var(--tool-kimi-code)",
    provider: "agnostic",
  },
  grok: {
    key: "grok",
    label: "Grok",
    // Deliberately neutral — xAI's brand is black/white; identity is
    // never color-alone (labels/legends everywhere).
    colorVar: "var(--tool-grok)",
    provider: "agnostic",
  },
  devin: {
    key: "devin",
    label: "Devin",
    colorVar: "var(--tool-devin)",
    provider: "agnostic",
  },
  aider: {
    key: "aider",
    label: "Aider",
    colorVar: "var(--tool-aider)",
    provider: "agnostic",
  },
  qoder: {
    key: "qoder",
    label: "Qoder",
    colorVar: "var(--tool-qoder)",
    provider: "agnostic",
  },
  goose: {
    key: "goose",
    label: "goose",
    colorVar: "var(--tool-goose)",
    provider: "agnostic",
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
