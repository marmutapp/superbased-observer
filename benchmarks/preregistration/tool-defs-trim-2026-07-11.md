# Pre-registration — tool-defs trim (`"tools"` sentinel), whole-task cost — 2026-07-11

> **FROZEN — 2026-07-11T20:18:01Z (session 2026-07-12). Operator approved.**
>
> This is the FIRST pre-registration for the compression/context-savings
> showcase (plan: `docs/plans/compression-savings-showcase-plan-2026-07-11.md`,
> §3.0). It is now **frozen** for the Phase-0a pilot: the protocol below is
> immutable for this card; changing it after seeing results invalidates the card
> (R16), and a new dated file supersedes it instead. The N/power section (§6)
> stays **parameterized on the Phase-0a pilot variance** by design — the pilot
> is the run that supplies σ_d and the computed block count, reported in
> `pilot-report-2026-07-12.md`. The pilot is authorized with a **hard $15 budget
> cap** (operator approval, session 2026-07-12). The full pre-registered run
> remains gated on the pilot's go/no-go verdict.
>
> Freeze record is §12. The pilot's measured numbers are NOT results and are NOT
> published as claims (plan §8): the pilot validates the pipeline and powers the
> full run only.

## 1. Capability & claim under test

- **Capability:** tool-defs trim (the `"tools"` sentinel — the envelope `tools`
  field is replaced by a stable sentinel on warm continuation turns so the
  full tool-definition block is not re-marshaled into every request prefix).
- **Package:** `internal/proxy` conversation envelope handling (the `"tools"`
  sentinel path); config surface `[compression]` tool-defs gate.
- **Stratum:** **representative** (headline-eligible). Mechanism tasks never
  headline (§3.7).
- **Cache regime reported:** **warm steady-state** (§3.3). This is exactly the
  regime A2 measured and the only regime the "cache-safe" scope covers. Cold
  enablement and mid-session enablement are **separate, non-headline regimes**
  registered here as exploratory (§4) and reported with their own cards later —
  they can force an initial cache write / move breakpoints and must not be
  folded into the steady-state number (finding 4 / R11).
- **The single claim this card will make, if supported:**
  *"Tool-definition trim: −X% estimated list-price cost (whole-task, n=N paired
  blocks, `<model-snapshot-id>`, `<representative refactor/build-fix workload>`,
  `<date>`, CI [a, b], steady-state warm cache; full cache vector published),
  quality non-inferior."* The bracketed model · workload · date · N · interval ·
  "estimated list price" are the **inseparable claim component** (§4.1) and
  travel with the headline into OG/social metadata.

  A2's historical −12.5% (2026-06-10/11, n=8, Claude Code, steady state) appears
  on the card ONLY as a dated **"prior measurement" footnote**, never as the
  headline (§Review-delta 5 / R15). The §2 CLOSED adoption
  (`project_tools_defs_trim_closed.md`) is **not** reopened by this measurement.

## 2. Primary endpoint (§3.1)

- **Metric:** **whole-task total estimated list-price cost, A vs B** — summed
  over **every** turn of a task including retries, tool-result-bearing
  continuation turns, summaries, and failed turns. Per-turn cost is strictly
  secondary color (compression damage historically surfaces as *more turns*, not
  per-turn price — the +88%-turns incident; §3.1).
- **Direction of a "win":** **lower** whole-task cost on arm B (trim) vs arm A
  (full tool defs).
- **Cost source:** `api_turns` cache-vector (cache_read / cache_creation /
  uncached-input split), cross-checked against the Claude Code SDK
  `total_cost_usd` result event, priced via **pricing-table v`<__>`** (the
  version is recorded in §11's manifest at freeze; there is no explicit version
  constant yet — the manifest uses the pricing source hash + "as of" date,
  plan §7 Q11). Labelled **estimated list price**, never "the bill" (§3.5 /
  finding 15).

## 3. Minimum practically-important effect (MPIE) (§3.0)

- **MPIE = ≥ 5% whole-task estimated list-price cost.**
- **Reasoning (why 5%, anchored on A2 with a conservative floor):**
  1. **Anchor.** A2's steady-state historical effect is **−12.5%** (n=8). A 5%
     MPIE is a **conservative floor at ~40% of that anchor**: it lets us make a
     claim even if the new binary/model realizes less than half of A2's effect,
     while refusing to publish a marginal wobble.
  2. **Drift margin.** Dollar figures are pricing-table-derived estimated list
     price, not invoiced (finding 15). Provider price changes, service-tier
     rounding, and pricing-table version drift move a headline by low-single-
     digit percent. A ≥5% floor keeps the *sign and materiality* of the claim
     robust to that drift (R6/R14) — a 2% "win" could invert under a routine
     price refresh; a ≥5% one, entirely on the favorable side of the boundary,
     will not.
  3. **Generalization margin.** The card generalizes a single representative
     workload to "your traffic" only with the "measure your own" caveat (R5). A
     higher floor than mechanism-level noise (CV ~7.6% in the A2 rig) guards
     against selling within-noise effects as savings.
  4. **Symmetry with the plan's proposal.** §3.0 and §7 Q5 propose 5%; this
     card adopts it pending operator sign-off. Adjust in §12 before freeze if
     appetite differs — but never *after* seeing results.
- **Below-MPIE handling:** a paired interval that does not lie entirely beyond
  the −5% boundary is published **verbatim** as *"inconclusive / no material
  effect on this workload"* (§7). No massaging (R8).

## 4. Design (§3.2)

- **Blocked replicate pairs.** Each **block** runs arm A and arm B as a
  **matched pair** on the same task; **order is randomized within each block**
  (`randomize_within_pair = true`, `seed_source = block-index` in `arms.toml`);
  analysis is on **paired per-block deltas** so block-level (time-of-day, load,
  routing-tier) variance cancels (§3.2). Not independent alternating arms.
- **Arms (primary comparison — one per card):**
  - **A = control** (`A-control`): `claude-code`, compression `off`, full tool
    definitions sent every turn. `env = ENABLE_TOOL_SEARCH=true`.
  - **B = candidate** (`B-toolsdefs-trim`): `claude-code`, compression
    `tools-trim` (the `"tools"` sentinel). `env = ENABLE_TOOL_SEARCH=true`.
  - **`ENABLE_TOOL_SEARCH=true` is mandatory on both CC arms** (R7): without it
    the CC SDK eager-inlines all MCP schemas under `ANTHROPIC_BASE_URL`
    (~+21K tokens/turn), which is rig error, not a result. `run.sh --dry-run`
    refuses an undeclared CC arm.
- **Exploratory arms (labelled exploratory, non-headline, §3.8):** the **cold**
  regime (first-request cache write) and the **mid-session enablement** regime
  (flip the trim partway through a task, measure the transition). These are
  registered here so they are disclosed, but they are **not** the headline
  comparison and are analyzed separately with their own decision gates. No
  best-of-N selection across regimes/tasks/models after seeing results.
- **Salt strategy (§3.2):** each arm receives a **documented, fixed-length
  (64-byte), semantically-inert** salt block (`<test-id>…</test-id>` shape),
  **equal in size across A and B within a pair** so the salt is not itself a
  confound, and **rotated per block** so no cross-block cache leakage inflates a
  delta.
- **Cache isolation (§3.2 / §7 Q6):** arms run in **separate cache namespaces**
  (`cache_namespace = ns-a | ns-b` in `arms.toml`; distinct API keys/accounts so
  each arm gets its own cold cache and B never warm-reads A's prefix). Because
  arms A and B intentionally carry **different** tool-definition prefixes, a
  shared namespace would confound the cache-write delta we are measuring; the
  namespace split is the primary isolation. In addition, a **salt-size control
  block** (a dedicated pair with the salt size varied, arms otherwise identical)
  quantifies and subtracts any residual salt-token effect (§7 Q6, warm-regime
  recommendation). Whichever of the two absorbs the confound, both are declared
  here; neither is chosen after results.

## 5. Cache / TTL protocol (§3.3)

- **Regime measured (headline):** **warm steady-state.** Each task is long
  enough (≥ `<__>` continuation turns — set from the corpus task profile at
  freeze) that after the turn-1 cache write the arm runs on a live warm cache,
  matching A2's 106-turn steady state.
- **Anthropic TTL / tier handling:**
  - Record the **Anthropic prompt-cache TTL/tier** in force (default 5-minute
    ephemeral tier unless the 1h tier is explicitly configured) in the manifest.
  - **Inter-turn delay** (`inter_turn_delay_s`) is fixed **well below the
    recorded TTL** so warm continuation turns land inside the live cache window
    — operator/queue latency must never be allowed to silently expire the cache
    and turn a warm-regime turn into an unrecorded cold write (R7). If a turn's
    gap exceeds the TTL it is **re-classified** (see §9), not silently averaged.
  - **Warmup turns:** the turn-1 cache **write is included** in the whole-task
    total (the primary endpoint is whole-task; §3.1) but is **reported
    separately** in the published cache vector so the reader sees write vs read
    (§3.5). `warmup_turns_excluded` in `arms.toml` stays `0` for the headline
    whole-task metric; any excluded-warmup secondary view is labelled as such.
  - **"Cache-safe" is a diagnostic, not the gate (finding 8):** we publish
    `tools_changed` / `prefix_churn` cachetrack event counts and the full cache
    vector; A2 saw **zero `tools_changed` across 106 turns**, and the claim
    "trim does not force extra cache writes on warm turns" is supported by the
    cache vector + event counts, not asserted absolutely.
- **Time windows (§3.3):** randomized blocks across **≥ several times of day**
  and **≥ 2 calendar days** (`min_calendar_days = 2`); **service tier held
  constant**; retry / rate-limit / routing-tier / transient-error events
  recorded per block for the exclusion rules (§9).

## 6. Sample size / power (§3.1) — parameterized on the Phase-0a pilot

**This card is NOT powered yet. The block count is computed from the pilot; it
is deliberately NOT a fixed 8** (which was a floor, never a justification).

- **Power target:** **80%**, **two-sided α = 0.05** (`power = 0.80`,
  `two_sided = true` in `arms.toml`).
- **Per-pair variance source:** the **Phase-0a pilot** — one paired block run
  end-to-end (plan §6, step 4) supplies the per-pair cost-delta standard
  deviation **σ_d** (the pilot also validates token/cost reconciliation, cache
  classification, no-op detection, quality-guard capture, artifact
  completeness).
- **Block-count formula (filled once σ_d lands):**

  ```
  n_blocks  =  ceil( (z_{1-α/2} + z_{1-β})^2 · σ_d^2 / Δ^2 )
            =  ceil( (1.96 + 0.84)^2 · σ_d^2 / Δ^2 )
            =  ceil( 7.849 · σ_d^2 / Δ^2 )
  ```

  where **σ_d = [PILOT — per-pair whole-task cost-delta SD, in $]** and
  **Δ = MPIE in the same $ units = 0.05 × (mean control whole-task cost)**.
  `arms.toml` `blocks = 0` until this is computed; `run.sh` refuses a live run
  while `blocks = 0` ("NOT POWERED").
- **Freeze note:** the computed `n_blocks` is written into `arms.toml` and this
  §6 at freeze, alongside the pilot's σ_d and the mean control cost used for Δ.
  No expensive measurement runs before that.

## 7. Analysis (§3.1)

- **Estimator:** the **paired per-block delta** (B − A whole-task cost per
  block), reported as a **paired bootstrap confidence interval** (≥ 10,000
  resamples over the block deltas) **and** a paired-t CI as a cross-check, plus
  **median and quantiles** (task cost is right-skewed; the mean alone is
  insufficient). CV appears only as color, never as the decision statistic.
- **Decision rule (pre-registered):** publish the −X% claim **only if** the
  paired CI lies **entirely beyond the −5% MPIE boundary** (favorable side)
  **AND** the quality guard (§8) shows **non-inferiority**. Otherwise the
  verdict is **"inconclusive on this workload,"** published verbatim (R8).
- **Secondary/exploratory analyses** (per-turn cost, cold + mid-session regimes,
  latency quantiles, cache-vector component deltas) are **labelled exploratory**
  and are **not** headline-eligible (§3.8); false-discovery risk is controlled
  by restricting claims to the single primary comparison.

## 8. Quality guard (§3.6)

Cost is meaningless without holding quality. For **both** arms, per block:

- **Completion gate:** the task's `[success].command` (e.g.
  `go build ./...` for `example-go-build-fix`) — exit 0 required.
- **Semantic assertions:** every task's `assertions/semantic.sh` (the
  quality-guard scripts under `benchmarks/corpus/representative/<task>/`), which
  a cheaper-but-worse arm must **fail** (e.g. `minimal-diff` catches a butchered
  contract even when `go build` passes).
- **Also scored:** patch-diff quality (over-broad / unrelated edits),
  regressions introduced, **tool-call / turn count**, failure/recovery rate.
- The card publishes a **cost–quality frontier** (cost vs quality with CIs),
  **not cost alone.** A cheaper arm that degrades quality is a **fail**,
  presented honestly (R10).

## 9. Exclusion rules — fixed BEFORE results (§3.0 / §3.3)

Machine-readable exclusion codes, decided now, applied without looking at
outcomes (no post-hoc exclusions):

- `rate_limit` — a block containing a 429 / rate-limit event is **excluded** and
  re-run in a fresh block (the event is retained in the ledger with this code).
- `ttl_expiry` — a warm-regime turn whose inter-turn gap exceeded the recorded
  cache TTL is **re-classified** (that turn's cache write is not counted as a
  steady-state read); if re-classification changes the block's regime, the block
  is **excluded** from the warm-regime headline and retained for the exploratory
  cold/mid-session analysis.
- `transient_error` — a network / 5xx / proxy transient failure aborts and
  **re-runs** the block; the aborted rows are retained with this code.
- `routing_tier_change` / `service_tier_mismatch` — a block whose recorded
  service tier differs from the frozen tier is **excluded**.
- `harness_confound` — e.g. a CC arm missing `ENABLE_TOOL_SEARCH=true`, a WSL
  watchdog timeout, or a salt-size mismatch within a pair → **excluded** and the
  cause logged.

**All runs (including failures and exclusions) are retained** in
`harness/ledger.md` with their exclusion code and reason (§3.12 data
governance). "All runs" means failures + machine-readable exclusion reasons, not
just the surviving ones.

## 10. Activation / no-op reporting (§3.9)

Over the representative tasks, report: **eligibility rate** (turns carrying a
tool-defs block), **firing rate** (turns where the `"tools"` sentinel actually
replaced the block), **savings conditional on firing**, **unconditional savings
across representative traffic**, **fallback-to-original count**, and
**invalid-JSON guard activations**. A conditional win on a small fraction of
turns is materially different from a broad win; the card states which it is
(R12).

## 11. Manifest binding (§3.4 / §4.5)

At freeze, the drift manifest (`benchmarks/harness/manifest.sh` output) is
attached and its hash written into §12. It captures:

- **observer binary** path + sha256 + `--version`;
- **model** snapshot id + the `api_turns`-returned model identifier (filled by
  the extractor at run time; alias-only is not reproducible — finding 13);
- **pricing** source file + sha256 + "as of" date (no explicit version constant
  yet — plan §7 Q11);
- **harness** git commit + `arms.toml` sha256 + **corpus manifest sha256**
  (binds the exact frozen tasks);
- **environment** os / arch / container image.

A material change in any manifest field **auto-expires** the dependent card
(§4.3): the website card is visibly marked "expired — re-measurement pending"
and no stale number is served.

## 12. Freeze

- **Header state:** **FROZEN.**
- **Frozen at:** `2026-07-11T20:18:01Z` (machine UTC; operator session
  `2026-07-12`).
- **Drift-manifest binding (stable components — the whole-manifest JSON also
  carries a volatile `collected_at`, so the reproducible binding is the
  component hashes below, from `benchmarks/harness/manifest.sh`):**
  - observer `binary_sha256` = `c7071fd6365cc4d5b424f41a52022cf6c804b12b5d735cad1128f90931fd0433`
    (`observer version dev`)
  - harness `git_commit` = `2e3a8ea4`
  - `arms_config_sha256` = `9e0d8e048888d48ccd02d5c1925dabd84d718a9930e7e75f31450e3c66df72cc`
  - `corpus_manifest_sha256` = `892e4afbbb5f5cb33e7d53af8d90801d11640d89ccb906fa7114541474b66bdb`
  - pricing `source_sha256` = `bcbff0b703fdbb8b3440138d31b5d3b5094fef1cf8406d345d0154e0b1c31f69`
  - whole-manifest sha256 (as collected 2026-07-11T20:18:01Z) =
    `57936c6f114d9844c94abd7fe8258019e7f69ddf273f8c62eb5ff05bc8a5f96d`
- **Pilot σ_d:** to be measured by the Phase-0a pilot (this session) and reported
  in `pilot-report-2026-07-12.md` (§6 is parameterized on it, by design).
- **Computed n_blocks:** **pilot-parameterized** — computed post-pilot from σ_d
  via `n_blocks = ceil(7.849 · σ_d² / Δ²)`, `Δ = 0.05 × mean control whole-task
  cost` (§6). Not fixed here; `arms.toml blocks = 0` ("NOT POWERED") stands until
  the full run is powered from the pilot report.
- **Pricing-table v/as-of:** no explicit version constant (plan §7 Q11); pinned
  by pricing `source_sha256` above + "as of `2026-04`" marker.
- **MPIE confirmed:** **5%** whole-task estimated list-price cost — **operator
  approved via session 2026-07-12**.
- **Signed off by:** **operator approved via session 2026-07-12** (Phase-0a pilot
  authorized with a hard $15 budget cap).
- The protocol above is now **immutable** for this card. The pilot may run under
  the frozen protocol; its numbers are pipeline-validation + power inputs only,
  never published claims (plan §8).
