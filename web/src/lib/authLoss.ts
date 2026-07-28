// Remote auth-loss latch.
//
// A remote-paired device whose device-session cookie is gone (dropped by the
// browser, expired, or revoked on the host) gets a 401 on EVERY request —
// including plain reads. Before this latch existed the shell rendered and each
// page showed its own raw `api 401 /api/...` string, which reads as "the
// dashboard is broken" rather than "this device needs to pair again".
//
// The API layer confirms the loss ONCE (a single /api/remote/whoami probe) and
// then flips this latch; the pairing gate renders one full-screen prompt. The
// latch is the reason a hundred concurrent failing requests produce ONE prompt:
// the first transition notifies, every later mark is a no-op.
//
// Local (loopback) dashboards never authenticate, so nothing on that path ever
// marks a loss — the API layer only calls markRemoteAuthLost() under
// isRemoteView().

type Listener = () => void;

let lost = false;
const listeners = new Set<Listener>();

// isRemoteAuthLost reports the latched state (for a subscriber's initial value,
// so a listener registered after the transition still renders the prompt).
export function isRemoteAuthLost(): boolean {
  return lost;
}

// onRemoteAuthLost subscribes to the (single) loss transition. Returns an
// unsubscribe function.
export function onRemoteAuthLost(fn: Listener): () => void {
  listeners.add(fn);
  return () => {
    listeners.delete(fn);
  };
}

// markRemoteAuthLost latches the loss and notifies subscribers exactly once.
export function markRemoteAuthLost(): void {
  if (lost) return;
  lost = true;
  for (const fn of [...listeners]) fn();
}

// clearRemoteAuthLost un-latches — used when a re-check finds the device is in
// fact authenticated again (for example it was re-paired in another tab).
export function clearRemoteAuthLost(): void {
  lost = false;
}
