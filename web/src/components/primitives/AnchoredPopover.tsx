import {
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type ReactNode,
  type RefObject,
} from "react";
import { createPortal } from "react-dom";
import { useCompanionRegistry } from "./companion";

// AnchoredPopover — a trigger-anchored, portalled popover.
//
// The dashboard already had this mechanic THREE times (TagEditor's tag
// popover, DateRangePopover, ContextMenu's cursor-anchored variant), each with
// its own copy of the same four concerns:
//
//   1. portal to <body>, so an ancestor's `overflow-x-auto` can't clip it;
//   2. register the portal root with the terminal-companion registry, so the
//      expanded terminal's focusin trap (LaunchTerminal) treats focus landing
//      inside as legitimate coexistence by EXACT NODE containment rather than a
//      spoofable attribute selector;
//   3. anchor under the trigger and clamp into the viewport (flip above when it
//      would overflow the bottom);
//   4. dismiss on outside pointer-down / Escape / scroll / resize.
//
// This is the extracted primitive (CLAUDE.md "fix systemically"). It is
// deliberately unopinionated about its contents: consumers own the trigger
// button and the body, and get told WHY they were dismissed so a "cancel"
// (Escape) can behave differently from a "close" (click away).
//
// NOT migrated in this pass: TagEditor / DateRangePopover / ContextMenu still
// carry their own copies. Porting them is mechanical but touches files outside
// this change's scope; the primitive exists so the next one doesn't add a
// fourth copy.

/** Why the popover asked to close. Consumers commonly save on "outside" /
 * "scroll" and revert on "escape". */
export type PopoverDismissReason = "outside" | "escape" | "scroll";

export function AnchoredPopover({
  open,
  anchorRef,
  onDismiss,
  ariaLabel,
  width = 320,
  reflowKey,
  children,
}: {
  open: boolean;
  /** The trigger element. Its rect is the anchor, and pointer-downs inside it
   * are NOT treated as "outside" (so the trigger can toggle). */
  anchorRef: RefObject<HTMLElement | null>;
  onDismiss: (reason: PopoverDismissReason) => void;
  ariaLabel: string;
  width?: number;
  /** Re-measure the anchor when this value changes (content that grows after
   * the first paint would otherwise keep the first frame's placement). */
  reflowKey?: unknown;
  children: ReactNode;
}) {
  const panelRef = useRef<HTMLDivElement | null>(null);
  const [pos, setPos] = useState<{ left: number; top: number } | null>(null);
  const companion = useCompanionRegistry();

  // Latest onDismiss without re-subscribing the document listeners on every
  // parent render (consumers pass inline arrows).
  const dismissRef = useRef(onDismiss);
  dismissRef.current = onDismiss;

  // Companion registration — see (2) above.
  useEffect(() => {
    const el = panelRef.current;
    if (!open || !el || !companion) return;
    return companion.register(el);
  }, [open, companion]);

  // Anchor under the trigger, clamped into the viewport; flip above when the
  // popover would overflow the bottom edge.
  useLayoutEffect(() => {
    if (!open) return;
    const t = anchorRef.current?.getBoundingClientRect();
    if (!t) return;
    const p = panelRef.current?.getBoundingClientRect();
    const m = 8;
    const w = p?.width ?? width;
    const h = p?.height ?? 240;
    let left = t.left;
    let top = t.bottom + 4;
    if (left + w > window.innerWidth - m) {
      left = Math.max(m, window.innerWidth - m - w);
    }
    if (top + h > window.innerHeight - m) {
      top = Math.max(m, t.top - 4 - h);
    }
    setPos({ left, top });
  }, [open, width, reflowKey, anchorRef]);

  // Reset the measured position on close so the next open can't paint one
  // frame at the previous anchor's coordinates.
  useEffect(() => {
    if (!open) setPos(null);
  }, [open]);

  // Dismissal. Escape is handled in the CAPTURE phase and stops propagation:
  // this popover lives inside a SlideOver whose own document-level Escape
  // handler closes the whole drawer, and dismissing a popover must not also
  // tear down the surface behind it.
  useEffect(() => {
    if (!open) return;
    const onDown = (e: PointerEvent) => {
      const target = e.target as Node;
      if (panelRef.current?.contains(target)) return;
      if (anchorRef.current?.contains(target)) return;
      dismissRef.current("outside");
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      e.preventDefault();
      e.stopPropagation();
      dismissRef.current("escape");
    };
    const onScroll = (e: Event) => {
      if (panelRef.current?.contains(e.target as Node)) return;
      dismissRef.current("scroll");
    };
    const onResize = () => dismissRef.current("scroll");
    document.addEventListener("pointerdown", onDown, true);
    document.addEventListener("keydown", onKey, true);
    window.addEventListener("scroll", onScroll, true);
    window.addEventListener("resize", onResize);
    return () => {
      document.removeEventListener("pointerdown", onDown, true);
      document.removeEventListener("keydown", onKey, true);
      window.removeEventListener("scroll", onScroll, true);
      window.removeEventListener("resize", onResize);
    };
  }, [open, anchorRef]);

  if (!open) return null;

  return createPortal(
    <div
      ref={panelRef}
      role="dialog"
      aria-label={ariaLabel}
      onClick={(e) => e.stopPropagation()}
      style={{
        position: "fixed",
        left: pos?.left ?? -9999,
        top: pos?.top ?? -9999,
        zIndex: 200,
        width,
        // Hidden until measured so the first paint never flashes at the
        // off-screen placeholder coordinates.
        visibility: pos ? "visible" : "hidden",
      }}
      className="overflow-hidden rounded-3 border border-line-2 bg-bg-1 shadow-drawer"
    >
      {children}
    </div>,
    document.body,
  );
}
