# Run-ledger format (template — §3.12 data governance)

The run ledger is the append-only, machine-readable record of **every**
run — including failed and aborted ones — with the exclusion reason
attached (§3.12: *"all runs" includes failures + machine-readable
exclusion reasons*). Phase 0 defines the format; no rows exist yet.

One JSONL file per card measurement: `benchmarks/runs/<card>-<date>.jsonl`.
One line per (block, pair, arm) agent session.

## Row schema

```json
{
  "run_id": "toolsdefs-trim-2026-07-20-b03-A",
  "card": "tool-defs-trim",
  "block": 3,
  "pair_index": 0,
  "arm": "A-control",
  "tool": "claude-code",
  "task_id": "example-go-build-fix",
  "stratum": "representative",
  "started_at": "<utc>",
  "ended_at": "<utc>",
  "cache_regime": "warm",
  "salt_bytes": 64,
  "cache_namespace": "ns-a",
  "manifest_ref": "benchmarks/runs/<card>-<date>.manifest.json",

  "status": "ok",              // ok | failed | aborted | excluded
  "excluded": false,
  "exclusion_reason": null,    // e.g. "rate_limit", "ttl_expiry", "transient_error" (§3.3)

  "endpoint_primary": {        // whole-task total (§3.1); PRIMARY
    "est_list_price_usd": null,
    "turns": null
  },
  "cache_vector": {            // §3.5, full vector — never an event gate
    "uncached_input": null,
    "cache_read": null,
    "cache_write": null,
    "cache_write_1h": null,
    "output": null,
    "hit_rate": null
  },
  "quality": {                 // §3.6 guard — cost is meaningless without it
    "success": null,           // [success].command exit 0
    "assertions": [],          // [{id, pass, weight}]
    "patch_diff_ok": null,
    "regressions": null,
    "turn_count": null
  },
  "activation": {              // §3.9
    "eligible": null,
    "fired": null,
    "fallback_to_original": null,
    "invalid_json_guard": null
  },
  "sdk_result_cost_usd": null, // cross-check vs endpoint_primary (§3.5)
  "latency_ms": { "median": null, "p95": null }  // secondary color only (§1)
}
```

## Rules

- **Never delete a row.** A bad run is `status:"failed"` /
  `excluded:true` with a reason — not a missing line (R3).
- Exclusion reasons are fixed in the pre-registration BEFORE results
  (§3.0) and must be one of the pre-declared machine-readable codes.
- Every ledger file has a sibling `.manifest.json` (from `manifest.sh`)
  and is covered by a checksum; the website reads results only through
  the hashed artifacts (§4.5 claim-manifest test).
- `null` = "not measured yet". Phase 0 ships the schema with all-null
  examples on purpose: **no numbers exist**.
