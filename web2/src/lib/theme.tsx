import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
} from "react";
import type { ReactNode } from "react";

// MIRROR of web/src/lib/theme.tsx — light / dark / system theme, applied by
// toggling data-theme on <html> so the token palette flips without dark:
// variants. Kept in sync with the primary dashboard.

export type ThemeMode = "light" | "dark" | "system";
export type EffectiveTheme = "light" | "dark";

const STORAGE_KEY = "superbased.org.theme";

type ThemeCtx = {
  mode: ThemeMode;
  effective: EffectiveTheme;
  setMode: (m: ThemeMode) => void;
};

const Ctx = createContext<ThemeCtx | null>(null);

function readStoredMode(): ThemeMode {
  try {
    const v = localStorage.getItem(STORAGE_KEY);
    if (v === "light" || v === "dark" || v === "system") return v;
  } catch {
    // localStorage unavailable (SSR / privacy mode); fall through.
  }
  return "system";
}

function systemPrefersDark(): boolean {
  if (typeof window === "undefined") return true;
  return window.matchMedia?.("(prefers-color-scheme: dark)").matches ?? true;
}

function resolveEffective(mode: ThemeMode): EffectiveTheme {
  if (mode === "system") return systemPrefersDark() ? "dark" : "light";
  return mode;
}

function applyToRoot(effective: EffectiveTheme) {
  if (typeof document === "undefined") return;
  document.documentElement.dataset.theme = effective;
}

export function ThemeProvider({ children }: { children: ReactNode }) {
  const [mode, setModeState] = useState<ThemeMode>(() => readStoredMode());
  const [effective, setEffective] = useState<EffectiveTheme>(() =>
    resolveEffective(readStoredMode()),
  );

  const setMode = useCallback((m: ThemeMode) => {
    setModeState(m);
    try {
      localStorage.setItem(STORAGE_KEY, m);
    } catch {
      // Ignore; preference falls back to default on next load.
    }
  }, []);

  // Apply the resolved theme + recompute when mode changes.
  useEffect(() => {
    const next = resolveEffective(mode);
    setEffective(next);
    applyToRoot(next);
  }, [mode]);

  // When the user picks System, follow OS-level changes live.
  useEffect(() => {
    if (mode !== "system") return;
    const mq = window.matchMedia?.("(prefers-color-scheme: dark)");
    if (!mq) return;
    const onChange = () => {
      const next: EffectiveTheme = mq.matches ? "dark" : "light";
      setEffective(next);
      applyToRoot(next);
    };
    mq.addEventListener?.("change", onChange);
    return () => mq.removeEventListener?.("change", onChange);
  }, [mode]);

  const value = useMemo<ThemeCtx>(
    () => ({ mode, effective, setMode }),
    [mode, effective, setMode],
  );

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useTheme(): ThemeCtx {
  const v = useContext(Ctx);
  if (!v) throw new Error("useTheme must be used within ThemeProvider");
  return v;
}
