#!/usr/bin/env bash
# analyze.sh — analysis stage (SCAFFOLD; §3.1).
#
# Phase 0 declares the statistics the analyzer computes and the decision
# gates it applies; it does NOT compute anything (there are no measured
# inputs yet). The concrete implementation lands with the Phase 0a pilot,
# which supplies the per-pair variance the power calc needs.
#
# Declared analysis (replaces the retired "N>=8 + mean/CV" bar):
#   - PRIMARY endpoint: whole-task total estimated list-price cost, A vs B,
#     on Stratum R (§3.1). Per-turn figures are strictly secondary.
#   - EFFECT: paired delta per replicate-pair; reported as a bootstrap /
#     paired confidence interval, plus median + quantiles (right-skewed).
#   - SAMPLE SIZE: computed from the Phase 0a pilot per-pair variance to
#     detect the MPIE at the pre-registered power (§3.1) — NOT a fixed 8.
#   - DECISION: publish ONLY if the paired interval lies entirely on the
#     favorable side of the MPIE AND the quality guard shows
#     non-inferiority (§3.6). Otherwise "inconclusive on this workload",
#     published verbatim (§3.0 inconclusive rule).
#   - MULTIPLE COMPARISONS: one pre-registered primary comparison per card;
#     all other arms labelled exploratory (§3.8).
#
# Usage: analyze.sh --plan   (Phase 0: prints the declared plan and exits)

set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "$HERE/lib/common.sh"

case "${1:---plan}" in
  --plan)
    sed -n '/^# Declared analysis/,/^# Usage/p' "$0" | sed 's/^# \{0,1\}//; /^Usage/d'
    log ""
    log "Phase 0: analysis is DECLARED, not computed. No inputs exist yet."
    log "Implementation + power calc land with the Phase 0a pilot."
    ;;
  --pilot)
    # Phase-0a PILOT analysis: paired per-task deltas from ONE block,
    # per-pair variance σ_d, and the pre-registered block-count formula
    # evaluated at the frozen MPIE. Pilot numbers are pipeline validation
    # + power inputs ONLY — never a published claim (plan §8).
    shift
    LEDGER=""; MPIE_PCT="5.0"
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --ledger) shift; LEDGER="$1" ;;
        --mpie-pct) shift; MPIE_PCT="$1" ;;
        *) die "unknown arg: $1" ;;
      esac
      shift
    done
    [[ -n "$LEDGER" && -f "$LEDGER" ]] || die "--pilot requires --ledger <jsonl> (found: '$LEDGER')"
    python3 - "$LEDGER" "$MPIE_PCT" <<'PY'
import json, math, sys
ledger, mpie_pct = sys.argv[1], float(sys.argv[2])
rows = []
with open(ledger) as f:
    for line in f:
        line = line.strip()
        if line:
            rows.append(json.loads(line))
# Keep only completed, non-excluded measurement rows.
ok = [r for r in rows if r.get("status") == "ok" and not r.get("excluded")]
by = {}  # task -> arm -> row
for r in ok:
    by.setdefault(r["task_id"], {})[r["arm"]] = r
def cost(r):
    ep = (r or {}).get("endpoint_primary") or {}
    v = ep.get("est_list_price_usd")
    return float(v) if v is not None else None
pairs = []
for task, arms in sorted(by.items()):
    A = next((arms[a] for a in arms if a.startswith("A-")), None)
    B = next((arms[a] for a in arms if a.startswith("B-")), None)
    ca, cb = cost(A), cost(B)
    if ca is None or cb is None:
        pairs.append((task, ca, cb, None, A, B)); continue
    pairs.append((task, ca, cb, cb - ca, A, B))
def qguard(r):
    if not r: return "n/a"
    q = r.get("quality") or {}
    s = q.get("success_exit")
    ap = q.get("assertion_pass")
    sx = "pass" if s == 0 else ("fail" if s is not None else "?")
    ax = "pass" if ap is True else ("fail" if ap is False else "?")
    return f"gate={sx} assert={ax}"
print("== Phase-0a PILOT analysis — tool-defs-trim (INTERNAL; not a published claim) ==")
print(f"ledger: {ledger}")
print(f"MPIE: {mpie_pct}% whole-task est. list-price cost\n")
hdr = f"{'task':<26}{'A $':>10}{'B $':>10}{'Δ=B-A $':>11}{'Δ %':>9}  quality(A|B)"
print(hdr); print("-"*len(hdr)+"-"*24)
deltas=[]; a_costs=[]
for task, ca, cb, d, A, B in pairs:
    if d is None:
        print(f"{task:<26}{'—':>10}{'—':>10}{'—':>11}{'—':>9}  INCOMPLETE ({qguard(A)} | {qguard(B)})")
        continue
    deltas.append(d); a_costs.append(ca)
    pct = (d/ca*100.0) if ca else float('nan')
    print(f"{task:<26}{ca:>10.5f}{cb:>10.5f}{d:>+11.5f}{pct:>+8.1f}%  {qguard(A)} | {qguard(B)}")
print()
n=len(deltas)
if n==0:
    print("No complete paired blocks — cannot compute σ_d / N. See ledger for exclusions.")
    sys.exit(0)
mean_d=sum(deltas)/n
mean_a=sum(a_costs)/n
if n>=2:
    var=sum((x-mean_d)**2 for x in deltas)/(n-1)
    sd=math.sqrt(var)
else:
    sd=float('nan')
print(f"paired blocks (complete):   n = {n}")
print(f"mean control (A) cost:      ${mean_a:.5f}")
print(f"mean paired delta (B-A):    ${mean_d:+.5f}  ({mean_d/mean_a*100:+.1f}% of control)" if mean_a else "")
if n>=2:
    print(f"per-pair delta SD (σ_d):    ${sd:.5f}")
    delta_mpie = (mpie_pct/100.0)*mean_a
    print(f"Δ (MPIE in $):              ${delta_mpie:.5f}  = {mpie_pct}% × mean control")
    if delta_mpie>0:
        nb = math.ceil(7.849 * (sd**2) / (delta_mpie**2))
        print(f"n_blocks = ceil(7.849·σ_d²/Δ²) = {nb}   (80% power, two-sided α=0.05)")
    else:
        print("Δ = 0 → cannot compute n_blocks")
else:
    print("σ_d needs ≥2 complete pairs; N not computable from this pilot.")
PY
    ;;
  -h|--help) echo "Usage: $0 --plan | --pilot --ledger <jsonl> [--mpie-pct 5]"; ;;
  *) die "unknown arg: $1 (--plan or --pilot)" ;;
esac
