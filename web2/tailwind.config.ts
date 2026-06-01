import type { Config } from "tailwindcss";

// A small, self-contained token set (the org dashboard mirrors the
// agent dashboard's look without sharing its component tree).
export default {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        bg: "#0b0d10",
        surface: "#14171c",
        surface2: "#1b1f26",
        line: "#262b33",
        fg: "#e6e9ef",
        muted: "#9aa3b2",
        faint: "#6b7280",
        accent: "#3b82f6",
        good: "#22c55e",
        warn: "#f59e0b",
        bad: "#ef4444",
      },
      fontFamily: {
        sans: ["Inter", "system-ui", "sans-serif"],
        mono: ["JetBrains Mono", "ui-monospace", "monospace"],
      },
    },
  },
  plugins: [],
} satisfies Config;
