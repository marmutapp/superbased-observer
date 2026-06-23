// Invite.tsx — admin-only page for minting a one-time enrolment token
// and surfacing it as a magic link + ready-to-share `observer enroll`
// command. M3.4 of the v1.8.0 teams remediation removed the need to
// `docker exec` into the org container to mint a token; v1.8.2 added the
// GET /api/org/members dropdown so an admin can pick a user instead of
// pasting a UUID.
//
// The dropdown calls /api/org/members, which is admin-only — non-admin
// callers get 403, and we fall back to the free-text input transparently
// (preserves the v1.8.0 UX for empty orgs or non-admin sessions).

import { useEffect, useState } from "react";
import { api, ApiError, type Member } from "@/lib/api";
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

  const [members, setMembers] = useState<Member[] | null>(null);
  const [membersError, setMembersError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const r = await api.listOrgMembers();
        if (cancelled) return;
        setMembers(r.members);
        // Pre-select the first member so the form is submit-ready.
        if (r.members.length > 0 && !userId) {
          setUserId(r.members[0].user_id);
        }
      } catch (err) {
        if (cancelled) return;
        const msg = err instanceof ApiError ? err.message : String(err);
        setMembersError(msg);
        setMembers([]);
      }
    })();
    return () => {
      cancelled = true;
    };
    // We intentionally run this once on mount.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const origin = typeof window !== "undefined" ? window.location.origin : "";
  const magicLink = result ? `${origin}/enrol/${result.token}` : "";
  const enrolCommand = result ? `observer enroll --link ${magicLink}` : "";

  // Use the dropdown only when members loaded AND returned at least one row.
  // 403 (non-admin) or an empty list falls back to the free-text input so the
  // page stays usable.
  const useDropdown = members !== null && members.length > 0;

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
            <label className="block text-xs uppercase tracking-wide text-fg-3 mb-1">SCIM user</label>
            {useDropdown ? (
              <>
                <select
                  value={userId}
                  onChange={(e) => setUserId(e.target.value)}
                  className="w-full rounded border border-line-2 bg-bg-0 px-3 py-2 text-sm"
                  required
                  autoFocus
                >
                  {members!.map((m) => (
                    <option key={m.user_id} value={m.user_id}>
                      {labelFor(m)}
                    </option>
                  ))}
                </select>
                <p className="mt-1 text-[11px] text-fg-3">
                  Active SCIM-provisioned users (sorted by email). Inactive users are filtered out.
                </p>
              </>
            ) : (
              <>
                <input
                  type="text"
                  value={userId}
                  onChange={(e) => setUserId(e.target.value)}
                  placeholder="paste the SCIM-provisioned user_id (UUID)"
                  className="w-full rounded border border-line-2 bg-bg-0 px-3 py-2 text-sm"
                  required
                  autoFocus
                />
                <p className="mt-1 text-[11px] text-fg-3">
                  {membersError
                    ? `Couldn't load the member list (${membersError}). Paste the user_id from your SCIM provisioning response.`
                    : members === null
                      ? "Loading members…"
                      : "No active members yet — paste the user_id from your SCIM provisioning response."}
                </p>
              </>
            )}
          </div>
          <div>
            <label className="block text-xs uppercase tracking-wide text-fg-3 mb-1">TTL (days)</label>
            <input
              type="number"
              min={1}
              max={30}
              value={ttlDays}
              onChange={(e) => setTtlDays(Number(e.target.value))}
              className="w-32 rounded border border-line-2 bg-bg-0 px-3 py-2 text-sm"
            />
          </div>
          <button
            type="submit"
            disabled={busy || !userId.trim()}
            className="rounded bg-accent px-4 py-2 text-sm font-medium text-accent-on disabled:opacity-50"
          >
            {busy ? "Minting…" : "Mint enrolment token"}
          </button>
        </form>
      </Card>

      {error && <ErrorState message={error} />}

      {result && (
        <Card className="mt-4 p-4">
          <h2 className="text-sm font-medium">One-time enrolment token</h2>
          <p className="mt-1 text-xs text-fg-3">
            Shown once. Expires {new Date(result.expires_at).toLocaleString()}.
          </p>

          <div className="mt-3 space-y-3 text-sm">
            <div>
              <div className="text-[11px] uppercase tracking-wide text-fg-3 mb-1">Magic link (share this)</div>
              <div className="flex items-center gap-2">
                <code className="flex-1 break-all rounded border border-line-2 bg-bg-0 px-2 py-1 text-xs">{magicLink}</code>
                <button
                  type="button"
                  onClick={() => copy(magicLink)}
                  className="rounded border border-line-2 px-2 py-1 text-xs"
                >
                  Copy
                </button>
              </div>
            </div>
            <div>
              <div className="text-[11px] uppercase tracking-wide text-fg-3 mb-1">Developer command</div>
              <div className="flex items-center gap-2">
                <code className="flex-1 break-all rounded border border-line-2 bg-bg-0 px-2 py-1 text-xs">{enrolCommand}</code>
                <button
                  type="button"
                  onClick={() => copy(enrolCommand)}
                  className="rounded border border-line-2 px-2 py-1 text-xs"
                >
                  Copy
                </button>
              </div>
            </div>
            <div>
              <div className="text-[11px] uppercase tracking-wide text-fg-3 mb-1">Raw token</div>
              <div className="flex items-center gap-2">
                <code className="flex-1 break-all rounded border border-line-2 bg-bg-0 px-2 py-1 text-xs">{result.token}</code>
                <button
                  type="button"
                  onClick={() => copy(result.token)}
                  className="rounded border border-line-2 px-2 py-1 text-xs"
                >
                  Copy
                </button>
              </div>
              <p className="mt-1 text-[11px] text-fg-3">
                token_id <code>{result.token_id}</code> · user_id <code>{result.user_id}</code>
              </p>
            </div>
          </div>

          <p className="mt-4 rounded border border-line-2 bg-bg-2 p-3 text-xs text-fg-3">
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

// labelFor renders an "<email> — <display_name>" label, omitting the dash
// when display_name is empty. Email is the stable primary key for humans
// (SCIM user_id is a UUID).
function labelFor(m: Member): string {
  if (m.display_name && m.display_name !== m.email) {
    return `${m.email} — ${m.display_name}`;
  }
  return m.email;
}
