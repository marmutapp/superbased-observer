import { Pill } from "@/components/primitives";

// TagPill — one session-classification tag rendered through the existing
// Pill primitive. There is deliberately NO tag_defs table in v1 (see
// docs/plans/session-classification-tags-plan-2026-07-31.md §0 "Explicitly
// deferred"), so the colour comes from a stable hash of the tag NAME: the
// same tag reads the same colour on the Sessions table, the detail panel,
// and the rollup panel, with zero stored state.

const TAG_VARIANTS = [
  "accent",
  "info",
  "success",
  "warn",
  "danger",
  "neutral",
] as const;

export type TagVariant = (typeof TAG_VARIANTS)[number];

// tagVariant maps a tag name onto a Pill variant deterministically (FNV-1a
// over the UTF-16 code units — small, dependency-free, and stable across
// reloads/pages, which is the whole point).
export function tagVariant(tag: string): TagVariant {
  let h = 0x811c9dc5;
  for (let i = 0; i < tag.length; i++) {
    h ^= tag.charCodeAt(i);
    h = Math.imul(h, 0x01000193) >>> 0;
  }
  return TAG_VARIANTS[h % TAG_VARIANTS.length];
}

export function TagPill({
  tag,
  onClick,
  onRemove,
  title,
  className,
}: {
  tag: string;
  // onClick makes the pill an activatable filter affordance. It stops
  // propagation so a pill inside a clickable table row filters the list
  // instead of also opening that row's detail panel.
  onClick?: (tag: string) => void;
  // onRemove renders a trailing × inside the pill (editor use).
  onRemove?: (tag: string) => void;
  title?: string;
  className?: string;
}) {
  const body = (
    <Pill
      variant={tagVariant(tag)}
      className={className}
      // Pill's own `title` renders a themed Tooltip and makes the span
      // focusable — only useful on the inert form; the interactive form
      // owns focus itself.
      title={!onClick && !onRemove ? title : undefined}
    >
      {tag}
      {onRemove && (
        <span
          role="button"
          tabIndex={0}
          aria-label={`Remove tag ${tag}`}
          onClick={(e) => {
            e.stopPropagation();
            onRemove(tag);
          }}
          onKeyDown={(e) => {
            if (e.key === "Enter" || e.key === " ") {
              e.preventDefault();
              e.stopPropagation();
              onRemove(tag);
            }
          }}
          className="-mr-0.5 cursor-pointer px-0.5 text-fg-3 hover:text-fg-0 focus:outline-none"
        >
          ×
        </span>
      )}
    </Pill>
  );
  if (!onClick) return body;
  return (
    <span
      role="button"
      tabIndex={0}
      title={title ?? `Filter sessions tagged "${tag}"`}
      onClick={(e) => {
        e.stopPropagation();
        onClick(tag);
      }}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          e.stopPropagation();
          onClick(tag);
        }
      }}
      className="inline-flex cursor-pointer rounded-pill focus:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent-ring)]"
    >
      {body}
    </span>
  );
}
