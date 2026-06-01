import type { Config } from "tailwindcss";

// Tailwind theme is wired entirely through CSS custom properties
// defined in src/styles/tokens.css. The benefit: theme switches
// (dark/light) flip the variables; Tailwind utilities don't need
// dark: variants. Component code stays close to the design tokens.
const config: Config = {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        bg: {
          0: "var(--bg-0)",
          1: "var(--bg-1)",
          2: "var(--bg-2)",
          3: "var(--bg-3)",
          4: "var(--bg-4)",
          5: "var(--bg-5)",
        },
        line: {
          1: "var(--line-1)",
          2: "var(--line-2)",
          3: "var(--line-3)",
        },
        fg: {
          0: "var(--fg-0)",
          1: "var(--fg-1)",
          2: "var(--fg-2)",
          3: "var(--fg-3)",
          4: "var(--fg-4)",
        },
        accent: {
          DEFAULT: "var(--accent)",
          strong: "var(--accent-strong)",
          soft: "var(--accent-soft)",
          ring: "var(--accent-ring)",
          on: "var(--on-accent)",
        },
        success: { DEFAULT: "var(--success)", soft: "var(--success-soft)" },
        warn: { DEFAULT: "var(--warn)", soft: "var(--warn-soft)" },
        danger: { DEFAULT: "var(--danger)", soft: "var(--danger-soft)" },
        info: { DEFAULT: "var(--info)", soft: "var(--info-soft)" },
        tok: {
          net: "var(--tok-net)",
          read: "var(--tok-read)",
          write: "var(--tok-write)",
          out: "var(--tok-out)",
        },
        tool: {
          "claude-code": "var(--tool-claude-code)",
          codex: "var(--tool-codex)",
          cursor: "var(--tool-cursor)",
          cline: "var(--tool-cline)",
          copilot: "var(--tool-copilot)",
          cowork: "var(--tool-cowork)",
          antigravity: "var(--tool-antigravity)",
          opencode: "var(--tool-opencode)",
          openclaw: "var(--tool-openclaw)",
          pi: "var(--tool-pi)",
          gemini: "var(--tool-gemini)",
          other: "var(--tool-other)",
        },
      },
      gridTemplateColumns: {
        // 24-col grid — for the HourHeatmap.
        24: "repeat(24, minmax(0, 1fr))",
      },
      fontFamily: {
        sans: ["Inter", "system-ui", "-apple-system", "Segoe UI", "sans-serif"],
        mono: ['"JetBrains Mono"', '"SF Mono"', "Menlo", "Consolas", "monospace"],
      },
      borderRadius: {
        1: "var(--r-1)",
        2: "var(--r-2)",
        3: "var(--r-3)",
        4: "var(--r-4)",
        pill: "var(--r-pill)",
      },
      boxShadow: {
        1: "var(--shadow-1)",
        2: "var(--shadow-2)",
        3: "var(--shadow-3)",
        drawer: "var(--shadow-drawer)",
      },
      transitionTimingFunction: {
        smooth: "var(--ease)",
      },
    },
  },
  plugins: [],
};

export default config;
