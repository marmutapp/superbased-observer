import React from "react";
import ReactDOM from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import App from "./App";
import { ThemeProvider } from "./lib/theme";
import "./index.css";

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
        <App />
      </BrowserRouter>
    </ThemeProvider>
  </React.StrictMode>,
);
