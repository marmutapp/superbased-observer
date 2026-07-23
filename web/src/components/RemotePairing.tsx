import { useEffect, useState, type ReactNode } from "react";
import { fetchJSON } from "@/lib/api";
import { isRemoteView, setRemoteCSRF } from "@/lib/remote";

// RemotePairingGate completes the "open the pairing link on your phone" flow.
// A pairing URL is `https://<tailnet-host>/#pair=<encoded-secret>` — the secret
// rides the URL FRAGMENT (never sent to the server as a query, never logged).
// The device that opens it must exchange that secret for a device-session
// cookie by POSTing it to /api/remote/pair (Public, body-only); without this
// handshake every View API call is anonymous and 401s. The gate runs ONCE on
// load: if a `#pair=` fragment is present it captures + strips it, pairs, and
// reloads so the whole app re-fetches authenticated. With no fragment (the
// local owner dashboard, or an already-paired device) it is a transparent
// pass-through.

// takePairingSecret reads the `#pair=<secret>` fragment and IMMEDIATELY strips
// it from the URL + history (defence: the secret must never linger in the
// address bar, back button, or a copied link). Returns null when absent.
function takePairingSecret(): string | null {
  const m = /^#pair=(.+)$/.exec(window.location.hash);
  if (!m) return null;
  const secret = m[1];
  history.replaceState(null, "", window.location.pathname + window.location.search);
  return secret;
}

type PairState = "pairing" | "error";

export function RemotePairingGate({ children }: { children: ReactNode }) {
  // Capture the secret during the first render pass (before any data fetch
  // fires), so a paired device never flashes anonymous 401s.
  const [secret] = useState<string | null>(takePairingSecret);
  const [state, setState] = useState<PairState | null>(secret ? "pairing" : null);
  const [msg, setMsg] = useState("");

  useEffect(() => {
    if (!secret) return;
    let cancelled = false;
    (async () => {
      try {
        const paired = await fetchJSON<{ csrf?: string }>("/api/remote/pair", undefined, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ secret }),
        });
        setRemoteCSRF(paired.csrf || "");
        if (cancelled) return;
        // The device-session cookie is now set. Reload so every useApi hook
        // re-fetches authenticated (the fragment is already stripped).
        window.location.reload();
      } catch {
        if (cancelled) return;
        setState("error");
        setMsg(
          "This pairing link didn't work — it may have expired or been rotated. Generate a fresh pairing link on the host dashboard (Remote → Enable / Rotate) and open it again.",
        );
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [secret]);

  useEffect(() => {
    if (secret || !isRemoteView()) return;
    let cancelled = false;
    (async () => {
      const who = await fetchJSON<{ authenticated?: boolean; csrf?: string }>(
        "/api/remote/whoami",
      ).catch(() => null);
      if (cancelled) return;
      setRemoteCSRF(who?.authenticated ? who.csrf || "" : "");
    })();
    return () => {
      cancelled = true;
    };
  }, [secret]);

  if (state === "pairing") {
    return (
      <PairingScreen
        title="Pairing this device…"
        body="Securely linking to your SuperBased dashboard over your tailnet."
      />
    );
  }
  if (state === "error") {
    return <PairingScreen title="Couldn't pair this device" body={msg} tone="error" />;
  }
  return <>{children}</>;
}

function PairingScreen({
  title,
  body,
  tone = "neutral",
}: {
  title: string;
  body: string;
  tone?: "neutral" | "error";
}) {
  return (
    <div className="flex min-h-screen items-center justify-center bg-bg-1 p-6">
      <div className="w-full max-w-sm space-y-3 rounded-3 border border-line-2 bg-bg-2 p-6 text-center">
        <div
          className={
            tone === "error"
              ? "text-[13px] font-semibold text-danger"
              : "text-[13px] font-semibold text-fg-1"
          }
        >
          {title}
        </div>
        {tone === "neutral" && (
          <div className="mx-auto h-6 w-6 animate-spin rounded-full border-2 border-line-2 border-t-accent" />
        )}
        <p className="text-[12px] leading-relaxed text-fg-3">{body}</p>
      </div>
    </div>
  );
}
