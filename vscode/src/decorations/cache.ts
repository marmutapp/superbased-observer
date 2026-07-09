// Pure TTL cache keyed by absolute file path. Drives both the
// FileDecorationProvider and the HoverProvider so they share one
// /api/file/state fetch per file per TTL window.

import type { FileStateResponse } from '../api/types';

export interface CachedEntry {
  state: FileStateResponse;
  expiresAt: number;
}

export const DEFAULT_TTL_MS = 5 * 60 * 1000;

export class FileFreshnessCache {
  private readonly entries = new Map<string, CachedEntry>();
  private readonly inFlight = new Map<string, Promise<FileStateResponse>>();

  constructor(
    private readonly fetcher: (path: string) => Promise<FileStateResponse>,
    private readonly now: () => number = () => Date.now(),
    private readonly ttlMs: number = DEFAULT_TTL_MS,
  ) {}

  /** Synchronous lookup — returns undefined on miss + on expired. */
  peek(absolutePath: string): FileStateResponse | undefined {
    const entry = this.entries.get(absolutePath);
    if (!entry) return undefined;
    if (this.now() >= entry.expiresAt) {
      this.entries.delete(absolutePath);
      return undefined;
    }
    return entry.state;
  }

  /**
   * Returns the cached value when fresh; otherwise fetches and
   * populates the cache. Concurrent calls for the same path share a
   * single in-flight promise.
   */
  async get(absolutePath: string): Promise<FileStateResponse> {
    const cached = this.peek(absolutePath);
    if (cached) return cached;
    const inflight = this.inFlight.get(absolutePath);
    if (inflight) return inflight;
    const p = this.fetcher(absolutePath)
      .then((state) => {
        this.entries.set(absolutePath, {
          state,
          expiresAt: this.now() + this.ttlMs,
        });
        return state;
      })
      .finally(() => {
        this.inFlight.delete(absolutePath);
      });
    this.inFlight.set(absolutePath, p);
    return p;
  }

  /** Forget the cached entry for `path` so the next get() refetches. */
  invalidate(absolutePath: string): void {
    this.entries.delete(absolutePath);
  }

  /** Drop all entries. */
  clear(): void {
    this.entries.clear();
    this.inFlight.clear();
  }

  /** For tests only. */
  size(): number {
    return this.entries.size;
  }
}
