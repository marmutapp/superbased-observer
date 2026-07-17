package benchmark

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// maxRepeats is the sane ceiling on the per-cell repeat count (audit P0.6): a
// spec must not fan the matrix out to an absurd number of attempts. 1000 is far
// beyond any real variance-estimation need (repeats 3–10 in practice).
const maxRepeats = 1000

// Spec is the declarative, versioned, hashable benchmark run definition
// (plan §3.1). It is parsed from a TOML file — never network-fetched. The cell
// matrix is tasks × configs × repeats.
type Spec struct {
	Name     string   `toml:"name" json:"name"`
	Repeats  int      `toml:"repeats" json:"repeats"`
	Budget   Budget   `toml:"budget" json:"budget"`
	Retry    Retry    `toml:"retry" json:"retry"`
	Analysis Analysis `toml:"analysis" json:"analysis"`
	Tasks    []Task   `toml:"tasks" json:"tasks"`
	Configs  []Config `toml:"configs" json:"configs"`
}

// Budget is the real-spend guardrail block (plan §3.7). The caps are the cost
// policy; `repeats` is a variance knob, NOT a cost policy.
type Budget struct {
	MaxTotalUSD     float64 `toml:"max_total_usd" json:"max_total_usd"`
	MaxCellUSD      float64 `toml:"max_cell_usd" json:"max_cell_usd"`
	MaxTurnsPerCell int     `toml:"max_turns_per_cell" json:"max_turns_per_cell"`
	MaxWallSecCell  int     `toml:"max_wall_sec_cell" json:"max_wall_sec_cell"`
	RequireConfirm  bool    `toml:"require_confirm" json:"require_confirm"`
	JudgeBudgetUSD  float64 `toml:"judge_budget_usd" json:"judge_budget_usd"`
	// Unlimited is the EXPLICIT opt-in that a spec runs without USD caps
	// (audit P0.6). Without it, Validate requires max_total_usd and
	// max_cell_usd to be > 0 — a zero/absent cap silently means unbounded
	// spend in budget.go, which is a spend-safety hole.
	Unlimited bool `toml:"unlimited" json:"unlimited"`
}

// Retry is the pre-declared, config-blind flaky-task policy (plan §3.6). Only
// infra/setup failures are retried; model/answer failures never are.
type Retry struct {
	InfraRetries int `toml:"infra_retries" json:"infra_retries"`
}

// Analysis pre-registers the primary outcome (plan §3.5 multiple-comparisons /
// pre-registration): the baseline config, the non-inferiority margin, and the
// minimum sample floor below which no verdict is declared.
type Analysis struct {
	// BaselineConfig is the config id every other config is compared AGAINST.
	// Empty = the first config in the list.
	BaselineConfig string `toml:"baseline_config" json:"baseline_config"`
	// NonInferiorityMargin is the pre-declared one-sided margin (in success-
	// rate points, 0..1) below which a candidate is "worse". REQUIRED for a
	// candidate_cheaper_noninferior verdict; absent (0), verdicts degrade to
	// no_detected_difference / inconclusive (never a bare "parity").
	NonInferiorityMargin float64 `toml:"noninferiority_margin" json:"noninferiority_margin"`
	// MinSample is the per-config attempt floor below which the report refuses
	// a directional verdict (inconclusive). Default 5.
	MinSample int `toml:"min_sample" json:"min_sample"`
	// MinTasks is the DISTINCT-TASK floor for a directional verdict (audit
	// P0.4): a blocked/paired comparison's independent unit is the task, not
	// the attempt, so N repeats over 2 tasks must NOT satisfy a sample floor.
	// Below MinTasks paired tasks the verdict is insufficient_distinct_tasks.
	// Default 3 (applied by ParseSpec). 0 disables the floor (used by
	// programmatically-built specs that opt out).
	MinTasks int `toml:"min_tasks" json:"min_tasks"`
}

// Task is one corpus entry: a pinned repo + prompt + success criterion.
type Task struct {
	ID              string `toml:"id" json:"id"`
	Repo            string `toml:"repo" json:"repo"`
	Ref             string `toml:"ref" json:"ref"`
	Setup           string `toml:"setup" json:"setup"`
	SetupTimeoutSec int    `toml:"setup_timeout_sec" json:"setup_timeout_sec"`
	Prompt          string `toml:"prompt" json:"prompt"`
	// StripGit is the per-task .git-retention choice (plan §3.9). Default false
	// (keep .git) — stripping can reduce coding-agent fidelity + break history
	// tasks, so it is opt-in per task, never a blanket strip.
	StripGit bool    `toml:"strip_git" json:"strip_git"`
	Success  Success `toml:"success" json:"success"`
}

// Success declares how an attempt on a task is scored. Scorer names match the
// reused obs-eval registry plus the benchmark-only `tests_pass`.
type Success struct {
	Scorer string `toml:"scorer" json:"scorer"`
	Rubric string `toml:"rubric" json:"rubric"` // llm_judge
	Value  string `toml:"value" json:"value"`   // exact_match / contains / icontains
	// Values is the set of substrings a `contains_all` answer must contain
	// (case-insensitive). Used to make a scorer echo-proof: pick discriminating
	// tokens a CORRECT answer must contain but the PROMPT does not, so a bare
	// prompt echo can never pass (audit P0.9 / #1).
	Values     []string `toml:"values" json:"values"`           // contains_all
	Pattern    string   `toml:"pattern" json:"pattern"`         // regex_match
	Cmd        string   `toml:"cmd" json:"cmd"`                 // tests_pass
	TimeoutSec int      `toml:"timeout_sec" json:"timeout_sec"` // tests_pass
	Threshold  float64  `toml:"threshold" json:"threshold"`     // llm_judge pass threshold
}

// Config is one matrix row: a launcher-drivable harness + a pinned model.
type Config struct {
	ID      string `toml:"id" json:"id"`
	Harness string `toml:"harness" json:"harness"`
	Model   string `toml:"model" json:"model"`
}

// Cell is one expanded (task × config × repeat) unit of work.
type Cell struct {
	Task      Task
	Config    Config
	RepeatIdx int
}

// knownScorers is the closed set of scorers a benchmark spec may declare, with
// the params each requires. Table-driven validation (CLAUDE.md #5).
var knownScorers = map[string]func(Success) error{
	"tests_pass": func(s Success) error {
		if strings.TrimSpace(s.Cmd) == "" {
			return fmt.Errorf("tests_pass requires a 'cmd'")
		}
		return nil
	},
	"exact_match": requireValue,
	"contains":    requireValue,
	"icontains":   requireValue,
	"contains_all": func(s Success) error {
		if len(s.Values) == 0 {
			return fmt.Errorf("contains_all requires a non-empty 'values' list")
		}
		for i, v := range s.Values {
			if strings.TrimSpace(v) == "" {
				return fmt.Errorf("contains_all values[%d] is empty", i)
			}
		}
		return nil
	},
	"regex_match": func(s Success) error {
		if strings.TrimSpace(s.Pattern) == "" {
			return fmt.Errorf("regex_match requires a 'pattern'")
		}
		return nil
	},
	"llm_judge": func(s Success) error {
		if strings.TrimSpace(s.Rubric) == "" {
			return fmt.Errorf("llm_judge requires a 'rubric'")
		}
		return nil
	},
}

func requireValue(s Success) error {
	if strings.TrimSpace(s.Value) == "" {
		return fmt.Errorf("%s requires a 'value'", s.Scorer)
	}
	return nil
}

// ParseSpec decodes a TOML spec, applies defaults, and validates it. Pure —
// the caller reads the file bytes and passes the string in.
func ParseSpec(data string) (Spec, error) {
	var spec Spec
	if _, err := toml.Decode(data, &spec); err != nil {
		return Spec{}, fmt.Errorf("benchmark.ParseSpec: decode: %w", err)
	}
	spec.applyDefaults()
	if err := spec.Validate(); err != nil {
		return Spec{}, err
	}
	return spec, nil
}

func (s *Spec) applyDefaults() {
	if s.Repeats == 0 {
		s.Repeats = 5
	}
	if s.Analysis.MinSample == 0 {
		s.Analysis.MinSample = 5
	}
	if s.Analysis.MinTasks == 0 {
		s.Analysis.MinTasks = 3
	}
	if s.Analysis.BaselineConfig == "" && len(s.Configs) > 0 {
		s.Analysis.BaselineConfig = s.Configs[0].ID
	}
}

// Validate enforces the spec's structural + referential integrity. All errors
// are loud (fail the run) — a typo'd scorer or a baseline pointing at no config
// must never silently degrade.
func (s Spec) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("benchmark: spec.name is required")
	}
	if s.Repeats < 1 {
		return fmt.Errorf("benchmark: repeats must be >= 1 (got %d)", s.Repeats)
	}
	if len(s.Tasks) == 0 {
		return fmt.Errorf("benchmark: at least one [[tasks]] is required")
	}
	if len(s.Configs) == 0 {
		return fmt.Errorf("benchmark: at least one [[configs]] is required")
	}
	seenTask := map[string]bool{}
	for i, t := range s.Tasks {
		if strings.TrimSpace(t.ID) == "" {
			return fmt.Errorf("benchmark: tasks[%d] missing id", i)
		}
		if seenTask[t.ID] {
			return fmt.Errorf("benchmark: duplicate task id %q", t.ID)
		}
		seenTask[t.ID] = true
		if strings.TrimSpace(t.Repo) == "" {
			return fmt.Errorf("benchmark: task %q missing repo", t.ID)
		}
		if strings.TrimSpace(t.Prompt) == "" {
			return fmt.Errorf("benchmark: task %q missing prompt", t.ID)
		}
		check, ok := knownScorers[t.Success.Scorer]
		if !ok {
			return fmt.Errorf("benchmark: task %q has unknown scorer %q (known: %s)", t.ID, t.Success.Scorer, strings.Join(ScorerNames(), ", "))
		}
		if err := check(t.Success); err != nil {
			return fmt.Errorf("benchmark: task %q success: %w", t.ID, err)
		}
	}
	seenCfg := map[string]bool{}
	for i, c := range s.Configs {
		if strings.TrimSpace(c.ID) == "" {
			return fmt.Errorf("benchmark: configs[%d] missing id", i)
		}
		if seenCfg[c.ID] {
			return fmt.Errorf("benchmark: duplicate config id %q", c.ID)
		}
		seenCfg[c.ID] = true
		if strings.TrimSpace(c.Harness) == "" {
			return fmt.Errorf("benchmark: config %q missing harness", c.ID)
		}
		if strings.TrimSpace(c.Model) == "" {
			return fmt.Errorf("benchmark: config %q missing model", c.ID)
		}
	}
	if !seenCfg[s.Analysis.BaselineConfig] {
		return fmt.Errorf("benchmark: analysis.baseline_config %q references no config", s.Analysis.BaselineConfig)
	}
	if s.Analysis.NonInferiorityMargin < 0 || s.Analysis.NonInferiorityMargin >= 1 {
		return fmt.Errorf("benchmark: analysis.noninferiority_margin must be in [0,1) (got %v)", s.Analysis.NonInferiorityMargin)
	}
	if s.Analysis.MinTasks < 0 {
		return fmt.Errorf("benchmark: analysis.min_tasks must be >= 0 (got %d)", s.Analysis.MinTasks)
	}
	if err := s.Budget.validate(); err != nil {
		return err
	}
	if s.Repeats > maxRepeats {
		return fmt.Errorf("benchmark: repeats %d exceeds the sane ceiling of %d", s.Repeats, maxRepeats)
	}
	return nil
}

// validate enforces the spend-safety bounds on the budget block (audit P0.6):
// finite caps, and — unless explicitly unlimited — positive USD caps so a
// zero/absent cap can't silently mean "unbounded spend".
func (b Budget) validate() error {
	for name, v := range map[string]float64{
		"max_total_usd": b.MaxTotalUSD, "max_cell_usd": b.MaxCellUSD, "judge_budget_usd": b.JudgeBudgetUSD,
	} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("benchmark: budget.%s must be a finite number (got %v)", name, v)
		}
	}
	if b.JudgeBudgetUSD < 0 {
		return fmt.Errorf("benchmark: budget.judge_budget_usd must be >= 0 (got %v)", b.JudgeBudgetUSD)
	}
	if b.MaxTurnsPerCell < 0 || b.MaxWallSecCell < 0 {
		return fmt.Errorf("benchmark: budget turn/wall caps must be >= 0")
	}
	if b.Unlimited {
		return nil
	}
	if b.MaxTotalUSD <= 0 {
		return fmt.Errorf("benchmark: budget.max_total_usd must be > 0 (or set budget.unlimited = true) — a zero/absent cap means unbounded spend")
	}
	if b.MaxCellUSD <= 0 {
		return fmt.Errorf("benchmark: budget.max_cell_usd must be > 0 (or set budget.unlimited = true) — a zero/absent per-attempt cap leaves each cell unbounded")
	}
	return nil
}

// ExpandCells returns the full (task × config × repeat) matrix, task-major then
// config-major then repeat, so a run's attempts are deterministically ordered.
func (s Spec) ExpandCells() []Cell {
	cells := make([]Cell, 0, len(s.Tasks)*len(s.Configs)*s.Repeats)
	for _, t := range s.Tasks {
		for _, c := range s.Configs {
			for r := 0; r < s.Repeats; r++ {
				cells = append(cells, Cell{Task: t, Config: c, RepeatIdx: r})
			}
		}
	}
	return cells
}

// PlannedCells is len(ExpandCells) without the allocation.
func (s Spec) PlannedCells() int { return len(s.Tasks) * len(s.Configs) * s.Repeats }

// CanonicalJSON returns the deterministic JSON encoding of the effective
// (defaulted) spec — the reproducibility/snapshot payload and the SpecHash
// pre-image. Spec has no maps, so field-ordered JSON is stable.
func (s Spec) CanonicalJSON() (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("benchmark.CanonicalJSON: %w", err)
	}
	return string(b), nil
}

// SpecHash is the sha256 of the canonical JSON — the intent pin (plan §3.1).
// It pins intent, not the moving world; the runtime manifest pins reality.
func (s Spec) SpecHash() string {
	b, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum)
}

// ScorerNames returns the known scorer names, sorted, for validation errors +
// discovery.
func ScorerNames() []string {
	names := make([]string, 0, len(knownScorers))
	for n := range knownScorers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
