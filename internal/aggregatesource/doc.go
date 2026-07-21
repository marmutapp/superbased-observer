// Package aggregatesource is the read+map seam of the G25 aggregate rail
// (design docs/plans/g25-optin-aggregate-rail-design-2026-07-11.md §4.2/§6.2).
//
// It is the ONLY place that reads SQL for the rail: it runs the joint
// (model × tool) cost cut (cost.GroupByModelTool) over a finalized UTC month,
// maps each row into the pure package's aggregate.ModelToolStat DTO with the
// per-cell _acc/_est provenance rollup (accurate iff the joint cut's
// weakest-reliability is "accurate"), resolves each tool's cache/fast coverage
// booleans from the capability registry, and hands the DTOs to
// aggregate.Build. It never talks to the network and never touches
// internal/store/orgpush.go — the aggregate rail is a SIBLING of org-push,
// never an extension of it.
//
// It reads only api_turns / token_usage (via the shared cost engine); neither
// is a privacy-forbidden table, so this seam raises no privacy-sentinel
// concern.
package aggregatesource
