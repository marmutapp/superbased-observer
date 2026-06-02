/* ============================================================
   Provider icons — distinctive abstract glyphs for each tool.
   These are NOT recreations of official brand logos — they're
   original abstract marks chosen to give visual identity in
   the dashboard. Each glyph is paired with its tool brand color.
   ============================================================ */

// Each glyph is SVG path drawn inside a 24×24 viewBox at stroke 1.8.
// Rendered inside a rounded-square frame tinted with the tool's color.
const PROVIDER_GLYPHS = {
  // claude-code: nested arcs (parens-like, suggests reasoning)
  "claude-code": <g>
    <path d="M9 5 C5 8, 5 16, 9 19" fill="none" strokeWidth="1.8" strokeLinecap="round"/>
    <path d="M15 5 C19 8, 19 16, 15 19" fill="none" strokeWidth="1.8" strokeLinecap="round"/>
    <circle cx="12" cy="12" r="1.6" fill="currentColor"/>
  </g>,
  // codex: terminal prompt
  "codex": <g>
    <path d="M5 9 L8 12 L5 15" fill="none" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round"/>
    <line x1="11" y1="16" x2="19" y2="16" strokeWidth="1.8" strokeLinecap="round"/>
  </g>,
  // cursor: cursor arrow
  "cursor": <g>
    <path d="M6 4 L6 18 L10 14 L13 20 L15 19 L12 13 L18 13 Z" fill="currentColor" stroke="none"/>
  </g>,
  // cline: chevron stack with bar (cli)
  "cline": <g>
    <path d="M5 8 L9 12 L5 16" fill="none" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round"/>
    <line x1="12" y1="16" x2="19" y2="16" strokeWidth="1.8" strokeLinecap="round"/>
    <line x1="14" y1="8" x2="19" y2="8" strokeWidth="1.8" strokeLinecap="round" opacity="0.5"/>
  </g>,
  // copilot: orbit (two dots + arc — suggests pair-programming)
  "copilot": <g>
    <circle cx="12" cy="12" r="6" fill="none" strokeWidth="1.6"/>
    <circle cx="8.5" cy="9.5" r="1.6" fill="currentColor"/>
    <circle cx="15.5" cy="14.5" r="1.6" fill="currentColor"/>
  </g>,
  // cowork: two linked circles (collaborative)
  "cowork": <g>
    <circle cx="9" cy="12" r="4" fill="none" strokeWidth="1.8"/>
    <circle cx="15" cy="12" r="4" fill="none" strokeWidth="1.8"/>
  </g>,
  // antigravity: upward triangle floating above a baseline
  "antigravity": <g>
    <path d="M12 5 L18 14 L6 14 Z" fill="currentColor" stroke="none"/>
    <line x1="5" y1="18" x2="19" y2="18" strokeWidth="1.6" strokeLinecap="round" opacity="0.5"/>
  </g>,
  // opencode: open square brackets
  "opencode": <g>
    <path d="M9 6 L5 6 L5 18 L9 18" fill="none" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round"/>
    <path d="M15 6 L19 6 L19 18 L15 18" fill="none" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round"/>
  </g>,
  // openclaw: three diagonal strokes (claw marks)
  "openclaw": <g>
    <path d="M7 6 L14 18" fill="none" strokeWidth="1.8" strokeLinecap="round"/>
    <path d="M11 5 L17 16" fill="none" strokeWidth="1.8" strokeLinecap="round"/>
    <path d="M15 4 L19 13" fill="none" strokeWidth="1.8" strokeLinecap="round" opacity="0.65"/>
  </g>,
  // pi: greek letter pi
  "pi": <g>
    <line x1="6" y1="9" x2="18" y2="9" strokeWidth="1.8" strokeLinecap="round"/>
    <line x1="9" y1="9" x2="9" y2="18" strokeWidth="1.8" strokeLinecap="round"/>
    <path d="M15 9 L15 16 Q15 18, 17 18" fill="none" strokeWidth="1.8" strokeLinecap="round"/>
  </g>,
  // gemini: 4-point sparkle
  "gemini": <g>
    <path d="M12 5 L13.5 10.5 L19 12 L13.5 13.5 L12 19 L10.5 13.5 L5 12 L10.5 10.5 Z" fill="currentColor" stroke="none"/>
  </g>,
  // generic / other
  "other": <g>
    <circle cx="12" cy="12" r="3.5" fill="currentColor"/>
  </g>,
};

function ProviderIcon({ tool, size = 18, radius, style = {} }) {
  const meta = window.OBS.TOOLS[tool];
  const color = meta ? meta.color : "var(--tool-other)";
  const glyph = PROVIDER_GLYPHS[tool] || PROVIDER_GLYPHS.other;
  const r = radius != null ? radius : Math.max(3, Math.round(size * 0.22));
  return (
    <span
      style={{
        display: "inline-grid", placeItems: "center",
        width: size, height: size,
        borderRadius: r,
        background: `color-mix(in oklab, ${color} 18%, transparent)`,
        border: `1px solid color-mix(in oklab, ${color} 35%, transparent)`,
        color: color,
        flexShrink: 0,
        ...style,
      }}
      title={meta ? meta.label : tool}
    >
      <svg width={Math.round(size * 0.75)} height={Math.round(size * 0.75)} viewBox="0 0 24 24" stroke="currentColor" fill="none">
        {glyph}
      </svg>
    </span>
  );
}

window.ProviderIcon = ProviderIcon;
window.PROVIDER_GLYPHS = PROVIDER_GLYPHS;
