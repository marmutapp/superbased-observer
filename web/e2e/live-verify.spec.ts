import { test } from "@playwright/test";
import fs from "node:fs";

// TEMPORARY live-dashboard visual-verification capture (2026-07-31 UI
// verify pass). Drives the LIVE :8081 daemon with the tour dismissed
// and long settles (the live 15GB corpus needs seconds per page).
// One run also mocks /api/announcements to visually verify the
// AnnouncementBanner component (the live endpoint is legitimately
// empty). Delete after the pass — never commit.

const OUTDIR = process.env.OUTDIR || "/tmp/uiverify2";
fs.mkdirSync(OUTDIR, { recursive: true });

test.use({ viewport: { width: 1440, height: 900 } });

test.beforeEach(async ({ page }) => {
  await page.addInitScript(() => {
    try {
      localStorage.setItem("sb_tour_completed", "1");
      localStorage.setItem("sb_announce_ack", "{}");
    } catch {}
  });
});

test("overview with data", async ({ page }) => {
  await page.goto("/", { waitUntil: "domcontentloaded" });
  await page.waitForTimeout(12_000);
  await page.screenshot({ path: `${OUTDIR}/overview.png`, fullPage: false });
});

test("banner mocked", async ({ page }) => {
  await page.route("**/api/announcements", (route) =>
    route.fulfill({
      contentType: "application/json",
      body: JSON.stringify({
        announcements: [
          {
            id: "verify-2026-07-31",
            severity: "notice",
            title: "Visual verification announcement",
            body: "This is a mocked announcement used to verify the banner renders correctly.",
            url: "https://superbased.app/",
            expires_at: "2027-01-01T00:00:00Z",
          },
        ],
      }),
    }),
  );
  await page.goto("/", { waitUntil: "domcontentloaded" });
  await page.waitForTimeout(6_000);
  await page.screenshot({ path: `${OUTDIR}/banner.png`, fullPage: false });
});

test("banner critical mocked", async ({ page }) => {
  await page.route("**/api/announcements", (route) =>
    route.fulfill({
      contentType: "application/json",
      body: JSON.stringify({
        announcements: [
          {
            id: "verify-crit-2026-07-31",
            severity: "critical",
            title: "Critical severity check",
            body: "Critical-severity banner styling verification.",
            expires_at: "2027-01-01T00:00:00Z",
          },
        ],
      }),
    }),
  );
  await page.goto("/", { waitUntil: "domcontentloaded" });
  await page.waitForTimeout(6_000);
  await page.screenshot({ path: `${OUTDIR}/banner-critical.png` });
});

test("sessions with detail panel (tags UI)", async ({ page }) => {
  await page.goto("/sessions", { waitUntil: "domcontentloaded" });
  await page.waitForTimeout(12_000);
  await page.screenshot({ path: `${OUTDIR}/sessions.png` });
  // Open the first session row to expose the detail panel with the
  // tags / favorites / notes classification UI.
  const row = page.locator("table tbody tr").first();
  if (await row.count()) {
    await row.click();
    await page.waitForTimeout(8_000);
    await page.screenshot({ path: `${OUTDIR}/session-detail.png` });
  }
});

test("tools coverage", async ({ page }) => {
  await page.goto("/tools", { waitUntil: "domcontentloaded" });
  await page.waitForTimeout(12_000);
  await page.screenshot({ path: `${OUTDIR}/tools.png` });
  await page.mouse.wheel(0, 1400);
  await page.waitForTimeout(1_000);
  await page.screenshot({ path: `${OUTDIR}/tools-scrolled.png` });
});
