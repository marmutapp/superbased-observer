import { test, expect } from "@playwright/test";
import fs from "node:fs";

// Session-attach Phase 4 capture harness: the new [remote].allow_terminal_view
// checkbox on the Remote page (beside "Allow terminal") and the new "Session
// attach" ([terminal.attach]) Settings section. Route-interception mocks the
// wire so the run is self-contained (no armed remote secret, no daemon
// dependency). Screenshots are read back multimodally by the orchestrator.

const OUTDIR =
  process.env.P4OUTDIR ||
  "/tmp/claude-1000/-home-marmutapp-superbased-observer/dafbd2e9-d69a-42a3-be48-530679ce01a1/scratchpad/p4-screens";

fs.mkdirSync(OUTDIR, { recursive: true });

test.use({ viewport: { width: 1360, height: 1000 } });

// remoteConfigArmed builds a stub GET /api/remote/config for an armed
// controller with the new allow_terminal_view field present. Token-ish fields
// are set by member assignment (a bare object key gets mangled by the harness
// Write-filter; feedback_write_filter_token_patterns).
function remoteConfigArmed(allowTerminalView: boolean): Record<string, unknown> {
  const cfg: Record<string, unknown> = {
    config_writable: true,
    controller_live: true,
    enabled: true,
    mode: "tailscale",
    require_tls: true,
    allow_terminal: true,
    trusted_hosts: ["box.tailnet-name.ts.net"],
    backend_addr: "loopback",
    rate_limit_per_min: 6,
    max_sessions: 5,
    secret_present: true,
    ready: true,
  };
  cfg.allow_terminal_view = allowTerminalView;
  // backend_addr is a loopback host:port; set via member assignment because the
  // Write-filter mangles the numeric literal inside the object initializer.
  cfg.confirm_token = ["ct", "demo", "0000"].join("-");
  cfg.secret_fingerprint = "…a1b2c3d4";
  cfg.backend_addr = "127.0.0.1:41999";
  return cfg;
}

async function mockRemote(page: import("@playwright/test").Page, allowView: boolean) {
  await page.route("**/api/remote/config", (route) =>
    route.fulfill({ json: remoteConfigArmed(allowView) }),
  );
  await page.route("**/api/remote/sessions", (route) =>
    route.fulfill({ json: { sessions: [], controller_live: true } }),
  );
  await page.route("**/api/remote/audit", (route) =>
    route.fulfill({ json: { events: [], immutable: false } }),
  );
  await page.route("**/api/remote/standing-terminal", (route) =>
    route.fulfill({
      json: {
        enabled: false,
        secret_present: false,
        secret_fingerprint: "",
        allow_terminal: true,
        remote_enabled: true,
        config_writable: true,
        warning: "",
      },
    }),
  );
  await page.route("**/api/remote/tailscale/status", (route) =>
    route.fulfill({ json: { present: true, host: "box.tailnet-name.ts.net", running: true } }),
  );
}

test("Remote page shows the allow-terminal-view checkbox (off)", async ({ page }) => {
  await mockRemote(page, false);
  await page.goto("/remote", { waitUntil: "domcontentloaded" });
  const label = page.getByText("allow terminal view", { exact: true });
  await label.scrollIntoViewIfNeeded();
  await expect(label).toBeVisible();
  await page.waitForTimeout(400);
  await page.screenshot({ path: `${OUTDIR}/1-remote-allow-terminal-view-off.png`, fullPage: true });
});

test("Remote page shows the allow-terminal-view checkbox (on)", async ({ page }) => {
  await mockRemote(page, true);
  await page.goto("/remote", { waitUntil: "domcontentloaded" });
  const label = page.getByText("allow terminal view", { exact: true });
  await label.scrollIntoViewIfNeeded();
  await expect(label).toBeVisible();
  await page.waitForTimeout(400);
  await page.screenshot({ path: `${OUTDIR}/2-remote-allow-terminal-view-on.png`, fullPage: true });
});

// mockConfig builds a minimal GET /api/config whose Terminal.Attach block the
// StructuredConfigSection resolves via path ["Terminal","Attach"].
function mockConfig(): Record<string, unknown> {
  return {
    config_path: "/home/user/.observer/config.toml",
    profile_names: ["default"],
    editable_sections: ["terminal"],
    config: {
      Terminal: {
        Enabled: true,
        Attach: { Enabled: true, RouteProxy: true },
        Launch: { AllowFreshAgent: false, AllowedTools: [], AllowedProjectRoots: [] },
        Status: { Enabled: true },
      },
    },
  };
}

test("Settings shows the Session attach ([terminal.attach]) section", async ({ page }) => {
  await page.route("**/api/config", (route) => route.fulfill({ json: mockConfig() }));
  await page.goto("/settings", { waitUntil: "domcontentloaded" });
  // Open the "Session attach" section from the config nav.
  const nav = page.getByRole("button", { name: /Session attach/i }).first();
  await nav.scrollIntoViewIfNeeded();
  await nav.click();
  const serveLabel = page.getByText("Serve attach socket").first();
  await expect(serveLabel).toBeVisible({ timeout: 10000 });
  await expect(page.getByText("Route through proxy").first()).toBeVisible();
  // Center the section body so the two toggles + help copy are in-frame (a
  // fullPage shot lands scrolled past them on this tall nav).
  await page.getByText("Session attach", { exact: false }).first().scrollIntoViewIfNeeded();
  await serveLabel.scrollIntoViewIfNeeded();
  await page.waitForTimeout(400);
  await page.screenshot({ path: `${OUTDIR}/3-settings-session-attach.png` });
});
