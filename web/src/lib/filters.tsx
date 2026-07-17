import {
  createContext,
  type ReactNode,
  useCallback,
  useContext,
  useMemo,
  useState,
} from "react";

// Window is the global time-scope selector shared by every page.
// Day-grained presets map to `days=<int>`; the two sub-day presets
// map to `hours=<int>`; "1d" is deliberately days=1 (NOT hours);
// "all" keeps each page's own far-horizon / zero sentinel; "custom"
// carries an explicit [since, until] range on the wire.
export type Window =
  | "1h"
  | "12h"
  | "1d"
  | "7d"
  | "14d"
  | "30d"
  | "90d"
  | "1y"
  | "all"
  | "custom";

// CustomRange holds RFC3339-UTC ISO strings for the "custom" window.
// `until` may be "" meaning "now" — resolved at request time so a
// left-open range keeps tracking the present without a re-apply.
export type CustomRange = { since: string; until: string };

// WindowParams is the wire-shape a window resolves to. It spreads
// cleanly into a page's QueryParams object (day preset → {days},
// sub-day preset → {hours}, custom → {since, until}).
export type WindowParams =
  | { days: number }
  | { hours: number }
  | { since: string; until: string };

export type Filters = {
  win: Window;
  customRange: CustomRange;
  tool: string;
  project: string;
  // Global free-text search, set by FilterBar's "Search anything…"
  // input. Pages opt-in by reading `query` and filtering whatever
  // makes sense for that surface (Sessions filters by id/project,
  // Actions by target, etc.). Empty string = no filter.
  query: string;
};

type FilterCtx = Filters & {
  setWin: (w: Window) => void;
  setCustomRange: (r: CustomRange) => void;
  setTool: (t: string) => void;
  setProject: (p: string) => void;
  setQuery: (q: string) => void;
};

const Ctx = createContext<FilterCtx | null>(null);

const WIN_LS_KEY = "sb_win";
const RANGE_LS_KEY = "sb_win_range";

const WINDOW_VALUES: Window[] = [
  "1h",
  "12h",
  "1d",
  "7d",
  "14d",
  "30d",
  "90d",
  "1y",
  "all",
  "custom",
];

function isWindow(v: unknown): v is Window {
  return typeof v === "string" && (WINDOW_VALUES as string[]).includes(v);
}

// RFC3339_TZ matches an ISO-8601 datetime that carries an EXPLICIT
// timezone (either `Z` or a `±HH:MM` offset). Date-only strings
// ("2026-01-01") and timezone-less datetimes ("2026-01-01T10:00")
// are deliberately excluded: `Date.parse` accepts them, but the
// backend is strict RFC3339 and rejects them, which would silently
// fall the whole window back to 30d while the chip still claims a
// custom range.
const RFC3339_TZ =
  /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2})?(\.\d+)?(Z|[+-]\d{2}:?\d{2})$/;

// canonicalIso normalizes an RFC3339 string to canonical UTC form
// (`new Date(ms).toISOString()`, always `…Z`), or returns null when
// the value is not a timezone-explicit, parseable datetime. Every
// custom-range value that is stored, restored, or sent on the wire
// passes through here so the strict backend always receives a value
// it round-trips.
function canonicalIso(v: unknown): string | null {
  if (typeof v !== "string" || !RFC3339_TZ.test(v)) return null;
  const ms = Date.parse(v);
  if (!Number.isFinite(ms)) return null;
  return new Date(ms).toISOString();
}

// readStoredRange returns a VALIDATED, canonicalized custom range or
// null. A range is valid when `since` canonicalizes and (if present)
// `until` canonicalizes AND is strictly after `since`. Both fields
// are normalized to canonical UTC before being returned.
function readStoredRange(): CustomRange | null {
  try {
    const raw = localStorage.getItem(RANGE_LS_KEY);
    if (!raw) return null;
    const obj = JSON.parse(raw) as { since?: unknown; until?: unknown };
    const since = canonicalIso(obj.since);
    if (!since) return null;
    let until = "";
    if (typeof obj.until === "string" && obj.until !== "") {
      const u = canonicalIso(obj.until);
      if (!u || Date.parse(u) <= Date.parse(since)) return null;
      until = u;
    }
    return { since, until };
  } catch {
    return null;
  }
}

// readStoredWin restores the persisted window, defaulting to "30d" on
// anything unrecognised. "custom" only survives restore when the
// stored range is itself valid — otherwise it falls back to default.
function readStoredWin(): Window {
  try {
    const v = localStorage.getItem(WIN_LS_KEY);
    if (isWindow(v)) {
      if (v === "custom") return readStoredRange() ? "custom" : "30d";
      return v;
    }
  } catch {
    // localStorage unavailable (SSR / privacy mode); fall through.
  }
  return "30d";
}

export function FilterProvider({ children }: { children: ReactNode }) {
  const [win, setWinState] = useState<Window>(() => readStoredWin());
  const [customRange, setCustomRangeState] = useState<CustomRange>(
    () => readStoredRange() ?? { since: "", until: "" },
  );
  const [tool, setTool] = useState<string>("all");
  const [project, setProject] = useState<string>("all");
  const [query, setQuery] = useState<string>("");

  const setWin = useCallback((w: Window) => {
    setWinState(w);
    try {
      localStorage.setItem(WIN_LS_KEY, w);
    } catch {
      // Ignore; preference falls back to default on next load.
    }
  }, []);

  const setCustomRange = useCallback((r: CustomRange) => {
    // Normalize to canonical RFC3339-UTC before persisting/using, so a
    // stored range always round-trips through the strict backend even
    // if a caller ever hands us a non-canonical value.
    const norm: CustomRange = {
      since: canonicalIso(r.since) ?? "",
      until: r.until ? (canonicalIso(r.until) ?? "") : "",
    };
    setCustomRangeState(norm);
    try {
      localStorage.setItem(RANGE_LS_KEY, JSON.stringify(norm));
    } catch {
      // Ignore; preference falls back to default on next load.
    }
  }, []);

  const value = useMemo(
    () => ({
      win,
      customRange,
      tool,
      project,
      query,
      setWin,
      setCustomRange,
      setTool,
      setProject,
      setQuery,
    }),
    [win, customRange, tool, project, query, setWin, setCustomRange],
  );

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useFilters(): FilterCtx {
  const v = useContext(Ctx);
  if (!v) throw new Error("useFilters must be used inside <FilterProvider>");
  return v;
}

const WINDOW_TO_DAYS: Record<
  Exclude<Window, "1h" | "12h" | "all" | "custom">,
  number
> = {
  "1d": 1,
  "7d": 7,
  "14d": 14,
  "30d": 30,
  "90d": 90,
  "1y": 365,
};

// capWindowParams clamps a resolved WindowParams to at most `maxDays`
// of span — used by pages (Compression) whose backend caps some
// endpoints at a year regardless of the global window.
function capWindowParams(p: WindowParams, maxDays: number): WindowParams {
  if ("days" in p) return { days: Math.min(p.days, maxDays) };
  if ("hours" in p) return { hours: Math.min(p.hours, maxDays * 24) };
  const untilMs = Date.parse(p.until) || Date.now();
  const minSinceMs = untilMs - maxDays * 86_400_000;
  const sinceMs = Date.parse(p.since);
  const since =
    Number.isFinite(sinceMs) && sinceMs < minSinceMs
      ? new Date(minSinceMs).toISOString()
      : p.since;
  return { since, until: p.until };
}

// windowParams is the SINGLE source of truth for turning the global
// window into query params. Pages spread the result into their
// useApi params instead of hand-rolling a daysParam. `allSentinel`
// customises the far-horizon value ("all") per endpoint family
// (36500 for most; 0 on the Cache endpoints). `maxDays` optionally
// caps the span (Compression's 1-year-capped endpoints).
export function windowParams(
  win: Window,
  custom: CustomRange,
  opts?: { allSentinel?: number; maxDays?: number },
): WindowParams {
  const allSentinel = opts?.allSentinel ?? 36500;
  let p: WindowParams;
  switch (win) {
    case "all":
      p = { days: allSentinel };
      break;
    case "1h":
      p = { hours: 1 };
      break;
    case "12h":
      p = { hours: 12 };
      break;
    case "custom": {
      // Defensive: an invalid stored custom range shouldn't reach
      // here (readStoredWin downgrades it), but if it does, fall back
      // to the 30-day default rather than emitting a bad range. Both
      // ends are re-canonicalized so the wire value is always a
      // timezone-explicit UTC string the strict backend accepts.
      const since = canonicalIso(custom.since);
      if (!since) {
        p = { days: 30 };
        break;
      }
      const until = canonicalIso(custom.until) ?? new Date().toISOString();
      p = { since, until };
      break;
    }
    default:
      p = { days: WINDOW_TO_DAYS[win] };
      break;
  }
  if (opts?.maxDays != null) p = capWindowParams(p, opts.maxDays);
  return p;
}

const WINDOW_SPAN_HOURS: Record<
  Exclude<Window, "all" | "custom">,
  number
> = {
  "1h": 1,
  "12h": 12,
  "1d": 24,
  "7d": 168,
  "14d": 336,
  "30d": 720,
  "90d": 2160,
  "1y": 8760,
};

// windowSpanHours returns the window's span in hours (Infinity for
// "all"). Pages use it to decide `bucket=hour` (span <= 48h) on the
// timeseries endpoints.
export function windowSpanHours(win: Window, custom: CustomRange): number {
  if (win === "all") return Infinity;
  if (win === "custom") {
    const since = Date.parse(custom.since);
    const until = custom.until ? Date.parse(custom.until) : Date.now();
    // A non-finite or REVERSED range (until <= since) is invalid.
    // Signal it as Infinity rather than clamping to 0 — a 0 span
    // reads as "sub-day" and would wrongly select bucket=hour.
    if (!Number.isFinite(since) || !Number.isFinite(until) || until <= since) {
      return Infinity;
    }
    return (until - since) / 3_600_000;
  }
  return WINDOW_SPAN_HOURS[win];
}

// windowDaysApprox returns an integer day-count for the few surfaces
// that need a plain number rather than the wire params (the Sessions
// calendar span, the verbosity endpoint's since_days). Sub-day
// windows round up to 1 day.
export function windowDaysApprox(
  win: Window,
  custom: CustomRange,
  allSentinel = 36500,
): number {
  const h = windowSpanHours(win, custom);
  if (!Number.isFinite(h)) return allSentinel;
  return Math.max(1, Math.ceil(h / 24));
}

function fmtRangePart(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "?";
  return d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
}

const WINDOW_SHORT_LABEL: Partial<Record<Window, string>> = {
  all: "All",
};

// windowLabel is the compact chip / sub-heading label for a window.
// Presets render as their key ("1h", "30d", "1y"); "all" → "All";
// "custom" → "Jul 10 → Jul 16" (or "Jul 10 → now" for an open end).
export function windowLabel(win: Window, custom: CustomRange): string {
  if (win === "custom") {
    const start = custom.since ? fmtRangePart(custom.since) : "?";
    const end = custom.until ? fmtRangePart(custom.until) : "now";
    return `${start} → ${end}`;
  }
  return WINDOW_SHORT_LABEL[win] ?? win;
}
