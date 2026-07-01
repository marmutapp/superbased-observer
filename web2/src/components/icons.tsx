// Per-tool distinctive glyph marks for the org dashboard. A trimmed MIRROR of
// web/src/components/icons.tsx — only ToolGlyph (the per-tool mark used by
// ToolBadge / ToolDot) is needed here; the org sidebar uses lucide-react for
// its nav icons. Keep the glyph paths in sync with web/ so a Claude Code badge
// looks identical across both dashboards.

// ToolGlyph — per-tool distinctive glyph mark. Each path is drawn inside the
// 24×24 design viewBox at stroke 1.8 — caller scales via `size` and tints via
// currentColor.
export function ToolGlyph({
  tool,
  size = 12,
  className,
}: {
  tool: string;
  size?: number;
  className?: string;
}) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      stroke="currentColor"
      fill="none"
      className={className}
      aria-hidden
    >
      {toolGlyphPath(tool)}
    </svg>
  );
}

function toolGlyphPath(tool: string) {
  switch (tool) {
    case "claude-code":
      // Nested arcs framing a center dot — suggests reasoning.
      return (
        <>
          <path
            d="M9 5 C5 8, 5 16, 9 19"
            strokeWidth="1.8"
            strokeLinecap="round"
          />
          <path
            d="M15 5 C19 8, 19 16, 15 19"
            strokeWidth="1.8"
            strokeLinecap="round"
          />
          <circle cx="12" cy="12" r="1.6" fill="currentColor" stroke="none" />
        </>
      );
    case "codex":
      // Terminal prompt "> _".
      return (
        <>
          <path
            d="M5 9 L8 12 L5 15"
            strokeWidth="1.8"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
          <line
            x1="11"
            y1="16"
            x2="19"
            y2="16"
            strokeWidth="1.8"
            strokeLinecap="round"
          />
        </>
      );
    case "cursor":
      // Cursor arrow.
      return (
        <path
          d="M6 4 L6 18 L10 14 L13 20 L15 19 L12 13 L18 13 Z"
          fill="currentColor"
          stroke="none"
        />
      );
    case "cline":
      // Chevron + bar (cli).
      return (
        <>
          <path
            d="M5 8 L9 12 L5 16"
            strokeWidth="1.8"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
          <line
            x1="12"
            y1="16"
            x2="19"
            y2="16"
            strokeWidth="1.8"
            strokeLinecap="round"
          />
          <line
            x1="14"
            y1="8"
            x2="19"
            y2="8"
            strokeWidth="1.8"
            strokeLinecap="round"
            opacity="0.5"
          />
        </>
      );
    case "copilot":
      // Orbit — two dots inside an arc (pair-programming).
      return (
        <>
          <circle cx="12" cy="12" r="6" strokeWidth="1.6" />
          <circle cx="8.5" cy="9.5" r="1.6" fill="currentColor" stroke="none" />
          <circle
            cx="15.5"
            cy="14.5"
            r="1.6"
            fill="currentColor"
            stroke="none"
          />
        </>
      );
    case "cowork":
      // Two linked circles.
      return (
        <>
          <circle cx="9" cy="12" r="4" strokeWidth="1.8" />
          <circle cx="15" cy="12" r="4" strokeWidth="1.8" />
        </>
      );
    case "antigravity":
      // Upward triangle floating above a baseline.
      return (
        <>
          <path d="M12 5 L18 14 L6 14 Z" fill="currentColor" stroke="none" />
          <line
            x1="5"
            y1="18"
            x2="19"
            y2="18"
            strokeWidth="1.6"
            strokeLinecap="round"
            opacity="0.5"
          />
        </>
      );
    case "opencode":
      // Open square brackets.
      return (
        <>
          <path
            d="M9 6 L5 6 L5 18 L9 18"
            strokeWidth="1.8"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
          <path
            d="M15 6 L19 6 L19 18 L15 18"
            strokeWidth="1.8"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
        </>
      );
    case "openclaw":
      // Three diagonal strokes (claw marks).
      return (
        <>
          <path d="M7 6 L14 18" strokeWidth="1.8" strokeLinecap="round" />
          <path d="M11 5 L17 16" strokeWidth="1.8" strokeLinecap="round" />
          <path
            d="M15 4 L19 13"
            strokeWidth="1.8"
            strokeLinecap="round"
            opacity="0.65"
          />
        </>
      );
    case "pi":
      // Greek letter pi.
      return (
        <>
          <line
            x1="6"
            y1="9"
            x2="18"
            y2="9"
            strokeWidth="1.8"
            strokeLinecap="round"
          />
          <line
            x1="9"
            y1="9"
            x2="9"
            y2="18"
            strokeWidth="1.8"
            strokeLinecap="round"
          />
          <path
            d="M15 9 L15 16 Q15 18, 17 18"
            strokeWidth="1.8"
            strokeLinecap="round"
          />
        </>
      );
    case "gemini":
    case "gemini-cli":
      // Four-point sparkle.
      return (
        <path
          d="M12 5 L13.5 10.5 L19 12 L13.5 13.5 L12 19 L10.5 13.5 L5 12 L10.5 10.5 Z"
          fill="currentColor"
          stroke="none"
        />
      );
    default:
      // Generic dot.
      return (
        <circle cx="12" cy="12" r="3.5" fill="currentColor" stroke="none" />
      );
  }
}
