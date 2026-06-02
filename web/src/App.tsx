import { lazy, Suspense, useCallback, useEffect, useState } from "react";
import { Navigate, Route, Routes, useLocation } from "react-router-dom";
import { AnimatePresence, motion } from "framer-motion";
import { Sidebar } from "@/components/Sidebar";
import { TopBar } from "@/components/TopBar";
import { FilterBar } from "@/components/FilterBar";
import { TweaksPill } from "@/components/TweaksPill";
import { CommandPalette } from "@/components/CommandPalette";
import { FilterProvider } from "@/lib/filters";

// HelpDrawer carries the 164-entry registry — defer until first
// open so it doesn't bloat the shell chunk.
const HelpDrawer = lazy(() =>
  import("@/components/HelpDrawer").then((m) => ({ default: m.HelpDrawer })),
);

// Lazy per-route — keeps recharts/tanstack-table chunks off the
// critical path. Pages export named components, so each import
// re-maps the named export onto `default` for React.lazy.
const OverviewPage = lazy(() =>
  import("@/pages/Overview").then((m) => ({ default: m.OverviewPage })),
);
const CostPage = lazy(() =>
  import("@/pages/Cost").then((m) => ({ default: m.CostPage })),
);
const AnalysisPage = lazy(() =>
  import("@/pages/Analysis").then((m) => ({ default: m.AnalysisPage })),
);
const SessionsPage = lazy(() =>
  import("@/pages/Sessions").then((m) => ({ default: m.SessionsPage })),
);
const ActionsPage = lazy(() =>
  import("@/pages/Actions").then((m) => ({ default: m.ActionsPage })),
);
const ToolsPage = lazy(() =>
  import("@/pages/Tools").then((m) => ({ default: m.ToolsPage })),
);
const CompressionPage = lazy(() =>
  import("@/pages/Compression").then((m) => ({ default: m.CompressionPage })),
);
const DiscoveryPage = lazy(() =>
  import("@/pages/Discovery").then((m) => ({ default: m.DiscoveryPage })),
);
const PatternsPage = lazy(() =>
  import("@/pages/Patterns").then((m) => ({ default: m.PatternsPage })),
);
const SettingsPage = lazy(() =>
  import("@/pages/Settings").then((m) => ({ default: m.SettingsPage })),
);

function RouteFallback() {
  return (
    <div className="flex h-full items-center justify-center p-12">
      <div className="flex items-center gap-3 text-[12px] text-fg-3">
        <span className="inline-block h-3 w-3 animate-spin rounded-full border border-line-3 border-t-accent" />
        Loading…
      </div>
    </div>
  );
}

// AnimatePresence keyed on pathname so a route change crossfades
// instead of snapping. mode="wait" lets the outgoing page fade
// out before the incoming page mounts — avoids stacked layouts
// during the swap.
function AnimatedRoutes() {
  const location = useLocation();
  return (
    <AnimatePresence mode="wait" initial={false}>
      <motion.div
        key={location.pathname}
        initial={{ opacity: 0, y: 4 }}
        animate={{ opacity: 1, y: 0 }}
        exit={{ opacity: 0, y: -2 }}
        transition={{ duration: 0.14, ease: "easeOut" }}
        className="h-full"
      >
        <Routes location={location}>
          <Route index element={<OverviewPage />} />
          <Route path="cost" element={<CostPage />} />
          <Route path="analysis" element={<AnalysisPage />} />
          <Route path="sessions" element={<SessionsPage />} />
          <Route path="actions" element={<ActionsPage />} />
          <Route path="tools" element={<ToolsPage />} />
          <Route path="compression" element={<CompressionPage />} />
          <Route path="discovery" element={<DiscoveryPage />} />
          <Route path="patterns" element={<PatternsPage />} />
          <Route path="settings" element={<SettingsPage />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </motion.div>
    </AnimatePresence>
  );
}

export default function App() {
  const [helpOpen, setHelpOpen] = useState(false);
  const [helpId, setHelpId] = useState<string | null>(null);
  // Once opened, keep the drawer mounted so subsequent ? presses
  // are instant. First open pays the chunk-fetch cost.
  const [helpEverOpened, setHelpEverOpened] = useState(false);
  const [paletteOpen, setPaletteOpen] = useState(false);
  useEffect(() => {
    if (helpOpen && !helpEverOpened) setHelpEverOpened(true);
  }, [helpOpen, helpEverOpened]);

  // Keyboard shortcuts global to the app shell. `?` toggles help when
  // no input is focused; ⌘K / Ctrl-K toggles the command palette
  // (works even when a field is focused — matches VSCode/Linear).
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      const t = e.target as HTMLElement | null;
      const tag = (t?.tagName || "").toLowerCase();
      const isInput =
        tag === "input" || tag === "textarea" || t?.isContentEditable;
      if ((e.key === "k" || e.key === "K") && (e.metaKey || e.ctrlKey)) {
        e.preventDefault();
        setPaletteOpen((o) => !o);
        return;
      }
      if (isInput) return;
      if (e.key === "?") {
        e.preventDefault();
        setHelpOpen((o) => !o);
      }
    }
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, []);

  // Clicking any HelpInd (data-help-id) opens the drawer scrolled to
  // that entry. One delegated listener keeps the indicator
  // lightweight and avoids prop-drilling.
  useEffect(() => {
    function onClick(e: MouseEvent) {
      const el = (e.target as HTMLElement | null)?.closest<HTMLElement>(
        "[data-help-id]",
      );
      if (!el) return;
      const id = el.getAttribute("data-help-id");
      if (id) {
        setHelpId(id);
        setHelpOpen(true);
      }
    }
    document.addEventListener("click", onClick);
    return () => document.removeEventListener("click", onClick);
  }, []);

  const openHelp = useCallback(() => setHelpOpen(true), []);

  return (
    <FilterProvider>
      <div className="flex h-full w-full bg-bg-0 text-fg-1">
        <Sidebar />
        <main className="flex min-w-0 flex-1 flex-col">
          <TopBar onHelp={openHelp} />
          <FilterBar onOpenPalette={() => setPaletteOpen(true)} />
          <div className="min-h-0 flex-1 overflow-y-auto">
            <Suspense fallback={<RouteFallback />}>
              <AnimatedRoutes />
            </Suspense>
          </div>
        </main>
        {helpEverOpened && (
          <Suspense fallback={null}>
            <HelpDrawer
              open={helpOpen}
              onClose={() => setHelpOpen(false)}
              initialId={helpId}
            />
          </Suspense>
        )}
        <TweaksPill />
        <CommandPalette
          open={paletteOpen}
          onClose={() => setPaletteOpen(false)}
        />
      </div>
    </FilterProvider>
  );
}
