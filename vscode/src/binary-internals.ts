// Pure helpers used by src/binary.ts.
//
// Kept in their own module so unit tests can import them without
// pulling in the `vscode` runtime (which only exists inside the
// Extension Development Host). resolveBinary + the download flow
// live in src/binary.ts where they belong; everything testable in
// isolation lives here.

import * as path from 'node:path';
import * as fs from 'node:fs/promises';
import * as fsSync from 'node:fs';
import * as crypto from 'node:crypto';
import * as https from 'node:https';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';

const exec = promisify(execFile);

export const PLATFORM_KEY = `${process.platform}-${process.arch}`;

// Mirrors the release asset names produced by
// .github/workflows/npm-release.yml (observer-v<version>-<asset>.{tar.gz,zip}).
// Keep this map in lock-step with the matrix in that workflow.
export const PLATFORM_TO_ASSET: Record<string, string> = {
  'linux-x64': 'linux-x64',
  'linux-arm64': 'linux-arm64',
  'darwin-x64': 'darwin-x64',
  'darwin-arm64': 'darwin-arm64',
  'win32-x64': 'win32-x64',
};

export function exeName(): string {
  return process.platform === 'win32' ? 'observer.exe' : 'observer';
}

export function parseSha256Sums(sums: string, target: string): string | undefined {
  for (const raw of sums.split(/\r?\n/)) {
    const line = raw.trim();
    if (!line || line.startsWith('#')) continue;
    const match = line.match(/^([0-9a-fA-F]{64})\s+\*?(\S+)$/);
    if (!match) continue;
    if (match[2] === target) return match[1].toLowerCase();
  }
  return undefined;
}

export async function sha256File(filePath: string): Promise<string> {
  const hash = crypto.createHash('sha256');
  await new Promise<void>((resolve, reject) => {
    const stream = fsSync.createReadStream(filePath);
    stream.on('data', (chunk) => hash.update(chunk));
    stream.on('end', () => resolve());
    stream.on('error', reject);
  });
  return hash.digest('hex').toLowerCase();
}

export async function httpGetText(url: string): Promise<string> {
  return (await httpGet(url)).toString('utf8');
}

export async function httpGetFile(url: string, dest: string): Promise<void> {
  await fs.writeFile(dest, await httpGet(url));
}

export function httpGet(url: string, redirectsLeft = 5): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    https
      .get(
        url,
        { headers: { 'User-Agent': 'superbased-observer-vscode' } },
        (res) => {
          // GitHub Releases serves a 302 to its S3 bucket; follow.
          if (
            res.statusCode &&
            res.statusCode >= 300 &&
            res.statusCode < 400 &&
            res.headers.location
          ) {
            if (redirectsLeft <= 0) {
              res.resume();
              return reject(new Error(`Too many redirects fetching ${url}`));
            }
            res.resume();
            httpGet(res.headers.location, redirectsLeft - 1).then(resolve, reject);
            return;
          }
          if (res.statusCode !== 200) {
            res.resume();
            return reject(new Error(`HTTP ${res.statusCode} fetching ${url}`));
          }
          const chunks: Buffer[] = [];
          res.on('data', (chunk) => chunks.push(chunk));
          res.on('end', () => resolve(Buffer.concat(chunks)));
          res.on('error', reject);
        },
      )
      .on('error', reject);
  });
}

export async function readVersion(binary: string): Promise<string> {
  try {
    const { stdout } = await exec(binary, ['--version'], { timeout: 5_000 });
    const match = stdout.match(/v?(\d+\.\d+\.\d+)/);
    return match ? match[1] : 'unknown';
  } catch {
    return 'unknown';
  }
}

export async function fileExists(p: string): Promise<boolean> {
  try {
    await fs.access(p);
    return true;
  } catch {
    return false;
  }
}

export async function readFileSafe(p: string): Promise<string | undefined> {
  try {
    return (await fs.readFile(p, 'utf8')).trim();
  } catch {
    return undefined;
  }
}

export async function which(
  cmd: string,
  env: NodeJS.ProcessEnv = process.env,
): Promise<string | undefined> {
  const pathEnv = env.PATH || env.Path;
  if (!pathEnv) return undefined;
  const isWin = process.platform === 'win32';
  const exts = isWin
    ? (env.PATHEXT || '.EXE;.CMD;.BAT;.COM').split(';').filter(Boolean)
    : [''];
  for (const dir of pathEnv.split(path.delimiter)) {
    if (!dir) continue;
    for (const ext of exts) {
      const candidate = path.join(dir, `${cmd}${ext}`);
      if (await fileExists(candidate)) {
        return candidate;
      }
    }
  }
  return undefined;
}
