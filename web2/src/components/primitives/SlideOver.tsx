import { useEffect, useRef, useState, type ReactNode } from "react";
import clsx from "clsx";
import { Tooltip } from "./Tooltip";

// MIRROR of web/src/components/primitives/SlideOver.tsx — a right-side
// slide-over drawer with backdrop. Escape closes, focus moves into the panel
// on open, scroll-lock on body while open.
//
// web/ uses framer-motion for spring physics; the org dashboard deliberately
// keeps its dependency surface minimal, so this port uses CSS transitions
// (transform + opacity, ~200ms ease-out) instead. The mount/unmount lifecycle
// is managed locally so the panel still unmounts cleanly after the exit
// transition rather than lingering in the DOM.
//
// Width defaults to 880px; pages can pass a larger value. We clamp to
// min(width, 96vw) via max-width so even on narrow viewports the panel stays
// inside the window.
export function SlideOver({
  open,
  onClose,
  title,
  subtitle,
  children,
  width = 880,
}: {
  open: boolean;
  onClose: () => void;
  title: ReactNode;
  subtitle?: ReactNode;
  children: ReactNode;
  width?: number;
}) {
  const panelRef = useRef<HTMLDivElement>(null);
  // `render` keeps the panel mounted through the exit transition; `shown`
  // drives the enter/exit transform+opacity.
  const [render, setRender] = useState(open);
  const [shown, setShown] = useState(false);

  useEffect(() => {
    if (open) {
      setRender(true);
      // Next frame: flip `shown` so the transition runs from the off-screen
      // start state, and move focus into the now-mounted panel.
      const id = requestAnimationFrame(() => {
        setShown(true);
        panelRef.current?.focus();
      });
      return () => cancelAnimationFrame(id);
    }
    setShown(false);
    const t = window.setTimeout(() => setRender(false), 220);
    return () => window.clearTimeout(t);
  }, [open]);

  useEffect(() => {
    if (!open) return;
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") onClose();
    }
    document.addEventListener("keydown", onKey);
    const prevOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.removeEventListener("keydown", onKey);
      document.body.style.overflow = prevOverflow;
    };
  }, [open, onClose]);

  if (!render) return null;

  return (
    <>
      <div
        aria-hidden
        onClick={onClose}
        className={clsx(
          "fixed inset-0 z-40 bg-black/60 transition-opacity duration-200 ease-out",
          shown ? "opacity-100" : "opacity-0",
        )}
      />
      <div
        ref={panelRef}
        tabIndex={-1}
        role="dialog"
        aria-modal="true"
        style={{ width, maxWidth: "96vw" }}
        className={clsx(
          "fixed inset-y-0 right-0 z-50 flex flex-col border-l border-line-2 bg-bg-1 shadow-drawer transition-transform duration-200 ease-out focus:outline-none",
          shown ? "translate-x-0" : "translate-x-full",
        )}
      >
        <header className="flex items-start justify-between gap-3 border-b border-line-1 px-5 py-3">
          <div className="min-w-0">
            <div className="truncate text-[14px] font-semibold text-fg-0">
              {title}
            </div>
            {subtitle && (
              <div className="mt-0.5 truncate text-[11.5px] text-fg-3">
                {subtitle}
              </div>
            )}
          </div>
          <Tooltip content={<>Close <kbd>Esc</kbd></>}>
            <button
              type="button"
              onClick={onClose}
              className="grid h-7 w-7 shrink-0 place-items-center rounded-2 border border-line-2 bg-bg-2 text-[14px] text-fg-2 hover:bg-bg-3 hover:text-fg-0"
              aria-label="Close"
            >
              ×
            </button>
          </Tooltip>
        </header>
        <div className="min-h-0 flex-1 overflow-y-auto">{children}</div>
      </div>
    </>
  );
}
