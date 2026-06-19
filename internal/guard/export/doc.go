// Package export renders guard audit data for consumption OUTSIDE the
// observer: SIEM event export (guard spec §11.4 — JSONL and CEF) and
// the §14.4 compliance evidence pack (`observer guard report`).
//
// # Purity contract (spec §17.1 / CLAUDE.md module rule 1)
//
// This package performs NO I/O and imports ZERO observer packages (the
// internal/policy strongest-form precedent, pinned by an imports
// test): it consumes plain Event / ReportInput values and returns
// strings and bytes. Reading guard_events, opening files, and writing
// stdout all live at the cmd layer (cmd/observer/guardexport.go),
// which maps store rows onto [Event] at the boundary — the same
// translate-at-the-seam discipline as store.PersistGuardVerdicts, in
// the opposite direction.
//
// The §11.4 no-network invariant is structural here: a formatter
// cannot exfiltrate. File and stdout export stay network-free; the
// only network path for guard events is the doubly-gated OTel feed
// ([exporter.otel].enabled AND [guard.export].otel — see
// cmd/observer/guardotel.go), and §15.4 push alerting is the
// [guard.cloud] webhook surface that shipped with G15.
//
// # CEF dialect (ArcSight Common Event Format rev. 25)
//
// One line per event:
//
//	CEF:0|SuperBased|ObserverGuard|<version>|<rule_id>|<name>|<severity>|<extensions>
//
// Header fields escape backslash and pipe; extension values escape
// backslash, equals, and newlines (the asymmetry is the CEF spec's,
// not ours — pipes are legal inside extension values). The severity
// map is the spec §11.4 table: info→2 warn→4 medium→5 high→8
// critical→10. "medium" is reserved vocabulary — the live policy
// enum emits only info/warn/high/critical today — and an unknown or
// empty severity maps to 5 with the raw label preserved in the
// sboSeverity extension: visible mid-scale rather than silently
// dropped to 0 or falsely paged at 10. Extension order is fixed and
// documented at [CEFLine] so exports diff cleanly.
//
// # Evidence pack (§14.4)
//
// [BuildReport] assembles the operator-facing compliance report:
// effective policy at period start and end plus the in-period change
// log (computed from the append-only guard_policy_state load log —
// note the log records LOADS, so a policy file deleted mid-period
// remains listed until a later load supersedes its (layer, path);
// documented approximation), verdict statistics, the audit-chain
// verification result, the §6.5 coverage matrix, and the currently
// active approval grants (the §14.4 exception register). [RenderText]
// is the human form; the [Report] struct is the --json form. Field
// mappings against SOC 2 CC-series and NIST 800-53 AU-2/AU-3/AU-9/
// AC-6 live in docs/guard-compliance.md.
package export
