import { useEffect, useRef, useState } from "react";
import clsx from "clsx";
import { useTheme, type ThemeMode } from "@/lib/theme";

// TweaksPill — floating bottom-right pill from the design. Opens a
// small popover with misc preferences. Currently surfaces the theme
// mode quickly (also reachable from the TopBar toggle) and a density
// hint placeholder — kept as a single home for future low-traffic
// tweaks so they don't crowd the main header.
export function TweaksPill() {
  const [open, setOpen] = useState(false);
  const { mode, setMode } = useTheme();
  const wrapRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    function onDocClick(e: MouseEvent) {
      if (!wrapRef.current) return;
      if (!wrapRef.current.contains(e.target as Node)) setOpen(false);
    }
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") setOpen(false);
    }
    document.addEventListener("click", onDocClick);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("click", onDocClick);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  return (
    <div
      ref={wrapRef}
      className="pointer-events-none fixed bottom-4 right-4 z-30 flex flex-col items-end gap-2"
    >
      {open && (
        <div className="pointer-events-auto w-[220px] rounded-3 border border-line-2 bg-bg-2 p-3 shadow-2">
          <div className="mb-1 text-[10px] font-semibold uppercase tracking-[0.06em] text-fg-3">
            Tweaks
          </div>
          <div className="mb-2 text-[10.5px] text-fg-3">
            Quick toggles. Settings → tab for the full surface.
          </div>
          <div className="space-y-2">
            <ThemeRow mode={mode} setMode={setMode} />
          </div>
        </div>
      )}
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        className={clsx(
          "pointer-events-auto inline-flex items-center gap-1.5 rounded-pill border bg-bg-2 px-3 py-1.5 text-[11px] font-medium text-fg-2 shadow-1 transition-colors hover:bg-bg-3 hover:text-fg-0",
          open
            ? "border-accent/40 text-accent"
            : "border-line-2",
        )}
        aria-expanded={open}
      >
        <SliderIcon />
        Tweaks
        <Caret open={open} />
      </button>
    </div>
  );
}

function ThemeRow({
  mode,
  setMode,
}: {
  mode: ThemeMode;
  setMode: (m: ThemeMode) => void;
}) {
  return (
    <div>
      <div className="mb-1 text-[10px] uppercase tracking-[0.06em] text-fg-3">
        Theme
      </div>
      <div className="flex items-center gap-0.5 rounded-2 border border-line-2 bg-bg-1 p-0.5">
        {(["light", "dark", "system"] as ThemeMode[]).map((m) => (
          <button
            key={m}
            type="button"
            onClick={() => setMode(m)}
            className={clsx(
              "flex-1 rounded-1 px-2 py-1 text-[10.5px] capitalize transition-colors",
              mode === m
                ? "bg-bg-3 text-fg-0"
                : "text-fg-2 hover:text-fg-1",
            )}
          >
            {m}
          </button>
        ))}
      </div>
    </div>
  );
}


function SliderIcon() {
  return (
    <svg width={12} height={12} viewBox="0 0 16 16" fill="none" aria-hidden>
      <path
        d="M2 4h7M11 4h3M2 8h3M7 8h7M2 12h9M13 12h1"
        stroke="currentColor"
        strokeWidth="1.4"
        strokeLinecap="round"
      />
      <circle cx="10" cy="4" r="1.4" fill="currentColor" />
      <circle cx="6" cy="8" r="1.4" fill="currentColor" />
      <circle cx="12" cy="12" r="1.4" fill="currentColor" />
    </svg>
  );
}

function Caret({ open }: { open: boolean }) {
  return (
    <svg
      width={9}
      height={9}
      viewBox="0 0 12 12"
      fill="none"
      className={clsx(
        "transition-transform",
        open ? "rotate-180" : "rotate-0",
      )}
      aria-hidden
    >
      <path
        d="m3 4.5 3 3 3-3"
        stroke="currentColor"
        strokeWidth="1.4"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  );
}
