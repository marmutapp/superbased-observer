# Phase-0a pilot report — tool-defs trim — 2026-07-12

> **INTERNAL PILOT. NOT RESULTS. NOT PUBLISHABLE.**
>
> Per the frozen pre-registration (`tool-defs-trim-2026-07-11.md`, plan §8 /
> §6 step 4), this pilot exists to (a) validate the pipeline end-to-end
> (token/cost reconciliation, cache classification, no-op detection,
> quality-guard capture, artifact completeness) and (b) supply the per-pair
> cost-delta variance **σ_d** that powers the full run's block count. **No
> number in this file is a claim, a headline, or a result.** The prereg
> forbids treating pilot numbers as results; the full pre-registered run is
> the only source of a publishable number.

## 1. What ran

- **Card:** tool-defs-trim (the `"tools"` sentinel), warm steady-state regime.
- **Design:** ONE paired block over the 5 frozen representative tasks; both
  arms per task; order randomized within each pair (deterministic on
  block-index + task per `arms.toml seed_source`); sequential execution
  (never parallel sessions); per-session timeout + ledger rows for every
  attempt including failures.
- **Arms** (isolated per-workspace via `.observer/config.toml` overlays —
  the daemon was never restarted or reconfigured; `config.ProjectCompression`
  resolves the overlay from the session CWD via the pidbridge):
  - **A-control:** `[compression.conversation] enabled = false` → full tool
    definitions every turn, no conversation compression. (A project overlay
    can only turn conversation compression OFF, never on — so the control is
    enforceable without touching the daemon master config.)
  - **B-toolsdefs-trim:** `enabled = true`, `compress_types = ["tools"]`,
    stash off → the ONLY difference from A is the tool-defs trim.
- **Launcher:** `observer claude` (Pro/Max OAuth re-export, ANTHROPIC_BASE_URL
  → proxy :8820) with `ENABLE_TOOL_SEARCH=true` on every session (R7).
- **Model:** claude CLI alias `sonnet`; **returned model identifier from
  `api_turns`: `claude-sonnet-5`** (undated — see anomalies §7.4).
- **Cost source:** `api_turns` whole-task cache vector per session
  (`extract.sh --db`, read-only, scoped to the exact `session_id` returned by
  the SDK result event + `id > pre-launch max` guard), cross-checked against
  the SDK `total_cost_usd`.
- **Budget enforcement:** hard $15 cap, checked after every session (and
  pre-flight before each launch against the per-session cap). Never
  approached: total ≈ **$1.07** (see §5).

Artifacts:

- Ledger (13 rows, incl. 1 failure + 1 orphan-attribution + block-2 re-run):
  `benchmarks/runs/tool-defs-trim-2026-07-12.jsonl`
- Drift manifest: `benchmarks/runs/tool-defs-trim-2026-07-12.manifest.json`
- Harness: `benchmarks/harness/run.sh --pilot` (implemented this session),
  `extract.sh --db`, `analyze.sh --pilot`.

## 2. Freeze confirmations (pre-run)

Frozen **before** any live session, at `2026-07-11T20:18:01Z` machine UTC
(operator session 2026-07-12, approval "operator approved via session
2026-07-12", hard $15 cap):

| Component | Hash |
|---|---|
| corpus manifest (`corpus/manifest.json`) | `892e4afbbb5f5cb33e7d53af8d90801d11640d89ccb906fa7114541474b66bdb` |
| `arms.toml` | `9e0d8e048888d48ccd02d5c1925dabd84d718a9930e7e75f31450e3c66df72cc` |
| observer binary | `c7071fd6365cc4d5b424f41a52022cf6c804b12b5d735cad1128f90931fd0433` |
| pricing source (`pricing.go`, "as of 2026-04") | `bcbff0b703fdbb8b3440138d31b5d3b5094fef1cf8406d345d0154e0b1c31f69` |
| harness git commit | `2e3a8ea4` |

Prereg header flipped DRAFT→FROZEN; §12 filled; inclusion criteria frozen in
`corpus/README.md`. `run.sh --pilot` refuses to start unless the prereg
header says FROZEN.

## 3. Per-pair whole-task cost deltas (estimated list price, pricing-table as-of 2026-04)

Primary pairing (block-1 pairs for four tasks; block-2 re-run pair for
`go-pricing-refactor` after the block-1 B session was excluded by the
pre-registered `harness_confound` timeout rule — see §7.1):

| task | A (control) $ | B (trim) $ | Δ = B−A $ | Δ % | quality A | quality B |
|---|---|---|---|---|---|---|
| example-go-build-fix | 0.076511 | 0.061023 | −0.015488 | −20.2% | gate+assert pass | gate+assert pass |
| js-median-test-fix | 0.081594 | 0.067950 | −0.013644 | −16.7% | gate+assert pass | gate+assert pass |
| shell-sum-range-fix | 0.118437 | 0.056798 | −0.061639 | −52.0% | gate+assert pass | gate+assert pass |
| go-brackets-feature-add | 0.127560 | 0.074421 | −0.053139 | −41.7% | gate+assert pass | gate+assert pass |
| go-pricing-refactor (block 2) | 0.090898 | 0.073189 | −0.017709 | −19.5% | gate+assert pass | gate+assert pass |

- **n (complete pairs) = 5**
- **mean control whole-task cost = $0.09900**
- **mean paired delta = −$0.03232 (−32.7% of control)** — *pilot color only,
  not a claim*
- **per-pair delta SD σ_d = $0.02312**
- paired-t 95% CI on the delta (n=5, color only): [−$0.06103, −$0.00362] =
  [−61.6%, −3.7%] of control — **crosses the −5% MPIE boundary**, exactly why
  the pilot is not a result and the full powered run exists.

**Sensitivity check (exclusion honesty, R3):** the excluded block-1 B session
(`d788f9ab…`, timeout) was later attributed at $0.10829 / 9 turns. Re-pairing
`go-pricing-refactor` with block-1 A ($0.137419) + the orphan B ($0.10829)
instead of the block-2 re-run gives mean delta −$0.03461 (−32.0%), σ_d
$0.02185, N = 128. The exclusion was mechanical (timeout exit code, applied
before extraction) and does not drive the direction or rough size of anything
above.

## 4. Power / block count for the full run (prereg §6 formula at the frozen MPIE)

```
n_blocks = ceil( 7.849 · σ_d² / Δ² )
σ_d = $0.02312          (this pilot, n=5 paired deltas)
Δ   = 0.05 × $0.09900 = $0.00495   (frozen MPIE = 5% of mean control cost)
n_blocks = ceil( 7.849 × 0.000535 / 0.0000245 ) = 172
```

**N = 172 paired deltas** (task-pairs) at 80% power, two-sided α = 0.05, to
detect the 5% MPIE. At the pilot's per-pair cost (~$0.166 A+B at our pricing
table, ~$0.25 at SDK pricing — §7.3) that is roughly **344 sessions ≈ $29–$52
estimated spend and ≈ 23 h of sequential runtime**, to be spread across ≥2
calendar days / several time windows per the frozen §3.3 discipline.

Honest context (not a protocol change): 172 powers detection of a **5%**
effect. The pilot's observed effect is ~6× the MPIE; **if** the true effect
is near the observed size, the pre-registered decision gate (paired CI
entirely beyond −5%) would already be met at much smaller n (order 10–20
pairs). The frozen protocol has no interim-analysis rule, so running to the
computed N is the by-the-book path; if the operator prefers a group-
sequential/interim design to bound spend, that requires a **new dated
superseding pre-registration before the full run starts** — never an
adaptive mid-run change (R16).

## 5. Spend accounting (estimated list price, our pricing table)

| Item | $ |
|---|---|
| Block 1 (10 sessions, incl. the timed-out B) — attributed at run time | 0.801713 |
| Orphan attribution for the timed-out B session (found post-hoc) | 0.108290 |
| Block 2 re-run pair (2 sessions) | 0.164087 |
| **Total** | **1.074090** |

Hard cap $15 — used ~7.2%. At the SDK's own pricing (×1.50, §7.3) the total
is ≈ $1.61. Two pre-pilot smoke sessions (pipeline shakeout, shell task,
~$0.30 combined at SDK pricing) were run before the pilot proper and are not
ledger rows; they are disclosed here for complete spend honesty.

## 6. Pipeline validation (the actual point of Phase 0a)

| Check | Outcome |
|---|---|
| Sessions land in `api_turns` via `observer claude` + proxy | **PASS** — every completed session's SDK `session_id` matched proxy rows; per-session scoping (`session_id` + `id > pre-launch max`) prevents cross-contamination from the orchestrator's own concurrent session |
| Arm isolation without daemon changes | **PASS** — per-workspace overlays: control rows `compression_count = 0` (10/10 A-turns), trim rows `compression_count > 0` (every B session; 24–54 events) |
| `ENABLE_TOOL_SEARCH=true` on CC arms | **PASS** — set by the runner; uncached-input ~3.5K/session (no ~21K MCP-inline blowup) |
| Token/cost reconciliation (`api_turns` vs SDK result) | **PARTIAL** — token-consistent, but a **constant ×1.50 cost divergence** on every row (§7.3); must be resolved before the full run |
| Cache classification (warm steady-state) | **PASS (diagnostic)** — turn-1 write then warm reads; B's cache_read is consistently far below A's (e.g. 78K vs 148K on example-go-build-fix), consistent with the trimmed tools block being part of the cached prefix; **no elevated cache_write on B** (13.2K vs 13.3K on that task) — no extra-write penalty visible at steady state |
| No-op detection / activation | **PASS** — `fired` flag from `compression_count`; A arms never fire, B arms always fired on these tasks |
| Quality-guard capture | **PASS** — `[success].command` + `assertions/semantic.sh` executed post-agent in every workspace; machine-readable in the ledger |
| Failure retention + exclusion codes | **PASS (exercised for real)** — the timed-out session produced a `status:"failed" / excluded:true / harness_confound_timeout` row, the run continued, and the pair was re-run as a fresh block |
| Budget enforcement | **PASS (logic exercised, cap never approached)** — cumulative check after each session + pre-flight projection before each launch |
| Artifact completeness | **PASS** — ledger JSONL + sibling drift manifest emitted; workspaces retained under `/tmp/observer-bench-2026-07-12.*` |

## 7. Anomalies (all of them)

1. **Session 9 timeout (`harness_confound_timeout`).** The block-1 B session
   on `go-pricing-refactor` (the corpus's LONG task) hit the 600 s
   per-session timeout with the work substantively done (success gate and
   assertion passed in its workspace) but no SDK result emitted → no
   session_id → unattributable at extraction time. Handled per the frozen §9
   rule (excluded, cause logged, fresh-block re-run). Its spend was
   identified post-hoc by time-window + compression signature and appended
   as an explicit `orphan_spend_attribution` ledger row. **Full run must use
   a ≥900 s per-session timeout** (block 2 used 900 s and completed in-time).
2. **`observer claude` SessionEnd hook noise.** Every session logs
   `SessionEnd hook … failed: Hook cancelled` on stderr. Cosmetic (capture
   is proxy-side), but worth understanding before the full run.
3. **Constant ×1.50 SDK-vs-pricing-table divergence.** `api_turns.cost_usd`
   prices `claude-sonnet-5` at $2/$10 per Mtok (pricing.go:399, "as of
   2026-04"); the SDK's `total_cost_usd` is exactly 1.50× on **every** row,
   implying $3/$15 (and ×1.5 on cache rates too). A constant multiplier
   cannot change the delta's sign or its percentage, so σ_d/Δ² and N are
   unaffected — but the **absolute dollar figures depend on which table is
   right**. Operator action before the full run: verify current sonnet-5
   list pricing and refresh/version the pricing table (plan §7 Q11). Both
   numbers are reported side-by-side in §5 pending that.
4. **Model pinning.** The returned identifier is undated
   (`claude-sonnet-5`). Finding 13 wants an immutable snapshot id; if the
   API does not return a dated SKU, the manifest must record the alias +
   date + returned string, and the card must say so.
5. **Single time window.** The whole pilot ran in one evening window on one
   calendar day (2026-07-11 20:20–21:20 UTC). Fine for a pilot; the full run
   must satisfy §3.3 (≥2 calendar days, several windows).
6. **Turn-count parity color.** A vs B turn counts were similar (A: 5,5,4,10,6;
   B: 5,5,4,6,4) — no sign of the +88%-turns failure mode, but n is tiny.

## 8. Quality-guard outcomes

**10/10 valid measurement sessions passed both the completion gate and the
semantic assertion** (including `go-pricing-refactor`, whose gate is green at
start and whose assertion is the real refactor detector). The excluded
timed-out session also passed both in its workspace (the failure was
harness-side, not agent-side). No arm produced a butchered contract, a
weakened test, a hardcoded checker, or stray files. No `is_error` results,
no rate-limit events, no 5xx.

## 9. Go / no-go recommendation

**GO — with four pre-conditions**, honestly stated:

1. **Resolve the ×1.50 pricing divergence** (refresh + version the pricing
   table entry for sonnet-5) so the full run's dollar unit is defensible.
   The delta % is robust to it; the absolute $ is not.
2. **Raise the per-session timeout to ≥900 s** (fixes the one observed
   failure mode).
3. **Decide the N strategy before starting:** run to the frozen formula's
   **N = 172 paired deltas** (≈ $29–$52, ≈ 23 h sequential, spread over ≥2
   days) — or, if that spend/time is unwanted, author a **superseding dated
   pre-registration** with an interim-analysis rule *before* the run. No
   mid-run adaptation.
4. **Multi-window discipline** per frozen §3.3 (≥2 calendar days, several
   times of day, service tier held constant).

The pipeline itself is validated end-to-end: capture, isolation, extraction,
exclusion handling, budget enforcement, and quality guards all worked on real
traffic, and the failure path was exercised for real and handled per the
frozen protocol. Nothing in this pilot is a publishable number; the direction
observed here (trim cheaper, no cache-write penalty, quality held) is
consistent with the A2 prior but must be established by the powered run.
