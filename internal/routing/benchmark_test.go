package routing

import "testing"

// TestImportBenchmarks_Rows — one row per §R7.3 import behavior.
func TestImportBenchmarks_Rows(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		data    string
		wantErr bool
		check   func(t *testing.T, imp BenchmarkImport)
	}{
		{
			name: "valid_thresholds",
			data: `{"version":"routerbench-2026-05","source":"RouterBench","scores":[
				{"model":"frontier-x","coding_score":0.9},
				{"model":"mid-y","coding_score":0.7},
				{"model":"small-z","coding_score":0.3}]}`,
			check: func(t *testing.T, imp BenchmarkImport) {
				if imp.Overrides["frontier-x"] != TierOpusClass ||
					imp.Overrides["mid-y"] != TierSonnetClass ||
					imp.Overrides["small-z"] != TierHaikuClass {
					t.Errorf("placements: %+v", imp.Overrides)
				}
				if imp.Placed != 3 || imp.Provenance() == "" {
					t.Errorf("provenance: %+v", imp)
				}
			},
		},
		{
			name: "low_score_never_places_below_haiku",
			data: `{"version":"v1","source":"SWE-bench","scores":[{"model":"weak","coding_score":0.01}]}`,
			check: func(t *testing.T, imp BenchmarkImport) {
				if imp.Overrides["weak"] != TierHaikuClass {
					t.Errorf("weak placed %s, want haiku-class (price classes are not quality classes)", imp.Overrides["weak"])
				}
			},
		},
		{name: "missing_version_refused", data: `{"source":"X","scores":[]}`, wantErr: true},
		{name: "missing_source_refused", data: `{"version":"v1","scores":[]}`, wantErr: true},
		{name: "out_of_range_score_refused", data: `{"version":"v1","source":"X","scores":[{"model":"m","coding_score":1.5}]}`, wantErr: true},
		{name: "empty_model_refused", data: `{"version":"v1","source":"X","scores":[{"model":"","coding_score":0.5}]}`, wantErr: true},
		{name: "malformed_refused", data: `{"version":`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			imp, err := ImportBenchmarks([]byte(tc.data))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.check != nil {
				tc.check(t, imp)
			}
		})
	}
}

// TestMergeTierOverrides_ExplicitWins pins the §R7.3 precedence:
// explicit > imported > seed.
func TestMergeTierOverrides_ExplicitWins(t *testing.T) {
	t.Parallel()
	merged := MergeTierOverrides(
		map[string]Tier{"m1": TierOpusClass, "m2": TierSonnetClass},
		map[string]Tier{"m1": TierHaikuClass},
	)
	if merged["m1"] != TierHaikuClass {
		t.Errorf("user override lost to benchmark: %v", merged)
	}
	if merged["m2"] != TierSonnetClass {
		t.Errorf("benchmark placement lost: %v", merged)
	}
	// And the resolver applies them over the seed.
	r := NewTierResolver()
	r.Reload(merged)
	if tier, _ := r.Lookup("m1"); tier != TierHaikuClass {
		t.Errorf("resolver placement = %s", tier)
	}
	if tier, _ := r.Lookup("claude-opus-4-8"); tier != TierOpusClass {
		t.Errorf("seed entry lost after merge reload: %s", tier)
	}
}
