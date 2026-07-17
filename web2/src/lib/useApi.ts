import { useCallback, useEffect, useState } from "react";
import { ApiError } from "./api";

export interface AsyncState<T> {
  data: T | null;
  error: string | null;
  loading: boolean;
  reload: () => void;
}

// useApi runs an async loader, tracking loading/error and exposing a reload.
// deps re-run the loader (e.g. the window-days selector or a route param).
export function useApi<T>(loader: () => Promise<T>, deps: unknown[]): AsyncState<T> {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [tick, setTick] = useState(0);

  // eslint-disable-next-line react-hooks/exhaustive-deps
  const run = useCallback(loader, deps);

  useEffect(() => {
    let live = true;
    setLoading(true);
    setError(null);
    run()
      .then((d) => {
        if (live) setData(d);
      })
      .catch((e: unknown) => {
        if (!live) return;
        const msg =
          e instanceof ApiError
            ? e.status === 403
              ? "You don't have access to this view."
              : e.status === 401
                ? "Your session has expired. Please sign in again."
                : e.message
            : String(e);
        setError(msg);
      })
      .finally(() => {
        if (live) setLoading(false);
      });
    return () => {
      live = false;
    };
  }, [run, tick]);

  const reload = useCallback(() => setTick((t) => t + 1), []);

  // Re-fire the loader when the TopBar Refresh button dispatches the global
  // "org-dashboard-refresh" CustomEvent (mirrors web/'s REFRESH_EVENT pattern).
  useEffect(() => {
    const onRefresh = () => setTick((t) => t + 1);
    window.addEventListener("org-dashboard-refresh", onRefresh);
    return () => window.removeEventListener("org-dashboard-refresh", onRefresh);
  }, []);
  return { data, error, loading, reload };
}
