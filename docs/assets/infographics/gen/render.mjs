// Render the README hero infographics from the HTML sources in this
// directory. Replaces the pre-2026-07-30 one-off images that had no
// regeneration source (repo-landing hygiene review P0-1).
//
// Prereqs: `cd web && npm ci` (supplies playwright + the Inter woff2s).
// Usage:   node docs/assets/infographics/gen/render.mjs
//
// Output lands in docs/assets/infographics/*.png at 2x device scale.
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import fs from 'node:fs';

const here = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(here, '..', '..', '..', '..');
const require = createRequire(path.join(repoRoot, 'web', 'package.json'));
const { chromium } = require('playwright');

const interDir = path.join(
  repoRoot, 'web', 'node_modules', '@fontsource', 'inter', 'files');
const b64 = (p, mime) =>
  `data:${mime};base64,${fs.readFileSync(p).toString('base64')}`;
const font = (w) =>
  b64(path.join(interDir, `inter-latin-${w}-normal.woff2`), 'font/woff2');

const vars = {
  '{{FONT_400}}': font(400),
  '{{FONT_500}}': font(500),
  '{{FONT_600}}': font(600),
  '{{FONT_700}}': font(700),
  '{{FONT_800}}': font(800),
  '{{LOGO}}': b64(path.join(repoRoot, 'icons', 'logo-dark.png'), 'image/png'),
};

const pages = [
  { src: 'one-local-path.html', out: 'one-local-path.png', w: 1672, h: 941 },
  { src: 'intelligence-across-tools.html', out: 'intelligence-across-tools.png', w: 1569, h: 1002 },
];

const browser = await chromium.launch();
for (const { src, out, w, h } of pages) {
  let html = fs.readFileSync(path.join(here, src), 'utf8');
  for (const [k, v] of Object.entries(vars)) html = html.replaceAll(k, v);
  const page = await browser.newPage({
    viewport: { width: w, height: h }, deviceScaleFactor: 2,
  });
  await page.setContent(html, { waitUntil: 'networkidle' });
  await page.waitForTimeout(300);
  const outPath = path.join(here, '..', out);
  await page.screenshot({ path: outPath });
  console.log('rendered', path.relative(repoRoot, outPath), `${w}x${h}@2x`);
  await page.close();
}
await browser.close();
