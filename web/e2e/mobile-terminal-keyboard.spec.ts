import { test, expect, type Page } from "@playwright/test";
import { MobileTerminalQuery } from "../src/lib/useMediaQuery";
import { installFakeVisualViewport, KEYBOARD_PX } from "./fake-visual-viewport";

// Mobile terminal ON-SCREEN-KEYBOARD occlusion pin (defect D7, operator-
// reported 2026-07-28: "when the user types on the keypad they can't see what
// they are typing — the keypad is blocking the input section").
//
// WHY THE PREVIOUS FIX DID NOT WORK — read this before "simplifying" the app
// code back. D7 was first addressed by sizing the phone panel `h-[100dvh]`
// instead of `h-[100vh]`, on the stated reasoning that `vh` "is the LARGE
// viewport and does not shrink when the soft keyboard opens". The premise is
// wrong for BOTH units: per the CSSWG a virtual keyboard does not affect any
// viewport unit. `dvh` is dynamic with respect to retractable BROWSER UI (the
// URL bar), not the keyboard — the default `interactive-widget=resizes-visual`
// shrinks only the VISUAL viewport and leaves the layout viewport, and every
// unit derived from it, at full height. So the panel stayed full height, its
// bottom rows stayed under the keyboard, and because nothing resized, the
// terminal's ResizeObserver never refit the PTY.
//
// THE FIX, AND ITS CORRECTION. LaunchDock reads window.visualViewport — the
// one surface that DOES track the keyboard; both engines shrink it and fire
// its `resize` under the default `resizes-visual`, so one mechanism covers iOS
// and Android alike.
//
// The first attempt SHRANK the panel to the visible height. That cleared the
// keyboard but resized the PTY, and a full-screen TUI lays itself out for the
// rows it is given — Claude Code's composer collapsed to 2 lines showing only
// the tail of what you typed. It now PANS instead: the panel keeps its
// unoccluded height and slides up, so the row count never changes.
//
// A second, native mechanism (`interactive-widget=resizes-content` in
// index.html) was tried the same day and REVERTED: on Chrome/Android it broke
// touch scrolling in the mobile terminal, including with the keyboard
// dismissed. See the last test in this file.
//
// HOW THE KEYBOARD IS SIMULATED. Playwright cannot raise a real on-screen
// keyboard, so an init script replaces window.visualViewport with a scriptable
// stand-in exposing the same contract the app consumes (height, offsetTop,
// addEventListener("resize"|"scroll")). __sbKeyboard(px) shrinks it and fires
// `resize`, which is exactly the event sequence a real keyboard produces. This
// tests OUR code path — "when the visual viewport reports occlusion, does the
// panel move so the prompt is visible, without costing the TUI any rows?".
//
// Harness: mirrors mobile-terminal.spec.ts (session injected via
// GET /api/launch/sessions, socket mocked with routeWebSocket). Run against
// the isolated demo dashboard (`observer dashboard --config <scratch> --port
// 8092` + `POST /api/demo/start`), NEVER the operator's :8820.

const TOKEN = ["mobile", "kbd", "tok"].join("-");

// A realistic phone. 393x852 is the Pixel-class viewport the sibling spec
// already exercises, so a failure here is not a viewport-choice artefact.
const VIEWPORT = { width: 393, height: 852 };

// Typical portrait keyboard occlusion (iOS/Android land in the 290-360 band
// with the suggestion strip). Comfortably over the hook's 40px tolerance.

async function mockTerminal(page: Page): Promise<void> {
  await page.addInitScript(() => {
    try {
      localStorage.setItem("sb_tour_completed", "1");
    } catch {
      /* private mode — ignore */
    }
  });
  const row: Record<string, unknown> = {
    subcommand: "claude-code",
    session_id: "s-mobile-kbd",
    exited: false,
    has_project_root: false,
  };
  row.token = TOKEN;
  await page.route("**/api/launch/sessions", (route) =>
    route.fulfill({ json: { sessions: [row] } }),
  );
  await page.routeWebSocket(/\/ws\/launch\//, () => {
    /* the mock IS the server: accept and stay silent */
  });
}

async function openTerminal(page: Page) {
  const pill = page.getByLabel("Restore claude-code terminal");
  await expect(pill).toBeVisible({ timeout: 15000 });
  await pill.click();
  await expect(page.getByTestId("terminal-float-panel")).toBeVisible({ timeout: 15000 });
  await expect(page.locator(".xterm").first()).toBeVisible({ timeout: 15000 });
  await page.waitForTimeout(900);
}

const panelRect = (page: Page) =>
  page.evaluate(() => {
    const el = document.querySelector('[data-testid="terminal-float-panel"]') as HTMLElement;
    const r = el.getBoundingClientRect();
    return { top: r.top, bottom: r.bottom, height: r.height };
  });

test.describe("mobile terminal — on-screen keyboard", () => {
  test.use({ viewport: VIEWPORT, hasTouch: true, isMobile: true });

  test("the panel slides up so the prompt clears the keyboard", async ({ page }) => {
    await installFakeVisualViewport(page);
    await mockTerminal(page);
    await page.goto("/");

    // Guard the guard: if the emulated context does not actually match the
    // app's mobile branch, everything below would pass for the wrong reason.
    expect(
      await page.evaluate((q) => window.matchMedia(q).matches, MobileTerminalQuery),
    ).toBe(true);

    await openTerminal(page);

    const before = await panelRect(page);
    expect(before.height).toBeGreaterThan(VIEWPORT.height - 40);

    await page.evaluate((px) => window.__sbKeyboard(px), KEYBOARD_PX);
    await page.waitForTimeout(400); // rAF coalesce + React commit + refit

    const after = await panelRect(page);

    // THE LOAD-BEARING ASSERTION: the panel's bottom edge is above where the
    // keyboard starts. This is what "I can see what I'm typing" means — the
    // last row of the terminal has to be on screen.
    const keyboardTop = VIEWPORT.height - KEYBOARD_PX;
    expect(after.bottom).toBeLessThanOrEqual(keyboardTop + 1);

    // AND IT GETS THERE BY MOVING, NOT BY SHRINKING. The panel keeps its full
    // height and slides up; the top simply goes off-screen. Shrinking would
    // resize the PTY, and a full-screen TUI lays itself out for the rows it is
    // given — see the row-count test below.
    expect(after.height).toBeCloseTo(before.height, 0);
    expect(after.top).toBeLessThan(0);

    // And it must come BACK when the keyboard closes.
    await page.evaluate(() => window.__sbKeyboard(0));
    await page.waitForTimeout(400);
    const restored = await panelRect(page);
    expect(restored.height).toBeGreaterThan(VIEWPORT.height - 40);
    expect(restored.top).toBeCloseTo(0, 0);
  });

  test("the keyboard does NOT cost the terminal any rows", async ({ page }) => {
    await installFakeVisualViewport(page);
    await mockTerminal(page);
    await page.goto("/");
    await openTerminal(page);

    const rows = () =>
      page.evaluate(() => document.querySelectorAll(".xterm-rows > div").length);

    const before = await rows();
    await page.evaluate((px) => window.__sbKeyboard(px), KEYBOARD_PX);
    await page.waitForTimeout(600);
    const after = await rows();

    // THE ROW COUNT MUST NOT CHANGE. This is the correction to the first
    // attempt at this fix, which shrank the panel and so refit the PTY to
    // fewer rows. A full-screen TUI lays itself out for the rows it has:
    // measured against a live claude 2.1.220 on 2026-07-28, its composer is 5
    // lines at 20+ rows, 3 lines at 14, and exactly 2 at 8 — and at 8 it shows
    // only the TAIL of what you type, so typing a third line looks like it
    // overwrites the second. That was the operator's report. Clearing the
    // keyboard must therefore cost zero rows.
    expect(before).toBeGreaterThan(0);
    expect(after).toBe(before);
  });

  test("the viewport meta does NOT set interactive-widget", async ({ page }) => {
    await page.goto("/");
    const content = await page.evaluate(
      () => document.querySelector('meta[name="viewport"]')?.getAttribute("content") ?? "",
    );
    // `interactive-widget=resizes-content` was added alongside the hook as a
    // native second mechanism and reverted hours later: on Chrome/Android it
    // broke touch scrolling in the mobile terminal, including with the
    // keyboard DISMISSED. It is redundant anyway — under the default
    // `resizes-visual` Chrome still shrinks the visual viewport and fires
    // visualViewport.resize, which is what the hook consumes.
    //
    // THIS ASSERTION IS A REMINDER, NOT A GUARD. The suite runs desktop
    // Chromium, where no virtual keyboard exists and this key is inert — every
    // mobile spec passed WITH it set. Only a real device can test it, so the
    // pin exists to stop it being reintroduced as an "obvious" improvement.
    expect(content).not.toContain("interactive-widget");
    // The notch opt-in must survive the revert.
    expect(content).toContain("viewport-fit=cover");
  });
});
