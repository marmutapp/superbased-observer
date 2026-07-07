package cachewarm

import "testing"

func recWin(value float64, ttl string, anthropic, gemini bool) CacheWindow {
	return CacheWindow{
		Model:              "m",
		PrefixTokens:       20000,
		TTLTier:            ttl,
		ValueAtRiskUSD:     value,
		Supports1hTier:     anthropic,
		RefreshableByPatch: gemini,
	}
}

func TestRecommend_DecisionTable(t *testing.T) {
	cases := []struct {
		name       string
		in         RecommendInput
		wantAction KeepWarmAction
		wantPays   bool
	}{
		{
			name:       "mode off → none",
			in:         RecommendInput{Window: recWin(1.0, "5m", true, false), ResumeConfidence: 0.9, Mode: "off", MinValueUSD: 0.2, MinResumeConfidence: 0.5},
			wantAction: ActionNone,
		},
		{
			name:       "value below floor → none",
			in:         RecommendInput{Window: recWin(0.10, "5m", true, false), ResumeConfidence: 0.9, Mode: "advise", MinValueUSD: 0.2, MinResumeConfidence: 0.5},
			wantAction: ActionNone,
		},
		{
			name:       "resume confidence below floor → none",
			in:         RecommendInput{Window: recWin(1.0, "5m", true, false), ResumeConfidence: 0.2, Mode: "advise", MinValueUSD: 0.2, MinResumeConfidence: 0.5},
			wantAction: ActionNone,
		},
		{
			name:       "gemini explicit → patch_ttl",
			in:         RecommendInput{Window: recWin(1.0, "5m", false, true), ResumeConfidence: 0.9, Mode: "advise", MinValueUSD: 0.2, MinResumeConfidence: 0.5},
			wantAction: ActionPatchTTL,
			wantPays:   true,
		},
		{
			name:       "anthropic 5m → use_1h_tier (advise)",
			in:         RecommendInput{Window: recWin(1.0, "5m", true, false), ResumeConfidence: 0.9, Mode: "advise", MinValueUSD: 0.2, MinResumeConfidence: 0.5},
			wantAction: ActionUse1hTier,
			wantPays:   true,
		},
		{
			name:       "anthropic already 1h, advise → none (no further content-free lever)",
			in:         RecommendInput{Window: recWin(1.0, "1h", true, false), ResumeConfidence: 0.9, Mode: "advise", MinValueUSD: 0.2, MinResumeConfidence: 0.5},
			wantAction: ActionNone,
		},
		{
			name:       "anthropic already 1h, enforce + proxied → replay_ping",
			in:         RecommendInput{Window: recWin(1.0, "1h", true, false), ResumeConfidence: 0.9, Mode: "enforce", Proxied: true, MinValueUSD: 0.2, MinResumeConfidence: 0.5},
			wantAction: ActionReplayPing,
			wantPays:   true,
		},
		{
			name:       "openai implicit, advise → none (no content-free lever)",
			in:         RecommendInput{Window: recWin(1.0, "5m", false, false), ResumeConfidence: 0.9, Mode: "advise", MinValueUSD: 0.2, MinResumeConfidence: 0.5},
			wantAction: ActionNone,
		},
		{
			name:       "openai implicit, enforce + proxied → replay_ping",
			in:         RecommendInput{Window: recWin(1.0, "5m", false, false), ResumeConfidence: 0.9, Mode: "enforce", Proxied: true, MinValueUSD: 0.2, MinResumeConfidence: 0.5},
			wantAction: ActionReplayPing,
			wantPays:   true,
		},
		{
			name:       "enforce but NOT proxied → none",
			in:         RecommendInput{Window: recWin(1.0, "5m", false, false), ResumeConfidence: 0.9, Mode: "enforce", Proxied: false, MinValueUSD: 0.2, MinResumeConfidence: 0.5},
			wantAction: ActionNone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Recommend(tc.in)
			if got.Action != tc.wantAction {
				t.Errorf("action = %q, want %q (rationale: %s)", got.Action, tc.wantAction, got.Rationale)
			}
			if got.PaysOff != tc.wantPays {
				t.Errorf("paysOff = %v, want %v", got.PaysOff, tc.wantPays)
			}
			if got.Rationale == "" {
				t.Errorf("rationale must never be empty")
			}
			if tc.wantPays && got.ProjectedSavingsUSD <= 0 {
				t.Errorf("paying recommendation must project positive savings, got %v", got.ProjectedSavingsUSD)
			}
		})
	}
}
