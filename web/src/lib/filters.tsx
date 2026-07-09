import {
  createContext,
  type ReactNode,
  useContext,
  useMemo,
  useState,
} from "react";

export type Window = "7d" | "14d" | "30d" | "90d" | "1y" | "all";

export type Filters = {
  win: Window;
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
  setTool: (t: string) => void;
  setProject: (p: string) => void;
  setQuery: (q: string) => void;
};

const Ctx = createContext<FilterCtx | null>(null);

export function FilterProvider({ children }: { children: ReactNode }) {
  const [win, setWin] = useState<Window>("30d");
  const [tool, setTool] = useState<string>("all");
  const [project, setProject] = useState<string>("all");
  const [query, setQuery] = useState<string>("");

  const value = useMemo(
    () => ({
      win,
      tool,
      project,
      query,
      setWin,
      setTool,
      setProject,
      setQuery,
    }),
    [win, tool, project, query],
  );

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useFilters(): FilterCtx {
  const v = useContext(Ctx);
  if (!v) throw new Error("useFilters must be used inside <FilterProvider>");
  return v;
}

const WINDOW_TO_DAYS: Record<Window, number | "all"> = {
  "7d": 7,
  "14d": 14,
  "30d": 30,
  "90d": 90,
  "1y": 365,
  all: "all",
};

export function windowDays(w: Window): number | "all" {
  return WINDOW_TO_DAYS[w];
}
