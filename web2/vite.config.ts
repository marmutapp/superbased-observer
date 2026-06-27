import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import path from "node:path";

// The org dashboard is served at root by the observer-org binary
// (internal/orgserver/dashboard/webapp). In `vite dev` /api is
// proxied to a locally running org server (TLS terminated upstream
// in prod; plain http in dev).
export default defineConfig({
  base: "/",
  plugins: [react()],
  resolve: {
    alias: { "@": path.resolve(__dirname, "src") },
  },
  server: {
    port: 5175,
    proxy: { "/api": "http://localhost:8443" },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    sourcemap: true,
    rollupOptions: {
      output: {
        // Peel heavy vendors out of the entry bundle (same gotcha as
        // web/: clsx must be pinned or Rollup sweeps it into the
        // recharts chunk, forcing eager preload).
        manualChunks(id) {
          if (id.includes("node_modules/recharts/")) return "vendor-recharts";
          if (id.includes("node_modules/d3-")) return "vendor-recharts";
          if (id.includes("node_modules/victory-vendor/")) return "vendor-recharts";
          if (id.includes("node_modules/@tanstack/react-table"))
            return "vendor-table";
          if (id.includes("node_modules/react-router")) return "vendor-router";
          if (
            id.includes("node_modules/react/") ||
            id.includes("node_modules/react-dom/") ||
            id.includes("node_modules/scheduler/") ||
            id.includes("node_modules/clsx/")
          )
            return "vendor-react";
          return undefined;
        },
      },
    },
  },
});
