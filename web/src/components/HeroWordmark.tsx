import { WORDMARK } from "@/lib/wordmark";

// HeroWordmark — a small, muted, corner-anchored product mark used on
// the four camera-ready hero stat surfaces (Cost page hero stats,
// Suggestions "avoidable spend" hero, MilestonesCard, Report.tsx) so
// a plain screenshot still identifies the tool — no export pipeline,
// the monkeytype model (docs/plans/growth-virality-product-review-
// 2026-07-30.md §4.1).
//
// `corner` (default) is absolutely positioned + pointer-events-none —
// the parent block must be `relative` — so it never intercepts clicks
// or shifts real layout. `inline` renders as a static, equally muted
// span for slim single-row surfaces (MilestonesCard's bar) where an
// absolute overlay risks colliding with the dismiss control.
export function HeroWordmark({
  variant = "corner",
  className = "",
}: {
  variant?: "corner" | "inline";
  className?: string;
}) {
  if (variant === "inline") {
    return (
      <span
        aria-hidden
        className={`select-none font-mono text-[9.5px] tracking-[0.04em] text-fg-4/80 ${className}`}
      >
        {WORDMARK}
      </span>
    );
  }
  return (
    <span
      aria-hidden
      className={`pointer-events-none absolute bottom-1.5 right-2 select-none font-mono text-[9.5px] tracking-[0.04em] text-fg-4/70 ${className}`}
    >
      {WORDMARK}
    </span>
  );
}
