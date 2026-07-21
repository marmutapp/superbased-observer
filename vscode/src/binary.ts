import * as vscode from 'vscode';
import * as path from 'node:path';
import * as fs from 'node:fs/promises';
import * as tar from 'tar';
import AdmZip from 'adm-zip';
import {
  PLATFORM_KEY,
  PLATFORM_TO_ASSET,
  evaluateCachedBinary,
  exeName,
  fileExists,
  httpGetFile,
  httpGetText,
  isExecutableFile,
  parseSha256Sums,
  probeVersion,
  readFileSafe,
  resolveLocalBinary,
  sha256File,
  which,
} from './binary-internals';
import { output } from './output';

export interface ResolvedBinary {
  path: string;
  version: string;
  source: 'setting' | 'path' | 'bundled' | 'download';
}

// EXTENSION_VERSION is stamped at build time via esbuild --define.
declare const EXTENSION_VERSION: string;

const RELEASE_BASE = 'https://github.com/superbasedapp/observer/releases/download';

export async function resolveBinary(ctx: vscode.ExtensionContext): Promise<ResolvedBinary> {
  const config = vscode.workspace.getConfiguration('observer');
  const fromSetting = config.get<string>('binary.path');
  const preferPathBinary = config.get<boolean>('binary.preferPathBinary', false);

  // Bundled-first resolution (GitHub #5): checking the version-matched bundled
  // binary before scanning PATH avoids the multi-second which('observer') stall
  // observed on Windows (hundreds of sequential PATH×PATHEXT fs probes). The
  // explicit setting still wins; preferPathBinary restores the old PATH-first
  // order. See resolveLocalBinary for the ordered rule set.
  const resolution = await resolveLocalBinary({
    settingPath: fromSetting || undefined,
    bundledPath: path.join(ctx.extensionPath, 'bin', exeName()),
    preferPathBinary,
    fileExists: isExecutableFile,
    which,
    probeVersion,
  });

  // An explicit observer.binary.path that was rejected is surfaced loudly and
  // by name — the user asked for it, so an honest failure beats a silent swap.
  // Resolution still falls back (below) so the extension keeps working.
  if (resolution.settingError) {
    output.appendLine(resolution.settingError);
    void vscode.window.showErrorMessage(`Observer: ${resolution.settingError}`);
  }

  if (resolution.hit) {
    return {
      path: resolution.hit.path,
      version: resolution.hit.version,
      source: resolution.hit.source,
    };
  }

  return await downloadFromReleases(ctx);
}

async function downloadFromReleases(ctx: vscode.ExtensionContext): Promise<ResolvedBinary> {
  const asset = PLATFORM_TO_ASSET[PLATFORM_KEY];
  if (!asset) {
    throw new Error(
      `No observer binary available for ${PLATFORM_KEY}. ` +
        `Supported: ${Object.keys(PLATFORM_TO_ASSET).join(', ')}.`,
    );
  }
  const version = EXTENSION_VERSION;
  const isWin = asset.startsWith('win32');
  const archiveExt = isWin ? 'zip' : 'tar.gz';
  const archiveName = `observer-v${version}-${asset}.${archiveExt}`;
  const archiveUrl = `${RELEASE_BASE}/v${version}/${archiveName}`;
  const sumsUrl = `${RELEASE_BASE}/v${version}/SHA256SUMS`;

  const cacheRoot = path.join(ctx.globalStorageUri.fsPath, `v${version}`);
  await fs.mkdir(cacheRoot, { recursive: true });
  const binPath = path.join(cacheRoot, exeName());
  const sentinel = path.join(cacheRoot, '.version');

  // Cached-reuse gate. The old check was existence-only
  // (`fileExists(binPath) && sentinel === version`), so a directory /
  // non-executable / corrupt cached binary bypassed both acceptance gates and
  // was re-selected every activation without ever re-downloading. Reuse now
  // passes the SAME isExecutableFile + probeVersion gates as local resolution;
  // on rejection we invalidate the stale cache entry and fall through to a
  // fresh download.
  const cached = await evaluateCachedBinary({
    binPath,
    sentinelPath: sentinel,
    expectedVersion: version,
    isExecutableFile,
    readFileSafe,
    probeVersion,
  });
  if (cached.reuse) {
    output.appendLine(`Reusing cached binary at ${binPath}`);
    return { path: binPath, version, source: 'download' };
  }
  if ((await fileExists(binPath)) || (await readFileSafe(sentinel)) !== undefined) {
    output.appendLine(`Invalidating cached binary (${cached.reason}); re-downloading`);
    // recursive:true so a cached path that is a DIRECTORY is removed too.
    await fs.rm(binPath, { force: true, recursive: true });
    await fs.rm(sentinel, { force: true });
  }

  output.appendLine(`Downloading ${archiveName} from ${archiveUrl}`);
  try {
    await vscode.window.withProgress(
      {
        location: vscode.ProgressLocation.Notification,
        title: `Observer: downloading v${version} (${asset})`,
        cancellable: false,
      },
      async (progress) => {
        const archivePath = path.join(cacheRoot, archiveName);

        progress.report({ message: 'fetching SHA256SUMS' });
        const sumsText = await httpGetText(sumsUrl);
        const expected = parseSha256Sums(sumsText, archiveName);
        if (!expected) {
          throw new Error(`SHA256SUMS does not list ${archiveName}`);
        }

        progress.report({ message: 'downloading archive' });
        await httpGetFile(archiveUrl, archivePath);

        progress.report({ message: 'verifying checksum' });
        const actual = await sha256File(archivePath);
        if (actual !== expected) {
          await fs.rm(archivePath, { force: true });
          throw new Error(
            `Checksum mismatch for ${archiveName}: expected ${expected}, got ${actual}.`,
          );
        }

        progress.report({ message: 'extracting' });
        if (isWin) {
          new AdmZip(archivePath).extractAllTo(cacheRoot, true);
        } else {
          await tar.x({ file: archivePath, cwd: cacheRoot });
        }

        if (process.platform !== 'win32') {
          try {
            await fs.chmod(binPath, 0o755);
          } catch {
            /* permissions on a no-exec FS — let the exec fail later if it matters */
          }
        }

        // Post-extraction gate: apply the SAME acceptance checks as the
        // cached-reuse and local-resolution paths before blessing the download
        // with a sentinel. If the archive extracted something that is not a
        // runnable executable, throw into the catch below (cleanup + Retry
        // toast) rather than caching a corrupt binary that would be reused —
        // and re-rejected — on every subsequent activation.
        if (!(await isExecutableFile(binPath))) {
          throw new Error(`extracted binary at ${binPath} is not an executable file`);
        }
        const probe = await probeVersion(binPath);
        if (!probe.ok) {
          throw new Error(
            `extracted binary at ${binPath} failed --version (${probe.error ?? 'unknown'})`,
          );
        }

        await fs.writeFile(sentinel, version, 'utf8');
        await fs.rm(archivePath, { force: true });
      },
    );
  } catch (err) {
    await fs.rm(binPath, { force: true, recursive: true });
    await fs.rm(sentinel, { force: true });
    const msg = err instanceof Error ? err.message : String(err);
    const choice = await vscode.window.showErrorMessage(
      `Observer: download failed (${msg}).`,
      'Retry',
      'Open Issue',
    );
    if (choice === 'Retry') {
      return downloadFromReleases(ctx);
    }
    if (choice === 'Open Issue') {
      vscode.env.openExternal(
        vscode.Uri.parse('https://github.com/superbasedapp/observer/issues/new'),
      );
    }
    throw err;
  }

  return { path: binPath, version, source: 'download' };
}
