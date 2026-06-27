package routing

import (
	"encoding/json"
	"fmt"
	"sort"
)

// External benchmark import (§R7.3): versioned LOCAL files refine the
// seed tier table — parity with OpenRouter Pareto's min_coding_score,
// but local and combinable with §R7.2 calibration. NO network at
// decision time: the importer parses bytes the boundary read from
// disk; provenance (source, version, count) is recorded on the result
// and logged at load.
//
// Precedence when the boundary merges: user [routing.tiers] overrides
// WIN over benchmark-derived placements, which win over the shipped
// seed — explicit > imported > default.

// BenchmarkFile is the accepted file shape — a deliberately small
// common denominator of the RouterBench / RouterEval result formats:
// a versioned header plus per-model coding scores in [0, 1].
type BenchmarkFile struct {
	// Version identifies the dataset snapshot (e.g.
	// "routerbench-2026-05"). Required — unversioned imports are
	// refused so provenance stays attributable.
	Version string `json:"version"`
	// Source names the benchmark family (e.g. "RouterBench",
	// "RouterEval", "SWE-bench"). Required.
	Source string `json:"source"`
	// Scores are the per-model results.
	Scores []BenchmarkScore `json:"scores"`
}

// BenchmarkScore is one model's result.
type BenchmarkScore struct {
	Model string `json:"model"`
	// CodingScore is the normalized [0, 1] coding benchmark score
	// (SWE-bench-class pass rate or the RouterBench quality score).
	CodingScore float64 `json:"coding_score"`
}

// BenchmarkImport is the parsed + derived result with provenance.
type BenchmarkImport struct {
	Version   string
	Source    string
	Scores    int
	Placed    int
	Overrides map[string]Tier
}

// Provenance renders the attribution string recorded in logs and
// status surfaces.
func (b BenchmarkImport) Provenance() string {
	return fmt.Sprintf("%s@%s (%d scores, %d placed)", b.Source, b.Version, b.Scores, b.Placed)
}

// Score → tier thresholds. Published mapping, deliberately
// conservative: a low score never places a model in free/local (price
// classes, not quality classes) — it bottoms out at haiku-class.
const (
	benchmarkOpusFloor   = 0.85
	benchmarkSonnetFloor = 0.65
)

// ImportBenchmarks parses a benchmark file and derives tier overrides.
// Malformed files, missing provenance, or out-of-range scores are
// errors — an import either round-trips cleanly or doesn't happen
// (never a silently partial table).
func ImportBenchmarks(data []byte) (BenchmarkImport, error) {
	var f BenchmarkFile
	if err := json.Unmarshal(data, &f); err != nil {
		return BenchmarkImport{}, fmt.Errorf("routing.ImportBenchmarks: parse: %w", err)
	}
	if f.Version == "" || f.Source == "" {
		return BenchmarkImport{}, fmt.Errorf("routing.ImportBenchmarks: version and source are required (provenance, §R7.3)")
	}
	out := BenchmarkImport{
		Version:   f.Version,
		Source:    f.Source,
		Scores:    len(f.Scores),
		Overrides: map[string]Tier{},
	}
	for _, s := range f.Scores {
		if s.Model == "" {
			return BenchmarkImport{}, fmt.Errorf("routing.ImportBenchmarks: score with empty model")
		}
		if s.CodingScore < 0 || s.CodingScore > 1 {
			return BenchmarkImport{}, fmt.Errorf("routing.ImportBenchmarks: %s score %.3f out of [0,1]", s.Model, s.CodingScore)
		}
		out.Overrides[s.Model] = tierForScore(s.CodingScore)
		out.Placed++
	}
	return out, nil
}

// tierForScore is the published score → tier mapping.
func tierForScore(score float64) Tier {
	switch {
	case score >= benchmarkOpusFloor:
		return TierOpusClass
	case score >= benchmarkSonnetFloor:
		return TierSonnetClass
	default:
		return TierHaikuClass
	}
}

// MergeTierOverrides composes benchmark-derived placements with the
// user's explicit [routing.tiers] overrides — explicit wins (§R7.3
// precedence). Deterministic key order for stable logs.
func MergeTierOverrides(benchmark map[string]Tier, user map[string]Tier) map[string]Tier {
	out := make(map[string]Tier, len(benchmark)+len(user))
	keys := make([]string, 0, len(benchmark))
	for k := range benchmark {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out[k] = benchmark[k]
	}
	for k, v := range user {
		out[k] = v
	}
	return out
}
