import { test, expect } from "@playwright/test";

// Terminal reconnect matrix (regression pin, 2026-07-25 mobile
// terminal-continuity arc).
//
// Context: the PTY is a SERVER-side object that outlives its viewers —
// termsession's Unsubscribe removes a viewer without killing the child, and idle
// reaping is off by default. So a closed websocket says nothing about the
// session. LaunchTerminal (src/components/LaunchTerminal.tsx) used to paint any
// close as "exited", a terminal state with no reconnect path; a phone that
// backgrounded its tab to fetch a pairing code from the mail app came back to a
// grid of tombstones reading "Session ended or is no longer running."
//
// These specs pin the three outcomes the client must distinguish:
//   1. transport drop        → "reconnecting…", a fresh socket is opened
//   2. authoritative exit    → "exited", NO reconnect
//   3. 1008 with no activity → "error" (the handle is genuinely gone), NO retry
//
// Harness: the same page.routeWebSocket mock terminal-keys.spec.ts uses. The
// mock IS the server, so it opens immediately and we can close it on command;
// connection attempts are counted to prove (or disprove) a retry.

test.use({ viewport: { width: 1360, height: 940 } });

const TOKEN = ["launch", "demo", "reconnect"].join("-");

type WSRoute = import("@playwright/test").WebSocketRoute;

type Harness = {
  /** How many times the page has opened /ws/launch/<token>. */
  connections: () => number;
  /** The most recently opened mock socket. */
  latest: () => WSRoute | null;
};

// mockTerminal wires the launch-session rehydrate + a WS mock that records every
// connection attempt. onConnect runs for each socket the page opens.
async function mockTerminal(
  page: import("@playwright/test").Page,
  onConnect?: (ws: WSRoute, index: number) => void,
): Promise<Harness> {
  let count = 0;
  let last: WSRoute | null = null;
  // Suppress the first-run tour: its full-viewport overlay would intercept
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
    session_id: "s-reconnect",
    exited: false,
    has_project_root: false,
  };
  row.token = TOKEN;
  await page.route("**/api/launch/sessions", (route) =>
    route.fulfill({ json: { sessions: [row] } }),
  );
  await page.routeWebSocket(/\/ws\/launch\//, (ws) => {
    // Do NOT connectToServer — the mock IS the server, so it opens immediately.
    const index = count;
    count += 1;
    last = ws;
    onConnect?.(ws, index);
  });
  return { connections: () => count, latest: () => last };
}

// openTerminal brings the injected session on-screen via its dock pill.
async function openTerminal(page: import("@playwright/test").Page) {
  const pill = page.getByLabel("Restore claude-code terminal");
  await expect(pill).toBeVisible({ timeout: 10000 });
  await pill.click();
  const xterm = page.locator(".xterm").first();
  await expect(xterm).toBeVisible({ timeout: 10000 });
}

test("a transport drop shows 'reconnecting…' and opens a fresh socket", async ({
  page,
}) => {
  // Close the first TWO sockets, as a frozen mobile tab's connection dies, so
  // the recoverable state is on screen long enough to assert without racing the
  // (~1s) first backoff; the third is left alive so recovery is observable too.
  const h = await mockTerminal(page, (ws, index) => {
    if (index < 2) setTimeout(() => ws.close(), index === 0 ? 500 : 0);
  });
  await page.goto("/", { waitUntil: "domcontentloaded" });
  await openTerminal(page);

  // The status pill reports a recoverable state, never an ended session.
  await expect(page.getByText("reconnecting…").first()).toBeVisible({
    timeout: 10000,
  });
  await expect(
    page.getByText("Session ended or is no longer running."),
  ).toHaveCount(0);

  // A second connection is attempted within the first backoff window (~1s
  // nominal, ×0.7–1.3 jitter) and the terminal returns to "live".
  await expect.poll(() => h.connections(), { timeout: 15000 }).toBeGreaterThan(1);
  await expect(page.getByText("live").first()).toBeVisible({ timeout: 15000 });
});

test("an authoritative exit frame ends the session and does NOT reconnect", async ({
  page,
}) => {
  const h = await mockTerminal(page, (ws, index) => {
    if (index !== 0) return;
    setTimeout(() => {
      // {"t":"exit"} is the ONLY proof the child is gone.
      ws.send(JSON.stringify({ t: "exit", code: 0 }));
      setTimeout(() => ws.close(), 100);
    }, 500);
  });
  await page.goto("/", { waitUntil: "domcontentloaded" });
  await openTerminal(page);

  await expect(page.getByText("exited (0)").first()).toBeVisible({
    timeout: 10000,
  });
  // Give the backoff more than one window to (not) fire.
  await page.waitForTimeout(4000);
  expect(h.connections()).toBe(1);
});

test("a 1008 close with no activity is reported dead and does NOT retry", async ({
  page,
}) => {
  const h = await mockTerminal(page, (ws, index) => {
    if (index === 0) {
      // The server's "session not found" policy close: the handle was already
      // gone. Nothing was ever delivered on this socket.
      setTimeout(() => ws.close({ code: 1008, reason: "session not found" }), 400);
    }
  });
  await page.goto("/", { waitUntil: "domcontentloaded" });
  await openTerminal(page);

  await expect(
    page.getByText(
      "Session ended before the terminal could connect — it was no longer running.",
    ),
  ).toBeVisible({ timeout: 10000 });
  await page.waitForTimeout(4000);
  expect(h.connections()).toBe(1);
});

test("returning to a foregrounded tab reconnects immediately", async ({
  page,
}) => {
  // Every socket dies at once, so the client climbs its backoff ladder
  // (~1s, 2s, 4s, 8s…). Waiting until several attempts have burned puts a LONG
  // timer in flight, which is what makes "the visibility handler bypassed the
  // backoff" observable rather than indistinguishable from the next scheduled
  // retry.
  const h = await mockTerminal(page, (ws) => {
    setTimeout(() => ws.close(), 0);
  });
  await page.goto("/", { waitUntil: "domcontentloaded" });
  await openTerminal(page);
  await expect(page.getByText("reconnecting…").first()).toBeVisible({
    timeout: 10000,
  });
  // 5 attempts ⇒ the pending timer is ~5.6–10.4s out.
  await expect.poll(() => h.connections(), { timeout: 30000 }).toBeGreaterThan(4);
  await page.waitForTimeout(500); // settle inside the long wait
  const before = h.connections();

  // Simulate the tab being backgrounded and brought back. A real mobile browser
  // FREEZES the tab; here we only need the visibility transition the handler
  // listens for, so the pending backoff is bypassed and a socket opens at once.
  await page.evaluate(() => {
    const doc = document as Document & { __sbVisibility?: string };
    Object.defineProperty(document, "visibilityState", {
      configurable: true,
      get: () => doc.__sbVisibility ?? "visible",
    });
    doc.__sbVisibility = "hidden";
    document.dispatchEvent(new Event("visibilitychange"));
    doc.__sbVisibility = "visible";
    document.dispatchEvent(new Event("visibilitychange"));
  });
  // Immediately — well inside the multi-second backoff that was in flight.
  await expect
    .poll(() => h.connections(), { timeout: 1500 })
    .toBeGreaterThan(before);
});
