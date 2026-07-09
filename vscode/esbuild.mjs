// esbuild.mjs — VS Code extension bundler.
//
// Why esbuild over webpack: tiny config, fast cold start, the
// pattern every modern VS Code extension uses in 2026. Bundles all
// non-`vscode` imports into a single CommonJS file at out/extension.js
// so the VSIX ships one runtime artefact.
//
// EXTENSION_VERSION is stamped here so binary.ts can pin the
// download URL to the matching observer release tag without reading
// package.json at runtime.

import esbuild from 'esbuild';
import { readFileSync } from 'node:fs';

const pkg = JSON.parse(readFileSync(new URL('./package.json', import.meta.url), 'utf8'));
const production = process.argv.includes('--production');
const watch = process.argv.includes('--watch');

const config = {
  entryPoints: ['src/extension.ts'],
  bundle: true,
  outfile: 'out/extension.js',
  external: ['vscode'],
  format: 'cjs',
  platform: 'node',
  target: 'node20',
  sourcemap: !production,
  minify: production,
  logLevel: 'info',
  define: {
    EXTENSION_VERSION: JSON.stringify(pkg.version),
  },
};

if (watch) {
  const ctx = await esbuild.context(config);
  await ctx.watch();
  // eslint-disable-next-line no-console
  console.log('esbuild: watching for changes…');
} else {
  await esbuild.build(config);
}
