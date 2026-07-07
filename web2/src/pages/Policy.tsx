import { Fragment, useState } from "react";
import { api, ApiError, type GuardPolicyBundleDetail, type GuardPolicyLintResult } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { dateTime, num } from "@/lib/format";
import { Badge, Button, Card, Empty, ErrorState, PageHeader, Spinner } from "@/components/ui";

const PLACEHOLDER = `# Org guard-policy bundle (§4.4 TOML; merged on every agent as the strictness floor)
[[override]]
rule = "R-110"
decision = "deny"
enforce = true
`;

// BundleViewer fetches and renders one version's TOML on demand.
function BundleViewer({ version }: { version: number }) {
  const { data, error, loading } = useApi<GuardPolicyBundleDetail>(
    () => api.guardPolicyBundleDetail(version),
    [version],
  );
  if (error) return <div className="py-2 text-sm text-danger">{error}</div>;
  if (loading || !data) return <Spinner label="Loading bundle…" />;
  return (
    <pre className="overflow-x-auto rounded border border-line-2 bg-bg-3 p-3 font-mono text-xs text-fg-1">
      {data.bundle_toml}
    </pre>
  );
}

// AuthorPanel is the policy_admin authoring surface: draft → lint (dry-run
// stats) → publish through the same server-side gate as the CLI. A 403 from
// lint/publish means the caller lacks the policy_admin role; the panel
// surfaces that instead of hiding itself (roles are config, not introspectable
// client-side).
function AuthorPanel({
  signingConfigured,
  onPublished,
}: {
  signingConfigured: boolean;
  onPublished: () => void;
}) {
  const [toml, setToml] = useState("");
  const [description, setDescription] = useState("");
  const [lint, setLint] = useState<GuardPolicyLintResult | null>(null);
  const [busy, setBusy] = useState<"lint" | "publish" | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [published, setPublished] = useState<number | null>(null);

  const friendly = (e: unknown) =>
    e instanceof ApiError && e.status === 403
      ? "Authoring requires the policy_admin role ([dashboard].policy_admin_emails)."
      : String(e);

  const runLint = () => {
    setBusy("lint");
    setError(null);
    setPublished(null);
    api
      .guardPolicyLint(toml)
      .then(setLint)
      .catch((e: unknown) => setError(friendly(e)))
      .finally(() => setBusy(null));
  };

  const runPublish = () => {
    setBusy("publish");
    setError(null);
    api
      .guardPolicyPublish(toml, description || undefined)
      .then((r) => {
        setPublished(r.version);
        setLint(null);
        setToml("");
        setDescription("");
        onPublished();
      })
      .catch((e: unknown) => setError(friendly(e)))
      .finally(() => setBusy(null));
  };

  return (
    <Card>
      <div className="mb-2 text-sm font-medium text-fg-1">Author a bundle version</div>
      {!signingConfigured && (
        <div className="mb-3 rounded border border-warn/40 bg-warn/10 p-2 text-xs text-warn">
          No policy signing key is configured ([policy].signing_key_path) — the channel is off and
          publishing is disabled. Generate one with <span className="font-mono">observer-org policy keygen</span>.
        </div>
      )}
      <textarea
        value={toml}
        onChange={(e) => setToml(e.target.value)}
        placeholder={PLACEHOLDER}
        spellCheck={false}
        rows={10}
        className="w-full rounded border border-line-2 bg-bg-3 p-3 font-mono text-xs text-fg-1 placeholder:text-fg-3 focus:border-accent focus:outline-none"
      />
      <input
        value={description}
        onChange={(e) => setDescription(e.target.value)}
        placeholder="Version note (shown in the history)"
        className="mt-2 w-full rounded border border-line-2 bg-bg-3 px-3 py-2 text-xs text-fg-1 placeholder:text-fg-3 focus:border-accent focus:outline-none"
      />
      <div className="mt-3 flex items-center gap-2">
        <Button onClick={runLint} disabled={!toml || busy !== null}>
          {busy === "lint" ? "Linting…" : "Lint + dry-run"}
        </Button>
        <Button
          variant="primary"
          onClick={runPublish}
          disabled={!toml || busy !== null || !signingConfigured || (lint !== null && !lint.ok)}
          title="Lint + sign + insert in one transaction — the same gate as `observer-org policy publish`. Audited."
        >
          {busy === "publish" ? "Publishing…" : "Publish (audited)"}
        </Button>
        {published !== null && <Badge tone="good">published v{published}</Badge>}
      </div>
      {error && <div className="mt-2 text-sm text-danger">{error}</div>}
      {lint && (
        <div className="mt-3 space-y-2">
          {lint.ok ? (
            <Badge tone="good">lints clean as an org bundle</Badge>
          ) : (
            <div className="space-y-1">
              <Badge tone="bad">refused by the org-layer lint</Badge>
              <ul className="list-inside list-disc text-xs text-danger">
                {lint.problems.map((p, i) => (
                  <li key={i} className="font-mono">{p}</li>
                ))}
              </ul>
            </div>
          )}
          {lint.dry_run.length > 0 && (
            <div>
              <div className="mb-1 text-xs font-medium text-fg-2">
                Dry-run — events in the last {lint.window_days}d each referenced rule would have affected:
              </div>
              <table className="w-full text-xs">
                <tbody className="divide-y divide-line-2">
                  {lint.dry_run.map((d) => (
                    <tr key={d.rule_id}>
                      <td className="px-2 py-1.5 font-mono text-fg-1">{d.rule_id}</td>
                      <td className="px-2 py-1.5 text-right font-mono text-fg-2">
                        {d.computable ? `${num(d.hits)} hits · ${num(d.agents)} agents` : "—"}
                      </td>
                      <td className="px-2 py-1.5 text-fg-3">
                        {d.computable
                          ? "override of a known rule"
                          : "new rule — no server-side history to dry-run against"}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      )}
    </Card>
  );
}

export function PolicyPage() {
  const [openVersion, setOpenVersion] = useState<number | null>(null);
  const { data, error, loading, reload } = useApi(() => api.guardPolicyBundles(), []);

  return (
    <>
      <PageHeader
        title="Policy"
        subtitle="Org guard-policy bundles: signed, versioned, distributed to every enrolled agent as an escalate-only strictness floor (§14.2)."
      />
      {error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : loading || !data ? (
        <Spinner />
      ) : (
        <div className="space-y-5">
          <div className="flex items-center gap-3 text-sm text-fg-2">
            {data.active_version > 0 ? (
              <>
                <Badge tone="accent">active: v{data.active_version}</Badge>
                <span>Agents poll the latest version and verify its signature against the pinned org key.</span>
              </>
            ) : (
              <span>No bundle published yet — agents run local-only policy.</span>
            )}
          </div>

          <AuthorPanel signingConfigured={data.signing_configured} onPublished={reload} />

          <Card className="p-0">
            <div className="border-b border-line-2 px-4 py-2.5 text-sm font-medium text-fg-1">Version history</div>
            {data.bundles.length === 0 ? (
              <Empty message="No versions yet." />
            ) : (
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b border-line-2 text-left text-[11px] uppercase tracking-wide text-fg-3">
                    <th className="px-4 py-2.5 font-medium">Version</th>
                    <th className="px-4 py-2.5 font-medium">Signed</th>
                    <th className="px-4 py-2.5 font-medium">By</th>
                    <th className="px-4 py-2.5 font-medium">Note</th>
                    <th className="px-4 py-2.5 text-right font-medium">Size</th>
                    <th className="px-4 py-2.5"></th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-line-2">
                  {data.bundles.map((b) => (
                    <Fragment key={b.version}>
                      <tr>
                        <td className="px-4 py-2.5 font-mono text-fg-1">
                          v{b.version}
                          {b.version === data.active_version && (
                            <span className="ml-2">
                              <Badge tone="good">active</Badge>
                            </span>
                          )}
                        </td>
                        <td className="px-4 py-2.5 text-fg-2">{dateTime(b.signed_at)}</td>
                        <td className="px-4 py-2.5 text-fg-2">{b.created_by || "—"}</td>
                        <td className="px-4 py-2.5 text-fg-2">{b.description || "—"}</td>
                        <td className="px-4 py-2.5 text-right font-mono text-fg-3">{num(b.toml_bytes)} B</td>
                        <td className="px-4 py-2.5 text-right">
                          <Button
                            onClick={() => setOpenVersion(openVersion === b.version ? null : b.version)}
                          >
                            {openVersion === b.version ? "Hide" : "View"}
                          </Button>
                        </td>
                      </tr>
                      {openVersion === b.version && (
                        <tr>
                          <td colSpan={6} className="px-4 py-3">
                            <BundleViewer version={b.version} />
                          </td>
                        </tr>
                      )}
                    </Fragment>
                  ))}
                </tbody>
              </table>
            )}
          </Card>
        </div>
      )}
    </>
  );
}
