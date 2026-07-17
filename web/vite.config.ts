import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import path from "node:path";

export default defineConfig({
  // Phase 8 cutover: dashboard now mounts at root. Phase 1–7 ran
  // the app under /v2/ as a coexistence path while the legacy
  // static SPA stayed at /. Both are now served from /.
  base: "/",
  plugins: [react()],
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "src"),
    },
  },
  server: {
    port: 5174,
    proxy: {
      // Dev API target. Defaults to the standard daemon on :8820;
      // override (e.g. for the mobile-view demo instance on :8092)
      // with VITE_API_PROXY. Runtime output is unaffected — this is
      // dev-server-only.
      "/api": process.env.VITE_API_PROXY || "http://localhost:8820",
    },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    sourcemap: true,
    rollupOptions: {
      output: {
        // Peel large vendor libs out of the entry bundle so the
        // 500 KB warning goes away and the browser can cache them
        // independently of app code. Recharts is the heaviest at
        // ~150 KB; TanStack Table ~50 KB; React Router ~30 KB.
        // d3-* deps that Recharts pulls in (d3-shape, d3-scale,
        // victory-vendor) land in the recharts chunk via prefix.
        manualChunks(id) {
          if (id.includes("node_modules/recharts/")) return "vendor-recharts";
          if (id.includes("node_modules/d3-")) return "vendor-recharts";
          if (id.includes("node_modules/victory-vendor/")) return "vendor-recharts";
          if (id.includes("node_modules/@tanstack/react-table"))
            return "vendor-table";
          if (id.includes("node_modules/react-router"))
            return "vendor-router";
          if (id.includes("node_modules/framer-motion"))
            return "vendor-framer";
          if (id.includes("node_modules/motion-")) return "vendor-framer";
          // @floating-ui/* powers the Tooltip primitive — positioning,
          // flip/shift/offset, arrow placement. Pinning to its own
          // chunk keeps Rollup from sweeping it into vendor-recharts
          // (which would force eager preload of ~110 KB just to
          // render a hover bubble). Same gotcha as clsx — see the
          // [[feedback_vite_manualchunks_clsx]] memory entry.
          if (id.includes("node_modules/@floating-ui/"))
            return "vendor-floating-ui";
          // @xterm/* powers the embedded web terminal (Continue-in… →
          // "Launch here"). It is dynamic-imported from LaunchTerminal so
          // this chunk stays lazy — nobody who never opens a terminal pays
          // for it. Pinning keeps Rollup from folding it into an eager
          // vendor bundle (same lesson as clsx/@floating-ui).
          if (id.includes("node_modules/@xterm/")) return "vendor-xterm";
          if (
            id.includes("node_modules/react/") ||
            id.includes("node_modules/react-dom/") ||
            id.includes("node_modules/scheduler/") ||
            // clsx is tiny but used everywhere; pinning it to the
            // react chunk keeps Rollup from sweeping it into
            // vendor-recharts (which would force eager preload).
            id.includes("node_modules/clsx/")
          )
            return "vendor-react";
          return undefined;
        },
      },
    },
  },
});
