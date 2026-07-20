import { useEffect, useState } from "react";
import { ApiError, fetchJSON } from "@/lib/api";
import { isRemoteView } from "@/lib/remote";

// Shared gate for fresh-terminal launches from a paired REMOTE device. A remote
// launch requires the owner to have set [remote].allow_terminal = true; when it
// is off the backend refuses the launch POST with 403 "insufficient
// capability". This helper lets the UI surface that as an honest, actionable
// state instead of a raw server error — both proactively (disabled control with
// a reason) and reactively (mapping the 403 body).
//
// On a loopback (owner-local) dashboard the gate never applies — local launches
// are always permitted — so the hook does no work and reports allowed.

// Actionable copy shown when a remote device can't launch a terminal. It names
// the exact missing dependency ([remote].allow_terminal) and the two steps to
// fix it, matching the honest-disabled-control convention used elsewhere.
export const REMOTE_TERMINAL_OFF_MSG =
  "Remote terminal launch is off. On the owner's machine, turn on “Allow terminal” on the Remote page, then restart Observer (Settings → Health) so paired devices can launch terminals.";

// Actionable copy shown when a paired REMOTE device is refused a session RESUME
// with "insufficient capability". Resume is an execute-tier action that — unlike
// a fresh terminal launch — is NOT lowered by [remote].allow_terminal (the
// "Allow terminal" toggle on the Remote page), so a view-only device can't reach
// it. The honest remedy is the always-execute local dashboard; naming the exact
// gate (not the wrong "allow terminal view is off") keeps this in step with the
// honest-disabled-control convention. Distinct from REMOTE_TERMINAL_OFF_MSG,
// which is correct only for the lowered fresh-launch route.
export const RESUME_EXECUTE_REQUIRED_MSG =
  "Resume needs execute access, which this paired remote device doesn't have. It's an execute-tier action, and “Allow terminal” on the Remote page doesn't lower it — resume from the owner's local dashboard instead, which always has execute access.";

export type RemoteTerminalGate = {
  /** True when this page is served to a remote-paired device (non-loopback). */
  isRemote: boolean;
  /**
   * Whether [remote].allow_terminal is on. Always true on a local dashboard
   * (the gate only applies to remote devices); undefined while the remote
   * config is still loading.
   */
  allowTerminal: boolean | undefined;
  /** Convenience: the launch affordance should be blocked for THIS view. */
  blocked: boolean;
};

// useRemoteTerminalGate resolves whether a fresh terminal launch is permitted
// for the current view. On loopback it is always allowed and no request is
// made. On a remote device it reads allow_terminal from the already-existing
// GET /api/remote/config (View capability — no new endpoint). The read fails
// OPEN (treated as allowed) so a transient config hiccup never hides the
// affordance; the launch POST's own 403 stays the authoritative backstop.
export function useRemoteTerminalGate(): RemoteTerminalGate {
  const isRemote = isRemoteView();
  const [allowTerminal, setAllowTerminal] = useState<boolean | undefined>(
    isRemote ? undefined : true,
  );

  useEffect(() => {
    if (!isRemote) return;
    let cancelled = false;
    fetchJSON<{ allow_terminal?: boolean }>("/api/remote/config")
      .then((d) => {
        if (!cancelled) setAllowTerminal(!!d.allow_terminal);
      })
      .catch(() => {
        // Fail open — the 403 from the launch POST remains the honest backstop.
        if (!cancelled) setAllowTerminal(true);
      });
    return () => {
      cancelled = true;
    };
  }, [isRemote]);

  return { isRemote, allowTerminal, blocked: isRemote && allowTerminal === false };
}

// isTerminalCapabilityError reports whether an error thrown by the launch POST
// is the [remote].allow_terminal capability gate ("insufficient capability").
// Classification is MESSAGE-based only — never a bare status check — because
// the dashboard resume endpoint (internal/intelligence/dashboard/launch.go)
// reuses 403 for an UNRELATED gate (the [terminal.launch].allowed_project_roots
// policy denial, F6), and other unrelated failures can also surface as 403. A
// status-only check would misclassify those as "remote terminal off"; the
// caller can swap the raw server text for guidance only when the capability
// message itself is present.
export function isTerminalCapabilityError(e: unknown): boolean {
  const msg = e instanceof Error ? e.message : String(e);
  return /insufficient capability/i.test(msg);
}

// Actionable copy shown when a fresh launch is refused because the chosen
// project root is not in [terminal.launch].allowed_project_roots. It names the
// exact missing dependency and where to fix it, mirroring the honest-disabled
// convention used for the remote-terminal gate above.
export const PROJECT_ROOT_DENIED_MSG =
  "That project root isn't allow-listed. Add it to [terminal.launch].allowed_project_roots (Terminals page → launch policy, on the owner's local dashboard), then restart Observer — or launch in the agent's default directory.";

// isProjectRootDeniedError reports whether an error thrown by the launch POST is
// the [terminal.launch].allowed_project_roots gate ("project root not
// permitted" / "allowed_project_roots"), so the caller can swap the raw server
// text for the actionable guidance instead of a bare error body. The status
// check accepts BOTH 400 and 403: the fresh-launch route
// (POST /api/terminal/launch) maps ErrLaunchProjectRootDenied to 400 (malformed
// client input — the project_root came straight off the request body), while
// the resume route (POST /api/session/<id>/resume) maps the SAME sentinel to
// 403 (F6 — the project root is loaded server-side from the stored session, so
// a denial there is an authorization-policy refusal, not bad input). Message
// match is required either way; when no ApiError status is available (a
// network/parse failure) the message match alone decides.
export function isProjectRootDeniedError(e: unknown): boolean {
  const msg = e instanceof Error ? e.message : String(e);
  const matches = /project root not permitted|allowed_project_roots/i.test(msg);
  if (!matches) return false;
  if (e instanceof ApiError) {
    return e.status === 400 || e.status === 403;
  }
  return true;
}

// --- Standing terminal-control secret (device side, §B) ---
//
// The OPT-IN standing secret lets a paired device re-acquire terminal writer
// control across websocket refreshes WITHOUT a fresh per-terminal owner
// approval. It is a SINGLE, GLOBAL secret (it controls EVERY terminal), so a
// single browser-local key holds it — never per-terminal. Presented in the
// `cap` field of the acquire-writer frame (confirm empty) exactly like a
// one-time capability, distinguished server-side by its `standing.` prefix.

// STANDING_SECRET_LS_KEY is the localStorage key the standing secret lives under
// when the user opts to "Remember on this device". Deliberately explicit so it
// is greppable + easy to clear manually. Storing it here means control survives
// a refresh at the cost of the secret living in this browser (see the risk copy).
export const STANDING_SECRET_LS_KEY = "sb_standing_terminal_secret";

// STANDING_REMEMBER_RISK is the inline risk copy shown beside the "Remember on
// this device" opt-in — it must spell out the localStorage exposure honestly.
export const STANDING_REMEMBER_RISK =
  "Stores the secret in THIS browser's localStorage so control survives refreshes. Anyone with access to this device + browser can then drive every terminal until the owner revokes the secret. Leave off to use it once without saving.";

// STANDING_REVOKED_MSG is shown when a stored standing secret is rejected by the
// server (revoked or rotated). The stored secret is cleared and the device falls
// back to the normal single-use approval flow.
export const STANDING_REVOKED_MSG =
  "Standing access was revoked or rotated — the saved secret no longer works and has been cleared from this device. Ask the owner for a new standing secret, or use a one-time approval.";

// getStoredStandingSecret returns the browser-stored standing secret, or null.
// A standing secret always carries the `standing.` prefix; a value missing it is
// treated as absent (and left in place — never guess). localStorage access is
// guarded (private mode / disabled storage never throws to the caller).
export function getStoredStandingSecret(): string | null {
  try {
    const v = window.localStorage.getItem(STANDING_SECRET_LS_KEY);
    if (v && v.startsWith("standing.")) return v;
    return null;
  } catch {
    return null;
  }
}

// storeStandingSecret persists the standing secret for this device (best-effort;
// a storage failure is swallowed so the acquire still proceeds for this socket).
export function storeStandingSecret(secret: string): void {
  try {
    window.localStorage.setItem(STANDING_SECRET_LS_KEY, secret);
  } catch {
    /* storage disabled — the in-memory acquire still works for this socket */
  }
}

// forgetStandingSecret removes the browser-stored standing secret (the device
// "Forget" control, and the automatic clear on a server rejection).
export function forgetStandingSecret(): void {
  try {
    window.localStorage.removeItem(STANDING_SECRET_LS_KEY);
  } catch {
    /* ignore */
  }
}
