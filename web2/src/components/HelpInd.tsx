import clsx from "clsx";
import { Tooltip } from "@/components/primitives/Tooltip";

// MIRROR of web/src/components/HelpInd.tsx — the inline "there's help for this"
// indicator a column header / tile / chart can render. The org dashboard has
// no help drawer yet, so the click target is currently inert (no delegated
// handler); the hover hint still works. Pages pass `helpId` only once a help
// registry lands. Imported from the Tooltip module directly (not the primitives
// barrel) to avoid an index ↔ StatCard ↔ HelpInd import cycle.
export function HelpInd({ id, className }: { id: string; className?: string }) {
  return (
    <Tooltip
      content={
        <span className="block">
          Click for help <span className="text-fg-3">·</span> press{" "}
          <kbd>?</kbd> for the full drawer
        </span>
      }
      side="top"
      maxWidth={240}
    >
      <button
        type="button"
        data-help-id={id}
        aria-label="Show help"
        className={clsx(
          "ml-1 inline-flex h-3.5 w-3.5 items-center justify-center rounded-full border border-line-3 text-[8px] font-semibold text-fg-3 hover:border-accent hover:text-accent focus:border-accent focus:text-accent focus:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent-ring)]",
          className,
        )}
      >
        ?
      </button>
    </Tooltip>
  );
}
