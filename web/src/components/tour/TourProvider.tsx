import {
  createContext,
  lazy,
  Suspense,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { TOUR_STEPS, type TourStep } from "@/lib/tour";

// TourProvider — owns the first-run guided tour state and the auto-start
// gate. Mounts INSIDE <BrowserRouter> (the overlay it lazy-renders uses
// useNavigate). The overlay chunk (TourOverlay) loads only while the tour
// is active, mirroring the HelpDrawer lazy gate in App.tsx.
//
// Persistence follows the OnboardingCard / FirstCaptureToast norm:
// localStorage key `sb_tour_completed = "1"` once the user finishes OR
// skips. All reads/writes are wrapped in try/catch so a private-mode
// browser (localStorage throws) degrades to "tour always available,
// never auto-persisted" rather than crashing.

const STORAGE_KEY = "sb_tour_completed";
const AUTO_START_MIN_WIDTH = 1024;
const AUTO_START_DELAY_MS = 600;

function hasCompleted(): boolean {
  try {
    return localStorage.getItem(STORAGE_KEY) === "1";
  } catch {
    return false;
  }
}

function markCompleted(): void {
  try {
    localStorage.setItem(STORAGE_KEY, "1");
  } catch {
    // ignore — persistence is best-effort (private mode / disabled storage)
  }
}

export type TourContextValue = {
  active: boolean;
  stepIndex: number;
  steps: TourStep[];
  startTour: () => void;
  endTour: (completed: boolean) => void;
  next: () => void;
  back: () => void;
  goTo: (index: number) => void;
};

const TourContext = createContext<TourContextValue | null>(null);

// useTour reads the tour context. Throws if used outside <TourProvider>
// so a mis-wire fails loudly at dev time rather than silently no-op'ing.
export function useTour(): TourContextValue {
  const ctx = useContext(TourContext);
  if (!ctx) {
    throw new Error("useTour must be used within <TourProvider>");
  }
  return ctx;
}

// Overlay is its own lazy chunk — the spotlight/floating-ui code never
// enters the shell bundle and only downloads once the tour runs.
const TourOverlay = lazy(() =>
  import("./TourOverlay").then((m) => ({ default: m.TourOverlay })),
);

export function TourProvider({ children }: { children: ReactNode }) {
  const [active, setActive] = useState(false);
  const [stepIndex, setStepIndex] = useState(0);

  // activeRef mirrors `active` so the deferred auto-start timer can bail
  // if a manual start won the race — without re-subscribing the timer.
  const activeRef = useRef(active);
  useEffect(() => {
    activeRef.current = active;
  }, [active]);

  // The element that had focus when the tour opened (Help button, command
  // palette item, or whatever was focused at auto-start). Restored on any
  // exit so focus doesn't collapse to <body> after the overlay unmounts.
  const lastFocusedRef = useRef<HTMLElement | null>(null);

  const captureFocus = useCallback(() => {
    const el = document.activeElement;
    lastFocusedRef.current = el instanceof HTMLElement ? el : null;
  }, []);

  const restoreFocus = useCallback(() => {
    const el = lastFocusedRef.current;
    lastFocusedRef.current = null;
    if (!el) return;
    // Defer past the overlay unmount, then restore only if the element is
    // still connected and focusable — else no-op (never throw).
    requestAnimationFrame(() => {
      if (el.isConnected && typeof el.focus === "function") {
        el.focus();
      }
    });
  }, []);

  const startTour = useCallback(() => {
    // Manual entry ignores the width gate + the completed flag (replay).
    captureFocus();
    setStepIndex(0);
    setActive(true);
  }, [captureFocus]);

  const endTour = useCallback(
    (completed: boolean) => {
      setActive(false);
      setStepIndex(0);
      if (completed) markCompleted();
      restoreFocus();
    },
    [restoreFocus],
  );

  const next = useCallback(() => {
    setStepIndex((i) => Math.min(TOUR_STEPS.length - 1, i + 1));
  }, []);

  const back = useCallback(() => {
    setStepIndex((i) => Math.max(0, i - 1));
  }, []);

  const goTo = useCallback((index: number) => {
    setStepIndex(() =>
      Math.max(0, Math.min(TOUR_STEPS.length - 1, Math.floor(index))),
    );
  }, []);

  // Auto-start: fire at most once on a real mount, ~600ms after mount, only
  // when the flag is absent and the desktop rail is present (>= 1024px).
  // A `mounted` generation guard + clearTimeout in cleanup means React 18
  // StrictMode's mount/cleanup/mount double-invoke reschedules cleanly (the
  // first schedule is cancelled, the second fires once) and no setter lands
  // on an unmounted fiber. The activeRef re-guard still lets a manual start
  // win the race.
  useEffect(() => {
    if (typeof window === "undefined") return;
    if (hasCompleted()) return;
    if (window.innerWidth < AUTO_START_MIN_WIDTH) return;
    let mounted = true;
    const id = window.setTimeout(() => {
      if (!mounted) return;
      if (activeRef.current) return; // a manual start already opened it
      if (hasCompleted()) return; // finished/skipped in the interim
      captureFocus();
      setStepIndex(0);
      setActive(true);
    }, AUTO_START_DELAY_MS);
    return () => {
      mounted = false;
      window.clearTimeout(id);
    };
  }, [captureFocus]);

  const value = useMemo<TourContextValue>(
    () => ({
      active,
      stepIndex,
      steps: TOUR_STEPS,
      startTour,
      endTour,
      next,
      back,
      goTo,
    }),
    [active, stepIndex, startTour, endTour, next, back, goTo],
  );

  return (
    <TourContext.Provider value={value}>
      {children}
      {active && (
        <Suspense fallback={null}>
          <TourOverlay />
        </Suspense>
      )}
    </TourContext.Provider>
  );
}
