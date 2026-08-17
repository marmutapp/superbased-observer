// Governance — the node's read of GET /api/governance, mirroring
// internal/govern.Effective (internal/govern/types.go). On an ordinary
// solo node the daemon returns {active: false, ...} with empty arrays,
// and every helper below is a no-op / identity function in that case —
// that property IS the solo-node guarantee: nothing in this file may
// change behavior unless a node has actually been enrolled and granted
// dashboard.visibility authority.
import { useApi, type ApiState } from "./useApi";
import type { NavGroup } from "./nav";

export type GovernanceState =
  | "no_grant"
  | "grant_expired"
  | "identity_changed"
  | "key_pin_mismatch"
  | "no_policy"
  | "inert"
  | "applied";

export type GovernanceDropped = {
  directive: string;
  reason: string;
};

export type GovernanceNotice = {
  org_display_name?: string;
  contact?: string;
  policy_url?: string;
};

// GovernanceShareSource is the three-value enum the Privacy page's Source
// column renders. It is resolved AT THE BOUNDARY (GET /api/governance) and
// never composed in the SPA, so there is no code path here that could
// describe an organisation as having increased what is shared.
//
//   "you"  - the organisation published nothing for this key
//   "org"  - the organisation LOWERED this key below your setting
//   "both" - the organisation pinned this key at the value you had set
export type GovernanceShareSource = "you" | "org" | "both";

// GovernanceShareKey is one row of the /api/governance `share` block: the
// value in force, the value this machine's own config asks for, and which of
// the two decided it.
export type GovernanceShareKey = {
  effective: boolean | string[];
  local: boolean | string[];
  source: GovernanceShareSource;
  policy_version?: number;
};

export type Governance = {
  active: boolean;
  state: GovernanceState;
  version: number;
  org_name?: string;
  hidden_sections: string[];
  read_only_sections: string[];
  hidden_settings: string[];
  read_only_settings: string[];
  notice: GovernanceNotice;
  authority?: string[];
  unknown_authority?: string[];
  dropped?: GovernanceDropped[];
  expires_at?: string;
  hash?: string;
  // share is the Phase-1b sharing block: one entry per [org_client.share]
  // key the organisation published a directive for.
  //
  // SEAM: the endpoint does not emit it on every build yet. Absent means
  // "no org sharing directives", which resolves every key to source "you" -
  // the honest default, and the same answer a solo node gives forever.
  share?: Record<string, GovernanceShareKey>;
};

// shareSourceOf resolves one sharing key's Source column. Absent governance,
// an inactive grant, or a key the organisation published nothing for all
// resolve to "you" - never to a claim that the organisation touched it.
export function shareSourceOf(
  gov: Governance | null | undefined,
  key: string,
): GovernanceShareKey | null {
  if (!gov?.active) return null;
  return gov.share?.[key] ?? null;
}

// SHARE_SOURCE_LABEL is the column copy, in plain hyphens, as a function of
// the org label so the policy version lands in the right clause. There is
// deliberately no label here that says the organisation increased what is
// shared, because it structurally cannot - and a string that could say it
// would eventually be shown by a bug.
export const SHARE_SOURCE_LABEL: Record<
  GovernanceShareSource,
  (version: string) => string
> = {
  you: () => "You",
  org: (v) => `Your organisation${v} - reduced`,
  both: (v) => `You, locked by your organisation${v}`,
};

// shareSourceLabel renders the column, naming the policy version whenever
// the organisation is a source.
export function shareSourceLabel(row: GovernanceShareKey | null): string {
  if (!row) return SHARE_SOURCE_LABEL.you("");
  return SHARE_SOURCE_LABEL[row.source](
    row.policy_version ? ` (policy v${row.policy_version})` : "",
  );
}

// useGovernance fetches the node's resolved governance posture once
// per mount (no polling — the posture only changes on daemon restart
// or a fresh policy pull, neither of which happens mid-session).
export function useGovernance(): ApiState<Governance> {
  return useApi<Governance>("/api/governance");
}

// filterNavGroups drops hidden nav items (and any group left with zero
// items) from NAV_GROUPS. Identity when gov is null/inactive or
// hidden_sections is empty — the common case, so this never allocates
// on a solo node.
export function filterNavGroups(
  groups: NavGroup[],
  gov: Governance | null | undefined,
): NavGroup[] {
  if (!gov?.active || gov.hidden_sections.length === 0) return groups;
  const hidden = new Set(gov.hidden_sections);
  return groups
    .map((g) => ({ ...g, items: g.items.filter((it) => !hidden.has(it.id)) }))
    .filter((g) => g.items.length > 0);
}

export function isSectionHidden(
  gov: Governance | null | undefined,
  id: string,
): boolean {
  return !!gov?.active && gov.hidden_sections.includes(id);
}

export function isSectionReadOnly(
  gov: Governance | null | undefined,
  id: string,
): boolean {
  return !!gov?.active && gov.read_only_sections.includes(id);
}

export function isSettingsHidden(
  gov: Governance | null | undefined,
  id: string,
): boolean {
  return !!gov?.active && gov.hidden_settings.includes(id);
}

export function isSettingsReadOnly(
  gov: Governance | null | undefined,
  id: string,
): boolean {
  return !!gov?.active && gov.read_only_settings.includes(id);
}

// governedOrgLabel is the best available name for the governing
// organization, falling back through notice.org_display_name →
// org_name → a generic label so copy never renders an empty string.
export function governedOrgLabel(gov: Governance | null | undefined): string {
  return gov?.notice?.org_display_name || gov?.org_name || "your organization";
}
