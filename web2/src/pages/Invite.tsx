// Invite.tsx — admin-only page for minting a one-time enrolment token
// and surfacing it as a magic link + ready-to-share `observer enroll`
// command. M3.4 of the v1.8.0 teams remediation: removes the previous
// hard requirement to `docker exec` into the org container to mint a
// token.
//
// The page does NOT list provisioned users (no GET /api/org/members
// endpoint yet) — it takes a free-text user_id the admin already has
// from their SCIM provisioning step, the same UX as the existing
// `observer-org new-enrolment-token --user-id` CLI flow.

import { useState } from "react";
import { api, ApiError } from "@/lib/api";
import { Card, ErrorState, PageHeader } from "@/components/ui";

interface MintResult {
  token: string;
  token_id: string;
  user_id: string;
  expires_at: string;
}

export function InvitePage() {
  const [userId, setUserId] = useState("");
  const [ttlDays, setTtlDays] = useState(7);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [result, setResult] = useState<MintResult | null>(null);

  const origin = typeof window !== "undefined" ? window.location.origin : "";
  const magicLink = result ? `${origin}/enrol/${result.token}` : "";
  const enrolCommand = result ? `observer enroll --link ${magicLink}` : "";

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!userId.trim()) return;
    setBusy(true);
    setError(null);
    try {
      const r = await api.mintEnrolmentToken(userId.trim(), ttlDays);
      setResult(r);
    } catch (err) {
      const msg = err instanceof ApiError ? err.message : String(err);
      setError(msg);
    } finally {
      setBusy(false);
    }
  };

  const copy = async (text: string) => {
    try {
      await navigator.clipboard.writeText(text);
    } catch {
      // ignore; the operator can select the text manually
    }
  };

  return (
    <>
      <PageHeader
        title="Invite developer"
        subtitle="Mint a single-use enrolment token and share the magic link with a developer. The developer runs `observer enroll --link <url>`; their agent ships only post-enrolment activity."
      />

      <Card className="p-4">
        <form onSubmit={onSubmit} className="space-y-3">
          <div>
            <label className="block text-xs uppercase tracking-wide text-faint mb-1">SCIM user id</label>
            <input
              type="text"
              value={userId}
              onChange={(e) => setUserId(e.target.value)}
              placeholder="paste the SCIM-provisioned user_id (UUID)"
              className="w-full rounded border border-line bg-bg px-3 py-2 text-sm"
              required
              autoFocus
            />
            <p className="mt-1 text-[11px] text-faint">
              From the SCIM provisioning response, or `SELECT user_id FROM org_members WHERE email = ...`. The
              corresponding GET /api/org/members endpoint isn&apos;t in this release; pick the user manually for now.
            </p>
          </div>
          <div>
            <label className="block text-xs uppercase tracking-wide text-faint mb-1">TTL (days)</label>
            <input
              type="number"
              min={1}
              max={30}
              value={ttlDays}
              onChange={(e) => setTtlDays(Number(e.target.value))}
              className="w-32 rounded border border-line bg-bg px-3 py-2 text-sm"
            />
          </div>
          <button
            type="submit"
            disabled={busy || !userId.trim()}
            className="rounded bg-accent px-4 py-2 text-sm font-medium text-bg disabled:opacity-50"
          >
            {busy ? "Minting…" : "Mint enrolment token"}
          </button>
        </form>
      </Card>

      {error && <ErrorState message={error} />}

      {result && (
        <Card className="mt-4 p-4">
          <h2 className="text-sm font-medium">One-time enrolment token</h2>
          <p className="mt-1 text-xs text-faint">
            Shown once. Expires {new Date(result.expires_at).toLocaleString()}.
          </p>

          <div className="mt-3 space-y-3 text-sm">
            <div>
              <div className="text-[11px] uppercase tracking-wide text-faint mb-1">Magic link (share this)</div>
              <div className="flex items-center gap-2">
                <code className="flex-1 break-all rounded border border-line bg-bg px-2 py-1 text-xs">{magicLink}</code>
                <button
                  type="button"
                  onClick={() => copy(magicLink)}
                  className="rounded border border-line px-2 py-1 text-xs"
                >
                  Copy
                </button>
              </div>
            </div>
            <div>
              <div className="text-[11px] uppercase tracking-wide text-faint mb-1">Developer command</div>
              <div className="flex items-center gap-2">
                <code className="flex-1 break-all rounded border border-line bg-bg px-2 py-1 text-xs">{enrolCommand}</code>
                <button
                  type="button"
                  onClick={() => copy(enrolCommand)}
                  className="rounded border border-line px-2 py-1 text-xs"
                >
                  Copy
                </button>
              </div>
            </div>
            <div>
              <div className="text-[11px] uppercase tracking-wide text-faint mb-1">Raw token</div>
              <div className="flex items-center gap-2">
                <code className="flex-1 break-all rounded border border-line bg-bg px-2 py-1 text-xs">{result.token}</code>
                <button
                  type="button"
                  onClick={() => copy(result.token)}
                  className="rounded border border-line px-2 py-1 text-xs"
                >
                  Copy
                </button>
              </div>
              <p className="mt-1 text-[11px] text-faint">
                token_id <code>{result.token_id}</code> · user_id <code>{result.user_id}</code>
              </p>
            </div>
          </div>

          <p className="mt-4 rounded border border-line bg-panel/40 p-3 text-xs text-faint">
            <b>Privacy note (v1.8.0+):</b> the agent ships sha256 hashes for command bodies, assistant prose, and
            filesystem paths by default. To share raw content the developer must opt in via{" "}
            <code>[org_client.share].full_content = true</code> on their local config — an org admin can&apos;t flip
            this remotely.
          </p>
        </Card>
      )}
    </>
  );
}
