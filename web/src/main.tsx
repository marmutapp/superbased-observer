import React from "react";
import ReactDOM from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import App from "./App";
import { ThemeProvider } from "./lib/theme";
import { LaunchDockProvider } from "./components/LaunchDock";
import { RemotePairingGate } from "./components/RemotePairing";
import "./index.css";

// Input-event trace overlay (lib/sttdebug.ts) — the diagnostic that cracked
// the 2026-07-23 dictation-paste bug (see docs/session-handoff.md "STT /
// dictation into terminals"). DEV BUILDS ONLY, and even there a no-op unless
// ?sttdebug=1 or localStorage sbo_sttdebug=1. The import.meta.env.DEV guard
// compile-gates it: the dynamic import is tree-shaken out of production
// bundles, so the shipped dashboard carries no input-capture surface
// (codex review 2026-07-23, finding 1).
if (import.meta.env.DEV) {
  import("./lib/sttdebug").then((m) => m.initSttDebug());
}

// Stale-preload auto-recovery. After `make web-build` ships a new
// bundle, every open dashboard tab still references the prior
// chunk hashes via its cached index.html — any lazy-imported route
// (Settings / Actions / ...) then throws
// "Failed to fetch dynamically imported module" on first visit.
// Vite dispatches a `vite:preloadError` event for exactly this case.
// We reload once per tab so the new index.html lands; the
// sessionStorage guard breaks out of any loop where the chunk is
// genuinely missing (server-side build error, not a stale tab).
window.addEventListener("vite:preloadError", () => {
  if (!sessionStorage.getItem("sb:stale-preload-reload")) {
    sessionStorage.setItem("sb:stale-preload-reload", "1");
    location.reload();
  }
});

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <ThemeProvider>
      <BrowserRouter>
        <LaunchDockProvider>
          <RemotePairingGate>
            <App />
          </RemotePairingGate>
        </LaunchDockProvider>
      </BrowserRouter>
    </ThemeProvider>
  </React.StrictMode>,
);
