// Remote-vs-local detection for the dashboard SPA.
//
// The owner-trusted dashboard binds loopback only (a non-loopback direct bind
// is refused unless the [remote] substrate is armed, and the tailnet-serve
// backend is itself loopback but reached through the tailnet host). So the SPA
// is being viewed from a REMOTE-paired device exactly when the page's own host
// is NOT a loopback name/IP -- the same distinction the Go browserGuard draws
// server-side (hostIsLoopback). This needs no network round-trip and no new
// endpoint: the terminal-control UX branches on it to decide whether input is
// owner-local (automatic writer) or remote (read-only until an execute
// capability is acquired over the WS acquire-writer frame).

// isLoopbackHost reports whether a bare hostname/IP is a loopback address.
export function isLoopbackHost(host: string): boolean {
  const h = host.trim().toLowerCase().replace(/^\[|\]$/g, "");
  if (h === "localhost" || h === "::1" || h === "0:0:0:0:0:0:0:1") return true;
  // 127.0.0.0/8
  return /^127(?:\.\d{1,3}){3}$/.test(h);
}

// isRemoteView reports whether the current page is being served to a
// remote-paired device (non-loopback host) rather than the owner-local loopback
// dashboard. Safe in non-browser contexts (returns false when no location).
export function isRemoteView(): boolean {
  if (typeof window === "undefined" || !window.location) return false;
  return !isLoopbackHost(window.location.hostname);
}

const REMOTE_CSRF_KEY = "sb_remote_csrf";

export function getRemoteCSRF(): string {
  if (typeof window === "undefined") return "";
  return window.localStorage.getItem(REMOTE_CSRF_KEY) || "";
}

export function setRemoteCSRF(value: string): void {
  if (typeof window === "undefined") return;
  if (value) {
    window.localStorage.setItem(REMOTE_CSRF_KEY, value);
  } else {
    window.localStorage.removeItem(REMOTE_CSRF_KEY);
  }
}
