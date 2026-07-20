import { test, expect } from "@playwright/test";

// Verifies the clarified Remote-page language + that "Pair a device" actually
// reveals a QR (the "pair a device does nothing" fix). Runs against an ISOLATED
// temp-config dashboard (BASE_URL=:8093) so arming/pairing never touch the real
// remote secret.
test.use({ viewport: { width: 1280, height: 1000 } });

test("turn on remote access → Pair a device reveals a QR", async ({ page }) => {
  await page.goto("/remote", { waitUntil: "domcontentloaded" });
  await page.waitForTimeout(600);

  // Off state: turn it on with a host.
  const hostInput = page.locator('input[placeholder*="tailnet"]').first();
  await hostInput.fill("box.tailnet-name.ts.net");
  await page.getByRole("button", { name: /Turn on remote access/i }).click();

  // Enable reveals the one-time pairing panel + QR.
  await expect(
    page.getByRole("heading", { name: /Scan to pair this device/i }),
  ).toBeVisible({ timeout: 15000 });
  await expect(page.getByAltText("Pairing QR code")).toBeVisible();
  await page.screenshot({ path: "e2e/shots/remote/armed.png", fullPage: true });

  // Dismiss the reveal, then prove "Pair a device" brings a FRESH one back
  // (this is the button that used to just scroll = "does nothing").
  await page.getByRole("button", { name: /^Done$/ }).click();
  await page.waitForTimeout(300);
  await expect(
    page.getByRole("heading", { name: /Scan to pair this device/i }),
  ).toHaveCount(0);

  await page.getByRole("button", { name: /^Pair a device$/ }).click();
  await expect(
    page.getByRole("heading", { name: /Scan to pair this device/i }),
  ).toBeVisible({ timeout: 15000 });
  await expect(page.getByAltText("Pairing QR code")).toBeVisible();

  // The reset control is present + clearly labelled.
  await expect(
    page.getByRole("button", { name: /Reset & unpair all devices/i }),
  ).toBeVisible();
  await page.screenshot({ path: "e2e/shots/remote/paired-again.png", fullPage: true });
});
