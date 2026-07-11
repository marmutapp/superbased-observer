// Minimal API client for the Go dashboard backend.
//
// Endpoints live at /api/* on the same origin (production) or
// proxied to localhost:8820 in `vite dev`. The client returns
// parsed JSON; per-endpoint TypeScript shapes get added as pages
// start wiring real data in Phase 2+.

export type QueryParams = Record<string, string | number | boolean | undefined>;

export class ApiError extends Error {
  constructor(
    public readonly status: number,
    public readonly path: string,
    body: string,
  ) {
    super(`api ${status} ${path}: ${body.slice(0, 200)}`);
  }
}

function buildUrl(path: string, params?: QueryParams): string {
  if (!params) return path;
  const qs = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === "" || v === false) continue;
    qs.set(k, String(v));
  }
  const s = qs.toString();
  return s ? `${path}?${s}` : path;
}

export async function fetchJSON<T>(
  path: string,
  params?: QueryParams,
  init?: RequestInit,
): Promise<T> {
  const url = buildUrl(path, params);
  const res = await fetch(url, {
    ...init,
    headers: {
      Accept: "application/json",
      ...(init?.headers ?? {}),
    },
  });
  if (!res.ok) {
    const body = await res.text().catch(() => "");
    throw new ApiError(res.status, url, body);
  }
  return res.json() as Promise<T>;
}
