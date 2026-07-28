import os from "node:os";
import { test, expect, type Page } from "@playwright/test";

// Verifies the "paired device lost its session" UX: when a REMOTE-paired
// device's device-session cookie is gone, every API call — including plain
// READS — 401s. The old client only ran auth recovery for unsafe methods, so a
// read-only 401 surfaced as a raw `api 401 /api/...` string inside a rendered,
// empty dashboard. The fix detects the loss centrally in web/src/lib/api.ts,
// confirms it with ONE /api/remote/whoami probe, and renders a single
// full-screen re-pair prompt.
//
// HOW TO RUN. These specs assert on source that is not in the embedded bundle
// until the orchestrator restages it, and the remote cases need a NON-LOOPBACK
// page host (isRemoteView() keys off exactly that, and the Go dashboard's
// browserGuard refuses a non-loopback Host by design). So point BASE_URL at the
// Vite dev server, bound to all interfaces — no backend is needed because every
// /api call is mocked here:
//
//   cd web && npx vite --port 5175 --strictPort --host 0.0.0.0 &
//   BASE_URL=http://127.0.0.1:5175 npx playwright test e2e/remote-auth-expired.spec.ts

const PHONE = { width: 393, height: 852 };

test.use({ viewport: PHONE });

// lanOrigin resolves a NON-LOOPBACK origin for the same dev server, so
// isRemoteView() reports true. Discovered from the host's own interfaces rather
// than hardcoded (same idiom as mobile-terminal-keys.spec.ts).
function lanOrigin(baseURL: string): string {
  const u = new URL(baseURL);
  for (const list of Object.values(os.networkInterfaces())) {
    for (const ni of list ?? []) {
      if (ni.family === "IPv4" && !ni.internal) {
        return `http://${ni.address}:${u.port}`;
      }
    }
  }
  throw new Error("no non-loopback IPv4 interface — cannot simulate a remote seat");
}

// mockAuth makes EVERY /api call 401 and pins what the whoami probe answers.
// `authenticated: false` is the confirmed-loss case; `true` is the "one unlucky
// 401" case, where the prompt must stay away.
async function mockAuth(page: Page, opts: { whoamiAuthenticated: boolean }) {
  await page.addInitScript(() => {
    try {
      // The first-run tour's full-viewport overlay would sit on top of the
      // prompt (same suppression every sibling spec uses).
      localStorage.setItem("sb_tour_completed", "1");
    } catch {
      /* private mode — ignore */
    }
  });
  await page.route("**/api/**", (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/remote/whoami") {
      return route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          authenticated: opts.whoamiAuthenticated,
          capability: opts.whoamiAuthenticated ? "view" : "public",
          csrf: opts.whoamiAuthenticated ? "csrf-token-abc" : "",
        }),
      });
    }
    return route.fulfill({
      status: 401,
      contentType: "text/plain",
      body: "unauthorized: authentication required",
    });
  });
}

const PROMPT = /Session expired — pair this device again/i;

test("remote device with a confirmed-dead session gets the full-screen re-pair prompt", async ({
  page,
  baseURL,
}) => {
  await mockAuth(page, { whoamiAuthenticated: false });
  await page.goto(`${lanOrigin(baseURL!)}/`, { waitUntil: "domcontentloaded" });

  await expect(page.getByText(PROMPT)).toBeVisible({ timeout: 15000 });
  // ONE prompt, not one per failed call.
  await expect(page.getByText(PROMPT)).toHaveCount(1);
  // It REPLACES the app — no half-rendered dashboard behind it, and none of
  // the raw `api 401 …` strings the operator actually saw.
  await expect(page.locator("nav")).toHaveCount(0);
  await expect(page.getByText(/api 401/i)).toHaveCount(0);
  await expect(page.getByRole("button", { name: /Check again/i })).toBeVisible();
  await page.screenshot({ path: "e2e/shots/remote/session-expired.png", fullPage: true });
});

test("a one-off 401 that whoami does not confirm never shows the prompt", async ({
  page,
  baseURL,
}) => {
  await mockAuth(page, { whoamiAuthenticated: true });
  await page.goto(`${lanOrigin(baseURL!)}/`, { waitUntil: "domcontentloaded" });

  await page.waitForTimeout(2500);
  await expect(page.getByText(PROMPT)).toHaveCount(0);
});

test("the owner-local loopback dashboard never shows the prompt", async ({ page, baseURL }) => {
  // Same total-401 storm, but on a loopback host: isRemoteView() is false, the
  // loopback dashboard does not authenticate, and the prompt must be
  // structurally unreachable there.
  await mockAuth(page, { whoamiAuthenticated: false });
  await page.goto(`${baseURL}/`, { waitUntil: "domcontentloaded" });

  await page.waitForTimeout(2500);
  await expect(page.getByText(PROMPT)).toHaveCount(0);
});
