import {
  createContext,
  type ReactNode,
  useContext,
  useMemo,
  useState,
} from "react";

// Global org-dashboard filter context. Mirrors the role web/'s FilterProvider
// plays for the primary dashboard, but the org endpoints currently scope ONLY
// by trailing window (?days=) — every /api/org/* rollup takes days and nothing
// else. Tool / project filters arrive with the enriched endpoints in later
// phases; until the backend supports them, surfacing those controls would be
// dishonest (a disabled control with no backing query). So the shared filter
// is the window, in days, driven by the TopBar's window control and read by
// every page instead of per-page useState.

export const WINDOW_OPTIONS = [7, 30, 90] as const;
export type WindowDays = (typeof WINDOW_OPTIONS)[number];

type FilterCtx = {
  days: WindowDays;
  setDays: (d: WindowDays) => void;
};

const Ctx = createContext<FilterCtx | null>(null);

export function FilterProvider({ children }: { children: ReactNode }) {
  const [days, setDays] = useState<WindowDays>(30);
  const value = useMemo(() => ({ days, setDays }), [days]);
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useFilters(): FilterCtx {
  const v = useContext(Ctx);
  if (!v) throw new Error("useFilters must be used inside <FilterProvider>");
  return v;
}
