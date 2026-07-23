import { test, expect } from "@playwright/test";

// Per-terminal Files / Git project panel (shipped aa55b9fa). Frontend:
// src/components/ProjectPanel.tsx; wire types + endpoints in
// src/lib/projectPanel.ts (GET /api/terminal/project/<token>{,/files,/file,/git}).
// The panel opens from a terminal's header "▤ Files" / "⎇ Git" buttons
// (LaunchTerminal) — provider-owned, one instance at a time.
//
// BEHAVIOUR-ONLY: these specs assert visible text + ARIA roles (tabs, buttons,
// headings, file content), never the panel's CONTAINER. The panel is planned
// to move from the right-docked SlideOver into a floating window; nothing here
// asserts backdrop presence, right-edge docking, or SlideOver internals, so the
// specs survive that rework.
//
// A live session is injected via GET /api/launch/sessions and restored from its
// dock pill so the terminal header (with the Files/Git buttons) is on screen.
// The four panel endpoints are mocked to the projectPanel.ts wire shapes.

test.use({ viewport: { width: 1360, height: 940 } });

const HANDLE = ["p", "tok", "demo"].join("-");

const NOW = new Date().toISOString();

// Directory listings keyed by the ?path= query the tree requests.
const FILES: Record<string, Array<Record<string, unknown>>> = {
  "": [
    { name: "src", type: "dir", size: 0, mtime: NOW },
    { name: "README.md", type: "file", size: 1234, mtime: NOW },
  ],
  src: [{ name: "index.ts", type: "file", size: 567, mtime: NOW }],
};

const FILE_CONTENT = "# Demo Project\nhello world from readme\n";

const GIT_INFO: Record<string, unknown> = {
  is_git: true,
  branch: "main",
  upstream: "origin/main",
  ahead: 2,
  behind: 1,
  status: [{ path: "src/index.ts", staged: "M", worktree: " ", renamed_from: "" }],
  log: [
    {
      hash: "abc1234def5678",
      parents: [],
      author: "Ada Lovelace",
      date: NOW,
      refs: ["HEAD -> main"],
      subject: "initial commit",
    },
  ],
  log_truncated: false,
};

type PanelOpts = { metaError?: string };

// mockSessionAndPanel wires the dock rehydrate, a silent WS mock, and the four
// project-panel endpoints. `metaError` makes the meta endpoint return the given
// error code (e.g. "no_project_root") to exercise the honest error copy.
async function mockSessionAndPanel(
  page: import("@playwright/test").Page,
  opts: PanelOpts = {},
) {
  await page.addInitScript(() => {
    try {
      localStorage.setItem("sb_tour_completed", "1"); // suppress first-run tour
    } catch {
      /* ignore */
    }
  });
  const row: Record<string, unknown> = {
    subcommand: "claude-code",
    session_id: "s-panel",
    exited: false,
    has_project_root: true,
  };
  row.token = HANDLE;
  await page.route("**/api/launch/sessions", (route) =>
    route.fulfill({ json: { sessions: [row] } }),
  );
  // Keep the terminal socket quiet (mock accepts + stays open).
  await page.routeWebSocket(/\/ws\/launch\//, () => {
    /* no-op mock server */
  });

  // Order matters: Playwright evaluates the MOST-RECENTLY-registered route
  // first, and the singular `/file**` glob also matches `/files`. Register
  // `file` BEFORE `files` so the plural route wins for a listing request while
  // the singular route still handles `/file?path=`.
  await page.route("**/api/terminal/project/*/file**", (route) => {
    const path = new URL(route.request().url()).searchParams.get("path") ?? "";
    route.fulfill({
      json: {
        path,
        content: FILE_CONTENT,
        size: FILE_CONTENT.length,
        truncated: false,
        binary: false,
        too_large: false,
      },
    });
  });
  await page.route("**/api/terminal/project/*/files**", (route) => {
    const path = new URL(route.request().url()).searchParams.get("path") ?? "";
    const entries = FILES[path] ?? [];
    route.fulfill({ json: { path, entries, truncated: false } });
  });
  await page.route("**/api/terminal/project/*/git**", (route) =>
    route.fulfill({ json: GIT_INFO }),
  );
  // Base meta (no /files|/file|/git suffix). Must be registered LAST so the
  // sub-routes win for their paths.
  await page.route("**/api/terminal/project/*", (route) => {
    if (opts.metaError) {
      route.fulfill({ status: 400, json: { error: opts.metaError } });
      return;
    }
    route.fulfill({
      json: { root: "/home/demo/projects/web-app", git_available: true, is_git: true, branch: "main" },
    });
  });
}

// Restore the terminal from its dock pill and open the given panel tab from the
// terminal header. Returns after the panel has begun rendering.
async function openPanel(
  page: import("@playwright/test").Page,
  which: "Files" | "Git",
) {
  const pill = page.getByTitle("Restore claude-code terminal");
  await expect(pill).toBeVisible({ timeout: 10000 });
  await pill.click();
  // Header buttons read "▤ Files" / "⎇ Git" (glyph is aria-hidden, so the
  // accessible name contains just the word).
  const header = page.getByRole("button", { name: which, exact: false });
  await expect(header.first()).toBeVisible({ timeout: 10000 });
  await header.first().click();
  // No terminal-minimize needed anymore: the panel is now a non-modal
  // FloatingPanel in a z-band ABOVE the expanded-terminal backdrop (z-80), so
  // clicks on the tree / tabs land directly even while the terminal stays open
  // — that coexistence is the whole point of the rework.
}

test("Files panel renders the tree, expands a dir, and shows file content", async ({
  page,
}) => {
  await mockSessionAndPanel(page);
  await page.goto("/", { waitUntil: "domcontentloaded" });
  await openPanel(page, "Files");

  // Root listing renders.
  await expect(page.getByRole("button", { name: /src/ })).toBeVisible({ timeout: 10000 });
  await expect(page.getByRole("button", { name: /README\.md/ })).toBeVisible();

  // Expanding the dir loads + reveals its children.
  await page.getByRole("button", { name: /src/ }).click();
  await expect(page.getByRole("button", { name: /index\.ts/ })).toBeVisible({ timeout: 10000 });

  // Clicking a file renders its content in the viewer.
  await page.getByRole("button", { name: /README\.md/ }).click();
  await expect(page.getByText("hello world from readme")).toBeVisible({ timeout: 10000 });
});

test("Git tab renders branch, ahead/behind, changes, and history", async ({ page }) => {
  await mockSessionAndPanel(page);
  await page.goto("/", { waitUntil: "domcontentloaded" });
  await openPanel(page, "Git");

  // Branch chip + ahead/behind counters.
  await expect(page.getByText("main").first()).toBeVisible({ timeout: 10000 });
  await expect(page.getByText("↑2")).toBeVisible();
  await expect(page.getByText("↓1")).toBeVisible();

  // Changes list: the section header count + the changed path.
  await expect(page.getByText(/Changes \(1\)/)).toBeVisible();
  await expect(page.getByText("src/index.ts")).toBeVisible();

  // History: the section header count + a commit subject and short hash.
  await expect(page.getByText(/History \(1\)/)).toBeVisible();
  await expect(page.getByText("initial commit")).toBeVisible();
  await expect(page.getByText("abc1234")).toBeVisible();
});

test("switching Files → Git tab works within the open panel", async ({ page }) => {
  await mockSessionAndPanel(page);
  await page.goto("/", { waitUntil: "domcontentloaded" });
  await openPanel(page, "Files");
  await expect(page.getByRole("button", { name: /README\.md/ })).toBeVisible({ timeout: 10000 });

  // The panel's own tab control (role=tab) switches to Git.
  await page.getByRole("tab", { name: "Git" }).click();
  await expect(page.getByText(/History \(1\)/)).toBeVisible({ timeout: 10000 });
  await expect(page.getByText("initial commit")).toBeVisible();
});

test("no_project_root renders the honest error copy", async ({ page }) => {
  await mockSessionAndPanel(page, { metaError: "no_project_root" });
  await page.goto("/", { waitUntil: "domcontentloaded" });
  await openPanel(page, "Files");

  await expect(page.getByText("No project root")).toBeVisible({ timeout: 10000 });
  await expect(
    page.getByText("This terminal was launched without a project root."),
  ).toBeVisible();
});
