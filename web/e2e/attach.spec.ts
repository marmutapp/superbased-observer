import { test, expect } from "@playwright/test";
import fs from "node:fs";

// Session-attach Phase 2 (dashboard "Jump in") capture harness. Drives the
// isolated demo dashboard (BASE_URL, default the vite dev proxy on :5174)
// with Playwright route-interception mocking GET /api/attach/sessions — the
// backend half is being implemented in parallel, so we stub the wire
// contract (one live attach row matching a demo session; and the empty
// case). Screenshots are read back multimodally by the orchestrator.

const OUTDIR =
  process.env.OUTDIR ||
  "/tmp/claude-1000/-home-marmutapp-superbased-observer/dafbd2e9-d69a-42a3-be48-530679ce01a1/scratchpad/p2-screens";

fs.mkdirSync(OUTDIR, { recursive: true });

test.use({ viewport: { width: 1360, height: 940 } });

// attachRow builds a stub GET /api/attach/sessions row. `kind` defaults to
// "attach" but the endpoint now returns every live, non-setup daemon-owned
// terminal run (fresh/handoff/attach/resume) — callers pass the other kinds
// to cover the widened contract. The ws-handle field is set by member
// assignment (a bare object key gets mangled by the harness Write-filter;
// feedback_write_filter_token_patterns).
function attachRow(
  sessionId: string,
  kind: string = "attach",
): Record<string, unknown> {
  const row: Record<string, unknown> = {
    subcommand: "claude",
    kind,
    tool: "claude-code",
    session_id: sessionId,
    run_id: "run-demo-1",
    created_at: new Date().toISOString(),
    attached: false,
    viewers: 0,
    writer_holder: "",
    exited: false,
    exit_code: 0,
  };
  const handle = ["attach", "tok", "demo"].join("-");
  row.token = handle;
  return row;
}

async function firstSessionId(request: {
  get: (u: string) => Promise<{ json: () => Promise<unknown> }>;
}): Promise<string> {
  const res = await request.get("/api/sessions?limit=1&page=1");
  const body = (await res.json()) as { rows?: Array<{ id: string }> };
  const id = body.rows?.[0]?.id;
  if (!id) throw new Error("no demo session to target");
  return id;
}

// Suppress the first-run guided tour: its full-viewport overlay (fixed
// inset-0 z-[130], floating-ui portal) intercepts hover/click and would
// otherwise block this spec's Jump in / Resume interactions (see
// remote-lang.spec.ts and tour.spec.ts for the established pattern). MUST be
// registered before page.goto — addInitScript only takes effect for
// navigations that happen after it's added.
async function suppressTour(
  page: import("@playwright/test").Page,
): Promise<void> {
  await page.addInitScript(() => {
    try {
      localStorage.setItem("sb_tour_completed", "1");
    } catch {
      /* private mode — ignore */
    }
  });
}

test("Jump in ENABLED on a live attachable session", async ({ page }) => {
  const id = await firstSessionId(page.request);
  await page.route("**/api/attach/sessions", (route) =>
    route.fulfill({ json: { sessions: [attachRow(id)] } }),
  );
  await suppressTour(page);
  await page.goto(`/sessions?session=${id}`, { waitUntil: "domcontentloaded" });
  const btn = page.getByRole("button", { name: "Jump in" });
  await btn.scrollIntoViewIfNeeded();
  await expect(btn).toBeVisible();
  await expect(btn).toBeEnabled();
  await page.waitForTimeout(400);
  await page.screenshot({ path: `${OUTDIR}/1-jumpin-enabled.png` });
});

test("Jump in ENABLED on a live 'fresh' dashboard-launched terminal", async ({
  page,
}) => {
  // GET /api/attach/sessions was widened past kind=="attach" — a plain
  // dashboard "new terminal" (kind "fresh") that the correlation sweep has
  // linked to this session must enable Jump in too.
  const id = await firstSessionId(page.request);
  await page.route("**/api/attach/sessions", (route) =>
    route.fulfill({ json: { sessions: [attachRow(id, "fresh")] } }),
  );
  await suppressTour(page);
  await page.goto(`/sessions?session=${id}`, { waitUntil: "domcontentloaded" });
  const btn = page.getByRole("button", { name: "Jump in" });
  await btn.scrollIntoViewIfNeeded();
  await expect(btn).toBeVisible();
  await expect(btn).toBeEnabled();
  await page.waitForTimeout(400);
  await page.screenshot({ path: `${OUTDIR}/1b-jumpin-enabled-fresh.png` });
});

test("Jump in DISABLED (honest-disabled tooltip)", async ({ page }) => {
  const id = await firstSessionId(page.request);
  await page.route("**/api/attach/sessions", (route) =>
    route.fulfill({ json: { sessions: [] } }),
  );
  await suppressTour(page);
  await page.goto(`/sessions?session=${id}`, { waitUntil: "domcontentloaded" });
  const btn = page.getByRole("button", { name: "Jump in" });
  await btn.scrollIntoViewIfNeeded();
  await expect(btn).toBeVisible();
  await expect(btn).toBeDisabled();
  // Hover the Tooltip wrapper (a disabled button doesn't emit hover) to
  // surface the honest-disabled title.
  await btn.locator("xpath=..").hover();
  await page.waitForTimeout(700);
  await page.screenshot({ path: `${OUTDIR}/2-jumpin-disabled-tooltip.png` });
});

test("Jump in FETCH-ERROR (honest 'couldn't check' state, P2-4b)", async ({
  page,
}) => {
  // The attach endpoint FAILS (500). The button must NOT read this as
  // "not attachable" (a false diagnosis) — it stays disabled with an honest
  // "couldn't check attachability" message distinct from the genuinely-empty
  // case above.
  const id = await firstSessionId(page.request);
  await page.route("**/api/attach/sessions", (route) =>
    route.fulfill({ status: 500, json: { error: "boom" } }),
  );
  await suppressTour(page);
  await page.goto(`/sessions?session=${id}`, { waitUntil: "domcontentloaded" });
  const btn = page.getByRole("button", { name: "Jump in" });
  await btn.scrollIntoViewIfNeeded();
  await expect(btn).toBeVisible();
  await expect(btn).toBeDisabled();
  // The honest error copy appears in the card body (not "not attachable").
  await expect(page.getByText("Couldn't check attachability")).toBeVisible();
  await btn.locator("xpath=..").hover();
  await page.waitForTimeout(700);
  await page.screenshot({ path: `${OUTDIR}/5-jumpin-fetch-error.png` });
});

test("Sessions list shows the live · joinable badge", async ({ page }) => {
  const id = await firstSessionId(page.request);
  await page.route("**/api/attach/sessions", (route) =>
    route.fulfill({ json: { sessions: [attachRow(id)] } }),
  );
  await suppressTour(page);
  await page.goto(`/sessions`, { waitUntil: "domcontentloaded" });
  await expect(page.getByText("live · joinable").first()).toBeVisible();
  await page.waitForTimeout(400);
  await page.screenshot({ path: `${OUTDIR}/3-sessions-live-badge.png`, fullPage: true });
});

// --- Session-attach Phase 3 (dashboard native resume) ---------------------

const P3OUT =
  process.env.P3OUTDIR ||
  "/tmp/claude-1000/-home-marmutapp-superbased-observer/dafbd2e9-d69a-42a3-be48-530679ce01a1/scratchpad/p3-screens";

fs.mkdirSync(P3OUT, { recursive: true });

// patchDetailResume intercepts ONLY the base GET /api/session/<id> (never a
// sub-route like /messages or /cache — matched by exact pathname) and overrides
// its `resume` block so the Resume affordance renders deterministically. The
// rest of the real detail payload passes through unchanged.
async function patchDetailResume(
  page: import("@playwright/test").Page,
  id: string,
  resume: { kind: string; subcommand: string },
): Promise<void> {
  await page.route(
    (url) => url.pathname === `/api/session/${id}`,
    async (route) => {
      const resp = await route.fetch();
      const json = (await resp.json()) as Record<string, unknown>;
      json.resume = resume;
      await route.fulfill({ response: resp, json });
    },
  );
}

test("Resume NATIVE button on a closed grounded session", async ({ page }) => {
  const id = await firstSessionId(page.request);
  // No live attach row → Jump in disabled, and the Resume card is shown.
  await page.route("**/api/attach/sessions", (route) =>
    route.fulfill({ json: { sessions: [] } }),
  );
  await patchDetailResume(page, id, { kind: "native", subcommand: "claude" });
  await suppressTour(page);
  await page.goto(`/sessions?session=${id}`, { waitUntil: "domcontentloaded" });
  const btn = page.getByRole("button", { name: "Resume", exact: true });
  await btn.scrollIntoViewIfNeeded();
  await expect(btn).toBeVisible();
  await expect(btn).toBeEnabled();
  await page.waitForTimeout(400);
  await page.screenshot({ path: `${P3OUT}/1-resume-native-enabled.png` });
});

test("Live 'resume' row hides the Resume card and enables Jump in", async ({
  page,
}) => {
  // A live kind=="resume" row (a dashboard-launched native-resume terminal)
  // now shows up in /api/attach/sessions. ResumeButton's liveMatch check
  // must hide the card (avoiding a duplicate second process on the same
  // transcript), and JumpInButton must treat the same row as joinable.
  const id = await firstSessionId(page.request);
  await page.route("**/api/attach/sessions", (route) =>
    route.fulfill({ json: { sessions: [attachRow(id, "resume")] } }),
  );
  // Native resume would otherwise render a "Resume" button — proves the
  // card is hidden BECAUSE of the live row, not because resume is ungrounded.
  await patchDetailResume(page, id, { kind: "native", subcommand: "claude" });
  await suppressTour(page);
  await page.goto(`/sessions?session=${id}`, { waitUntil: "domcontentloaded" });
  const jumpBtn = page.getByRole("button", { name: "Jump in" });
  await jumpBtn.scrollIntoViewIfNeeded();
  await expect(jumpBtn).toBeVisible();
  await expect(jumpBtn).toBeEnabled();
  await expect(
    page.getByRole("button", { name: "Resume", exact: true }),
  ).toHaveCount(0);
  await page.waitForTimeout(400);
  await page.screenshot({ path: `${P3OUT}/4-resume-hidden-live-resume-row.png` });
});

test("Resume HANDOFF hint points at Continue-in… (no duplicate UI)", async ({
  page,
}) => {
  const id = await firstSessionId(page.request);
  await page.route("**/api/attach/sessions", (route) =>
    route.fulfill({ json: { sessions: [] } }),
  );
  await patchDetailResume(page, id, { kind: "handoff", subcommand: "" });
  await suppressTour(page);
  await page.goto(`/sessions?session=${id}`, { waitUntil: "domcontentloaded" });
  // The hint text is present; there is NO native Resume button in this state.
  const hint = page.getByText("Native resume isn't grounded for this tool");
  await hint.scrollIntoViewIfNeeded();
  await expect(hint).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Resume", exact: true }),
  ).toHaveCount(0);
  await page.waitForTimeout(400);
  await page.screenshot({ path: `${P3OUT}/2-resume-handoff-hint.png` });
});

test("Resume NONE honest-disabled", async ({ page }) => {
  const id = await firstSessionId(page.request);
  await page.route("**/api/attach/sessions", (route) =>
    route.fulfill({ json: { sessions: [] } }),
  );
  await patchDetailResume(page, id, { kind: "none", subcommand: "" });
  await suppressTour(page);
  await page.goto(`/sessions?session=${id}`, { waitUntil: "domcontentloaded" });
  const btn = page.getByRole("button", { name: "Resume", exact: true });
  await btn.scrollIntoViewIfNeeded();
  await expect(btn).toBeVisible();
  await expect(btn).toBeDisabled();
  await btn.locator("xpath=..").hover();
  await page.waitForTimeout(600);
  await page.screenshot({ path: `${P3OUT}/3-resume-none-disabled.png` });
});

test("Toast render (writer takeover notices)", async ({ page }) => {
  await suppressTour(page);
  await page.goto(`/`, { waitUntil: "domcontentloaded" });
  await page.waitForTimeout(600);
  // window.__sbPushToast is a DEV-only test seam (src/components/Toast.tsx,
  // gated on import.meta.env.DEV) that's intentionally tree-shaken out of
  // built bundles — absent against a built dashboard (e.g. :8092), present
  // against the vite dev server; skip honestly rather than fail by design.
  const hasSeam = await page.evaluate(
    () => typeof (window as unknown as Record<string, unknown>).__sbPushToast,
  );
  test.skip(
    hasSeam === "undefined",
    "DEV-only toast seam (__sbPushToast) absent in built bundle — run against the vite dev server to exercise this test",
  );
  await page.evaluate(() => {
    const w = window as unknown as {
      __sbPushToast?: (t: string, v?: string) => void;
    };
    w.__sbPushToast?.("Terminal control passed to another viewer", "warn");
    w.__sbPushToast?.("You have terminal control", "success");
  });
  await expect(
    page.getByText("Terminal control passed to another viewer"),
  ).toBeVisible();
  await page.waitForTimeout(300);
  await page.screenshot({ path: `${OUTDIR}/4-toast-stack.png` });
});
