// Invite.tsx — page for minting a one-time enrolment token and surfacing it
// as a magic link + ready-to-share `observer enroll` commands. M3.4 of the
// v1.8.0 teams remediation removed the need to `docker exec` into the org
// container to mint a token; v1.8.2 added the GET /api/org/members dropdown
// so an admin can pick a user instead of pasting a UUID.
//
// The dropdown calls /api/org/members, which is admin-only — non-admin
// callers get 403, and we fall back to the free-text input transparently
// (preserves the v1.8.0 UX for empty orgs or non-admin sessions).
//
// The Teams bottom-up invite arc made the page usable by a MEMBER as well:
// when the org sets [server].member_invites, any active member may mint. Two
// consequences visible here — the free-text field accepts an EMAIL (a member
// cannot read the member list, but knows their teammate's address), and the
// outstanding-tokens rail below renders only for admins (it names every
// invited developer, so it stays admin-only; its 403 is not an error state).

import { useEffect, useState } from "react";
import {
  api,
  ApiError,
  type EnrolmentTokenRow,
  type Member,
  type MintedEnrolmentToken,
} from "@/lib/api";
import { Card, ErrorState, PageHeader } from "@/components/ui";

export function InvitePage() {
  const [userId, setUserId] = useState("");
  const [ttlDays, setTtlDays] = useState(7);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [result, setResult] = useState<MintedEnrolmentToken | null>(null);

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
  // The enrol link embeds the LIVE token in a URL. It is kept because the CLI
  // consumes it directly (`observer enroll --link <url>`, cmd/observer/org.go),
  // but it is deliberately NOT rendered as an <a href>:
  //
  //   - there is no /enrol/:token route in this SPA (see App.tsx), so
  //     navigating it gains nothing at all; and
  //   - a live-credential URL that looks clickable gets pasted into chat and
  //     mail, where unfurlers, link scanners, and proxies fetch and LOG it.
  //
  // Copy-only text, with the command form kept primary.
  const enrolLink = result ? `${origin}/enrol/${result.token}` : "";
  const enrolCommandLink = result ? `observer enroll --link ${enrolLink}` : "";
  // The two-argument form, for a developer who would rather paste the org
  // URL and the token than follow a link. Identical outcome — the link IS
  // just those two values, so both are shown rather than picking for them.
  const enrolCommandPlain = result ? `observer enroll ${origin} ${result.token}` : "";

  // Use the dropdown only when members loaded AND returned at least one row.
  // A 403 (the member list is an admin-only read) or an empty list falls back
  // to the free-text field — which now accepts an EMAIL, the thing an
  // inviting member actually knows, as well as a raw SCIM user_id.
  const useDropdown = members !== null && members.length > 0;
  const isEmail = (s: string) => s.includes("@");

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    const target = userId.trim();
    if (!target) return;
    setBusy(true);
    setError(null);
    try {
      const r = await api.mintEnrolmentToken(
        isEmail(target) ? { email: target } : { userId: target },
        ttlDays,
      );
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
        subtitle="Mint a single-use enrolment token and share it with a developer who is already a member of this org. They run `observer enroll <org-url> <token>` (or the magic link); their agent ships only post-enrolment activity. An invite hands over a token — it does not create an account."
      />

      <Card className="p-4">
        <form onSubmit={onSubmit} className="space-y-3">
          <div>
            <label className="block text-xs uppercase tracking-wide text-fg-3 mb-1">
              Existing member
            </label>
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
                  placeholder="teammate@example.com — or a SCIM user_id (UUID)"
                  className="w-full rounded border border-line-2 bg-bg-0 px-3 py-2 text-sm"
                  required
                  autoFocus
                />
                <p className="mt-1 text-[11px] text-fg-3">
                  {membersError
                    ? `The member list is an admin-only read (${membersError}) — type your teammate's email instead. They must already be a member of this org.`
                    : members === null
                      ? "Loading members…"
                      : "No active members yet — type an email, or paste the user_id from your SCIM provisioning response."}
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
            <p className="mt-1 text-[11px] text-fg-3">
              How long the invite stays usable. This form offers 1–30; the server accepts 1–90 and
              rejects anything outside that. Shorter is better — an invite is a handoff, not a
              standing credential.
            </p>
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
            Shown once — the server keeps only a hash. Expires{" "}
            {new Date(result.expires_at).toLocaleString()}.
            {result.monthly_cap ? (
              <>
                {" "}
                You have used {result.minted_this_month} of {result.monthly_cap} invites this
                month.
              </>
            ) : null}
          </p>

          <p className="mt-2 rounded border border-line-2 bg-bg-2 p-2 text-xs text-fg-2">
            <b>Treat everything below like a password.</b> It is a live, single-use credential that
            enrols an agent as {result.user_email || "this member"} — anyone holding it can use it
            once, until it expires. Send it over a channel you would send a password over; don&apos;t
            post it in a shared chat, and don&apos;t email it as a clickable link (link previewers and
            mail scanners fetch URLs, which spends the invite).
          </p>

          <div className="mt-3 space-y-3 text-sm">
            <div>
              <div className="text-[11px] uppercase tracking-wide text-fg-3 mb-1">
                Developer command (paste-ready)
              </div>
              <div className="flex items-center gap-2">
                <code className="flex-1 break-all rounded border border-line-2 bg-bg-0 px-2 py-1 text-xs">{enrolCommandPlain}</code>
                <button
                  type="button"
                  onClick={() => copy(enrolCommandPlain)}
                  className="rounded border border-line-2 px-2 py-1 text-xs"
                >
                  Copy
                </button>
              </div>
            </div>
            <div>
              <div className="text-[11px] uppercase tracking-wide text-fg-3 mb-1">
                Enrol link (copy — not a link to click)
              </div>
              <div className="flex items-center gap-2">
                <code className="flex-1 break-all rounded border border-line-2 bg-bg-0 px-2 py-1 text-xs">{enrolLink}</code>
                <button
                  type="button"
                  onClick={() => copy(enrolLink)}
                  className="rounded border border-line-2 px-2 py-1 text-xs"
                >
                  Copy
                </button>
              </div>
              <p className="mt-1 text-[11px] text-fg-3">
                Shown as text on purpose — the token is IN the URL, and nothing here opens it.
                It exists for the CLI: <code className="break-all">{enrolCommandLink}</code>
              </p>
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

      <OutstandingTokens refreshKey={result?.token_id ?? ""} />
    </>
  );
}

// OutstandingTokens is the ADMIN rail: every enrolment token with its owner,
// its inviter, and whether it was redeemed — i.e. the invite→enrolment
// conversion view, computed server-side in the org's own DB.
//
// It is admin-only by design (it names every invited developer), so a 403 is
// the NORMAL answer for a member and renders nothing at all rather than an
// error: a member seeing "forbidden" on a page they are entitled to use would
// be a lie about their permissions. Only a non-403 failure is surfaced.
//
// refreshKey re-runs the read after a mint so a freshly minted token appears
// without a page reload.
function OutstandingTokens({ refreshKey }: { refreshKey: string }) {
  const [tokens, setTokens] = useState<EnrolmentTokenRow[] | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  const [visible, setVisible] = useState(true);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const r = await api.listEnrolmentTokens();
        if (cancelled) return;
        setTokens(r.tokens);
        setFailure(null);
      } catch (err) {
        if (cancelled) return;
        if (err instanceof ApiError && (err.status === 403 || err.status === 404)) {
          setVisible(false); // not an admin (or an older server) — show nothing
          return;
        }
        setFailure(err instanceof ApiError ? err.message : String(err));
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [refreshKey]);

  if (!visible) return null;
  if (failure) return <ErrorState message={`Couldn't load enrolment tokens: ${failure}`} />;
  if (tokens === null) return null;

  const redeemed = tokens.filter((t) => t.redeemed).length;

  return (
    <Card className="mt-4 p-4">
      <h2 className="text-sm font-medium">Enrolment tokens</h2>
      <p className="mt-1 text-xs text-fg-3">
        {tokens.length === 0
          ? "No tokens minted yet."
          : `${tokens.length} minted · ${redeemed} redeemed. Token secrets are never stored, so they can't be listed — only the non-secret id.`}
      </p>
      {tokens.length > 0 && (
        <div className="mt-3 overflow-x-auto">
          <table className="w-full text-left text-xs">
            <thead className="text-fg-3">
              <tr>
                <th className="py-1 pr-3 font-normal">For</th>
                <th className="py-1 pr-3 font-normal">Invited by</th>
                <th className="py-1 pr-3 font-normal">Minted</th>
                <th className="py-1 pr-3 font-normal">State</th>
              </tr>
            </thead>
            <tbody>
              {tokens.map((t) => (
                <tr key={t.token_id} className="border-t border-line-2">
                  <td className="py-1 pr-3">{t.user_email}</td>
                  <td className="py-1 pr-3 text-fg-3">{t.minted_by_email || "—"}</td>
                  <td className="py-1 pr-3 text-fg-3">
                    {new Date(t.created_at).toLocaleDateString()}
                  </td>
                  <td className="py-1 pr-3">
                    {t.redeemed ? "enrolled" : t.expired ? "expired" : "outstanding"}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Card>
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
