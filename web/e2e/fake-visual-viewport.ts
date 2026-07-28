import type { Page } from "@playwright/test";

// Shared on-screen-keyboard simulator for the mobile specs.
//
// Playwright cannot raise a real virtual keyboard, and window.visualViewport is
// read-only, so this installs a scriptable stand-in BEFORE any app code runs.
// It exposes the same contract the app consumes (height / offsetTop /
// addEventListener("resize"|"scroll")) and `__sbKeyboard(px)` drives it,
// firing `resize` exactly as a real keyboard does.
//
// Deliberately NOT a *.spec.ts: Playwright's default testMatch only collects
// spec/test files, so importing this from several specs cannot duplicate their
// test registrations.

declare global {
  interface Window {
    __sbKeyboard: (occludedPx: number) => void;
  }
}

/** Typical portrait keyboard occlusion (iOS/Android land in the 290-360 band
 * with the suggestion strip). Comfortably over useVisualViewport's 40px
 * tolerance. */
export const KEYBOARD_PX = 336;

export async function installFakeVisualViewport(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const listeners: Record<string, Set<() => void>> = { resize: new Set(), scroll: new Set() };
    // How much of the viewport the simulated keyboard currently covers.
    let occluded = 0;
    // height/width are GETTERS over the live window, never values snapshotted
    // at init time. An init script runs before Playwright has applied the
    // emulated viewport, so snapshotting captured the default 980x2125 window
    // and every consumer inherited a baseline that no test ever asked for —
    // which is exactly the kind of quietly-wrong instrument that makes a real
    // app look broken.
    const vv = {
      get height() {
        return window.innerHeight - occluded;
      },
      get width() {
        return window.innerWidth;
      },
      offsetTop: 0,
      offsetLeft: 0,
      pageTop: 0,
      pageLeft: 0,
      scale: 1,
      addEventListener: (type: string, fn: () => void) => {
        listeners[type]?.add(fn);
      },
      removeEventListener: (type: string, fn: () => void) => {
        listeners[type]?.delete(fn);
      },
    };
    Object.defineProperty(window, "visualViewport", {
      configurable: true,
      get: () => vv,
    });
    window.__sbKeyboard = (occludedPx: number) => {
      occluded = occludedPx;
      listeners.resize.forEach((fn) => fn());
    };
  });
}

/** Raise the simulated keyboard and let the rAF-coalesced resize, the React
 * commit and the terminal's ResizeObserver refit all settle. */
export async function raiseKeyboard(page: Page, px = KEYBOARD_PX): Promise<void> {
  await page.evaluate((n) => window.__sbKeyboard(n), px);
  await page.waitForTimeout(600);
}
