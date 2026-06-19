import {
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import clsx from "clsx";
import { Tooltip } from "./Tooltip";

// ComboChip — filter-chip button + anchored popover combobox.
// Mirrors design/shell.jsx:155-166 + design/app.css:259-275: icon +
// label + value + optional swatch + chevron in the resting chip;
// popover lists options with type-to-filter and keyboard nav.
//
// Single-select; replace useState in parent. Empty/clear is modelled
// as a sentinel option in `options` (typically value="all"). Caller
// owns the option list — the chip just renders + filters + selects.

export type ComboOption = {
  value: string;
  label: ReactNode;
  // searchable: lower-cased text the type-ahead input matches against.
  // Pages should derive from label + value + side data so models like
  // "anthropic/claude-opus-4-7" match a search for "opus".
  searchable: string;
  // Optional adornment rendered before the label in the popover row.
  // Useful for ToolDot in the Tool filter.
  leading?: ReactNode;
  // Right-side aux text (e.g. action_count).
  rightMeta?: ReactNode;
  // Optional tooltip on the popover row.
  title?: string;
};

export function ComboChip({
  value,
  onChange,
  options,
  icon,
  label,
  className,
  popoverWidth = 320,
  placeholder = "Filter…",
  emptyHint = "No matches.",
  buttonValueRender,
}: {
  value: string;
  onChange: (next: string) => void;
  options: ComboOption[];
  icon?: ReactNode;
  label: string;
  className?: string;
  popoverWidth?: number;
  placeholder?: string;
  emptyHint?: string;
  // When set, overrides the default chip-value rendering. Receives the
  // currently-selected option (or `undefined` when the selected value
  // isn't in the options list). Lets ToolSelect render a ToolDot +
  // pretty label without coupling that detail into the primitive.
  buttonValueRender?: (selected: ComboOption | undefined) => ReactNode;
}) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [activeIdx, setActiveIdx] = useState(0);
  const rootRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLDivElement>(null);

  const selected = options.find((o) => o.value === value);

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return options;
    return options.filter((o) => o.searchable.includes(q));
  }, [options, query]);

  // Reset state when the popover opens/closes so each open starts
  // fresh with the active row at the top.
  useEffect(() => {
    if (open) {
      setQuery("");
      setActiveIdx(0);
      // Focus the search input on next paint so the autofocus
      // doesn't fight the popover's mount animation.
      requestAnimationFrame(() => inputRef.current?.focus());
    }
  }, [open]);

  // Click-outside closes.
  useEffect(() => {
    if (!open) return;
    function onDown(e: MouseEvent) {
      if (!rootRef.current) return;
      if (!rootRef.current.contains(e.target as Node)) setOpen(false);
    }
    document.addEventListener("mousedown", onDown);
    return () => document.removeEventListener("mousedown", onDown);
  }, [open]);

  // Keep the active option in view as the user navigates.
  useEffect(() => {
    if (!open || !listRef.current) return;
    const el = listRef.current.querySelector<HTMLButtonElement>(
      `[data-idx="${activeIdx}"]`,
    );
    el?.scrollIntoView({ block: "nearest" });
  }, [open, activeIdx]);

  function selectIdx(i: number) {
    const opt = filtered[i];
    if (!opt) return;
    onChange(opt.value);
    setOpen(false);
  }

  function onKey(e: React.KeyboardEvent) {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setActiveIdx((i) => Math.min(filtered.length - 1, i + 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setActiveIdx((i) => Math.max(0, i - 1));
    } else if (e.key === "Enter") {
      e.preventDefault();
      selectIdx(activeIdx);
    } else if (e.key === "Escape") {
      e.preventDefault();
      setOpen(false);
    }
  }

  return (
    <div ref={rootRef} className={clsx("relative", className)}>
      <Tooltip
        content={`${label}: ${selected?.searchable ?? value}`}
        disabled={open}
      >
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        aria-haspopup="listbox"
        aria-expanded={open}
        className={clsx(
          "flex h-7 items-center gap-1.5 rounded-2 border bg-bg-2 px-2 text-[11px] text-fg-1 transition-colors",
          open
            ? "border-accent"
            : "border-line-2 hover:bg-bg-3 hover:text-fg-0",
        )}
      >
        {icon}
        <span className="text-fg-3">{label}</span>
        {buttonValueRender ? (
          buttonValueRender(selected)
        ) : (
          <b className="font-semibold text-fg-0">
            {selected?.label ?? value}
          </b>
        )}
        <ChevronDown />
      </button>
      </Tooltip>

      {open && (
        <div
          className="absolute left-0 top-[calc(100%+4px)] z-50 overflow-hidden rounded-3 border border-line-2 bg-bg-1 shadow-drawer"
          style={{ width: popoverWidth }}
          role="listbox"
          aria-label={label}
        >
          <div className="border-b border-line-1 bg-bg-2/60 px-2 py-1.5">
            <input
              ref={inputRef}
              type="search"
              value={query}
              onChange={(e) => {
                setQuery(e.target.value);
                setActiveIdx(0);
              }}
              onKeyDown={onKey}
              placeholder={placeholder}
              className="h-7 w-full appearance-none rounded-2 border border-line-2 bg-bg-1 px-2 text-[11px] text-fg-1 placeholder:text-fg-4 focus:border-accent focus:outline-none"
            />
          </div>
          <div
            ref={listRef}
            className="max-h-[280px] overflow-y-auto py-1"
            onKeyDown={onKey}
            tabIndex={-1}
          >
            {filtered.length === 0 ? (
              <p className="px-3 py-3 text-[11px] text-fg-3">{emptyHint}</p>
            ) : (
              filtered.map((o, i) => {
                const sel = o.value === value;
                const active = i === activeIdx;
                return (
                  <Tooltip key={o.value} content={o.title ?? null} side="right" disabled={!o.title}>
                  <button
                    type="button"
                    data-idx={i}
                    role="option"
                    aria-selected={sel}
                    onMouseEnter={() => setActiveIdx(i)}
                    onClick={() => selectIdx(i)}
                    className={clsx(
                      "flex w-full items-center gap-2 px-2.5 py-1.5 text-left text-[11.5px] transition-colors",
                      active ? "bg-bg-3" : "bg-transparent",
                      sel ? "text-accent" : "text-fg-1",
                    )}
                  >
                    {o.leading}
                    <span className="min-w-0 flex-1 truncate">{o.label}</span>
                    {o.rightMeta && (
                      <span className="shrink-0 font-mono text-[10.5px] text-fg-3">
                        {o.rightMeta}
                      </span>
                    )}
                    {sel && <CheckIcon />}
                  </button>
                  </Tooltip>
                );
              })
            )}
          </div>
        </div>
      )}
    </div>
  );
}

function ChevronDown() {
  return (
    <svg
      width="10"
      height="10"
      viewBox="0 0 16 16"
      fill="none"
      aria-hidden
      className="ml-0.5 text-fg-3"
    >
      <path
        d="m4 6 4 4 4-4"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  );
}

function CheckIcon() {
  return (
    <svg
      width="11"
      height="11"
      viewBox="0 0 16 16"
      fill="none"
      aria-hidden
      className="shrink-0 text-accent"
    >
      <path
        d="m3.5 8.5 3 3 6-6"
        stroke="currentColor"
        strokeWidth="1.6"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  );
}
