import {
  createContext,
  type ReactNode,
  useContext,
  useEffect,
  useMemo,
  useState,
} from "react";

// Detail-crumb context (G11). Gives the TopBar a third breadcrumb segment on
// `:id` detail routes — the entity's own name (team / project / trace / eval
// run) — which the route pattern alone can't supply because it lives in the
// page's fetched data. A detail page calls useDetailCrumb(name) once its data
// resolves; the hook publishes it and clears on unmount so list routes stay at
// two segments. Kept tiny and I/O-free, in the same spirit as filters.tsx.

type CrumbCtx = {
  crumb: string | null;
  setCrumb: (c: string | null) => void;
};

const Ctx = createContext<CrumbCtx | null>(null);

export function CrumbProvider({ children }: { children: ReactNode }) {
  const [crumb, setCrumb] = useState<string | null>(null);
  const value = useMemo(() => ({ crumb, setCrumb }), [crumb]);
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

// useCrumbValue is for the consumer (TopBar). Returns null outside a provider
// so the breadcrumb degrades gracefully rather than throwing.
export function useCrumbValue(): string | null {
  return useContext(Ctx)?.crumb ?? null;
}

// useDetailCrumb publishes a detail breadcrumb for the lifetime of the calling
// component. Pass the entity name once known (or null/undefined while loading).
export function useDetailCrumb(name: string | null | undefined): void {
  const ctx = useContext(Ctx);
  useEffect(() => {
    if (!ctx) return;
    ctx.setCrumb(name && name.trim() ? name : null);
    return () => ctx.setCrumb(null);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [name]);
}
