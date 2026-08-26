// Minimal API client for the Go dashboard backend.
//
// Endpoints live at /api/* on the same origin (production) or
// proxied to localhost:8820 in `vite dev`. The client returns
// parsed JSON; per-endpoint TypeScript shapes get added as pages
// start wiring real data in Phase 2+.

import { markRemoteAuthLost } from "@/lib/authLoss";
import { getRemoteCSRF, isRemoteView, setRemoteCSRF } from "@/lib/remote";
import type {
  SessionTagsRequest,
  SessionTagsResponse,
  TagManageRequest,
  TagManageResponse,
  TagRollupResponse,
} from "@/lib/types";

// QueryParams values may be a plain scalar (one `k=v`) or a string ARRAY,
// which serializes as a REPEATED key (`k=a&k=b`) rather than a joined value.
// The repeated form is what /api/sessions?tag=…&tag=… expects for its AND
// semantics; an empty array contributes nothing at all.
export type QueryParams = Record<
  string,
  string | number | boolean | string[] | undefined
>;

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
    if (Array.isArray(v)) {
      // Repeated key, one entry per element — `append`, never `set`, or the
      // last element would silently win over the rest.
      for (const item of v) {
        if (item === "") continue;
        qs.append(k, item);
      }
      continue;
    }
    qs.set(k, String(v));
  }
  const s = qs.toString();
  return s ? `${path}?${s}` : path;
}

function unsafeMethod(init?: RequestInit): boolean {
  const method = (init?.method || "GET").toUpperCase();
  return !["GET", "HEAD", "OPTIONS"].includes(method);
}

type WhoAmI = { authenticated?: boolean; csrf?: string };

// The auth endpoints are exempt from auth-loss detection: /pair 401s on a bad
// secret and /whoami is the probe itself, so treating either as evidence of a
// lost session would fight the pairing gate for the screen (or recurse).
const AUTH_PATHS = ["/api/remote/pair", "/api/remote/whoami"];

// The owner-local management routes use one readable double-submit cookie for
// every privileged panel. Several panels can mount together (Terminals loads
// launch policy + sandbox settings in parallel), and an older daemon may rotate
// that cookie on each GET. A token copied from a sibling response can therefore
// be stale by the time its Save button is clicked. The cookie is deliberately
// readable so the SPA can echo it; always prefer its CURRENT value at mutation
// time. Newer daemons also reuse a valid cookie, making both sides convergent.
const LOCAL_CONFIRM_COOKIE = "sb_remote_confirm";

function currentLocalConfirmToken(fallback: string): string {
  if (typeof document === "undefined") return fallback;
  const prefix = `${LOCAL_CONFIRM_COOKIE}=`;
  for (const part of document.cookie.split(";")) {
    const item = part.trim();
    if (!item.startsWith(prefix)) continue;
    const value = item.slice(prefix.length).trim();
    return value || fallback;
  }
  return fallback;
}

function isAuthEndpoint(path: string): boolean {
  return AUTH_PATHS.some((p) => path === p || path.startsWith(`${p}?`));
}

let whoamiInFlight: Promise<WhoAmI | null> | null = null;

// probeRemoteAuth asks the server who we are, SINGLE-FLIGHT: when a page fires
// twenty requests and all twenty 401, they share ONE /api/remote/whoami round
// trip and one verdict. Returns null when the answer is unknown (local view, or
// the probe itself failed) — an unknown answer is never treated as auth loss,
// so a blip can't flash a "session expired" screen.
function probeRemoteAuth(): Promise<WhoAmI | null> {
  if (!isRemoteView()) return Promise.resolve(null);
  if (whoamiInFlight) return whoamiInFlight;
  whoamiInFlight = (async () => {
    const res = await fetch("/api/remote/whoami", {
      headers: { Accept: "application/json" },
    }).catch(() => null);
    if (!res?.ok) return null;
    return (await res.json().catch(() => null)) as WhoAmI | null;
  })().finally(() => {
    whoamiInFlight = null;
  });
  return whoamiInFlight;
}

async function refreshRemoteCSRF(): Promise<string> {
  const body = await probeRemoteAuth();
  if (!body) return "";
  const csrf = body.authenticated ? body.csrf || "" : "";
  setRemoteCSRF(csrf);
  return csrf;
}

// checkRemoteAuth interprets a 401/403 on a remote-paired device. It CONFIRMS
// the loss with the whoami probe before latching it (requirement: one unlucky
// 401 must not produce a scary screen), and returns a fresh CSRF token when the
// session is in fact still good — which is the pre-existing rotated-CSRF
// recovery, now reachable from reads as well as writes.
async function checkRemoteAuth(): Promise<string> {
  const body = await probeRemoteAuth();
  if (!body) return ""; // probe failed ⇒ unknown ⇒ assume nothing
  if (body.authenticated) {
    const csrf = body.csrf || "";
    setRemoteCSRF(csrf);
    return csrf;
  }
  // Confirmed: the server does not know this device any more.
  setRemoteCSRF("");
  markRemoteAuthLost();
  return "";
}

export async function fetchJSON<T>(
  path: string,
  params?: QueryParams,
  init?: RequestInit,
): Promise<T> {
  const url = buildUrl(path, params);
  const remoteView = isRemoteView();
  const needsRemoteCSRF = unsafeMethod(init) && remoteView;
  let csrf = needsRemoteCSRF ? getRemoteCSRF() || (await refreshRemoteCSRF()) : "";
  const buildInit = (csrfValue: string): RequestInit => {
    const headers = new Headers(init?.headers);
    headers.set("Accept", "application/json");
    // Replace a response-captured owner-local confirm token with the latest
    // cookie value immediately before fetch. This closes cross-panel rotation
    // races without weakening the server's constant-time double-submit check.
    const suppliedConfirm = headers.get("X-Observer-Confirm");
    if (suppliedConfirm !== null) {
      const currentConfirm = currentLocalConfirmToken(suppliedConfirm);
      if (currentConfirm) headers.set("X-Observer-Confirm", currentConfirm);
    }
    if (needsRemoteCSRF && csrfValue) headers.set("X-Remote-CSRF", csrfValue);
    return { ...init, headers };
  };
  let res = await fetch(url, buildInit(csrf));
  // Auth recovery is gated on the REMOTE VIEW, not on the method. It used to be
  // gated on needsRemoteCSRF, which is false for GET/HEAD/OPTIONS — so a device
  // that had lost its session got a raw `api 401 …` on every read with no
  // recovery and no explanation at all.
  if (!res.ok && remoteView && !isAuthEndpoint(path) && (res.status === 401 || res.status === 403)) {
    const next = await checkRemoteAuth();
    if (needsRemoteCSRF && next && next !== csrf) {
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

// ---------- session classification (tags / favorites / notes) ----------
//
// Thin typed wrappers over fetchJSON so every caller inherits the remote
// CSRF + auth-recovery handling above rather than reaching for bare fetch.
// docs/plans/session-classification-tags-plan-2026-07-31.md §4.

const jsonPost = (body: unknown): RequestInit => ({
  method: "POST",
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify(body),
});

// fetchTagRollup returns the tag vocabulary plus per-tag session/cost/token
// rollup. Also the source of the TagEditor's suggestion list.
export function fetchTagRollup(signal?: AbortSignal): Promise<TagRollupResponse> {
  return fetchJSON<TagRollupResponse>("/api/sessions/tags", undefined, { signal });
}

// postSessionTags mutates one session's tags/favorite/note and returns the
// server's post-mutation truth. `favorite`/`note` default to null =
// "unchanged"; pass a value only when the mutation actually touches them.
export function postSessionTags(
  sessionId: string,
  patch: Partial<SessionTagsRequest>,
): Promise<SessionTagsResponse> {
  const body: SessionTagsRequest = {
    add: patch.add ?? [],
    remove: patch.remove ?? [],
    favorite: patch.favorite ?? null,
    note: patch.note ?? null,
    rating: patch.rating ?? null,
  };
  return fetchJSON<SessionTagsResponse>(
    `/api/session/${encodeURIComponent(sessionId)}/tags`,
    undefined,
    jsonPost(body),
  );
}

// manageTags renames or deletes a tag across every session that carries it.
// The request is rename XOR delete — the union type enforces it at the call
// site so a body carrying both can't be constructed.
export function manageTags(req: TagManageRequest): Promise<TagManageResponse> {
  return fetchJSON<TagManageResponse>(
    "/api/sessions/tags/manage",
    undefined,
    jsonPost(req),
  );
}
