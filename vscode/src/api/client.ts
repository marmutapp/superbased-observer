// Typed HTTP client for the observer dashboard's /api/* surface.
//
// Pure Node (no vscode dependency) so unit tests can exercise it
// directly. Uses the global `fetch` (Node >= 18 — VS Code 1.90+ ships
// Node 20).

import type {
  CacheStatusResponse,
  CostResponse,
  DiscoverResponse,
  FileStateResponse,
  HeadlineResponse,
  HealthResponse,
  SessionsResponse,
  WatcherHealthResponse,
} from './types';

export interface ClientOptions {
  dashboardPort: number;
  host?: string; // defaults to 127.0.0.1
  fetchImpl?: typeof fetch;
  signalTimeoutMs?: number; // per-request timeout; default 5_000
}

export class Client {
  readonly host: string;
  readonly port: number;
  private readonly fetchImpl: typeof fetch;
  private readonly timeoutMs: number;

  constructor(opts: ClientOptions) {
    this.host = opts.host ?? '127.0.0.1';
    this.port = opts.dashboardPort;
    this.fetchImpl = opts.fetchImpl ?? fetch;
    this.timeoutMs = opts.signalTimeoutMs ?? 5_000;
  }

  url(pathAndQuery: string): string {
    const suffix = pathAndQuery.startsWith('/') ? pathAndQuery : `/${pathAndQuery}`;
    return `http://${this.host}:${this.port}${suffix}`;
  }

  async headline(days = 1): Promise<HeadlineResponse> {
    return this.get<HeadlineResponse>(`/api/analysis/headline?days=${days}`);
  }

  async sessions(limit = 20): Promise<SessionsResponse> {
    return this.get<SessionsResponse>(`/api/sessions?limit=${limit}`);
  }

  async discover(): Promise<DiscoverResponse> {
    return this.get<DiscoverResponse>(`/api/discover`);
  }

  async cost(days = 7, groupBy = 'model'): Promise<CostResponse> {
    return this.get<CostResponse>(
      `/api/cost?days=${days}&group-by=${encodeURIComponent(groupBy)}`,
    );
  }

  async cacheStatus(sessionId?: string): Promise<CacheStatusResponse> {
    const q = sessionId ? `?session=${encodeURIComponent(sessionId)}` : '';
    return this.get<CacheStatusResponse>(`/api/cache/status${q}`);
  }

  async fileState(absolutePath: string): Promise<FileStateResponse> {
    return this.get<FileStateResponse>(
      `/api/file/state?path=${encodeURIComponent(absolutePath)}`,
    );
  }

  async watcherHealth(): Promise<WatcherHealthResponse> {
    return this.get<WatcherHealthResponse>(`/api/health/watcher`);
  }

  async health(): Promise<HealthResponse> {
    // The Go server doesn't expose a dedicated /api/health right now,
    // but a fast HEAD on /api/analysis/headline works as a liveness
    // probe (returns 200 even if the analysis window is empty).
    const res = await this.fetchWithTimeout(this.url('/api/analysis/headline?days=1'), {
      method: 'GET',
    });
    return { ok: res.ok, status: res.status };
  }

  private async get<T>(pathAndQuery: string): Promise<T> {
    const res = await this.fetchWithTimeout(this.url(pathAndQuery), { method: 'GET' });
    if (!res.ok) {
      throw new Error(`Observer API ${pathAndQuery} → HTTP ${res.status} ${res.statusText}`);
    }
    return (await res.json()) as T;
  }

  private async fetchWithTimeout(input: string, init: RequestInit): Promise<Response> {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), this.timeoutMs);
    try {
      return await this.fetchImpl(input, { ...init, signal: controller.signal });
    } finally {
      clearTimeout(timer);
    }
  }
}

export interface BackoffOptions {
  // Fixed retry sequence in ms — first index is the delay BEFORE the
  // second attempt (the first attempt is immediate).
  delaysMs?: readonly number[];
  // Async sleep function — injectable so tests can run instantly.
  sleep?: (ms: number) => Promise<void>;
}

export const DEFAULT_BACKOFF_DELAYS = Object.freeze([200, 500, 1_000, 2_000, 5_000]);

const defaultSleep = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms));

/**
 * withBackoff runs `fn` with retries on rejection: attempt #1 fires
 * immediately, then waits delaysMs[0] before attempt #2, etc. Total
 * attempts = delays.length + 1. The last error propagates if every
 * attempt fails.
 */
export async function withBackoff<T>(
  fn: () => Promise<T>,
  opts: BackoffOptions = {},
): Promise<T> {
  const delays = opts.delaysMs ?? DEFAULT_BACKOFF_DELAYS;
  const sleep = opts.sleep ?? defaultSleep;
  let lastErr: unknown;
  for (let attempt = 0; attempt <= delays.length; attempt++) {
    try {
      return await fn();
    } catch (err) {
      lastErr = err;
      if (attempt === delays.length) break;
      await sleep(delays[attempt]);
    }
  }
  throw lastErr;
}
