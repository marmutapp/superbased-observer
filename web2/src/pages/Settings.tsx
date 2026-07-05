import { useEffect, useState } from "react";
import { Trash2 } from "lucide-react";
import { api, ApiError, type BudgetInput, type Member } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { pct, usd } from "@/lib/format";
import { Badge, Button, Card, Empty, ErrorState, PageHeader, Spinner } from "@/components/ui";

export function SettingsPage() {
  return (
    <>
      <PageHeader title="Settings" subtitle="Budgets, enrolment, and identity configuration." />
      <div className="space-y-6">
        <BudgetsSection />
        <EnrolmentSection />
        <IdentitySection />
      </div>
    </>
  );
}

function inputClass() {
  return "rounded border border-line-2 bg-bg-0 px-2 py-1.5 text-sm text-fg-1 outline-none focus:border-accent";
}

function BudgetsSection() {
  const { data, error, loading, reload } = useApi(() => api.budgets(), []);
  const [form, setForm] = useState({ scope: "team", scope_id: "", cap: "", webhook: "" });
  const [busy, setBusy] = useState(false);
  const [formErr, setFormErr] = useState<string | null>(null);

  async function create() {
    setFormErr(null);
    const cap = parseFloat(form.cap);
    if (!form.scope_id || !(cap > 0)) {
      setFormErr("scope id and a positive cap are required");
      return;
    }
    const body: BudgetInput = {
      scope: form.scope as "team" | "project",
      scope_id: form.scope_id.trim(),
      monthly_usd_cap: cap,
      ...(form.webhook ? { alert_webhook_url: form.webhook.trim() } : {}),
    };
    setBusy(true);
    try {
      await api.createBudget(body);
      setForm({ scope: "team", scope_id: "", cap: "", webhook: "" });
      reload();
    } catch (e) {
      setFormErr(e instanceof ApiError ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  async function remove(id: string) {
    await api.deleteBudget(id).catch(() => undefined);
    reload();
  }

  const adminOnly = error && error.toLowerCase().includes("access");

  return (
    <Card>
      <div className="mb-3 text-sm font-medium text-fg-1">Budgets</div>
      {adminOnly ? (
        <p className="text-sm text-fg-3">Budgets are an admin-only surface.</p>
      ) : error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : loading || !data ? (
        <Spinner />
      ) : (
        <>
          {data.budgets.length === 0 ? (
            <Empty message="No budgets configured." />
          ) : (
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b border-line-2 text-left text-[11px] uppercase tracking-wide text-fg-3">
                  <th className="py-2 pr-3 font-medium">Scope</th>
                  <th className="px-3 py-2 text-right font-medium">Cap</th>
                  <th className="px-3 py-2 text-right font-medium">Spend</th>
                  <th className="px-3 py-2 text-right font-medium">Used</th>
                  <th className="px-3 py-2 font-medium">Webhook</th>
                  <th className="px-3 py-2" />
                </tr>
              </thead>
              <tbody className="divide-y divide-line-2">
                {data.budgets.map((b) => {
                  const tone = b.current_ratio >= 1 ? "bad" : b.current_ratio >= 0.9 ? "warn" : "good";
                  return (
                    <tr key={b.id}>
                      <td className="py-2 pr-3 text-fg-1">
                        <Badge>{b.scope}</Badge> <span className="ml-1">{b.scope_label}</span>
                      </td>
                      <td className="px-3 py-2 text-right font-mono text-fg-2">{usd(b.monthly_usd_cap)}</td>
                      <td className="px-3 py-2 text-right font-mono text-fg-1">{usd(b.current_spend_usd)}</td>
                      <td className="px-3 py-2 text-right">
                        <Badge tone={tone}>{pct(b.current_ratio)}</Badge>
                      </td>
                      <td className="px-3 py-2 text-fg-3">{b.alert_webhook_url ? "✓" : "—"}</td>
                      <td className="px-3 py-2 text-right">
                        <button onClick={() => remove(b.id)} title="Delete budget" className="text-fg-3 hover:text-danger">
                          <Trash2 className="h-4 w-4" />
                        </button>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          )}

          <div className="mt-4 border-t border-line-2 pt-4">
            <div className="mb-2 text-xs font-medium uppercase tracking-wide text-fg-3">New budget</div>
            <div className="flex flex-wrap items-end gap-2">
              <select className={inputClass()} value={form.scope} onChange={(e) => setForm({ ...form, scope: e.target.value })}>
                <option value="team">team</option>
                <option value="project">project</option>
              </select>
              <input
                className={inputClass()}
                placeholder={form.scope === "team" ? "team_id" : "project_root"}
                value={form.scope_id}
                onChange={(e) => setForm({ ...form, scope_id: e.target.value })}
              />
              <input
                className={inputClass()}
                placeholder="monthly cap (USD)"
                value={form.cap}
                onChange={(e) => setForm({ ...form, cap: e.target.value })}
              />
              <input
                className={`${inputClass()} min-w-[14rem] flex-1`}
                placeholder="alert webhook URL (optional)"
                value={form.webhook}
                onChange={(e) => setForm({ ...form, webhook: e.target.value })}
              />
              <Button variant="primary" onClick={create} disabled={busy}>
                {busy ? "Saving…" : "Add budget"}
              </Button>
            </div>
            {formErr && <p className="mt-2 text-xs text-danger">{formErr}</p>}
          </div>
        </>
      )}
    </Card>
  );
}

function EnrolmentSection() {
  const [userId, setUserId] = useState("");
  const [token, setToken] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  // GET /api/org/members is admin-only. On 403 (non-admin) or any error we
  // fall back to the free-text input so the section stays usable.
  const [members, setMembers] = useState<Member[] | null>(null);
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const r = await api.listOrgMembers();
        if (cancelled) return;
        setMembers(r.members);
        if (r.members.length > 0 && !userId) {
          setUserId(r.members[0].user_id);
        }
      } catch {
        if (cancelled) return;
        setMembers([]);
      }
    })();
    return () => {
      cancelled = true;
    };
    // Run once on mount.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  const useDropdown = members !== null && members.length > 0;

  async function mint() {
    setErr(null);
    setToken(null);
    if (!userId.trim()) {
      setErr("user id is required");
      return;
    }
    setBusy(true);
    try {
      const res = await api.mintEnrolmentToken(userId.trim());
      setToken(res.token);
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Card>
      <div className="mb-1 text-sm font-medium text-fg-1">Enrolment tokens</div>
      <p className="mb-3 text-xs text-fg-3">
        Mint a one-time token for a SCIM-provisioned user. The cleartext is shown once — hand it to the
        developer to run <span className="font-mono">observer enroll</span>.
      </p>
      <div className="flex flex-wrap items-end gap-2">
        {useDropdown ? (
          <select
            className={inputClass()}
            value={userId}
            onChange={(e) => setUserId(e.target.value)}
          >
            {members!.map((m) => (
              <option key={m.user_id} value={m.user_id}>
                {m.display_name && m.display_name !== m.email
                  ? `${m.email} — ${m.display_name}`
                  : m.email}
              </option>
            ))}
          </select>
        ) : (
          <input
            className={inputClass()}
            placeholder="SCIM user_id"
            value={userId}
            onChange={(e) => setUserId(e.target.value)}
          />
        )}
        <Button variant="primary" onClick={mint} disabled={busy}>
          {busy ? "Minting…" : "Mint token"}
        </Button>
      </div>
      {err && <p className="mt-2 text-xs text-danger">{err}</p>}
      {token && (
        <div className="mt-3 rounded border border-accent/40 bg-accent/10 p-3">
          <div className="text-[11px] uppercase tracking-wide text-accent">One-time token (copy now)</div>
          <code className="mt-1 block break-all font-mono text-xs text-fg-1">{token}</code>
        </div>
      )}
    </Card>
  );
}

function IdentitySection() {
  return (
    <Card>
      <div className="mb-1 text-sm font-medium text-fg-1">Identity (SCIM &amp; SAML)</div>
      <p className="text-xs text-fg-3">
        Users and teams are provisioned via SCIM 2.0; dashboard sign-in is SAML 2.0. Both the SCIM
        bearer token and the SAML SP keys are configured in the server config file
        (<span className="font-mono">/etc/observer-org/config.toml</span>) and rotated by replacing the
        on-disk secret and restarting — they are never editable from the dashboard.
      </p>
      <div className="mt-3 flex gap-2">
        <a href="/saml/metadata" className="text-xs text-accent hover:underline">
          SP metadata
        </a>
      </div>
    </Card>
  );
}
