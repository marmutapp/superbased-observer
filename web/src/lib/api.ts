// Minimal API client for the Go dashboard backend.
//
// Endpoints live at /api/* on the same origin (production) or
// proxied to localhost:8820 in `vite dev`. The client returns
// parsed JSON; per-endpoint TypeScript shapes get added as pages
// start wiring real data in Phase 2+.

import { getRemoteCSRF, isRemoteView, setRemoteCSRF } from "@/lib/remote";

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

function unsafeMethod(init?: RequestInit): boolean {
  const method = (init?.method || "GET").toUpperCase();
  return !["GET", "HEAD", "OPTIONS"].includes(method);
}

async function refreshRemoteCSRF(): Promise<string> {
  if (!isRemoteView()) return "";
  const res = await fetch("/api/remote/whoami", {
    headers: { Accept: "application/json" },
  }).catch(() => null);
  if (!res?.ok) return "";
  const body = (await res.json().catch(() => null)) as
    | { authenticated?: boolean; csrf?: string }
    | null;
  const csrf = body?.authenticated ? body.csrf || "" : "";
  setRemoteCSRF(csrf);
  return csrf;
}

export async function fetchJSON<T>(
  path: string,
  params?: QueryParams,
  init?: RequestInit,
): Promise<T> {
  const url = buildUrl(path, params);
  const needsRemoteCSRF = unsafeMethod(init) && isRemoteView();
  let csrf = needsRemoteCSRF ? getRemoteCSRF() || (await refreshRemoteCSRF()) : "";
  const buildInit = (csrfValue: string): RequestInit => {
    const headers = new Headers(init?.headers);
    headers.set("Accept", "application/json");
    if (needsRemoteCSRF && csrfValue) headers.set("X-Remote-CSRF", csrfValue);
    return { ...init, headers };
  };
  let res = await fetch(url, buildInit(csrf));
  if (!res.ok && needsRemoteCSRF && (res.status === 401 || res.status === 403)) {
    const next = await refreshRemoteCSRF();
    if (next && next !== csrf) {
      csrf = next;
      res = await fetch(url, buildInit(csrf));
    }
  }
  if (!res.ok) {
    const body = await res.text().catch(() => "");
    throw new ApiError(res.status, url, body);
  }
  return res.json() as Promise<T>;
}
