import { test, expect, type Page } from "@playwright/test";
import { MobileTerminalQuery } from "../src/lib/useMediaQuery";

// Mobile terminal LAYOUT regression pin.
//
// THE BUG THIS EXISTS TO CATCH (measured, 2026-07-26). The floating terminal
// panel in LaunchDock is `min-w-[480px] max-w-[96vw]` with an inline
// width/height style. Per CSS 2.1 §10.4 min-width WINS over max-width, so on a
// 393px phone the box resolved to 480px inside a `flex items-center
// justify-center` backdrop — the 87px of overflow split evenly and the LEFT
// HALF OF THE TERMINAL WAS UNREACHABLE (you cannot scroll left out of a centred
// flex item). Measured: 393→480 @ left −43; 390→480 @ −45; 360→480 @ −60.
//
// WHY THE OBVIOUS GUARD IS NOT ENOUGH — read before "simplifying" this file.
// The previous mobile regressions in this repo were guarded by
// `documentElement.scrollWidth <= clientWidth`. That assertion PASSED on the
// broken tree at all three widths, because the panel lives inside a
// `position: fixed` backdrop and fixed elements do not extend the document
// scroll area. A guard that misses the bug it exists to catch is worse than no
// guard, so the load-bearing assertions here are:
//   (2) the panel's own getBoundingClientRect() lies inside [0, innerWidth];
//   (3) no leaf TEXT NODE inside the panel escapes [0, innerWidth].
// Assertion (1) is kept as a cheap document-level canary, not as the guard.
//
// The breakpoint string is IMPORTED from the app (MobileTerminalQuery) rather
// than copied, so a change to the app's branch condition cannot silently drift
// away from what this spec emulates. Each mobile test asserts that the emulated
// context actually matches that query — otherwise the whole file would be
// exercising the desktop branch and passing for the wrong reason.
//
// Harness: same shape as terminal-keys.spec.ts — a live session is injected via
// GET /api/launch/sessions (the dock rehydrate path) and the terminal socket is
// mocked with page.routeWebSocket, so no daemon, no PTY, no real process. Run
// against the isolated demo dashboard (see the e2e recipe: `observer dashboard
// --config <scratch> --port 8092` + `POST /api/demo/start`, vite dev on :5174
// with VITE_API_PROXY pointed at it). NEVER against the operator's :8820.

const TOKEN = ["mobile", "layout", "tok"].join("-");

const FLOAT_SIZE_KEY = "sb_terminal_float_size";

// Desktop baseline, measured on the shipping tree at 1360x940. These are the
// numbers the mobile branch must leave bit-identical.
const DESKTOP = {
  viewport: { width: 1360, height: 940 },
  panel: { width: 880, height: 564, left: 240 },
  backdropPadding: "24px",
  minWidth: "480px",
  headerHeight: 33, // one row — a wrap would roughly double this
};

const MOBILE_VIEWPORTS = [
  { name: "small-360x640", width: 360, height: 640 },
  { name: "iphone-390x844", width: 390, height: 844 },
  { name: "pixel-393x852", width: 393, height: 852 },
];

// mockTerminal injects one live launch session and a no-op terminal socket.
async function mockTerminal(page: Page): Promise<void> {
  // Suppress the first-run tour: its full-viewport overlay intercepts the
  // clicks on the dock pill (see tour.spec.ts).
  await page.addInitScript(() => {
    try {
      localStorage.setItem("sb_tour_completed", "1");
    } catch {
      /* private mode — ignore */
    }
  });
  const row: Record<string, unknown> = {
    subcommand: "claude-code",
    session_id: "s-mobile-layout",
    exited: false,
    has_project_root: false,
  };
  row.token = TOKEN;
  await page.route("**/api/launch/sessions", (route) =>
    route.fulfill({ json: { sessions: [row] } }),
  );
  await page.routeWebSocket(/\/ws\/launch\//, () => {
    // The mock IS the server: do not connectToServer, just accept and stay
    // silent. The client sees onopen → status "open".
  });
}

// openTerminal restores the injected session from its dock pill so the
// floating panel is on screen, and waits for xterm to have laid itself out.
async function openTerminal(page: Page) {
  const pill = page.getByLabel("Restore claude-code terminal");
  await expect(pill).toBeVisible({ timeout: 15000 });
  await pill.click();
  const panel = page.getByTestId("terminal-float-panel");
  await expect(panel).toBeVisible({ timeout: 15000 });
  await expect(page.locator(".xterm").first()).toBeVisible({ timeout: 15000 });
  // xterm fits itself on a rAF after mount; give the ResizeObserver-driven
  // refit (and the 400ms float-size persist debounce) room to settle.
  await page.waitForTimeout(900);
}

type Overflow = {
  doc: { scrollWidth: number; clientWidth: number };
  panel: { left: number; right: number; top: number; width: number; height: number };
  innerWidth: number;
  innerHeight: number;
  backdropPadding: string;
  minWidth: string;
  headerHeight: number;
  panelClass: string;
  inlineWidth: string;
  resize: string;
  overscroll: { panel: string; backdrop: string };
  textOverflows: Array<{ text: string; left: number; right: number; cls: string }>;
};

// measure reads every geometry fact the assertions need in ONE round trip.
async function measure(page: Page): Promise<Overflow> {
  return page.evaluate(() => {
    const panel = document.querySelector(
      '[data-testid="terminal-float-panel"]',
    ) as HTMLElement | null;
    if (!panel) throw new Error("terminal-float-panel not in the DOM");
    const backdrop = panel.parentElement as HTMLElement;
    const r = panel.getBoundingClientRect();
    const header = panel.querySelector('div[class*="bg-[#14141a]"]') as HTMLElement | null;

    // xterm parks off-screen helpers (.xterm-char-measure-element, the
    // accessibility live-region, the helper textarea) at `left: -9999em`,
    // which at a 12px root font size lands around left:-119979. A naive text
    // scan flags those as "overflow" and the guard is permanently red. They
    // are deliberately off-screen, are not the panel's content, and are not
    // what D1 breaks — so they are excluded, both by class and by an
    // absolute -1000px floor (a genuinely broken panel is off by tens of
    // pixels, never by six figures).
    const PARKED = [
      ".xterm-char-measure-element",
      ".xterm-accessibility",
      ".xterm-helper-textarea",
      ".live-region",
      "[aria-live]",
    ].join(",");

    const bad: Array<{ text: string; left: number; right: number; cls: string }> = [];
    const walker = document.createTreeWalker(panel, NodeFilter.SHOW_TEXT);
    let n: Node | null;
    while ((n = walker.nextNode())) {
      const value = n.nodeValue || "";
      if (!value.trim()) continue;
      const parent = n.parentElement;
      if (!parent) continue;
      if (parent.closest(PARKED)) continue;
      const range = document.createRange();
      range.selectNodeContents(n);
      for (const rc of Array.from(range.getClientRects())) {
        if (rc.width === 0 && rc.height === 0) continue;
        if (rc.left < -1000) continue; // parked helper that dodged the class filter
        if (rc.left < -1 || rc.right > window.innerWidth + 1) {
          bad.push({
            text: value.trim().slice(0, 40),
            left: Math.round(rc.left),
            right: Math.round(rc.right),
            cls: parent.className?.toString().slice(0, 60) ?? "",
          });
        }
      }
    }

    return {
      doc: {
        scrollWidth: document.documentElement.scrollWidth,
        clientWidth: document.documentElement.clientWidth,
      },
      panel: {
        left: Math.round(r.left),
        right: Math.round(r.right),
        top: Math.round(r.top),
        width: Math.round(r.width),
        height: Math.round(r.height),
      },
      innerWidth: window.innerWidth,
      innerHeight: window.innerHeight,
      backdropPadding: getComputedStyle(backdrop).paddingLeft,
      minWidth: getComputedStyle(panel).minWidth,
      headerHeight: header ? Math.round(header.getBoundingClientRect().height) : -1,
      panelClass: panel.className,
      inlineWidth: panel.style.width,
      resize: getComputedStyle(panel).resize,
      overscroll: {
        panel: getComputedStyle(panel).overscrollBehaviorY,
        backdrop: getComputedStyle(backdrop).overscrollBehaviorY,
      },
      textOverflows: bad,
    };
  });
}

for (const vp of MOBILE_VIEWPORTS) {
  test.describe(`mobile terminal ${vp.name}`, () => {
    test.use({
      viewport: { width: vp.width, height: vp.height },
      isMobile: true,
      hasTouch: true,
      deviceScaleFactor: 3,
    });

    test("panel and its text stay inside the viewport", async ({ page }) => {
      await mockTerminal(page);
      await page.goto("/", { waitUntil: "domcontentloaded" });

      // Pin that the emulated context really is the one the app branches on.
      // If this ever goes false the rest of the file is testing desktop.
      const matches = await page.evaluate(
        (q) => window.matchMedia(q).matches,
        MobileTerminalQuery,
      );
      expect(
        matches,
        `emulated context must match the app's own breakpoint: ${MobileTerminalQuery}`,
      ).toBe(true);

      await openTerminal(page);
      const m = await measure(page);
      // eslint-disable-next-line no-console
      console.log(`[${vp.name}] panel ${m.panel.width}x${m.panel.height} @ left ${m.panel.left}` +
        ` | doc ${m.doc.scrollWidth}/${m.doc.clientWidth} | text overflows ${m.textOverflows.length}`);

      await page.screenshot({ path: `e2e/shots/mobile-${vp.name}.png`, fullPage: false });

      // SOFT on purpose: when this goes red you want every geometry fact in
      // one report, not just whichever assertion happens to be first.
      //
      // (1) Document-level canary. Kept, but STRUCTURALLY BLIND to D1 — see
      //     the header comment. Never let this be the only assertion.
      expect
        .soft(m.doc.scrollWidth, "document horizontal overflow")
        .toBeLessThanOrEqual(m.doc.clientWidth + 1);

      // (2) The real guard: the panel's own box is inside the viewport.
      expect
        .soft(m.panel.left, `panel left edge off-screen (${m.panel.left})`)
        .toBeGreaterThanOrEqual(-1);
      expect
        .soft(
          m.panel.right,
          `panel right edge past the viewport (${m.panel.right} > ${m.innerWidth})`,
        )
        .toBeLessThanOrEqual(m.innerWidth + 1);

      // (3) No leaf text escapes either edge.
      expect
        .soft(
          m.textOverflows,
          `text nodes outside [0, ${m.innerWidth}]: ${JSON.stringify(m.textOverflows.slice(0, 6))}`,
        )
        .toEqual([]);

      // (D6) A phone visit must NOT feed its clamped geometry back into the
      // persisted desktop float size — one phone visit used to shrink the
      // user's desktop terminal window to 480px forever.
      const persisted = await page.evaluate(
        (k) => localStorage.getItem(k),
        FLOAT_SIZE_KEY,
      );
      expect
        .soft(persisted, `mobile geometry leaked into ${FLOAT_SIZE_KEY}: ${persisted}`)
        .toBeNull();

      // (D1 mechanism) The geometry is an INLINE style on the shipping tree,
      // which no Tailwind breakpoint can outrank — the mobile branch must drop
      // it, not restyle around it. If this comes back, `width` wins again and
      // the class-based fit above is decoration.
      expect.soft(m.inlineWidth, "mobile must carry no inline width").toBe("");
      // (D11) pull-to-refresh must not be able to tear the live PTY down.
      expect.soft(m.overscroll.panel).toBe("contain");
      expect.soft(m.overscroll.backdrop).toBe("contain");
      // No finger-draggable resize grip on touch.
      expect.soft(m.resize).toBe("none");
      // (D7/D12) Headless cannot raise a soft keyboard or synthesise a notch,
      // so `dvh` and the safe-area inset are unobservable at runtime here —
      // env(safe-area-inset-bottom) computes to 0px exactly as its absence
      // would. These are therefore SOURCE-LEVEL pins: they assert the app
      // asked for the right thing. `vh` (the LARGE viewport) does not shrink
      // when the keyboard opens; `dvh` does.
      expect.soft(m.panelClass, "must size in dvh, not vh").toContain("h-[100dvh]");
      expect.soft(m.panelClass, "must reserve the home-indicator inset").toContain(
        "pb-[env(safe-area-inset-bottom)]",
      );
      expect.soft(m.panelClass, "min-w-[480px] is the D1 bug — never here").not.toContain(
        "min-w-[480px]",
      );
    });
  });
}

test.describe("desktop terminal panel (non-regression)", () => {
  test.use({ viewport: DESKTOP.viewport });

  test("geometry is unchanged by the mobile branch", async ({ page }) => {
    await mockTerminal(page);
    await page.goto("/", { waitUntil: "domcontentloaded" });

    // A desktop window has a FINE pointer, so it must NOT match — even though
    // 1360 is wide, this is the assertion that pins the capability gate.
    const matches = await page.evaluate(
      (q) => window.matchMedia(q).matches,
      MobileTerminalQuery,
    );
    expect(matches, "desktop must not take the mobile branch").toBe(false);

    await openTerminal(page);
    const m = await measure(page);
    // eslint-disable-next-line no-console
    console.log(`[desktop] panel ${m.panel.width}x${m.panel.height} @ left ${m.panel.left}` +
      ` | padding ${m.backdropPadding} | min-width ${m.minWidth} | header ${m.headerHeight}` +
      ` | doc ${m.doc.scrollWidth}/${m.doc.clientWidth}`);

    await page.screenshot({ path: "e2e/shots/mobile-desktop-baseline.png", fullPage: false });

    expect(m.panel.width).toBe(DESKTOP.panel.width);
    expect(m.panel.height).toBe(DESKTOP.panel.height);
    expect(m.panel.left).toBe(DESKTOP.panel.left);
    expect(m.backdropPadding).toBe(DESKTOP.backdropPadding);
    expect(m.minWidth).toBe(DESKTOP.minWidth);
    expect(m.headerHeight).toBe(DESKTOP.headerHeight);
    expect(m.doc.scrollWidth).toBe(DESKTOP.viewport.width);
    expect(m.doc.clientWidth).toBe(DESKTOP.viewport.width);
    expect(m.textOverflows).toEqual([]);
    // The desktop side keeps its inline size, its resize grip and its default
    // overscroll — none of the mobile-branch properties leak across.
    expect(m.inlineWidth).toBe("880px");
    expect(m.resize).toBe("both");
    expect(m.overscroll.panel).toBe("auto");
    expect(m.overscroll.backdrop).toBe("auto");
  });
});
