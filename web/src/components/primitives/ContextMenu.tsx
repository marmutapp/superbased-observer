import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { useCompanionRegistry } from "./companion";

// ContextMenu — the dashboard's first cursor-anchored context menu, built as a
// primitive. Opens at a client (x,y), clamps into the viewport, and closes on
// outside pointer-down, Escape, scroll, or item activation. Standard menu
// semantics: role="menu"/"menuitem", roving arrow-key focus, Home/End.
//
// It portals to <body> (to escape any panel's overflow clipping) and registers
// its portal root with the terminal-companion registry (primitives/companion)
// so the expanded-terminal focusin trap (LaunchTerminal) treats menu focus as
// legitimate by exact-node containment rather than a spoofable attribute.

export type ContextMenuItem = {
  key: string;
  label: string;
  onSelect: () => void;
  disabled?: boolean;
};

export function ContextMenu({
  x,
  y,
  items,
  onClose,
}: {
  x: number;
  y: number;
  items: ContextMenuItem[];
  onClose: () => void;
}) {
  const menuRef = useRef<HTMLDivElement | null>(null);
  const [pos, setPos] = useState<{ left: number; top: number }>({ left: x, top: y });
  const enabledIdx = items.map((it, i) => (it.disabled ? -1 : i)).filter((i) => i >= 0);
  const enabledKey = enabledIdx.join(",");
  const [active, setActive] = useState<number>(enabledIdx[0] ?? -1);
  // Capture the element that had focus when the menu opened, BEFORE the menu
  // moves focus onto its first item, so we can restore it on close. useRef's
  // initializer runs once on first render — earlier than any focus effect.
  const openerRef = useRef<HTMLElement | null>(
    typeof document !== "undefined" ? (document.activeElement as HTMLElement | null) : null,
  );
  const companion = useCompanionRegistry();

  // Register the menu's portal root as a terminal-companion surface (exact
  // node, provider-owned allowlist) so focusing a menu item under the expanded
  // terminal's focusin trap is treated as legitimate coexistence, not a
  // stealer. Declared BEFORE the active-focus effect so registration lands
  // before the first item is focused.
  useEffect(() => {
    const el = menuRef.current;
    if (!el || !companion) return;
    return companion.register(el);
  }, [companion]);

  // Restore focus to the opener when the menu closes/unmounts (Escape, Tab,
  // activation, outside-click all lead to unmount) so focus doesn't fall to
  // <body> — and, under the terminal trap, isn't yanked into the xterm.
  useEffect(() => {
    return () => {
      const o = openerRef.current;
      if (o && document.contains(o)) o.focus?.();
    };
  }, []);

  // Keep the active index valid when the enabled item set changes (e.g. the
  // paste items appear/disappear as a writer registers): snap to the first
  // enabled item if the current one is no longer selectable.
  useEffect(() => {
    setActive((cur) => (enabledIdx.includes(cur) ? cur : (enabledIdx[0] ?? -1)));
    // enabledIdx is a fresh array each render; key on its stable string form.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [enabledKey]);

  // Clamp into the viewport once the menu has measured. Flip to the left/up of
  // the cursor when it would overflow the right/bottom edge.
  useLayoutEffect(() => {
    const el = menuRef.current;
    if (!el) return;
    const m = 6;
    const r = el.getBoundingClientRect();
    let left = x;
    let top = y;
    if (left + r.width > window.innerWidth - m) left = Math.max(m, x - r.width);
    if (top + r.height > window.innerHeight - m) top = Math.max(m, y - r.height);
    setPos({ left, top });
  }, [x, y]);

  // Move DOM focus onto the active item so arrow navigation + activation read
  // as a real menu. Focusing here is safe under the terminal trap because the
  // menu's root is registered with the companion registry (above).
  useEffect(() => {
    if (active < 0) return;
    const el = menuRef.current?.querySelector<HTMLButtonElement>(`[data-idx="${active}"]`);
    el?.focus();
  }, [active]);

  // Dismiss on any interaction that should not fall through to the menu.
  useEffect(() => {
    const onDown = (e: PointerEvent) => {
      if (!menuRef.current?.contains(e.target as Node)) onClose();
    };
    const onScroll = () => onClose();
    document.addEventListener("pointerdown", onDown, true);
    window.addEventListener("scroll", onScroll, true);
    window.addEventListener("resize", onScroll);
    return () => {
      document.removeEventListener("pointerdown", onDown, true);
      window.removeEventListener("scroll", onScroll, true);
      window.removeEventListener("resize", onScroll);
    };
  }, [onClose]);

  const step = (dir: 1 | -1) => {
    if (enabledIdx.length === 0) return;
    const cur = enabledIdx.indexOf(active);
    const next = cur < 0 ? 0 : (cur + dir + enabledIdx.length) % enabledIdx.length;
    setActive(enabledIdx[next]);
  };

  const onKeyDown = (e: React.KeyboardEvent) => {
    switch (e.key) {
      case "Escape":
        e.preventDefault();
        e.stopPropagation();
        onClose();
        break;
      case "Tab":
        // Don't let Tab move focus INTO the terminal (or its trap) while the
        // menu is open: close the menu, which restores focus to the opener.
        e.preventDefault();
        e.stopPropagation();
        onClose();
        break;
      case "ArrowDown":
        e.preventDefault();
        step(1);
        break;
      case "ArrowUp":
        e.preventDefault();
        step(-1);
        break;
      case "Home":
        e.preventDefault();
        if (enabledIdx.length) setActive(enabledIdx[0]);
        break;
      case "End":
        e.preventDefault();
        if (enabledIdx.length) setActive(enabledIdx[enabledIdx.length - 1]);
        break;
      case "Enter":
      case " ": {
        e.preventDefault();
        const it = items[active];
        if (it && !it.disabled) {
          it.onSelect();
          onClose();
        }
        break;
      }
    }
  };

  return createPortal(
    <div
      ref={menuRef}
      role="menu"
      tabIndex={-1}
      onKeyDown={onKeyDown}
      style={{ position: "fixed", left: pos.left, top: pos.top, zIndex: 200 }}
      className="min-w-[184px] overflow-hidden rounded-2 border border-line-2 bg-bg-1 py-1 text-[12px] shadow-drawer"
    >
      {items.map((it, i) => (
        <button
          key={it.key}
          type="button"
          role="menuitem"
          data-idx={i}
          disabled={it.disabled}
          tabIndex={i === active ? 0 : -1}
          onMouseEnter={() => !it.disabled && setActive(i)}
          onClick={() => {
            if (it.disabled) return;
            it.onSelect();
            onClose();
          }}
          className={
            "flex w-full items-center px-3 py-1.5 text-left focus:outline-none " +
            (it.disabled
              ? "cursor-not-allowed text-fg-4 opacity-60"
              : "text-fg-2 hover:bg-bg-2 hover:text-fg-0 focus:bg-bg-2 focus:text-fg-0")
          }
        >
          {it.label}
        </button>
      ))}
    </div>,
    document.body,
  );
}
