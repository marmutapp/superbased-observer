import { test, expect } from "@playwright/test";

// Settings → Health: the remote-projection notice on the doctor card.
//
// GET /api/health/doctor is capability VIEW, so a paired remote device can
// reach it; for such a caller the daemon rewrites filesystem roots and user
// names to placeholders and sets `local_detail_withheld`. This pins the three
// states the field can arrive in — true, false, and ABSENT (the Go field
// carries `omitempty`, and a daemon older than the feature never sends it at
// all), because only the first may draw the notice.
//
// Route-interception mocks the whole wire so the run is self-contained: a
// catch-all /api/** stub is registered FIRST and the doctor stub second
// (Playwright matches the most recently registered route first), which keeps
// the run off the real daemon entirely.

test.use({ viewport: { width: 1280, height: 900 } });

const NOTICE = '[data-testid="doctor-redaction-notice"]';

// doctorReport builds a two-check report. `withheld` is spread in rather than
// assigned so `undefined` produces a body with NO local_detail_withheld key —
// the older-daemon shape — instead of an explicit null.
function doctorReport(withheld: boolean | undefined): Record<string, unknown> {
  return {
    checks: [
      {
        name: "db.integrity",
        status: "ok",
        message: "integrity_check ok",
        details: ["~/.observer/observer.db"],
      },
      {
        name: "hooks.checksums",
        status: "warn",
        message: "1 hook checksum is stale",
        details: ["<config> lists a hook the binary no longer matches"],
      },
    ],
    ok: 1,
    warn: 1,
    fail: 0,
    all_ok: false,
    generated_at: "2026-07-27T10:00:00Z",
    ...(withheld === undefined ? {} : { local_detail_withheld: withheld }),
  };
}

async function openHealth(
  page: import("@playwright/test").Page,
  withheld: boolean | undefined,
) {
  // Catch-all first, so nothing reaches the live daemon and no card hangs on a
  // pending request. It answers 503 rather than `{}`: an empty OBJECT is a
  // successful response of the wrong shape, and the shell's own panels then
  // dereference fields that aren't there (the sidebar crashes the render tree
  // on `live_sessions`). A failure status routes through each card's normal
  // error path instead, which is what an unmocked endpoint should look like.
  await page.route("**/api/**", (route) =>
    route.fulfill({
      status: 503,
      contentType: "application/json",
      body: JSON.stringify({ error: "not mocked in this spec" }),
    }),
  );
  await page.route("**/api/health/doctor", (route) =>
    route.fulfill({ json: doctorReport(withheld) }),
  );
  // The first-run tour overlay covers the viewport and would swallow the
  // assertions (same suppression the sibling specs use).
  await page.addInitScript(() => {
    try {
      localStorage.setItem("sb_tour_completed", "1");
    } catch {
      /* private mode — ignore */
    }
  });
  await page.goto("/settings?section=health", { waitUntil: "domcontentloaded" });
  // The card itself must have rendered before an absence assertion means
  // anything — otherwise "no notice" passes on a blank page.
  await expect(page.getByText("integrity_check ok")).toBeVisible({
    timeout: 15000,
  });
}

test("local_detail_withheld true → the remote-projection notice is shown", async ({
  page,
}) => {
  await openHealth(page, true);
  const notice = page.locator(NOTICE);
  await expect(notice).toBeVisible();
  await expect(notice).toContainText("connected remotely");
  // The honest hedge, not a completeness claim.
  await expect(notice).toContainText("can still come through");
  // The remedy the reader can act on.
  await expect(notice).toContainText("observer doctor");
});

test("local_detail_withheld false → no notice", async ({ page }) => {
  await openHealth(page, false);
  await expect(page.locator(NOTICE)).toHaveCount(0);
});

test("local_detail_withheld absent (older daemon) → no notice", async ({
  page,
}) => {
  await openHealth(page, undefined);
  await expect(page.locator(NOTICE)).toHaveCount(0);
});
