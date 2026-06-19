// version.ts — fetch the latest published observer version from the
// npm registry and compare against the running daemon's version.
//
// Surfaces an "↑ vX.Y.Z available" pill in the TopBar when the
// daemon is behind. Quiet by design: network failures, dev builds,
// and pre-release versions all suppress the pill.

import { useEffect, useState } from "react";

// SESSION_KEY caches the npm probe across reloads in the same browser
// tab. 6h is well under our typical release cadence (multiple-per-day
// during arc weeks) but long enough that an open dashboard doesn't
// hammer the registry. Cleared when the tab closes.
const SESSION_KEY = "sb_latest_version";
const CACHE_TTL_MS = 6 * 60 * 60 * 1000;

// NPM_LATEST_URL serves a tiny JSON with `{version: "1.8.2"}`. CORS is
// enabled on registry.npmjs.org for browsers, so this is a direct
// frontend fetch with no proxy round-trip.
const NPM_LATEST_URL =
  "https://registry.npmjs.org/@superbased/observer/latest";

type CacheEntry = {
  version: string;
  fetchedAt: number; // epoch ms
};

function readCache(): CacheEntry | null {
  if (typeof window === "undefined") return null;
  try {
    const raw = window.sessionStorage.getItem(SESSION_KEY);
    if (!raw) return null;
    const parsed = JSON.parse(raw) as CacheEntry;
    if (typeof parsed?.version !== "string") return null;
    if (typeof parsed?.fetchedAt !== "number") return null;
    return parsed;
  } catch {
    return null;
  }
}

function writeCache(entry: CacheEntry): void {
  if (typeof window === "undefined") return;
  try {
    window.sessionStorage.setItem(SESSION_KEY, JSON.stringify(entry));
  } catch {
    // Quota / disabled — ignore. The pill just won't cache.
  }
}

// useLatestVersion returns the latest version published on npm, or
// null when unknown (still loading, network error, cache miss). Never
// throws. Fires the network request at most once per CACHE_TTL_MS per
// browser tab.
export function useLatestVersion(): string | null {
  const [latest, setLatest] = useState<string | null>(() => {
    const cached = readCache();
    if (!cached) return null;
    if (Date.now() - cached.fetchedAt > CACHE_TTL_MS) return null;
    return cached.version;
  });

  useEffect(() => {
    const cached = readCache();
    if (cached && Date.now() - cached.fetchedAt <= CACHE_TTL_MS) {
      // Fresh enough — already mirrored into state above; skip fetch.
      return;
    }
    let cancelled = false;
    fetch(NPM_LATEST_URL, { headers: { Accept: "application/json" } })
      .then((res) => (res.ok ? res.json() : null))
      .then((json: { version?: unknown } | null) => {
        if (cancelled) return;
        const v = typeof json?.version === "string" ? json.version : null;
        if (!v) return;
        writeCache({ version: v, fetchedAt: Date.now() });
        setLatest(v);
      })
      .catch(() => {
        // Silent. We don't want to clobber the topbar if npm is
        // unreachable or CORS is misbehaving in some user's browser.
      });
    return () => {
      cancelled = true;
    };
  }, []);

  return latest;
}

// compareSemver returns -1 / 0 / 1 for a < b / a == b / a > b across
// X.Y.Z[-suffix] strings. Suffix (pre-release tag) is stripped before
// comparing — pre-releases never trigger an "update available" pill,
// since we ship pre-releases only to ad-hoc tag pushes that operators
// shouldn't be alerted to. Returns null for any malformed input.
export function compareSemver(a: string, b: string): number | null {
  const pa = parseSemver(a);
  const pb = parseSemver(b);
  if (!pa || !pb) return null;
  for (let i = 0; i < 3; i++) {
    if (pa[i] < pb[i]) return -1;
    if (pa[i] > pb[i]) return 1;
  }
  return 0;
}

function parseSemver(v: string): [number, number, number] | null {
  if (!v) return null;
  // Strip leading 'v' and any pre-release suffix.
  const core = v.replace(/^v/, "").split(/[-+]/, 1)[0];
  const parts = core.split(".");
  if (parts.length < 3) return null;
  const nums: number[] = [];
  for (let i = 0; i < 3; i++) {
    const n = Number(parts[i]);
    if (!Number.isFinite(n) || n < 0) return null;
    nums.push(n);
  }
  return [nums[0], nums[1], nums[2]];
}

// isUpdateAvailable returns true when latest is a strict semver
// greater than current. Returns false for any non-comparable pair
// (dev build, missing version, malformed string) — defaults to "no
// pill" on uncertainty.
export function isUpdateAvailable(
  current: string | undefined | null,
  latest: string | undefined | null,
): boolean {
  if (!current || !latest) return false;
  if (current === "dev") return false;
  // Pre-release current versions (e.g. "1.8.2-rc.1") also skip — we
  // don't want pre-release builds nagging about a stable release that
  // they're effectively ahead of.
  if (/[-+]/.test(current)) return false;
  const cmp = compareSemver(current, latest);
  return cmp === -1;
}
