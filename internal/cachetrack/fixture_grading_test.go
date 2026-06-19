package cachetrack_test

// Fixture-grading test for the §14.3 Tier-2 live emitters
// (opencode, kilo-cli, cline-cli). Feeds each adapter's
// fixture through the cachetrack engine and reports BOTH:
//
//   1. Attribute-Kind breakdown (the §7 outcome.Kind on
//      cache_events.kind) — what the engine PERSISTS.
//   2. §10 bucket-mispredict count (rec.Mispredicted from
//      ReconcilePrediction) — the predicted-vs-observed
//      mismatch the §10 rate sums over.
//
// These are NOT the same quantity. Outcome.Kind == KindMispredict
// is only the §7 row-12 fallthrough (cache_read=0 AND
// cache_creation=0). §10 mispredict is bucket-of-predicted vs
// bucket-of-observed disagreement, fires independently. The
// earlier (pre-2026-06-09 revision) version of this test
// undercounted by only watching outcome.Kind; this revision
// measures both.
//
// codex is intentionally absent — codex Tier-2 is scaffold-only
// pending §15.3 (see docs/audits/cachetrack-codex-tier2-audit-
// 2026-06-09.md).

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/marmutapp/superbased-observer/internal/adapter/kilocode"
	"github.com/marmutapp/superbased-observer/internal/adapter/opencode"
	"github.com/marmutapp/superbased-observer/internal/cachetrack"
	"github.com/marmutapp/superbased-observer/internal/models"
)

// computeLevelHashesPerTurn mirrors the engine's per-turn fresh
// level-hash computation (engine.go ~300-340): a separate sha256
// over the CanonicalBytes of every block at LevelTools,
// LevelSystem, and (for inspection only — the engine doesn't
// track this) LevelMessage, scoped to ONE turn's block slice.
// Empty when the level had no blocks that turn.
//
// This is the operator's calibrate-log-style probe for the §14.3
// audit: if turn 2's tools/system hash differs from turn 1's
// while the adapter is supposed to be emitting pure
// message-suffix continuation, a volatile field leaked into a
// non-message level. If both are empty across turns, the engine's
// `tools_or_system_changed` cause label is the residual rule
// firing, not a real change-detection signal.
func computeLevelHashesPerTurn(blocks []models.CacheBlockMeta) (tools, system, message string) {
	toolsH := sha256.New()
	systemH := sha256.New()
	messageH := sha256.New()
	var sawT, sawS, sawM bool
	for _, b := range blocks {
		switch b.LevelLabel {
		case "tools":
			toolsH.Write(b.CanonicalBytes)
			sawT = true
		case "system":
			systemH.Write(b.CanonicalBytes)
			sawS = true
		case "message", "":
			messageH.Write(b.CanonicalBytes)
			sawM = true
		}
	}
	if sawT {
		tools = hex.EncodeToString(toolsH.Sum(nil))
	}
	if sawS {
		system = hex.EncodeToString(systemH.Sum(nil))
	}
	if sawM {
		message = hex.EncodeToString(messageH.Sum(nil))
	}
	return
}

// dumpPerLevelHashes reports per-turn tools/system/message hashes
// for an ordered slice of observations. Operator gating step:
// if tools and system hashes are byte-stable (or empty) across
// turns AND only message-level changes, the
// `tools_or_system_changed` cause label is the engine's residual
// catch — NOT a real change. Otherwise the diff names the level
// where a volatile field is leaking.
func dumpPerLevelHashes(t *testing.T, label string, observations []models.CacheTurnObservation) {
	t.Helper()
	t.Logf("---- %s per-level canonical hashes ----", label)
	for i, o := range observations {
		tools, system, message := computeLevelHashesPerTurn(o.BlockHashes)
		short := func(h string) string {
			if h == "" {
				return "(empty — no blocks at this level)"
			}
			if len(h) < 16 {
				return h
			}
			return h[:16] + "…"
		}
		t.Logf("  turn %d (msg=%s, blocks=%d)", i+1, o.MessageID, len(o.BlockHashes))
		t.Logf("    tools-level:   %s", short(tools))
		t.Logf("    system-level:  %s", short(system))
		t.Logf("    message-level: %s", short(message))
	}
}

type gradingResult struct {
	turnCount     int
	kindCounts    map[cachetrack.Kind]int
	causeCounts   map[cachetrack.Cause]int
	totalRead     int64
	totalWritten  int64
	bucketMispred int // §10 bucket-of-predicted != bucket-of-observed (the real §10 rate denominator-numerator)
	perTurnTraces []turnTrace
	graded        int // turns not bucketSkipped (the §10 denominator)
}

type turnTrace struct {
	predicted     cachetrack.Kind
	observed      cachetrack.Kind
	cause         cachetrack.Cause
	tokensRead    int64
	tokensWritten int64
	mispredicted  bool
}

// gradeObservations feeds an ordered slice of observations
// through a fresh engine, recording the result of EACH turn's
// reconciliation. The engine's clock is fixed so the trace
// reproduces across runs.
//
// §15.3 (c)-phase-2 G2 anchor closure: bucket-mispredict reads
// EventOut.Mispredicted directly (the engine's real value), and
// bucket labels come from cachetrack.BucketLabel — eliminating
// the local bucketOf mirror that previously stayed in sync by
// doc comment alone (silent-drift risk a future Kind addition
// would have masked).
func gradeObservations(t *testing.T, label string, observations []models.CacheTurnObservation) gradingResult {
	t.Helper()
	eng := cachetrack.NewEngine(1024)
	base := time.Unix(1_780_000_000, 0)
	eng.Clock = func() time.Time { return base }

	res := gradingResult{
		kindCounts:  map[cachetrack.Kind]int{},
		causeCounts: map[cachetrack.Cause]int{},
	}
	for i, o := range observations {
		blocks := make([]cachetrack.ObserveBlock, 0, len(o.BlockHashes))
		for _, b := range o.BlockHashes {
			blocks = append(blocks, cachetrack.ObserveBlock{
				Level:          cachetrack.LevelMessage,
				Kind:           b.Kind,
				CanonicalBytes: b.CanonicalBytes,
				Role:           b.Role,
			})
		}
		in := cachetrack.ObserveInput{
			SessionID: o.SessionID,
			Model:     o.Model,
			Scope:     "default",
			Tier:      cachetrack.TierTranscript,
			MessageID: o.MessageID,
			Now:       base.Add(time.Duration(i) * time.Second),
			Blocks:    blocks,
			Usage: cachetrack.CacheUsageObserved{
				NetInputTokens:      o.Usage.NetInputTokens,
				OutputTokens:        o.Usage.OutputTokens,
				CacheReadTokens:     o.Usage.CacheReadTokens,
				CacheCreationTokens: o.Usage.CacheCreationTokens,
			},
		}
		out := eng.ObserveTurn(in)
		res.turnCount++
		res.totalRead += o.Usage.CacheReadTokens
		res.totalWritten += o.Usage.CacheCreationTokens
		for _, ev := range out.Events {
			res.kindCounts[ev.Outcome.Kind]++
			res.causeCounts[ev.Outcome.Cause]++

			predBucket := cachetrack.BucketLabel(ev.PredictedKind)
			obsBucket := cachetrack.BucketLabel(ev.Outcome.Kind)
			if predBucket != "skipped" && obsBucket != "skipped" {
				res.graded++
			}
			if ev.Mispredicted {
				res.bucketMispred++
			}
			res.perTurnTraces = append(res.perTurnTraces, turnTrace{
				predicted:     ev.PredictedKind,
				observed:      ev.Outcome.Kind,
				cause:         ev.Outcome.Cause,
				tokensRead:    ev.TokensRead,
				tokensWritten: ev.TokensWritten,
				mispredicted:  ev.Mispredicted,
			})
		}
	}
	t.Logf("---- %s grading ----", label)
	t.Logf("  turns observed:        %d", res.turnCount)
	t.Logf("  total cache_read:      %d", res.totalRead)
	t.Logf("  total cache_write:     %d", res.totalWritten)
	t.Logf("  kind breakdown:        %v", res.kindCounts)
	t.Logf("  cause breakdown:       %v", res.causeCounts)
	t.Logf("  §10 graded turns:      %d / %d (Reanchor turns excluded)", res.graded, res.turnCount)
	t.Logf("  §10 bucket-mispred:    %d / %d", res.bucketMispred, res.graded)
	for i, tr := range res.perTurnTraces {
		t.Logf("    turn %d: predicted=%s observed=%s cause=%s read=%d write=%d mispred=%v",
			i+1, tr.predicted, tr.observed, tr.cause, tr.tokensRead, tr.tokensWritten, tr.mispredicted)
	}
	return res
}

// TestGradeOpenCodeMultiTurnFixture runs a 2-turn opencode
// Anthropic-shape fixture through the engine.
//
// Turn 1 (cold seed): cache_read=0, cache_creation=4096
//
//	→ §7 row 1 (no prior) → Reanchor
//	→ entry created at end-of-chain hash
//
// Turn 2 (warm + grow): cache_read=12288, cache_creation=512
//
//	→ engine pushes new blocks → chain hash differs from turn-1 entry
//	→ predictKind: matched=nil + prior!=nil → KindWrite
//	→ attribute walks §7: Tier-2 caps gate rows 4-5 (tools/system
//	  invisible), then ruleToolsOrSystemResidual fires (Tier-2's
//	  CONSERVATIVE residual rule: any write turn beyond the first
//	  with neither tools nor system visible defaults to
//	  KindInvalidationRewrite + CauseToolsOrSystemChanged).
//	→ Cause label is "tools_or_system_changed" but means
//	  "we couldn't see, so we're hedging" — NOT "tools or system
//	  literally changed." Same shape claudecode hits.
//	→ §10 bucket: predicted=write (bucketWrite), observed=
//	  invalidation_rewrite (bucketWrite) → NOT mispredict.
//
// This exercises the chain-comparison path (turn 2 against
// turn 1's reconstructed state) AND demonstrates the volatile-
// exclusion code path runs against multiple turns of the same
// content shape. Without volatile exclusion, the wall-clock
// timestamps inside the tool/reasoning parts would churn the
// chain hash; even with hashes nondeterministic, that wouldn't
// affect the bucket-mispredict count for this fixture because
// the engine creates an entry on turn 2's write — but it WOULD
// break determinism across re-parses (the R3 invariant that the
// idempotency test pins).
func TestGradeOpenCodeMultiTurnFixture(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "opencode.db")
	openCodeMultiTurnFixture(t, dbPath)

	a := opencode.NewWithOptions(nil, []string{dir})
	parsed, err := a.ParseSessionFile(context.Background(), dbPath, 0)
	if err != nil {
		t.Fatalf("opencode parse: %v", err)
	}
	res := gradeObservations(t, "opencode 2-turn", parsed.CacheObservations)

	if res.turnCount != 2 {
		t.Fatalf("opencode: expected 2 graded turns, got %d", res.turnCount)
	}
	if res.kindCounts[cachetrack.KindReanchor] != 1 {
		t.Errorf("opencode: expected 1 Reanchor (turn 1), got kinds=%v", res.kindCounts)
	}
	// Turn 2 classifies as write-bucket. Specific Kind depends
	// on which §7 rule fires; under Tier-2 caps the residual
	// ruleToolsOrSystemResidual is the typical match (returns
	// KindInvalidationRewrite). Either way, the bucket-grade is
	// what §10 sums over.
	writeBucket := res.kindCounts[cachetrack.KindWrite] +
		res.kindCounts[cachetrack.KindInvalidationRewrite] +
		res.kindCounts[cachetrack.KindExpiryRewrite] +
		res.kindCounts[cachetrack.KindModelSwitchRewrite]
	if writeBucket != 1 {
		t.Errorf("opencode: expected 1 write-bucket classification on turn 2, got kinds=%v", res.kindCounts)
	}
	if res.bucketMispred != 0 {
		t.Errorf("opencode: expected 0 §10 mispredicts on Anthropic-shape multi-turn, got %d", res.bucketMispred)
	}
	if res.graded != 1 {
		t.Errorf("opencode: expected 1 §10-graded turn (turn 2; turn 1 is Reanchor=skipped), got %d", res.graded)
	}
	// Read:write coherence. Turn 1: 0/4096. Turn 2: 4096/128.
	// Total: 4096 : 4224 ≈ 0.97. cache_read on turn 2 equals the
	// cumulative write from turn 1 — physically realistic
	// (Anthropic Sonnet-grow pattern: read the cached prefix +
	// write the small new tail).
	if res.totalRead > res.totalWritten*2 {
		t.Errorf("opencode: read:write %d:%d implausible — turn 2 read should not exceed cumulative writes", res.totalRead, res.totalWritten)
	}
	if res.totalWritten > 0 {
		t.Logf("  read:write ratio:      %.2f", float64(res.totalRead)/float64(res.totalWritten))
	}
}

// TestGradeKiloCLIMultiTurnFixture runs a 2-turn kilo-cli
// Anthropic-shape fixture through the engine. Same shape as
// opencode (kilo-cli is an OpenCode fork; Anthropic envelope
// when routed through Anthropic providers via the kilo gateway).
func TestGradeKiloCLIMultiTurnFixture(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "kilo.db")
	kiloCLIMultiTurnFixture(t, dbPath)

	a := kilocode.NewCLIWithOptions(nil, []string{dir})
	parsed, err := a.ParseSessionFile(context.Background(), dbPath, 0)
	if err != nil {
		t.Fatalf("kilo-cli parse: %v", err)
	}
	res := gradeObservations(t, "kilo-cli 2-turn", parsed.CacheObservations)

	if res.turnCount != 2 {
		t.Fatalf("kilo-cli: expected 2 graded turns, got %d", res.turnCount)
	}
	if res.kindCounts[cachetrack.KindReanchor] != 1 {
		t.Errorf("kilo-cli: expected 1 Reanchor, got kinds=%v", res.kindCounts)
	}
	writeBucket := res.kindCounts[cachetrack.KindWrite] +
		res.kindCounts[cachetrack.KindInvalidationRewrite] +
		res.kindCounts[cachetrack.KindExpiryRewrite] +
		res.kindCounts[cachetrack.KindModelSwitchRewrite]
	if writeBucket != 1 {
		t.Errorf("kilo-cli: expected 1 write-bucket classification on turn 2, got kinds=%v", res.kindCounts)
	}
	if res.bucketMispred != 0 {
		t.Errorf("kilo-cli: expected 0 §10 mispredicts on Anthropic-shape multi-turn, got %d", res.bucketMispred)
	}
	if res.graded != 1 {
		t.Errorf("kilo-cli: expected 1 §10-graded turn, got %d", res.graded)
	}
	// Read:write coherence. Turn 1: 0/8192. Turn 2: 8000/256.
	// Distinct numbers from opencode (4096 / 4128).
	if res.totalRead > res.totalWritten*2 {
		t.Errorf("kilo-cli: read:write %d:%d implausible", res.totalRead, res.totalWritten)
	}
	if res.totalWritten != 8448 {
		t.Errorf("kilo-cli total_written = %d, expected 8192+256=8448 (kilo numbers must stay distinct from opencode's 4096+128=4224)", res.totalWritten)
	}
	if res.totalWritten > 0 {
		t.Logf("  read:write ratio:      %.2f", float64(res.totalRead)/float64(res.totalWritten))
	}
}

// TestGradeClineCLIImplicitCacheShape grades the cline-cli
// 3-turn synthetic fixture that has cache_write=0 on every turn
// (deepseek-flash via OpenRouter is implicit-cache; cache_creation
// is never billed separately). The result OPENLY documents the
// §15.3 implicit-cache gap the codex deferral rationale rests
// on: turns 2+ predict Write (no matched entry) but observe Hit
// (cache_read>0 fallthrough), surfacing as §10 bucket-mispredicts.
//
// This is the informational case — the test asserts on the
// shape that EXPOSES the gap, not on it being fixed. The §15.3
// work owns the actual fix (an ImplicitCache capability flag
// + a dedicated §7 attribution rule that recognizes
// cache_read>0 + cache_creation=0 as an implicit-cache hit
// without requiring a matched entry).
func TestGradeClineCLIImplicitCacheShape(t *testing.T) {
	observations := []models.CacheTurnObservation{
		{
			SessionID: "sess-implicit",
			MessageID: "msg_a1",
			Model:     "deepseek/deepseek-v4-flash",
			Usage: models.CacheUsage{
				NetInputTokens: 5875, OutputTokens: 112,
				CacheReadTokens: 0, CacheCreationTokens: 0,
			},
			BlockHashes: []models.CacheBlockMeta{
				{LevelLabel: "message", Kind: "text", Role: "user", CanonicalBytes: []byte(`{"type":"text","text":"Hi"}`)},
				{LevelLabel: "message", Kind: "thinking", Role: "assistant", CanonicalBytes: []byte(`{"type":"thinking","text":"greeting"}`)},
				{LevelLabel: "message", Kind: "text", Role: "assistant", CanonicalBytes: []byte(`{"type":"text","text":"Hello"}`)},
			},
		},
		{
			SessionID: "sess-implicit",
			MessageID: "msg_a2",
			Model:     "deepseek/deepseek-v4-flash",
			Usage: models.CacheUsage{
				NetInputTokens: 140169, OutputTokens: 355,
				CacheReadTokens: 5888, CacheCreationTokens: 0,
			},
			BlockHashes: []models.CacheBlockMeta{
				{LevelLabel: "message", Kind: "text", Role: "user", CanonicalBytes: []byte(`{"type":"text","text":"more"}`)},
				{LevelLabel: "message", Kind: "thinking", Role: "assistant", CanonicalBytes: []byte(`{"type":"thinking","text":"catch up"}`)},
				{LevelLabel: "message", Kind: "text", Role: "assistant", CanonicalBytes: []byte(`{"type":"text","text":"Welcome back"}`)},
			},
		},
		{
			SessionID: "sess-implicit",
			MessageID: "msg_a3",
			Model:     "deepseek/deepseek-v4-flash",
			Usage: models.CacheUsage{
				NetInputTokens: 140535, OutputTokens: 244,
				CacheReadTokens: 140416, CacheCreationTokens: 0,
			},
			BlockHashes: []models.CacheBlockMeta{
				{LevelLabel: "message", Kind: "text", Role: "user", CanonicalBytes: []byte(`{"type":"text","text":"what model?"}`)},
				{LevelLabel: "message", Kind: "thinking", Role: "assistant", CanonicalBytes: []byte(`{"type":"thinking","text":"identify"}`)},
				{LevelLabel: "message", Kind: "text", Role: "assistant", CanonicalBytes: []byte(`{"type":"text","text":"Claude"}`)},
			},
		},
	}
	res := gradeObservations(t, "cline-cli 3-turn implicit-cache shape (cache_write=0)", observations)

	if res.turnCount != 3 {
		t.Fatalf("cline-cli: expected 3 graded turns, got %d", res.turnCount)
	}
	// Turn 1: Reanchor (skipped from §10). Turns 2+3 predict Write
	// (no matched entry — T1's cache_creation=0 means no entry was
	// created, and T2's Hit doesn't create one either), observe
	// Hit (cache_read>0 fallthrough). Both fire as §10
	// bucket-mispredicts.
	//
	// §15.3 (c)-phase-2 NO-FLIP CONFIRMATION (Drift #2): the
	// snapshot-before-push lookup fix cannot help cline-cli
	// because the cline-cli session never creates an entry at all
	// — every turn has cache_creation=0. matched=nil at every
	// snapshot lookup, predictKind falls to the "no matched
	// entry" else branch → predict Write → mispredict.
	//
	// The handover/plan claim that this fixture flips 2 → 1 was
	// wrong (operator-confirmed). The implicit-cache gap is the
	// distinct fix path: ImplicitCache capability flag +
	// dedicated §7 attribution rule that emits Hit (or a new
	// ImplicitHit) without requiring a matched entry. Tracked
	// in docs/cache-tracking.md § Known limitations.
	if res.bucketMispred != 2 {
		t.Errorf("cline-cli: expected 2 §10 mispredicts on implicit-cache shape (turns 2+3), got %d — §15.3 (c) does NOT close this gap; the implicit-cache fix path is separately scoped", res.bucketMispred)
	}
	if res.graded != 2 {
		t.Errorf("cline-cli: expected 2 §10-graded turns (turns 2+3; turn 1 Reanchor=skipped), got %d", res.graded)
	}
	t.Logf("  POST-FIX READOUT — cline-cli implicit-cache shape (unchanged):")
	t.Logf("  §15.3 (c)-phase-2 doesn't apply: every turn has cache_creation=0 so no")
	t.Logf("  entry is ever created. Snapshot-before-push has nothing to find.")
	t.Logf("  The implicit-cache gap is the SEPARATE fix path (ImplicitCache cap +")
	t.Logf("  dedicated §7 rule). 2 mispredicts pre AND post §15.3 (c).")
}

// TestLiveSection10Rate_OverContinuationSequence is the operator's
// decisive gate test for the §14.3 push. Runs a realistic Tier-2
// turn sequence through the engine, then calls
// cachetrack.MispredictRateGraded — the EXACT function
// cmd/observer/cache_health.go::computeCacheHealth invokes against
// persisted cache_events rows. Reports numerator, denominator,
// rate, and the per-turn classification so the operator can
// verify whether the Tier-2 continuation case lands in the
// numerator (= would pollute the live regression guard) or in
// denominator-only (= clean).
//
// The sequence includes ALL the shapes the operator named:
//
//	Turn 1 — Reanchor cold seed (cache_creation=4096, read=0)
//	Turn 2 — Tier-2 clean-continuation (cache_read=4096, creation=0)
//	         THIS IS THE CASE THE GATE TURNS ON
//	Turn 3 — Below-min shape (input=200, BOTH cache fields=0)
//	         tests whether isZeroUsage actually excludes it
//	Turn 4 — Anthropic-grow (cache_read=8000, creation=256) —
//	         the clean baseline case
//
// Reconciliation of round-2 vs round-4 framing:
//
//   - rec.Mispredicted (set inside ReconcilePrediction's internal
//     ReconcileResult.Mispredicted bool — reconcile.go:60) fires
//     when bucket(predicted) != bucket(observed). PURPOSE: triggers
//     matched.Apply(TriggerMispredict) in engine.go:427 — entry-
//     state machinery, demotes a live entry to unverified for the
//     forecaster's calibration. ENGINE-INTERNAL ONLY; never
//     persisted as a column on cache_events.
//
//   - outcome.Kind == KindMispredict (the §7 row-12 fallthrough)
//     is what MispredictRate/MispredictRateGraded read from
//     persisted cache_events.kind. PURPOSE: the live regression
//     guard the operator P0-validated. Different from the above.
//
// Round-2 framing error: I said "you counted the wrong thing"
// when telling the operator the cline-cli 0/3 should have been
// 2/2. The 2/2 is the bucket-mispredict signal (entry-state).
// The LIVE §10 rate is the row-12 signal. Both are real metrics;
// they answer different questions. Round 4's claim that the
// live regression guard uses outcome.Kind is the correct one
// for the §14.3 gate. This test proves it through the actual
// function call.
//
// LIVE REGRESSION GUARD COMPOSITION (the P0-proven tools):
//
//	(1) MispredictRateGraded — the headline rate
//	(2) per-event cause breakdown — §10 cause concentration
//	(3) read:write consistency on rewrite events
//
// (1) is necessary but not sufficient. (2) + (3) catch attribution
// regressions that (1) might miss — e.g. a sudden surge in
// "tools_changed" cause on a session that's never seen one before
// would flag in (2) without needing (1) to register a mispredict.
func TestLiveSection10Rate_OverContinuationSequence(t *testing.T) {
	eng := cachetrack.NewEngine(1024)
	base := time.Unix(1_780_000_000, 0)
	eng.Clock = func() time.Time { return base }

	type turnSpec struct {
		label  string
		blocks []models.CacheBlockMeta
		usage  cachetrack.CacheUsageObserved
		msgID  string
	}

	// Four-turn sequence. Same (session, model, scope) tuple so
	// the engine maintains a single chain across all turns.
	specs := []turnSpec{
		{
			label: "T1 cold-seed",
			msgID: "msg_1",
			blocks: []models.CacheBlockMeta{
				{
					LevelLabel: "message", Kind: "text", Role: "user",
					CanonicalBytes: []byte(`{"type":"text","text":"hi"}`),
				},
				{
					LevelLabel: "message", Kind: "text", Role: "assistant",
					CanonicalBytes: []byte(`{"type":"text","text":"hello there"}`),
				},
			},
			usage: cachetrack.CacheUsageObserved{
				NetInputTokens: 1200, OutputTokens: 300,
				CacheReadTokens: 0, CacheCreationTokens: 4096,
			},
		},
		{
			label: "T2 clean-continuation (read>0, creation=0)",
			msgID: "msg_2",
			blocks: []models.CacheBlockMeta{
				{
					LevelLabel: "message", Kind: "text", Role: "user",
					CanonicalBytes: []byte(`{"type":"text","text":"more"}`),
				},
				{
					LevelLabel: "message", Kind: "text", Role: "assistant",
					CanonicalBytes: []byte(`{"type":"text","text":"okay"}`),
				},
			},
			usage: cachetrack.CacheUsageObserved{
				NetInputTokens: 80, OutputTokens: 50,
				CacheReadTokens: 4096, CacheCreationTokens: 0,
			},
		},
		{
			label: "T3 below-min (input>0, both cache fields=0)",
			msgID: "msg_3",
			blocks: []models.CacheBlockMeta{
				{
					LevelLabel: "message", Kind: "text", Role: "user",
					CanonicalBytes: []byte(`{"type":"text","text":"q"}`),
				},
				{
					LevelLabel: "message", Kind: "text", Role: "assistant",
					CanonicalBytes: []byte(`{"type":"text","text":"a"}`),
				},
			},
			usage: cachetrack.CacheUsageObserved{
				NetInputTokens: 200, OutputTokens: 50,
				CacheReadTokens: 0, CacheCreationTokens: 0,
			},
		},
		{
			label: "T4 Anthropic-grow (clean Hit+Write baseline)",
			msgID: "msg_4",
			blocks: []models.CacheBlockMeta{
				{
					LevelLabel: "message", Kind: "text", Role: "user",
					CanonicalBytes: []byte(`{"type":"text","text":"next"}`),
				},
				{
					LevelLabel: "message", Kind: "text", Role: "assistant",
					CanonicalBytes: []byte(`{"type":"text","text":"sure"}`),
				},
			},
			usage: cachetrack.CacheUsageObserved{
				NetInputTokens: 300, OutputTokens: 200,
				CacheReadTokens: 8000, CacheCreationTokens: 256,
			},
		},
	}

	var allKinds []cachetrack.Kind
	var allTokensRead, allTokensWritten []int64
	var perTurnKind []cachetrack.Kind

	for i, spec := range specs {
		blocks := make([]cachetrack.ObserveBlock, 0, len(spec.blocks))
		for _, b := range spec.blocks {
			blocks = append(blocks, cachetrack.ObserveBlock{
				Level:          cachetrack.LevelMessage,
				Kind:           b.Kind,
				CanonicalBytes: b.CanonicalBytes,
				Role:           b.Role,
			})
		}
		out := eng.ObserveTurn(cachetrack.ObserveInput{
			SessionID: "sess-live",
			Model:     "claude-sonnet-4",
			Scope:     "default",
			Tier:      cachetrack.TierTranscript,
			MessageID: spec.msgID,
			Now:       base.Add(time.Duration(i) * time.Second),
			Blocks:    blocks,
			Usage:     spec.usage,
		})
		for _, ev := range out.Events {
			allKinds = append(allKinds, ev.Outcome.Kind)
			allTokensRead = append(allTokensRead, ev.TokensRead)
			allTokensWritten = append(allTokensWritten, ev.TokensWritten)
			perTurnKind = append(perTurnKind, ev.Outcome.Kind)
		}
	}

	// THIS IS THE CALL cmd/observer/cache_health.go LINE 308 MAKES.
	rate, denom := cachetrack.MispredictRateGraded(allKinds, allTokensRead, allTokensWritten)

	// Count numerator manually mirroring MispredictRateGraded's
	// internal loop (reconcile.go:263-280) so the report can show
	// it explicitly rather than back-derive from rate*denom.
	var numerator int
	for i, k := range allKinds {
		if k == cachetrack.Kind("hit") || k == cachetrack.Kind("write") ||
			k == cachetrack.Kind("invalidation_rewrite") || k == cachetrack.Kind("expiry_rewrite") ||
			k == cachetrack.Kind("model_switch_rewrite") {
			// non-skipped, non-mispredict → goes to denom only
			continue
		}
		if k != cachetrack.Kind("mispredict") {
			continue
		}
		zeroUsage := allTokensRead[i] == 0 && allTokensWritten[i] == 0
		if zeroUsage {
			continue // isZeroUsage carve-out
		}
		numerator++
	}

	t.Logf("---- LIVE §10 rate (MispredictRateGraded) over 4-turn sequence ----")
	t.Logf("  call: cachetrack.MispredictRateGraded(kinds, reads, writes)")
	t.Logf("  (this is the EXACT function cmd/observer/cache_health.go:308 calls")
	t.Logf("   against persisted cache_events rows)")
	t.Logf("")
	for i, spec := range specs {
		k := perTurnKind[i]
		// Mirror the exact logic from reconcile.go::MispredictRateGraded
		// (NOT bucketOf — that's the entry-demotion predicate, which is
		// the round-2-vs-round-4 confusion the operator just called out).
		// isRateSkipped: KindBelowMin, KindReanchor, KindCompactionReset.
		// KindMispredict is NOT in isRateSkipped; it's caught by the
		// inline zero-usage check OR flows through to the denom++.
		isSkipped := k == cachetrack.KindBelowMin ||
			k == cachetrack.KindReanchor ||
			k == cachetrack.KindCompactionReset
		isZeroUsageMispredict := k == cachetrack.KindMispredict &&
			allTokensRead[i] == 0 && allTokensWritten[i] == 0
		var roleInRate string
		switch {
		case isSkipped:
			roleInRate = "EXCLUDED from both numerator and denominator (isRateSkipped — Reanchor/BelowMin/CompactionReset)"
		case isZeroUsageMispredict:
			roleInRate = "EXCLUDED from both numerator and denominator (isZeroUsage carve-out: cache_read=0 AND cache_written=0)"
		case k == cachetrack.KindMispredict:
			roleInRate = "NUMERATOR + denominator (real mispredict — non-zero cache usage)"
		default:
			roleInRate = "denominator-only (classified non-mispredict outcome)"
		}
		t.Logf("  turn %d (%s)", i+1, spec.label)
		t.Logf("    outcome.Kind = %s, cache_read=%d, cache_write=%d", k, allTokensRead[i], allTokensWritten[i])
		t.Logf("    role in live rate: %s", roleInRate)
	}
	t.Logf("")
	t.Logf("  HEADLINE NUMBERS:")
	t.Logf("    numerator (mispredicts):  %d", numerator)
	t.Logf("    denominator (graded):     %d", denom)
	t.Logf("    rate:                     %.4f", rate)

	// ASSERTIONS — the operator's gate.

	// (A) Turn 2 (Tier-2 clean-continuation) MUST land as
	// outcome.Kind=KindHit, NOT KindMispredict. This is the
	// case the gate hinges on.
	if perTurnKind[1] != cachetrack.KindHit {
		t.Fatalf("turn 2 outcome.Kind = %s, want KindHit — the structural Tier-2 continuation case IS landing as KindMispredict, which WOULD pollute the live rate; the scoped exclusion the operator asked about must be added before push", perTurnKind[1])
	}

	// (B) Turn 3 (below-min, input>0, cache fields both 0) MUST
	// land as outcome.Kind=KindMispredict, AND the zero-usage
	// carve-out MUST exclude it from the numerator. Confirms
	// isZeroUsage keys on CACHE tokens, not total tokens.
	if perTurnKind[2] != cachetrack.KindMispredict {
		t.Fatalf("turn 3 outcome.Kind = %s, want KindMispredict — below-min not falling through to row-12", perTurnKind[2])
	}
	// Numerator should count zero true mispredicts: turn 1
	// Reanchor (skipped), turn 2 Hit (denom-only), turn 3
	// Mispredict but zero-usage excluded, turn 4 Write/rewrite
	// (denom-only).
	if numerator != 0 {
		t.Fatalf("expected numerator=0 (all mispredict-class events excluded by isRateSkipped or isZeroUsage), got %d — a turn that's supposed to be classified or carved-out leaked into the numerator", numerator)
	}

	// (C) The rate itself is 0.0 since numerator=0.
	if rate != 0.0 {
		t.Fatalf("expected live rate=0.0 for this fixture (no real mispredicts), got %v", rate)
	}
}

// TestPerLevelHashDump_OpenCodeMultiTurn is the calibrate-log-style
// probe the operator gated on: dump per-level canonical hashes
// for the existing 2-turn opencode fixture so we can see whether
// tools/system actually CHANGED between turns. If both levels are
// byte-stable (or empty) across turns, the
// `tools_or_system_changed` cause label that the engine slapped
// on turn 2 is the residual rule firing — NOT a real change
// detection. If either level changed, the byte-diff names the
// volatile leak.
//
// The test does not assert a specific shape — it's informational
// evidence for §14.3 sign-off. Hashes are logged via t.Logf so a
// `go test -v` run shows the per-turn fingerprint.
func TestPerLevelHashDump_OpenCodeMultiTurn(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "opencode.db")
	openCodeMultiTurnFixture(t, dbPath)

	a := opencode.NewWithOptions(nil, []string{dir})
	parsed, err := a.ParseSessionFile(context.Background(), dbPath, 0)
	if err != nil {
		t.Fatalf("opencode parse: %v", err)
	}
	dumpPerLevelHashes(t, "opencode 2-turn", parsed.CacheObservations)

	// Assertion: tools + system MUST be empty across ALL turns
	// for opencode Tier-2. The R1 finding pins it: transcripts
	// never expose tools or system bytes, so the adapter emits
	// only LevelLabel="message" blocks. If this assertion ever
	// fails, the adapter started emitting non-message blocks —
	// audit that change before shipping.
	for i, o := range parsed.CacheObservations {
		tools, system, _ := computeLevelHashesPerTurn(o.BlockHashes)
		if tools != "" {
			t.Errorf("turn %d: tools-level hash non-empty = %q — opencode adapter should never emit LevelTools blocks (R1)", i+1, tools)
		}
		if system != "" {
			t.Errorf("turn %d: system-level hash non-empty = %q — opencode adapter should never emit LevelSystem blocks (R1)", i+1, system)
		}
	}
}

// TestPerLevelHashDump_KiloCLIMultiTurn is the same probe for
// kilo-cli. Same R1 invariant: tools + system must be empty.
func TestPerLevelHashDump_KiloCLIMultiTurn(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "kilo.db")
	kiloCLIMultiTurnFixture(t, dbPath)

	a := kilocode.NewCLIWithOptions(nil, []string{dir})
	parsed, err := a.ParseSessionFile(context.Background(), dbPath, 0)
	if err != nil {
		t.Fatalf("kilo-cli parse: %v", err)
	}
	dumpPerLevelHashes(t, "kilo-cli 2-turn", parsed.CacheObservations)

	for i, o := range parsed.CacheObservations {
		tools, system, _ := computeLevelHashesPerTurn(o.BlockHashes)
		if tools != "" {
			t.Errorf("turn %d: tools-level hash non-empty = %q — kilo-cli adapter should never emit LevelTools blocks (R1)", i+1, tools)
		}
		if system != "" {
			t.Errorf("turn %d: system-level hash non-empty = %q — kilo-cli adapter should never emit LevelSystem blocks (R1)", i+1, system)
		}
	}
}

// TestGradeOpenCodeCleanContinuation grades the operator-requested
// clean-continuation case: turn 1 cold-seeds a small prefix
// (cache_creation=4096), turn 2 reads exactly that prefix back
// (cache_read=4096) with NO new cache_creation. Tools + system
// stay byte-stable (empty) across turns; only message-level
// grows. Pure suffix append.
//
// EXPECTED §7 outcome on turn 2: KindHit + CauseSuffixGrowth
// (the engine's fallthrough rule when cache_read>0 +
// cache_creation==0).
//
// EXPECTED §10 result on turn 2: MISPREDICT. Engine's Tier-2
// predict-Hit code path requires the current end-of-chain hash
// to match an existing entry; turn 1's entry was created at
// hash_after_turn1_blocks, but turn 2's lookup uses
// hash_after_turn1_plus_turn2_blocks (no match). PredictKind
// returns KindWrite; observed is KindHit; bucketWrite vs
// bucketHit → mispredicted=TRUE.
//
// This is the SAME engine pattern that drives the
// cline-cli-implicit-cache mispredicts — but on Anthropic-shape
// providers too, not just implicit-cache providers. The §15.3
// work owns the fix (per-marker entry creation under Tier-2,
// using assumedBreakpoints() which is currently defined but
// not consumed by the engine's lookup path).
//
// The test ASSERTS bucketMispred=1 so a future engine change
// that closes this gap will visibly break the test and force a
// re-evaluation. Until then, this is the structural Tier-2
// behavior the §14.3 batch ships.
//
// Read:write coherence: turn 1 writes 4096, turn 2 reads 4096
// (≤ cumulative writes). Realistic.
func TestGradeOpenCodeCleanContinuation(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "opencode.db")
	openCodeCleanContinuationFixture(t, dbPath)

	a := opencode.NewWithOptions(nil, []string{dir})
	parsed, err := a.ParseSessionFile(context.Background(), dbPath, 0)
	if err != nil {
		t.Fatalf("opencode parse: %v", err)
	}
	dumpPerLevelHashes(t, "opencode clean-continuation", parsed.CacheObservations)
	res := gradeObservations(t, "opencode clean-continuation", parsed.CacheObservations)

	if res.turnCount != 2 {
		t.Fatalf("expected 2 graded turns, got %d", res.turnCount)
	}
	if res.kindCounts[cachetrack.KindReanchor] != 1 {
		t.Errorf("expected 1 Reanchor (turn 1), got kinds=%v", res.kindCounts)
	}
	if res.kindCounts[cachetrack.KindHit] != 1 {
		t.Errorf("expected 1 Hit (turn 2 clean continuation), got kinds=%v", res.kindCounts)
	}
	if res.causeCounts[cachetrack.CauseSuffixGrowth] != 1 {
		t.Errorf("expected 1 suffix_growth cause on turn 2, got causes=%v", res.causeCounts)
	}
	// §15.3 (c)-phase-2 INTENDED FLIP: this fixture used to land
	// 1 bucket-mispredict pre-fix (predict=Write, observed=Hit).
	// Post-fix: snapshot-before-push locates T1's entry, the
	// CacheableTokens gate predicts Hit (tiny per-turn delta on
	// Tier-2 transcript), bucket matches → 0 mispredicts.
	if res.bucketMispred != 0 {
		t.Errorf("[POST-FIX FLIP] expected 0 §10 mispredicts on turn 2 (predicted=Hit, observed=Hit — §15.3 (c) closure), got %d", res.bucketMispred)
	}
	// Read:write sanity: 4096 read / 4096 write = 1.0. Realistic.
	if res.totalRead > res.totalWritten*2 {
		t.Errorf("read:write %d:%d is implausible — clean-continuation should have cache_read ≤ cumulative cache_write", res.totalRead, res.totalWritten)
	}
	t.Logf("  POST-FIX READOUT — clean-continuation grade:")
	t.Logf("  Turn 2: predicted=Hit (snapshot lookup finds T1-entry; tiny per-turn")
	t.Logf("  delta below min-cacheable → CacheableTokens gate emits Hit), observed=Hit.")
	t.Logf("  Bucket match. §15.3 (c)-phase-2 closes this gap (no longer §15.3 land).")
}

// TestGradeKiloCLICleanContinuation grades a DIFFERENT clean-
// continuation case on kilo-cli: turn 1 cold-seeds 8192 tokens
// (distinct from opencode's 4096), turn 2 reads 8000 (≤
// cumulative write). The OpenRouter reasoning_details metadata
// block on the tool part is included to exercise the kilo-
// specific exclusion code path that opencode doesn't have.
//
// Same engine outcome shape as the opencode clean-continuation
// (Reanchor + Hit, 1 §10 mispredict). The distinctness is in
// the fixture data: different token numbers AND the kilo-only
// metadata path. Identical token numbers across adapters would
// prove nothing about the OpenRouter exclusion.
func TestGradeKiloCLICleanContinuation(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "kilo.db")
	kiloCLICleanContinuationFixture(t, dbPath)

	a := kilocode.NewCLIWithOptions(nil, []string{dir})
	parsed, err := a.ParseSessionFile(context.Background(), dbPath, 0)
	if err != nil {
		t.Fatalf("kilo-cli parse: %v", err)
	}
	dumpPerLevelHashes(t, "kilo-cli clean-continuation", parsed.CacheObservations)
	res := gradeObservations(t, "kilo-cli clean-continuation", parsed.CacheObservations)

	if res.turnCount != 2 {
		t.Fatalf("expected 2 graded turns, got %d", res.turnCount)
	}
	if res.kindCounts[cachetrack.KindReanchor] != 1 {
		t.Errorf("expected 1 Reanchor, got %v", res.kindCounts)
	}
	if res.kindCounts[cachetrack.KindHit] != 1 {
		t.Errorf("expected 1 Hit on turn 2, got %v", res.kindCounts)
	}
	if res.causeCounts[cachetrack.CauseSuffixGrowth] != 1 {
		t.Errorf("expected 1 suffix_growth cause, got %v", res.causeCounts)
	}
	// §15.3 (c)-phase-2 INTENDED FLIP (same closure as the
	// opencode clean-continuation case).
	if res.bucketMispred != 0 {
		t.Errorf("[POST-FIX FLIP] expected 0 §10 mispredicts on turn 2 (predicted=Hit, observed=Hit — §15.3 (c) closure), got %d", res.bucketMispred)
	}
	if res.totalRead > res.totalWritten*2 {
		t.Errorf("read:write %d:%d implausible", res.totalRead, res.totalWritten)
	}
	// Sanity check on distinctness from opencode fixture.
	if res.totalWritten != 8192 {
		t.Errorf("kilo-cli total_write = %d, expected 8192 (intentionally distinct from opencode's 4096 to prove this isn't a copy)", res.totalWritten)
	}
	if res.totalRead != 8000 {
		t.Errorf("kilo-cli total_read = %d, expected 8000", res.totalRead)
	}
}

// ---- fixture helpers ----

// openCodeMultiTurnFixture builds a 2-turn opencode session
// with Anthropic-shape Sonnet-grow usage. Turn 1 cold-seeds
// cache_creation=4096; turn 2 reads the seeded prefix back
// (cache_read=4096, exactly what was cached — operator
// coherence requirement: read ≤ cumulative write) AND writes
// 128 new tokens for the small new tail.
func openCodeMultiTurnFixture(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT NOT NULL, time_updated INTEGER NOT NULL)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`INSERT INTO session(id, directory, time_updated) VALUES ('ses_1', '/tmp/oc', 10000)`,
		// Turn 1: cold seed (cache_creation=4096, no read)
		// Turn 2: read=4096 (≤ cumulative write), small new write=128
		`INSERT INTO message(id, session_id, time_created, time_updated, data) VALUES
			('msg_u1', 'ses_1', 1000, 1001,
			 '{"role":"user","agent":"build","model":{"providerID":"anthropic","modelID":"claude-sonnet-4"},"time":{"created":1000}}'),
			('msg_a1', 'ses_1', 2000, 2500,
			 '{"role":"assistant","agent":"build","modelID":"claude-sonnet-4","providerID":"anthropic","path":{"cwd":"/tmp/oc"},"time":{"created":2000,"completed":2500},"finish":"stop","tokens":{"input":1200,"output":300,"reasoning":42,"cache":{"read":0,"write":4096}},"cost":0.022}'),
			('msg_u2', 'ses_1', 5000, 5001,
			 '{"role":"user","agent":"build","model":{"providerID":"anthropic","modelID":"claude-sonnet-4"},"time":{"created":5000}}'),
			('msg_a2', 'ses_1', 6000, 7000,
			 '{"role":"assistant","agent":"build","modelID":"claude-sonnet-4","providerID":"anthropic","path":{"cwd":"/tmp/oc"},"time":{"created":6000,"completed":7000},"finish":"stop","tokens":{"input":300,"output":250,"reasoning":56,"cache":{"read":4096,"write":128}},"cost":0.012}')`,
		// Parts include wall-clock fields inside tool / reasoning
		// to exercise the volatile-exclusion code path across
		// multiple turns; step-finish to exercise the
		// usage-rollup skip.
		`INSERT INTO part(id, message_id, session_id, time_created, time_updated, data) VALUES
			('prt_u1',     'msg_u1', 'ses_1', 1000, 1001,
			 '{"type":"text","text":"Build the cache"}'),
			('prt_tool1',  'msg_a1', 'ses_1', 2100, 2300,
			 '{"type":"tool","tool":"bash","callID":"c1","state":{"status":"completed","input":{"command":"go test","description":"Run"},"output":"ok","metadata":{"output":"ok","exit":0,"description":"Run","truncated":false},"title":"Run @ 2:30 PM","time":{"start":2100,"end":2300}}}'),
			('prt_text1',  'msg_a1', 'ses_1', 2400, 2450,
			 '{"type":"text","text":"Tests pass."}'),
			('prt_step1',  'msg_a1', 'ses_1', 2450, 2500,
			 '{"type":"step-finish","reason":"stop","tokens":{"input":1200,"output":300,"reasoning":42,"total":1542,"cache":{"read":0,"write":4096}},"cost":0.022}'),
			('prt_u2',     'msg_u2', 'ses_1', 5000, 5001,
			 '{"type":"text","text":"Now add a benchmark"}'),
			('prt_tool2',  'msg_a2', 'ses_1', 6100, 6300,
			 '{"type":"tool","tool":"bash","callID":"c2","state":{"status":"completed","input":{"command":"go test -bench .","description":"Bench"},"output":"BenchmarkX-8 10000 ops","metadata":{"output":"BenchmarkX-8 10000 ops","exit":0,"description":"Bench","truncated":false},"title":"Bench @ 2:35 PM","time":{"start":6100,"end":6300}}}'),
			('prt_text2',  'msg_a2', 'ses_1', 6400, 6450,
			 '{"type":"text","text":"` + strings.Repeat("Benchmark added with a long explanation of the results. ", 80) + `"}'),
			('prt_step2',  'msg_a2', 'ses_1', 6450, 7000,
			 '{"type":"step-finish","reason":"stop","tokens":{"input":300,"output":250,"reasoning":56,"total":606,"cache":{"read":4096,"write":128}},"cost":0.012}')`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
}

// openCodeCleanContinuationFixture is the OPERATOR-REQUESTED
// clean-continuation case: turn 1 cold-seeds 4096 tokens; turn 2
// is a PURE message-suffix append with cache_read=4096 (exactly
// the seeded prefix) and cache_creation=0. Tools + system stay
// byte-stable empty across both turns (R1: transcripts never
// expose them; the adapter only emits LevelMessage blocks).
// Only message-level changes.
func openCodeCleanContinuationFixture(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT NOT NULL, time_updated INTEGER NOT NULL)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`INSERT INTO session(id, directory, time_updated) VALUES ('ses_1', '/tmp/oc-cc', 10000)`,
		// Turn 1: cache_read=0, cache_creation=4096 (cold)
		// Turn 2: cache_read=4096 (= cumulative write), cache_creation=0 (clean continuation)
		`INSERT INTO message(id, session_id, time_created, time_updated, data) VALUES
			('msg_u1', 'ses_1', 1000, 1001,
			 '{"role":"user","agent":"build","model":{"providerID":"anthropic","modelID":"claude-sonnet-4"},"time":{"created":1000}}'),
			('msg_a1', 'ses_1', 2000, 2500,
			 '{"role":"assistant","agent":"build","modelID":"claude-sonnet-4","providerID":"anthropic","path":{"cwd":"/tmp/oc-cc"},"time":{"created":2000,"completed":2500},"finish":"stop","tokens":{"input":1200,"output":300,"reasoning":42,"cache":{"read":0,"write":4096}},"cost":0.022}'),
			('msg_u2', 'ses_1', 5000, 5001,
			 '{"role":"user","agent":"build","model":{"providerID":"anthropic","modelID":"claude-sonnet-4"},"time":{"created":5000}}'),
			('msg_a2', 'ses_1', 6000, 7000,
			 '{"role":"assistant","agent":"build","modelID":"claude-sonnet-4","providerID":"anthropic","path":{"cwd":"/tmp/oc-cc"},"time":{"created":6000,"completed":7000},"finish":"stop","tokens":{"input":80,"output":50,"reasoning":0,"cache":{"read":4096,"write":0}},"cost":0.001}')`,
		`INSERT INTO part(id, message_id, session_id, time_created, time_updated, data) VALUES
			('prt_u1',    'msg_u1', 'ses_1', 1000, 1001,
			 '{"type":"text","text":"Seed the cache"}'),
			('prt_tool1', 'msg_a1', 'ses_1', 2100, 2300,
			 '{"type":"tool","tool":"bash","callID":"c1","state":{"status":"completed","input":{"command":"go test","description":"Run"},"output":"ok","metadata":{"output":"ok","exit":0,"description":"Run","truncated":false},"title":"Run @ 2:30 PM","time":{"start":2100,"end":2300}}}'),
			('prt_text1', 'msg_a1', 'ses_1', 2400, 2450,
			 '{"type":"text","text":"Cache seeded."}'),
			('prt_u2',    'msg_u2', 'ses_1', 5000, 5001,
			 '{"type":"text","text":"thanks"}'),
			('prt_text2', 'msg_a2', 'ses_1', 6400, 6450,
			 '{"type":"text","text":"Glad to hear."}')`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
}

// kiloCLIMultiTurnFixture mirrors openCodeMultiTurnFixture for
// the kilo-cli adapter. Same Anthropic-shape numbers; kilo-
// specific additions are messageData.parentID + path.root +
// tokens.total + the openrouter metadata bag on tools (excluded
// from chain per the audit).
func kiloCLIMultiTurnFixture(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE project (id TEXT PRIMARY KEY, worktree TEXT NOT NULL, time_updated INTEGER NOT NULL, sandboxes TEXT NOT NULL DEFAULT '[]')`,
		`CREATE TABLE session (id TEXT PRIMARY KEY, project_id TEXT NOT NULL, directory TEXT NOT NULL, time_updated INTEGER NOT NULL)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`INSERT INTO project(id, worktree, time_updated) VALUES ('proj_a', '/tmp/kilo-test', 1000)`,
		`INSERT INTO session(id, project_id, directory, time_updated) VALUES ('ses_1', 'proj_a', '/tmp/kilo-test', 10000)`,
		// Token numbers intentionally DISTINCT from openCodeMultiTurnFixture
		// (8192 / 8000 vs 4096 / 4096) so identical-number copy-paste
		// errors can't slip past the grader. Kilo's OpenRouter
		// reasoning_details metadata bag on the tool part exercises
		// the kilo-specific exclusion code path (excluded per the
		// audit; would churn the chain if not excluded).
		`INSERT INTO message(id, session_id, time_created, time_updated, data) VALUES
			('msg_u1', 'ses_1', 1000, 1001,
			 '{"role":"user","agent":"code","model":{"providerID":"anthropic","modelID":"claude-opus-4-7"},"time":{"created":1000}}'),
			('msg_a1', 'ses_1', 2000, 2500,
			 '{"parentID":"msg_u1","role":"assistant","mode":"code","agent":"code","path":{"cwd":"/tmp/kilo-test","root":"/tmp/kilo-test"},"modelID":"claude-opus-4-7","providerID":"anthropic","time":{"created":2000,"completed":2500},"finish":"stop","tokens":{"total":9692,"input":1200,"output":300,"reasoning":42,"cache":{"read":0,"write":8192}},"cost":0.034}'),
			('msg_u2', 'ses_1', 5000, 5001,
			 '{"role":"user","agent":"code","model":{"providerID":"anthropic","modelID":"claude-opus-4-7"},"time":{"created":5000}}'),
			('msg_a2', 'ses_1', 6000, 7000,
			 '{"parentID":"msg_a1","role":"assistant","mode":"code","agent":"code","path":{"cwd":"/tmp/kilo-test","root":"/tmp/kilo-test"},"modelID":"claude-opus-4-7","providerID":"anthropic","time":{"created":6000,"completed":7000},"finish":"stop","tokens":{"total":8862,"input":300,"output":250,"reasoning":56,"cache":{"read":8000,"write":256}},"cost":0.014}')`,
		`INSERT INTO part(id, message_id, session_id, time_created, time_updated, data) VALUES
			('prt_u1',    'msg_u1', 'ses_1', 1000, 1001,
			 '{"type":"text","text":"Build the cache"}'),
			('prt_tool1', 'msg_a1', 'ses_1', 2100, 2300,
			 '{"type":"tool","tool":"bash","callID":"c1","state":{"status":"completed","input":{"command":"go test"},"output":"ok","metadata":{"output":"ok","exit":0,"description":"Run","truncated":false},"title":"Run @ 2:30 PM","time":{"start":2100,"end":2300}},"metadata":{"openrouter":{"reasoning_details":[{"type":"text","text":"step1","format":"plain","index":0}]}}}'),
			('prt_text1', 'msg_a1', 'ses_1', 2400, 2450,
			 '{"type":"text","text":"Tests pass."}'),
			('prt_u2',    'msg_u2', 'ses_1', 5000, 5001,
			 '{"type":"text","text":"Now add a benchmark"}'),
			('prt_tool2', 'msg_a2', 'ses_1', 6100, 6300,
			 '{"type":"tool","tool":"bash","callID":"c2","state":{"status":"completed","input":{"command":"go test -bench ."},"output":"BenchmarkX-8 10000 ops","metadata":{"output":"BenchmarkX-8 10000 ops","exit":0,"description":"Bench","truncated":false},"title":"Bench @ 2:35 PM","time":{"start":6100,"end":6300}}}'),
			('prt_text2', 'msg_a2', 'ses_1', 6400, 6450,
			 '{"type":"text","text":"` + strings.Repeat("Benchmark added with a long detailed explanation of the results. ", 320) + `"}')`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
}

// kiloCLICleanContinuationFixture is the kilo equivalent of
// openCodeCleanContinuationFixture, with DISTINCT numbers
// (write=8192 / read=8000 — versus opencode's 4096 / 4096) and
// the kilo-only OpenRouter reasoning_details bag on the tool
// part. Same shape: turn 1 cold-seeds; turn 2 reads back, no new
// write. Tools + system stay byte-stable empty across turns.
func kiloCLICleanContinuationFixture(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE project (id TEXT PRIMARY KEY, worktree TEXT NOT NULL, time_updated INTEGER NOT NULL, sandboxes TEXT NOT NULL DEFAULT '[]')`,
		`CREATE TABLE session (id TEXT PRIMARY KEY, project_id TEXT NOT NULL, directory TEXT NOT NULL, time_updated INTEGER NOT NULL)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`INSERT INTO project(id, worktree, time_updated) VALUES ('proj_a', '/tmp/kilo-cc', 1000)`,
		`INSERT INTO session(id, project_id, directory, time_updated) VALUES ('ses_1', 'proj_a', '/tmp/kilo-cc', 10000)`,
		// Turn 1: cache_creation=8192 (cold), cache_read=0
		// Turn 2: cache_read=8000 (≤ cumulative write), cache_creation=0 (clean continuation)
		`INSERT INTO message(id, session_id, time_created, time_updated, data) VALUES
			('msg_u1', 'ses_1', 1000, 1001,
			 '{"role":"user","agent":"code","model":{"providerID":"anthropic","modelID":"claude-opus-4-7"},"time":{"created":1000}}'),
			('msg_a1', 'ses_1', 2000, 2500,
			 '{"parentID":"msg_u1","role":"assistant","mode":"code","agent":"code","path":{"cwd":"/tmp/kilo-cc","root":"/tmp/kilo-cc"},"modelID":"claude-opus-4-7","providerID":"anthropic","time":{"created":2000,"completed":2500},"finish":"stop","tokens":{"total":9692,"input":1200,"output":300,"reasoning":42,"cache":{"read":0,"write":8192}},"cost":0.034}'),
			('msg_u2', 'ses_1', 5000, 5001,
			 '{"role":"user","agent":"code","model":{"providerID":"anthropic","modelID":"claude-opus-4-7"},"time":{"created":5000}}'),
			('msg_a2', 'ses_1', 6000, 7000,
			 '{"parentID":"msg_a1","role":"assistant","mode":"code","agent":"code","path":{"cwd":"/tmp/kilo-cc","root":"/tmp/kilo-cc"},"modelID":"claude-opus-4-7","providerID":"anthropic","time":{"created":6000,"completed":7000},"finish":"stop","tokens":{"total":8080,"input":80,"output":50,"reasoning":0,"cache":{"read":8000,"write":0}},"cost":0.002}')`,
		`INSERT INTO part(id, message_id, session_id, time_created, time_updated, data) VALUES
			('prt_u1',    'msg_u1', 'ses_1', 1000, 1001,
			 '{"type":"text","text":"Seed the cache"}'),
			('prt_tool1', 'msg_a1', 'ses_1', 2100, 2300,
			 '{"type":"tool","tool":"bash","callID":"c1","state":{"status":"completed","input":{"command":"go test"},"output":"ok","metadata":{"output":"ok","exit":0,"description":"Run","truncated":false},"title":"Run @ 2:30 PM","time":{"start":2100,"end":2300}},"metadata":{"openrouter":{"reasoning_details":[{"type":"text","text":"step1","format":"plain","index":0}]}}}'),
			('prt_text1', 'msg_a1', 'ses_1', 2400, 2450,
			 '{"type":"text","text":"Cache seeded."}'),
			('prt_u2',    'msg_u2', 'ses_1', 5000, 5001,
			 '{"type":"text","text":"thanks"}'),
			('prt_text2', 'msg_a2', 'ses_1', 6400, 6450,
			 '{"type":"text","text":"Glad to help."}')`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
}
