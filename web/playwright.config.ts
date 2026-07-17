import { defineConfig } from "@playwright/test";

// Mobile-view verification harness (docs/plans mobile-view pass).
// Drives an already-running demo-mode dashboard (see e2e/README.md);
// BASE_URL points at the isolated demo instance, default :8092.
// NOT part of the app build — vite/tsc never see this dir.
export default defineConfig({
  testDir: "./e2e",
  timeout: 60_000,
  fullyParallel: false,
  workers: 1,
  reporter: [["list"]],
  use: {
    baseURL: process.env.BASE_URL || "http://localhost:8092",
    // Chromium is already installed under ~/.cache/ms-playwright.
    headless: true,
  },
});
