import { useEffect, useState } from "react";

// Shared media-query primitive + the ONE definition of "this is a touch
// phone/tablet, present the terminal full-viewport".
//
// WHY A CAPABILITY QUERY AND NOT A WIDTH ALONE (CLAUDE.md rule 3). A desktop
// user who narrows their browser window still has a FINE pointer, a physical
// keyboard, and a resizable floating window that works — flipping them into the
// touch presentation because of width alone would be a regression for them. So
// the mobile branch requires BOTH a coarse pointer AND a small viewport. The
// 1023px bound matches the `lg` breakpoint the rest of the dashboard's mobile
// pass uses, so tablets land on the same side of the line everywhere.

/** MobileTerminalQuery is the single source of truth for the touch-presentation
 * breakpoint. Exported so the Playwright regression specs assert against the
 * SAME string the app branches on, instead of a copy that can drift. */
export const MobileTerminalQuery = "(pointer: coarse) and (max-width: 1023px)";

/**
 * useMediaQuery subscribes to a CSS media query and re-renders on change.
 *
 * SSR/jsdom-safe: when `window.matchMedia` is unavailable the hook reports
 * false, which is the DESKTOP branch — the conservative default, since a false
 * negative leaves today's shipped behaviour untouched while a false positive
 * would restyle a desktop window.
 */
export function useMediaQuery(query: string): boolean {
  const [matches, setMatches] = useState(() => {
    if (typeof window === "undefined" || !window.matchMedia) return false;
    return window.matchMedia(query).matches;
  });

  useEffect(() => {
    if (typeof window === "undefined" || !window.matchMedia) return;
    const mql = window.matchMedia(query);
    // Re-read on (re)subscribe: the query may have changed, or the viewport may
    // have moved between the initial render and this effect.
    setMatches(mql.matches);
    const onChange = (e: MediaQueryListEvent) => setMatches(e.matches);
    mql.addEventListener("change", onChange);
    return () => mql.removeEventListener("change", onChange);
  }, [query]);

  return matches;
}

/**
 * useMobileTerminal reports whether the terminal surfaces should use the
 * touch/full-viewport presentation instead of the desktop floating window.
 *
 * Consumers must treat `false` as "everything exactly as it shipped" — every
 * mobile-branch style is additive and gated on `true`, so desktop geometry
 * (880x564 panel, 480px min-width, 24px backdrop padding) is bit-identical.
 */
export function useMobileTerminal(): boolean {
  return useMediaQuery(MobileTerminalQuery);
}
