import * as vscode from 'vscode';
import * as path from 'node:path';
import * as fs from 'node:fs/promises';
import * as tar from 'tar';
import AdmZip from 'adm-zip';
import {
  PLATFORM_KEY,
  PLATFORM_TO_ASSET,
  exeName,
  fileExists,
  httpGetFile,
  httpGetText,
  parseSha256Sums,
  readFileSafe,
  readVersion,
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

const RELEASE_BASE = 'https://github.com/marmutapp/superbased-observer/releases/download';

export async function resolveBinary(ctx: vscode.ExtensionContext): Promise<ResolvedBinary> {
  const fromSetting = vscode.workspace
    .getConfiguration('observer')
    .get<string>('binary.path');
  if (fromSetting && (await fileExists(fromSetting))) {
    return { path: fromSetting, version: await readVersion(fromSetting), source: 'setting' };
  }

  const onPath = await which('observer');
  if (onPath) {
    return { path: onPath, version: await readVersion(onPath), source: 'path' };
  }

  const bundled = path.join(ctx.extensionPath, 'bin', exeName());
  if (await fileExists(bundled)) {
    return { path: bundled, version: await readVersion(bundled), source: 'bundled' };
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

  if ((await fileExists(binPath)) && (await readFileSafe(sentinel)) === version) {
    output.appendLine(`Reusing cached binary at ${binPath}`);
    return { path: binPath, version, source: 'download' };
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
        await fs.writeFile(sentinel, version, 'utf8');
        await fs.rm(archivePath, { force: true });
      },
    );
  } catch (err) {
    await fs.rm(binPath, { force: true });
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
        vscode.Uri.parse('https://github.com/marmutapp/superbased-observer/issues/new'),
      );
    }
    throw err;
  }

  return { path: binPath, version, source: 'download' };
}
