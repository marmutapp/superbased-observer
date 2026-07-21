# Benchmark harness (Phase 0 scaffold + Phase-0a pilot)

The pre-registration-grade A/B harness for the compression/context-savings
showcase (plan: `docs/plans/compression-savings-showcase-plan-2026-07-11.md`,
§3).

**Two runnable modes today:** `--dry-run` (spend-free — validates + enumerates
the plan) and `--pilot` (the Phase-0a live path that **spends real money**:
ONE paired block over the representative stratum, gated on a frozen
pre-registration + a mandatory `--budget-usd` cap). Everything else — the full
powered run — is still gated (`lib/common.sh` refuses any non-pilot live mode).
A checked-in pilot ledger + report already exist; see `../README.md`.

This is the compression SHELL harness — distinct from the Go `{harness ×
model}` rig (`internal/benchmark` + `observer benchmark`) and from
`internal/routing/benchmark.go` (the RouterBench score importer). See the
"Three different things are called benchmark" note in `../README.md`.

## Files

| File | Role | Runs in Phase 0? |
|---|---|---|
| `arms.toml` | Declarative protocol: arms / pairs / blocks, MPIE, power, cache regime, salts, cache-namespace isolation. No numbers. | read-only |
| `run.sh` | Blocked-pair runner. `--dry-run` (spend-free) + `--pilot` (Phase-0a, **spends money**, one block, needs `--budget-usd`) are implemented; the full powered live run is still refused by the gate guard. | `--dry-run` |
| `manifest.sh` | Drift/version manifest collector (§3.4): observer binary hash + version, harness commit, config/corpus hashes, pricing-table version. Local reads only. | yes (safe) |
| `extract.sh` | Result extractor. `--schema` prints the read-only `api_turns` cache-vector SQL; live `--db` mode gated to Phase 0a+. | `--schema` |
| `analyze.sh` | Analysis stage. `--plan` prints the declared statistics + decision gates; computation lands with the Phase 0a pilot. | `--plan` |
| `ledger.md` | Run-ledger format (all-null template; failed/aborted rows are retained with an exclusion reason). | n/a |
| `lib/common.sh` | Shared helpers: minimal flat-TOML reader, logging, the Phase 0a gate guard. Sourced, not run. | n/a |

## Dry-run (proves no execution)

```
benchmarks/harness/run.sh --dry-run
```

Parses `arms.toml`, checks each referenced task has a `[success]` gate and
a semantic assertion, checks every `claude-code` arm declares
`ENABLE_TOOL_SEARCH=true` (the §R7 rig invariant), enumerates
`blocks × tasks × arms`, and prints a NO-NETWORK / NO-SPEND banner. With
`blocks = 0` it reports "NOT POWERED — run the Phase 0a pilot" and still
exits 0 (validating the protocol shape is the point).

Any non-`--dry-run` invocation calls the Phase 0a gate guard and **dies**
with the gate message — there is deliberately no live-execution code in
this scaffold.

## Inherited rig invariants (codified so a confound reads as a refusal)

- **`ENABLE_TOOL_SEARCH=true`** on every Claude Code arm (else CC eager-
  inlines MCP schemas under `ANTHROPIC_BASE_URL`; a missing flag is rig
  error, not a result — R7). `run.sh --dry-run` refuses an undeclared CC arm.
- **Codex**: `codex exec` defaults read-only → editing tasks need
  `-s workspace-write`; add `--skip-git-repo-check`; give each arm a
  **sequential, distinct `CODEX_HOME`**. See the commented codex arm in
  `arms.toml`.
- **Salts**: documented, fixed-length, semantically-inert, EQUAL size
  across A/B within a pair (§3.2); cache namespaces isolate arms.
- **Timeout + WSL-hang watchdog** wrap each live agent session (Phase 0a+;
  not in this scaffold since nothing executes).
