package cachetrack

import (
	"testing"
	"time"
)

// t0 anchors all time arithmetic in attribute_test deterministically.
var t0 = time.Date(2026, 4, 28, 7, 32, 0, 0, time.UTC)

// TestAttribute is the §7 decision-table test. One case per
// attributionRules row in order; assert (Kind, Cause,
// DivergedSeq, DivergedLevel). Per §24.5 + §24.6: any row added
// or removed in attribute.go must add or remove a case here in
// lockstep.
//
// The inputs are minimal — each case sets up the SIGNAL that
// triggers that row, with prior/level/match plumbing crafted so
// earlier rules don't pre-empt. Precedence + Tier-2 residual +
// suffix-growth get their own tests below.
func TestAttribute(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   AttributeInput
		want AttributeOutcome
	}{
		{
			name: "row 1: reanchor (no prior)",
			in: AttributeInput{
				Caps:  CapabilitiesFor(TierProxy),
				Model: "claude-sonnet-4-6",
				Now:   t0,
				Prior: nil,
			},
			want: AttributeOutcome{Kind: KindReanchor, Cause: CauseReanchor, DivergedSeq: -1, DivergedLevel: LevelUnknown},
		},
		{
			name: "row 2: model_changed + warm new-model entry → hit (b9bd459d 07:32 shape)",
			in: AttributeInput{
				Caps:                CapabilitiesFor(TierProxy),
				Model:               "claude-opus-4-7",
				Now:                 t0,
				CacheReadTokens:     101690,
				CacheCreationTokens: 13598,
				Prior:               &PriorSignals{Model: "claude-haiku-4-5"},
				MatchedEntry: &Entry{
					Key:           EntryKey{Model: "claude-opus-4-7", Scope: "x", PrefixHash: "h"},
					State:         StateLive,
					TTL:           TTL1h,
					LastRefreshAt: t0.Add(-30 * time.Minute),
					ExpiresAt:     t0.Add(30 * time.Minute),
				},
			},
			want: AttributeOutcome{Kind: KindHit, Cause: CauseModelChanged, DivergedSeq: -1, DivergedLevel: LevelUnknown},
		},
		{
			name: "row 2: model_changed + cold new-model entry → model_switch_rewrite",
			in: AttributeInput{
				Caps:                CapabilitiesFor(TierProxy),
				Model:               "claude-opus-4-7",
				Now:                 t0,
				CacheCreationTokens: 181864,
				Prior:               &PriorSignals{Model: "claude-haiku-4-5"},
				// No MatchedEntry → cold rewrite for the new model.
			},
			want: AttributeOutcome{Kind: KindModelSwitchRewrite, Cause: CauseModelChanged, DivergedSeq: -1, DivergedLevel: LevelUnknown},
		},
		{
			name: "row 3: fast_toggle",
			in: AttributeInput{
				Caps:                CapabilitiesFor(TierProxy),
				Model:               "claude-opus-4-8",
				Fast:                true,
				Now:                 t0,
				CacheCreationTokens: 5000,
				Prior:               &PriorSignals{Model: "claude-opus-4-8", Fast: false},
			},
			want: AttributeOutcome{Kind: KindInvalidationRewrite, Cause: CauseFastToggle, DivergedSeq: -1, DivergedLevel: LevelUnknown},
		},
		{
			name: "row 4: tools_changed (Tier 1)",
			in: AttributeInput{
				Caps:                CapabilitiesFor(TierProxy),
				Model:               "claude-opus-4-8",
				Now:                 t0,
				CacheCreationTokens: 5000,
				ToolsLevelHash:      "tools-new",
				Prior: &PriorSignals{
					Model:          "claude-opus-4-8",
					ToolsLevelHash: "tools-old",
				},
			},
			want: AttributeOutcome{Kind: KindInvalidationRewrite, Cause: CauseToolsChanged, DivergedSeq: -1, DivergedLevel: LevelTools},
		},
		{
			name: "row 5: system_changed (Tier 1)",
			in: AttributeInput{
				Caps:                CapabilitiesFor(TierProxy),
				Model:               "claude-opus-4-8",
				Now:                 t0,
				CacheCreationTokens: 5000,
				ToolsLevelHash:      "tools",
				SystemLevelHash:     "sys-new",
				Prior: &PriorSignals{
					Model:           "claude-opus-4-8",
					ToolsLevelHash:  "tools",
					SystemLevelHash: "sys-old",
				},
			},
			want: AttributeOutcome{Kind: KindInvalidationRewrite, Cause: CauseSystemChanged, DivergedSeq: -1, DivergedLevel: LevelSystem},
		},
		{
			name: "row 6: context_compacted",
			in: AttributeInput{
				Caps:                CapabilitiesFor(TierProxy),
				Model:               "claude-opus-4-8",
				Now:                 t0,
				CacheCreationTokens: 50000,
				Compaction:          true,
				Prior:               &PriorSignals{Model: "claude-opus-4-8"},
			},
			want: AttributeOutcome{Kind: KindCompactionReset, Cause: CauseContextCompacted, DivergedSeq: -1, DivergedLevel: LevelUnknown},
		},
		{
			name: "row 7: ttl_expired (matched entry past expires_at)",
			in: AttributeInput{
				Caps:                CapabilitiesFor(TierProxy),
				Model:               "claude-opus-4-8",
				Now:                 t0,
				CacheCreationTokens: 100000,
				Prior:               &PriorSignals{Model: "claude-opus-4-8"},
				MatchedEntry: &Entry{
					Key:       EntryKey{Model: "claude-opus-4-8", Scope: "x", PrefixHash: "h"},
					State:     StateLive,
					ExpiresAt: t0.Add(-time.Second),
				},
			},
			want: AttributeOutcome{Kind: KindExpiryRewrite, Cause: CauseTTLExpired, DivergedSeq: -1, DivergedLevel: LevelUnknown},
		},
		{
			name: "row 8: lookback_window_missed (entry >20 blocks behind breakpoint)",
			in: AttributeInput{
				Caps:                  CapabilitiesFor(TierProxy),
				Model:                 "claude-opus-4-8",
				Now:                   t0,
				CacheCreationTokens:   50000,
				BlocksSinceBreakpoint: LookbackWindow + 1,
				Prior:                 &PriorSignals{Model: "claude-opus-4-8"},
				MatchedEntry: &Entry{
					Key:       EntryKey{Model: "claude-opus-4-8", Scope: "x", PrefixHash: "h"},
					State:     StateLive,
					ExpiresAt: t0.Add(30 * time.Minute),
				},
			},
			want: AttributeOutcome{Kind: KindWrite, Cause: CauseLookbackMissed, DivergedSeq: -1, DivergedLevel: LevelUnknown},
		},
		{
			name: "row 9: block_diverged at seq=7",
			in: AttributeInput{
				Caps:                CapabilitiesFor(TierProxy),
				Model:               "claude-opus-4-8",
				Now:                 t0,
				CacheCreationTokens: 50000,
				DivergedSeq:         7,
				Prior:               &PriorSignals{Model: "claude-opus-4-8"},
			},
			want: AttributeOutcome{Kind: KindInvalidationRewrite, Cause: CauseBlockDiverged, DivergedSeq: 7, DivergedLevel: LevelMessage},
		},
		{
			name: "row 10: below_min_cacheable (prefix < family minimum)",
			in: AttributeInput{
				Caps:         CapabilitiesFor(TierProxy),
				Model:        "claude-haiku-4-5", // 4096 family
				Now:          t0,
				PrefixTokens: 512,
				DivergedSeq:  -1,
				Prior:        &PriorSignals{Model: "claude-haiku-4-5"},
			},
			want: AttributeOutcome{Kind: KindBelowMin, Cause: CauseBelowMinCacheable, DivergedSeq: -1, DivergedLevel: LevelUnknown},
		},
		{
			name: "row 11: parallel_cold_start (gap < prior first-response latency)",
			in: AttributeInput{
				Caps:                CapabilitiesFor(TierProxy),
				Model:               "claude-opus-4-8",
				Now:                 t0,
				CacheCreationTokens: 50000,
				DivergedSeq:         -1,
				Prior: &PriorSignals{
					Model:           "claude-opus-4-8",
					FirstResponseMS: 8000, // prior took 8s to first event
					SecondsSince:    3,    // we fired 3s after prior started
				},
			},
			want: AttributeOutcome{Kind: KindWrite, Cause: CauseParallelColdStart, DivergedSeq: -1, DivergedLevel: LevelUnknown},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Attribute(tt.in)
			if got != tt.want {
				t.Errorf("\n got:  %+v\n want: %+v", got, tt.want)
			}
		})
	}
}

// TestAttribute_Precedence_b9bd459dModelSwitchWinsOverTTL pins
// the load-bearing precedence guarantee: row 2 (model_changed)
// beats row 7 (ttl_expired) when BOTH would match. The b9bd459d
// 07:32 haiku→opus capture is the canonical case — a model
// switch is the more-specific explanation, even when TTL would
// also explain the rewrite/hit.
//
// Construction: prior turn was haiku; current turn is opus; the
// matched opus entry is past its expires_at (would fire row 7
// independently) but ALSO present and tagged StateLive (would
// fire row 2 with the entry-lookup hit path). Per first-match-
// wins, row 2 wins → cause=model_changed; kind=hit (the entry
// is still tagged Live by the engine state machine; ttl_expired
// would normally sweep it but the precedence test deliberately
// constructs the racy interleaving).
func TestAttribute_Precedence_b9bd459dModelSwitchWinsOverTTL(t *testing.T) {
	t.Parallel()
	in := AttributeInput{
		Caps:                CapabilitiesFor(TierProxy),
		Model:               "claude-opus-4-7",
		Now:                 t0, // 2026-04-28 07:32 UTC — the captured switch turn
		CacheReadTokens:     101690,
		CacheCreationTokens: 13598,
		Prior: &PriorSignals{
			Model:        "claude-haiku-4-5", // SWITCH
			SecondsSince: 41 * 60,            // 41-min gap — would expire 5m default
		},
		MatchedEntry: &Entry{
			Key:           EntryKey{Model: "claude-opus-4-7", Scope: "x", PrefixHash: "h"},
			State:         StateLive,
			TTL:           TTL1h,
			LastRefreshAt: t0.Add(-2 * time.Hour),
			ExpiresAt:     t0.Add(-time.Hour), // PAST expires_at — would fire row 7
		},
	}
	got := Attribute(in)
	if got.Cause != CauseModelChanged {
		t.Errorf("precedence: Cause = %q, want %q (row 2 beats row 7)", got.Cause, CauseModelChanged)
	}
	if got.Kind != KindHit {
		t.Errorf("kind from entry lookup: Kind = %q, want %q (warm opus entry present + state=live)", got.Kind, KindHit)
	}
}

// TestAttribute_Tier2Residual_ToolsOrSystemChanged pins the
// Tier-2 residual rule: !ToolsVisible AND !SystemVisible AND a
// rewrite happened AND nothing else fired → emit
// tools_or_system_changed. The R1 transcript-half finding
// specifically calls this out as the correct shape for Tier 2.
func TestAttribute_Tier2Residual_ToolsOrSystemChanged(t *testing.T) {
	t.Parallel()
	caps := CapabilitiesFor(TierTranscript)
	if caps.ToolsVisible || caps.SystemVisible {
		t.Fatalf("test invariant: TierTranscript capabilities should hide tools+system, got %+v", caps)
	}
	in := AttributeInput{
		Caps:                caps,
		Model:               "claude-sonnet-4-6",
		Now:                 t0,
		CacheCreationTokens: 75000, // rewrite observed
		DivergedSeq:         -1,    // message chain stable
		Prior: &PriorSignals{
			Model: "claude-sonnet-4-6", // same model
			Fast:  false,
		},
	}
	got := Attribute(in)
	if got.Cause != CauseToolsOrSystemChanged {
		t.Errorf("Cause = %q, want %q (Tier-2 residual)", got.Cause, CauseToolsOrSystemChanged)
	}
	if got.Kind != KindInvalidationRewrite {
		t.Errorf("Kind = %q, want %q", got.Kind, KindInvalidationRewrite)
	}
}

// TestAttribute_Tier2Residual_RequiresWrite proves the residual
// does NOT fire on a clean read — the residual is for
// unexplained REWRITES, not unexplained hits.
func TestAttribute_Tier2Residual_RequiresWrite(t *testing.T) {
	t.Parallel()
	in := AttributeInput{
		Caps:            CapabilitiesFor(TierTranscript),
		Model:           "claude-sonnet-4-6",
		Now:             t0,
		CacheReadTokens: 50000, // read, no write
		DivergedSeq:     -1,
		Prior:           &PriorSignals{Model: "claude-sonnet-4-6"},
	}
	got := Attribute(in)
	if got.Cause == CauseToolsOrSystemChanged {
		t.Errorf("residual fired on a clean read (cause=%q); should fall through to suffix_growth", got.Cause)
	}
	if got.Cause != CauseSuffixGrowth || got.Kind != KindHit {
		t.Errorf("clean read fallthrough: got (kind=%q, cause=%q), want (hit, suffix_growth)", got.Kind, got.Cause)
	}
}

// TestAttribute_SuffixGrowthBaseline_Write proves the unflagged
// baseline: no rule matches AND CacheCreationTokens > 0 →
// kind=write + cause=suffix_growth. Per §7 preamble: the normal
// case "is NOT flagged" by the dashboard.
func TestAttribute_SuffixGrowthBaseline_Write(t *testing.T) {
	t.Parallel()
	in := AttributeInput{
		Caps:                CapabilitiesFor(TierProxy),
		Model:               "claude-opus-4-8",
		Now:                 t0,
		CacheCreationTokens: 1234,
		DivergedSeq:         -1,
		ToolsLevelHash:      "tools",
		SystemLevelHash:     "sys",
		Prior: &PriorSignals{
			Model:           "claude-opus-4-8",
			ToolsLevelHash:  "tools", // unchanged
			SystemLevelHash: "sys",   // unchanged
		},
	}
	got := Attribute(in)
	if got.Kind != KindWrite || got.Cause != CauseSuffixGrowth {
		t.Errorf("got (kind=%q, cause=%q), want (write, suffix_growth)", got.Kind, got.Cause)
	}
}

// TestAttribute_SuffixGrowthBaseline_Hit proves the read-side
// counterpart: no rule + CacheReadTokens>0 → KindHit + suffix_growth.
func TestAttribute_SuffixGrowthBaseline_Hit(t *testing.T) {
	t.Parallel()
	in := AttributeInput{
		Caps:            CapabilitiesFor(TierProxy),
		Model:           "claude-opus-4-8",
		Now:             t0,
		CacheReadTokens: 99999,
		DivergedSeq:     -1,
		ToolsLevelHash:  "tools",
		SystemLevelHash: "sys",
		Prior: &PriorSignals{
			Model:           "claude-opus-4-8",
			ToolsLevelHash:  "tools",
			SystemLevelHash: "sys",
		},
	}
	got := Attribute(in)
	if got.Kind != KindHit || got.Cause != CauseSuffixGrowth {
		t.Errorf("got (kind=%q, cause=%q), want (hit, suffix_growth)", got.Kind, got.Cause)
	}
}

// TestAttribute_FallthroughMispredict proves the row 12 fallthrough:
// no rule matches AND no usage observed → kind=mispredict +
// cause=unknown. §10 reconciliation scores these.
func TestAttribute_FallthroughMispredict(t *testing.T) {
	t.Parallel()
	in := AttributeInput{
		Caps:            CapabilitiesFor(TierProxy),
		Model:           "claude-opus-4-8",
		Now:             t0,
		DivergedSeq:     -1,
		ToolsLevelHash:  "tools",
		SystemLevelHash: "sys",
		Prior: &PriorSignals{
			Model:           "claude-opus-4-8",
			ToolsLevelHash:  "tools",
			SystemLevelHash: "sys",
		},
	}
	got := Attribute(in)
	if got.Kind != KindMispredict || got.Cause != CauseUnknown {
		t.Errorf("got (kind=%q, cause=%q), want (mispredict, unknown)", got.Kind, got.Cause)
	}
}

// TestAttribute_Tier1CapsGateRows4and5 proves that without
// caps.ToolsVisible / caps.SystemVisible, rows 4 + 5 do NOT
// fire even when the input would otherwise match — the spec
// §24.3 capability-branch discipline holds.
func TestAttribute_Tier1CapsGateRows4and5(t *testing.T) {
	t.Parallel()
	// Tier-2 caps (Tools+System hidden) + tools/system signals
	// in the input shouldn't matter; the rules can't see them.
	in := AttributeInput{
		Caps:                CapabilitiesFor(TierTranscript),
		Model:               "claude-opus-4-8",
		Now:                 t0,
		CacheCreationTokens: 50000,
		ToolsLevelHash:      "tools-new",
		SystemLevelHash:     "sys-new",
		DivergedSeq:         -1,
		Prior: &PriorSignals{
			Model:           "claude-opus-4-8",
			ToolsLevelHash:  "tools-old",
			SystemLevelHash: "sys-old",
		},
	}
	got := Attribute(in)
	if got.Cause == CauseToolsChanged || got.Cause == CauseSystemChanged {
		t.Errorf("Tier-2 caps allowed row 4/5 to fire: cause = %q", got.Cause)
	}
	if got.Cause != CauseToolsOrSystemChanged {
		t.Errorf("expected residual to catch the rewrite, got cause = %q", got.Cause)
	}
}

// TestAttribute_Row11Conservative covers the post-C4
// tightening: parallel_cold_start fires ONLY when timing is
// unambiguous. Operator-requested guard against false positives
// (rule is on the soak watch-list). Ambiguous cases (gap close
// to prior latency, sub-second prior latency) fall through to
// suffix_growth — table-driven.
func TestAttribute_Row11Conservative(t *testing.T) {
	t.Parallel()
	base := AttributeInput{
		Caps:                CapabilitiesFor(TierProxy),
		Model:               "claude-opus-4-8",
		Now:                 t0,
		CacheCreationTokens: 50000,
		DivergedSeq:         -1,
	}
	tests := []struct {
		name            string
		firstResponseMS int64
		secondsSince    float64
		wantCause       Cause
	}{
		// Clear inflight: gap 3s < 0.5 * 8s = 4s, latency ≥ 1s → fires.
		{"clear inflight (gap 3s, latency 8s)", 8000, 3, CauseParallelColdStart},
		// Just inside the half threshold: gap 1.9s < 0.5 * 4s = 2s.
		{"just inside half (gap 1.9s, latency 4s)", 4000, 1.9, CauseParallelColdStart},
		// Just outside the half threshold: gap 2.1s ≥ 0.5 * 4s = 2s.
		{"ambiguous: gap == half latency (gap 2s, latency 4s)", 4000, 2.0, CauseSuffixGrowth},
		{"ambiguous: gap above half (gap 2.1s, latency 4s)", 4000, 2.1, CauseSuffixGrowth},
		// Sub-second prior latency: rule self-disqualifies even with tiny gap.
		{"sub-second prior latency (gap 0.05s, latency 0.5s)", 500, 0.05, CauseSuffixGrowth},
		{"exactly at floor (gap 0.4s, latency 1.0s)", 1000, 0.4, CauseParallelColdStart},
		{"under floor (gap 0.4s, latency 0.99s)", 990, 0.4, CauseSuffixGrowth},
		// Zero/negative gap: rule won't fire.
		{"zero gap", 8000, 0, CauseSuffixGrowth},
		{"negative gap (clock skew)", 8000, -1, CauseSuffixGrowth},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := base
			in.Prior = &PriorSignals{
				Model:           "claude-opus-4-8",
				FirstResponseMS: tt.firstResponseMS,
				SecondsSince:    tt.secondsSince,
			}
			got := Attribute(in)
			if got.Cause != tt.wantCause {
				t.Errorf("Cause = %q, want %q (parallel guard)", got.Cause, tt.wantCause)
			}
		})
	}
}

// TestAttribute_Row11ParallelColdStart_DoesNotFireWithoutTiming
// proves Tier 2/3 (no FirstResponseMS) cannot fire row 11.
func TestAttribute_Row11ParallelColdStart_DoesNotFireWithoutTiming(t *testing.T) {
	t.Parallel()
	in := AttributeInput{
		Caps:                CapabilitiesFor(TierTranscript),
		Model:               "claude-opus-4-8",
		Now:                 t0,
		CacheCreationTokens: 50000,
		DivergedSeq:         -1,
		Prior: &PriorSignals{
			Model:           "claude-opus-4-8",
			FirstResponseMS: 0, // not observed → row 11 self-gates off
			SecondsSince:    3,
		},
	}
	got := Attribute(in)
	if got.Cause == CauseParallelColdStart {
		t.Errorf("row 11 fired without timing data: cause = %q", got.Cause)
	}
}

// TestAttribute_Row7VsRow8_LookbackBeatsExpiryNot proves the
// precedence ORDER table-driven: row 7 (ttl_expired) precedes
// row 8 (lookback_missed) in the table, so an entry that is
// BOTH expired AND beyond the lookback window gets attributed
// to ttl_expired (the more-fundamental cause).
func TestAttribute_Row7VsRow8_TTLExpiredBeatsLookback(t *testing.T) {
	t.Parallel()
	in := AttributeInput{
		Caps:                  CapabilitiesFor(TierProxy),
		Model:                 "claude-opus-4-8",
		Now:                   t0,
		CacheCreationTokens:   50000,
		BlocksSinceBreakpoint: LookbackWindow + 5, // would fire row 8
		DivergedSeq:           -1,
		Prior:                 &PriorSignals{Model: "claude-opus-4-8"},
		MatchedEntry: &Entry{
			Key:       EntryKey{Model: "claude-opus-4-8", Scope: "x", PrefixHash: "h"},
			State:     StateLive,
			ExpiresAt: t0.Add(-time.Minute), // would fire row 7
		},
	}
	got := Attribute(in)
	if got.Cause != CauseTTLExpired {
		t.Errorf("row 7 should precede row 8: cause = %q, want %q", got.Cause, CauseTTLExpired)
	}
}

// TestAttribute_Reanchor_BeatsAllOtherSignals proves row 1's
// position: when Prior is nil, no other row can fire because
// they all require Prior or MatchedEntry context the engine
// hasn't built yet.
func TestAttribute_Reanchor_BeatsAllOtherSignals(t *testing.T) {
	t.Parallel()
	in := AttributeInput{
		Caps:                CapabilitiesFor(TierProxy),
		Model:               "claude-opus-4-8",
		Now:                 t0,
		CacheCreationTokens: 999999,
		DivergedSeq:         7,
		ToolsLevelHash:      "tools",
		SystemLevelHash:     "sys",
		Prior:               nil, // first turn — reanchor wins
		MatchedEntry: &Entry{
			Key:       EntryKey{Model: "claude-opus-4-8", Scope: "x", PrefixHash: "h"},
			State:     StateLive,
			ExpiresAt: t0.Add(-time.Hour),
		},
	}
	got := Attribute(in)
	if got.Cause != CauseReanchor || got.Kind != KindReanchor {
		t.Errorf("reanchor should win: got (kind=%q, cause=%q)", got.Kind, got.Cause)
	}
}
