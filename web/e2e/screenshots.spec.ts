import { test } from "@playwright/test";
import fs from "node:fs";

// Screenshot every nav route at a viewport matrix so we can read the
// captures back and SEE overflow/clipping on a phone. Drive the
// isolated demo-mode dashboard (data present). Not a pass/fail test —
// it's a capture harness; assertions live in the human/multimodal read.
//
// VIEWPORT and OUTDIR are env-driven so a single spec covers the
// whole matrix without N files. Run one viewport per invocation:
//   VIEWPORT=360x640 OUTDIR=e2e/shots/baseline/360 npx playwright test

const ROUTES: Array<{ path: string; name: string }> = [
  { path: "/", name: "overview" },
  { path: "/live", name: "live" },
  { path: "/search", name: "search" },
  { path: "/cost", name: "cost" },
  { path: "/report", name: "report" },
  { path: "/analysis", name: "analysis" },
  { path: "/sessions", name: "sessions" },
  { path: "/actions", name: "actions" },
  { path: "/security", name: "security" },
  { path: "/egress", name: "egress" },
  { path: "/tools", name: "tools" },
  { path: "/compression", name: "compression" },
  { path: "/cache", name: "cache" },
  { path: "/suggestions", name: "suggestions" },
  { path: "/routing", name: "routing" },
  { path: "/benchmarks", name: "benchmarks" },
  { path: "/discovery", name: "discovery" },
  { path: "/patterns", name: "patterns" },
  { path: "/privacy", name: "privacy" },
  { path: "/settings", name: "settings" },
  { path: "/remote", name: "remote" },
  { path: "/terminals", name: "terminals" },
];

const [vw, vh] = (process.env.VIEWPORT || "360x640").split("x").map(Number);
const OUTDIR = process.env.OUTDIR || "e2e/shots/latest";

test.use({ viewport: { width: vw, height: vh } });

fs.mkdirSync(OUTDIR, { recursive: true });

for (const r of ROUTES) {
  test(`shot ${r.name} @ ${vw}x${vh}`, async ({ page }) => {
    // NOT networkidle — the dashboard polls (/api/status @5s etc.) so
    // the network never goes idle. domcontentloaded + a fixed settle
    // lets charts/tables paint without hanging the whole run.
    await page.goto(r.path, { waitUntil: "domcontentloaded" });
    await page.waitForTimeout(1400);
    // Detect horizontal overflow of the document — the shell should
    // NEVER scroll sideways on a phone; only inner scroll containers.
    // Also find the widest offending element so a fix has a target.
    const info = await page.evaluate(() => {
      const doc = document.documentElement.scrollWidth;
      const client = document.documentElement.clientWidth;
      let widest = "";
      if (doc > client + 1) {
        let maxRight = 0;
        for (const el of Array.from(document.querySelectorAll("*"))) {
          const rect = (el as HTMLElement).getBoundingClientRect();
          if (rect.right > maxRight && rect.right > client + 1) {
            maxRight = rect.right;
            const e = el as HTMLElement;
            widest = `${e.tagName.toLowerCase()}.${(e.className || "")
              .toString()
              .split(" ")
              .slice(0, 4)
              .join(".")} right=${Math.round(rect.right)}`;
          }
        }
      }
      return { doc, client, widest };
    });
    await page.screenshot({ path: `${OUTDIR}/${r.name}.png`, fullPage: true });
    if (info.doc > info.client + 1) {
      fs.appendFileSync(
        `${OUTDIR}/_overflow.txt`,
        `${r.name}: doc ${info.doc} > client ${info.client} | widest: ${info.widest}\n`,
      );
    }
  });
}
