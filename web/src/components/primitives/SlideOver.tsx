import { useEffect, useRef, type ReactNode } from "react";
import { AnimatePresence, motion } from "framer-motion";
import { Tooltip } from "./Tooltip";

// Right-side slide-over drawer with backdrop. Escape closes,
// focus moves into the panel on open, scroll lock on body while
// open. Phase 10: framer-motion replaces the prior CSS-translate
// approach so the panel unmounts cleanly on close and the slide
// uses spring physics.
//
// Width defaults to 880px; pages can pass a larger value, and
// SessionDetailPanel passes 1400. We clamp to min(width, 96vw) via
// max-width on the panel container so even on narrow viewports the
// panel stays inside the window (it'll just take ~all of the
// available width on a smaller screen).
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

  useEffect(() => {
    if (!open) return;
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") onClose();
    }
    document.addEventListener("keydown", onKey);
    const prevOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    panelRef.current?.focus();
    return () => {
      document.removeEventListener("keydown", onKey);
      document.body.style.overflow = prevOverflow;
    };
  }, [open, onClose]);

  return (
    <AnimatePresence>
      {open && (
        <>
          <motion.div
            key="backdrop"
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            exit={{ opacity: 0 }}
            transition={{ duration: 0.18, ease: "easeOut" }}
            onClick={onClose}
            className="fixed inset-0 z-40 bg-black/60"
          />
          <motion.div
            key="panel"
            ref={panelRef}
            tabIndex={-1}
            role="dialog"
            aria-modal="true"
            initial={{ x: "100%" }}
            animate={{ x: 0 }}
            exit={{ x: "100%" }}
            transition={{
              type: "spring",
              stiffness: 320,
              damping: 32,
              mass: 0.7,
            }}
            style={{ width, maxWidth: "96vw" }}
            className="fixed inset-y-0 right-0 z-50 flex flex-col border-l border-line-2 bg-bg-1 shadow-drawer focus:outline-none"
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
          </motion.div>
        </>
      )}
    </AnimatePresence>
  );
}
