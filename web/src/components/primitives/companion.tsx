import {
  createContext,
  useCallback,
  useContext,
  useMemo,
  useRef,
  type ReactNode,
} from "react";

// Terminal-companion registry — a provider-owned Set of the EXACT DOM root
// elements that count as legitimate coexisting surfaces beside an expanded
// terminal (a floating project panel and its context menu). The expanded
// terminal's focusin trap (LaunchTerminal) consults `contains()` to decide
// whether a focus landing outside the terminal is a real stealer or legitimate
// coexistence.
//
// This replaces the earlier self-asserted `[data-terminal-companion]` selector
// allowlist: any element (even <body>) could set that attribute and bypass the
// trap. Membership is now by explicit registration of a specific node the
// primitive owns, tested by DOM containment — no selector, no self-assertion.
//
// It lives in its own module (rendered by LaunchDockProvider) rather than on
// the LaunchDock context object itself, to avoid a static import cycle between
// LaunchTerminal (which reads it) and LaunchDock (which imports LaunchTerminal).

export type CompanionRegistry = {
  /** Register a root element; returns an unregister fn for effect cleanup. */
  register: (el: HTMLElement) => () => void;
  /** True when `node` is (or is contained by) any registered companion root. */
  contains: (node: Node | null) => boolean;
};

const CompanionCtx = createContext<CompanionRegistry | null>(null);

export function CompanionProvider({ children }: { children: ReactNode }) {
  const els = useRef<Set<HTMLElement>>(new Set());

  const register = useCallback((el: HTMLElement) => {
    els.current.add(el);
    return () => {
      els.current.delete(el);
    };
  }, []);

  const contains = useCallback((node: Node | null): boolean => {
    if (!node) return false;
    for (const el of els.current) {
      if (el === node || el.contains(node)) return true;
    }
    return false;
  }, []);

  const value = useMemo<CompanionRegistry>(
    () => ({ register, contains }),
    [register, contains],
  );

  return <CompanionCtx.Provider value={value}>{children}</CompanionCtx.Provider>;
}

/** The companion registry, or null outside a CompanionProvider (safe no-op). */
export function useCompanionRegistry(): CompanionRegistry | null {
  return useContext(CompanionCtx);
}
