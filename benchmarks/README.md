# Observer benchmarks — compression / context-savings showcase

Open, pre-registration-grade benchmark suite + harness for Observer's
compression / context-efficiency capabilities. This is the trust artifact
(§3.13): the corpus, the harness, the pre-registrations, and the
regeneration script live here so any number the website shows can be
reproduced from hashed inputs.

**Plan of record:**
`docs/plans/compression-savings-showcase-plan-2026-07-11.md` (DRAFT v2).

## Three different things are called "benchmark" — this is #1

Don't confuse the three; they share nothing but the word:

1. **This shell harness (`benchmarks/`)** — the compression / context-savings
   A/B showcase. Declarative `arms.toml`, `run.sh` blocked-pair runner,
   pre-registrations, drift manifest. `--dry-run` is spend-free; **`--pilot`
   is a LIVE mode that spends real money** (Phase-0a, one paired block, gated
   on a frozen pre-registration + `--budget-usd`).
2. **The Go rig (`internal/benchmark` + `cmd/observer/benchmark*`)** — the
   general `{harness × model}` A/B rig that powers the dashboard Benchmarks
   page. Different specs (`testdata/benchmark/*.toml`), a different schema
   (migration 061), its own statistics. `observer benchmark run` is a dry run
   by default; `--confirm-spend` spends. Unrelated to this directory.
3. **`internal/routing/benchmark.go`** — NOT a runner at all: it IMPORTS an
   external RouterBench score file to refine the routing tier table. No agent,
   no session, no spend.

## Status: Phase 0 scaffold + one diagnostic pilot (no publishable number)

A **Phase-0a diagnostic pilot HAS run** — its ledger and report are checked in
(`runs/tool-defs-trim-2026-07-12.jsonl`,
`preregistration/pilot-report-2026-07-12.md`). Its own report marks the numbers
**non-publishable** (one block, 5 tasks — it exists to power the full run's
sample size, not to produce a headline). **No full, pre-registered result
exists**, so no showcasable number exists yet. The rest of this tree is
structure + a dry-run validator + the drift-manifest collector.

**No capability is showcasable today** (plan §8). The default paths here — the
`--dry-run` validator + the drift-manifest collector — execute no agent, open
no session DB, hit no network endpoint, and spend no money. The one exception
is `run.sh --pilot`, the Phase-0a live path that DOES spend (gated on a frozen
pre-registration + a mandatory `--budget-usd` cap); it produced the checked-in
diagnostic pilot above.

## The honesty frame (read before adding anything — plan §0/§1)

Observer carries a **retracted** −14.8% compression-savings claim; a later
rigorous A/B measured **+60% cost / +88% turns** for aggressive
conversation compression because it broke Anthropic's prompt-prefix cache.
This suite is built so it **cannot** recreate that:

- **Bytes/tokens ≠ dollars.** A byte% is never dressed as a cost%. Dollar
  claims require **cache-aware, whole-task** measurement from `api_turns` +
  `cachetrack`, labelled **"estimated list-price cost (pricing-table vN)"**
  — never "the bill".
- **Every published number is a new, pre-registered, reproducible
  measurement** on the current binary/model. Legacy numbers (e.g. A2's
  −12.5%) may appear only as dated "prior measurement" footnotes, never as a
  headline.
- **Headline numbers come only from the representative stratum**, selected
  independently of whether a compressor fires (§3.7).
- **Cost is meaningless without a quality guard** (§3.6): every cost card
  publishes a cost–quality frontier, not cost alone.
- **Inconclusive stays inconclusive** (§3.0), published verbatim.

The harness embeds the **claim hygiene, not the claims**. There are no
numbers to defend in Phase 0 — there are guards that keep future numbers
honest.

## Layout

```
benchmarks/
  README.md            this file
  regenerate.sh        Phase 0: validate corpus+drift manifests, dry-run harness, print gates
  corpus/              two strata (representative headline / mechanism stress) + manifest
    hash-corpus.sh     local hasher -> corpus/manifest.json
  harness/             blocked-pair runner (dry-run only), extractor/analyzer stubs,
                       drift-manifest collector, run-ledger format
  preregistration/     TEMPLATE.md (freeze one per card before measuring)
```

## Verify the scaffold (safe — no spend)

```
benchmarks/regenerate.sh --validate      # corpus hash + drift manifest + harness dry-run + gates
benchmarks/harness/run.sh --dry-run      # protocol validation only, NO network/spend
benchmarks/harness/manifest.sh           # drift manifest to stdout (local reads only)
benchmarks/harness/extract.sh --schema   # prints the api_turns cache-vector SQL, opens no DB
benchmarks/harness/analyze.sh --plan     # declared statistics + decision gates
```

## Operator gates before Phase 0a (hard gate — plan §6)

Phase 0a (the pilot + protocol freeze) is a **hard gate** before any
expensive run. Before it can start, the operator must:

1. **Freeze the representative inclusion criteria** (`corpus/README.md`) —
   before any task is measured.
2. **Freeze the first pre-registration.** The DRAFT is now authored at
   `preregistration/tool-defs-trim-2026-07-11.md` (primary endpoint =
   whole-task est. list-price cost; MPIE = 5%; paired/blocked design over
   the representative stratum; cold/warm/mid-session cache protocol with
   Anthropic TTL handling; exclusion rules; paired-bootstrap CI +
   inconclusive rule; quality guard; drift-manifest binding — with the
   N/power section explicitly parameterized on the Phase-0a pilot
   variance). It is **not yet frozen**: the remaining action is to fill its
   §12 (utc timestamp, sign-off, computed block count), flip the header
   from "DRAFT" to "FROZEN", and hash it into the drift manifest.
3. **Grant spend approval** for the Phase 0a pilot — **one** paired block
   for the tool-defs-trim card only. This is the first real API cost; the
   Phase 0 scaffold deliberately cannot incur it.
4. **Sign off the protocol freeze** (§3.0/§6). After freeze, the protocol
   is immutable for that card.

Gate 2 is **partially complete** (draft authored); the outstanding
operator actions are: **freeze** gate 2, **freeze** the inclusion criteria
(gate 1), and **grant spend + sign off** (gates 3–4). Only after all four
does live execution get implemented and run. Until then, `run.sh` refuses
any non-dry-run mode.

## Deliberately deferred to Phase 0a+ (not built in Phase 0)

- **Live execution** in `run.sh` (agent invocation, timeout/WSL watchdog,
  per-block isolated observer daemons) — gated on operator spend approval.
- **Live extraction** in `extract.sh` (`--db` mode) and the **analyzer**
  computation in `analyze.sh` — both need the pilot's real inputs + the
  power calc the pilot feeds.
- **Real `git-snapshot` task freezing** (external repos; e.g. `lumen`) —
  needs the operator's pinned commits + licensing review (§3.12).
- **The website `/benchmarks` page + claim-manifest test** (Phase 5) —
  downstream of Phase 3 results and the Phase 4 independent-reproduction
  gate.
