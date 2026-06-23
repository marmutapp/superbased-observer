import React from "react";
import ReactDOM from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import App from "./App";
import { ThemeProvider } from "./lib/theme";
import { FilterProvider } from "./lib/filters";
import "./index.css";

// Stale-preload auto-recovery: after a new bundle ships, an open tab still
// references the prior chunk hashes via its cached index.html and a lazy route
// throws on first visit. Reload once per tab to pick up the new index.html.
window.addEventListener("vite:preloadError", () => {
  if (!sessionStorage.getItem("sbo-org:stale-reload")) {
    sessionStorage.setItem("sbo-org:stale-reload", "1");
    location.reload();
  }
});

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <ThemeProvider>
      <FilterProvider>
        <BrowserRouter>
          <App />
        </BrowserRouter>
      </FilterProvider>
    </ThemeProvider>
  </React.StrictMode>,
);
