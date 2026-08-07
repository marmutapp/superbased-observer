import { test } from "@playwright/test";
import fs from "node:fs";

// TEMPORARY network-forensics capture for the live overview (2026-07-31
// UI verify pass). Logs every /api request's timing + status + size and
// any console errors during a 40s live-overview load. Delete after.

const OUTDIR = "/tmp/uiverify2";
fs.mkdirSync(OUTDIR, { recursive: true });

test.use({ viewport: { width: 1440, height: 900 } });

test("overview network forensics", async ({ page }) => {
  test.setTimeout(120_000);
  const lines: string[] = [];
  const started = new Map<string, number>();
  page.on("request", (r) => {
    if (r.url().includes("/api/")) started.set(r.url() + r.frame(), Date.now());
  });
  page.on("response", async (r) => {
    if (!r.url().includes("/api/")) return;
    const t0 = started.get(r.url() + r.frame());
    const ms = t0 ? Date.now() - t0 : -1;
    let size = -1;
    try {
      size = (await r.body()).length;
    } catch {}
    lines.push(
      `${r.status()} ${ms}ms ${size}B ${r.url().replace("http://localhost:8081", "")}`,
    );
  });
  page.on("requestfailed", (r) => {
    if (r.url().includes("/api/"))
      lines.push(`FAILED ${r.failure()?.errorText} ${r.url()}`);
  });
  page.on("console", (m) => {
    if (m.type() === "error" || m.type() === "warning")
      lines.push(`CONSOLE ${m.type()}: ${m.text().slice(0, 300)}`);
  });
  await page.addInitScript(() => {
    try {
      localStorage.setItem("sb_tour_completed", "1");
    } catch {}
  });
  await page.goto("/", { waitUntil: "domcontentloaded" });
  await page.waitForTimeout(40_000);
  await page.screenshot({ path: `${OUTDIR}/overview-40s.png` });
  fs.writeFileSync(`${OUTDIR}/network.log`, lines.join("\n"));
});
