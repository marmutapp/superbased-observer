# Pre-registration — <capability card> — <YYYY-MM-DD>

> **TEMPLATE. No numbers. Fill and FREEZE this before a single measured
> run (§3.0).** Once frozen it is hashed into the drift manifest (§3.4);
> changing it after seeing results invalidates the card (R16). Copy to
> `benchmarks/preregistration/<capability>-<date>.md`.

## 1. Capability & claim under test

- **Capability:** <e.g. tool-defs trim (`"tools"` sentinel)>
- **Package:** <internal/...>
- **Stratum:** representative (headline) — mechanism tasks never headline.
- **Cache regime reported:** <cold | warm steady-state | mid-session> (§3.3)
- **The single claim this card will make, if supported:** <one sentence,
  carrying its inseparable component: model · workload · date · N ·
  interval · "estimated list price" (§4.1)>

## 2. Primary endpoint (§3.1)

- **Metric:** whole-task total estimated list-price cost, A vs B
  (sum over all turns incl. retries, summaries, retrieval, failed turns).
- **Direction of a "win":** <lower cost on arm B>
- **Cost source:** `api_turns` cache-vector, cross-checked vs SDK result
  event, priced via pricing-table v<__> (record the version). Labelled
  **estimated list price**, never "the bill" (§3.5).

## 3. Minimum practically-important effect (MPIE) (§3.0)

- **MPIE:** ≥ <__>% whole-task est. cost (proposal: 5%; operator sign-off
  pending, §7 Q5). A delta smaller than the MPIE is published as
  **"inconclusive / no material effect on this workload"**, verbatim.

## 4. Design (§3.2)

- **Blocked replicate pairs**; order randomized within each block;
  analysis on paired deltas.
- **Arms:** A = <control>, B = <candidate>. Any other arms are
  **exploratory** and non-headline (§3.8).
- **Salt strategy:** <fixed-length, semantically-inert, equal size across
  A/B; rotated per block> (§3.2).
- **Cache isolation:** <separate API keys / namespaces per arm | documented
  salt-size control> (§3.2 / §7 Q6).

## 5. Cache/TTL protocol (§3.3)

- **Regime measured:** <cold | warm | mid-session>.
- **TTL/tier in use:** <__>; **inter-turn delay:** <__>; **warmup turns
  excluded:** <__>.
- **Behavior after retry / TTL expiry:** <excluded | re-classified>.
- **Time windows:** ≥ <__> times of day across ≥ <__> calendar days;
  service tier held constant.

## 6. Sample size / power (§3.1)

- **Power target:** <80%>, two-sided.
- **Per-pair variance source:** the Phase 0a pilot (§6).
- **Computed block count:** <__ — filled from the pilot; NOT a fixed 8>.

## 7. Analysis (§3.1)

- Paired delta → bootstrap / paired CI + median + quantiles.
- **Decision rule:** publish only if the paired interval lies entirely on
  the favorable side of the MPIE **and** the quality guard shows
  non-inferiority (§3.6). Else "inconclusive on this workload".

## 8. Quality guard (§3.6)

- **Completion gate:** <task `[success].command`>.
- **Semantic assertions:** <list task assertion ids>.
- **Also scored:** patch-diff quality, regressions introduced, tool-call /
  turn count, failure/recovery rate. The card publishes a **cost–quality
  frontier**, not cost alone.

## 9. Exclusion rules — fixed BEFORE results (§3.0/§3.3)

- Excluded events (machine-readable codes): <rate_limit, ttl_expiry,
  transient_error, ...>. No post-hoc exclusions.

## 10. Activation / no-op reporting (§3.9)

- Report: eligibility rate, firing rate, savings conditional on firing,
  unconditional savings across representative traffic, fallback-to-original
  count, invalid-JSON guard activations.

## 11. Manifest binding (§3.4/§4.5)

- Drift manifest attached (`manifest.sh` output): observer binary hash,
  model snapshot id + returned model, pricing-table version, corpus hash,
  harness commit. A material change auto-expires the card (§4.3).

## 12. Freeze

- **Frozen at:** <utc> — **manifest hash:** <__> — **signed off by:** <__>.
- After this line is filled, the protocol is immutable for this card.
