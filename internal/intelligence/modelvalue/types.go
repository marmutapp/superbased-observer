package modelvalue

import (
	"time"

	"github.com/marmutapp/superbased-observer/internal/routing"
)

// TurnRow is one observed model turn from the deduped proxy∪JSONL
// substrate. The store seam builds these; latency/status fields exist
// only on proxy-observed rows, so the capability flags say what was
// actually observed rather than zero-conflating (§24.3).
type TurnRow struct {
	SessionID   string
	ProjectID   int64
	ProjectRoot string
	Timestamp   time.Time
	Model       string

	Input             int64
	Output            int64
	CacheRead         int64
	CacheCreation     int64
	CacheCreation1h   int64
	Reasoning         int64
	WebSearchRequests int64
	Fast              bool

	MessageCount int
	ToolUseCount int

	// TTFTMs / TotalMs are observed proxy timings; meaningful only when
	// HasLatency. HTTPStatus / ErrorClass likewise gate on HasStatus.
	TTFTMs     int64
	TotalMs    int64
	HasLatency bool
	HTTPStatus int
	ErrorClass string
	HasStatus  bool

	// StoredCostUSD is the row's insert-time cost, used as fallback when
	// the injected PriceFn cannot price the model.
	StoredCostUSD float64

	// IsSidechain marks proxy turns the boundary attributed to sub-agent
	// traffic (approximation: the action stream adjacent to the turn).
	IsSidechain bool
}

// ActionRow is one normalized action from the same window, with all
// content resolved away at the store boundary: run_command rows arrive
// with a CommandClass, permission-mode rows with a PhaseHint, and
// subagent_start rows with the persona name — never targets or command
// text.
type ActionRow struct {
	SessionID    string
	Timestamp    time.Time
	Type         string
	CommandClass routing.CommandClass
	Success      bool
	IsSidechain  bool
	DurationMs   int64
	// PhaseHint carries the permission-mode value ("plan" | "default" |
	// "acceptEdits") for permission_mode rows; empty otherwise.
	PhaseHint string
	// SubagentName carries the persona id for subagent_start rows
	// (e.g. "code-reviewer"); empty otherwise.
	SubagentName string
}

// Facts is the loaded evidence bundle Build consumes. The store seam
// (internal/store/modelvalue.go) is the only production constructor;
// tests build literal Facts.
type Facts struct {
	WindowDays  int
	GeneratedAt time.Time
	// Turns and Actions are each sorted (session, timestamp) by the
	// loader; Build re-sorts defensively since ordering is load-bearing.
	Turns   []TurnRow
	Actions []ActionRow
	// Price is the injected pricing seam (cost.ComputeBreakdown at the
	// boundary). Nil degrades to StoredCostUSD per row.
	Price routing.PriceFn
	// Tiers places models; nil uses the shipped seed table.
	Tiers *routing.TierTable
}

// LoadOptions scopes the store-seam loader (internal/store/
// modelvalue.go). Defined here so store and cmd share one shape without
// the store exporting loader types of its own.
type LoadOptions struct {
	// WindowDays bounds the evidence window (default 30).
	WindowDays int
	// ProjectRoot, when set, filters to one project.
	ProjectRoot string
	// Now anchors the window; zero means time.Now().UTC(). Injectable
	// for deterministic replay and tests.
	Now time.Time
}

// EffectiveWindowDays resolves the window default.
func (o LoadOptions) EffectiveWindowDays() int {
	if o.WindowDays <= 0 {
		return 30
	}
	return o.WindowDays
}

// EffectiveNow resolves the window anchor.
func (o LoadOptions) EffectiveNow() time.Time {
	if o.Now.IsZero() {
		return time.Now().UTC()
	}
	return o.Now
}

// Options tunes Build.
type Options struct {
	// MinSample is the evidence floor below which a delta's parity
	// verdict is insufficient_evidence (§R7.2 default n ≥ 50).
	MinSample int64
	// RecentActionWindow is the last-N action window handed to the
	// turn-kind classifier per turn. Default 10.
	RecentActionWindow int
}

func (o Options) minSample() int64 {
	if o.MinSample <= 0 {
		return 50
	}
	return o.MinSample
}

func (o Options) recentWindow() int {
	if o.RecentActionWindow <= 0 {
		return 10
	}
	return o.RecentActionWindow
}

// Cell is one (model, turn-kind, project) aggregate. ProjectID 0 with
// empty ProjectRoot is the global roll-up cell.
type Cell struct {
	Model       string           `json:"model"`
	Tier        routing.Tier     `json:"tier"`
	TurnKind    routing.TurnKind `json:"turn_kind"`
	ProjectID   int64            `json:"project_id"`
	ProjectRoot string           `json:"project_root,omitempty"`

	Turns       int64   `json:"turns"`
	CostUSD     float64 `json:"cost_usd"`
	CostPerTurn float64 `json:"cost_per_turn_usd"`

	LatencyP50Ms  int64 `json:"latency_p50_ms"`
	LatencyP95Ms  int64 `json:"latency_p95_ms"`
	LatencyGraded int64 `json:"latency_graded"`

	ErrorCount  int64   `json:"error_count"`
	ErrorGraded int64   `json:"error_graded"`
	ErrorRate   float64 `json:"error_rate"`

	ToolFailures    int64   `json:"tool_failures"`
	ToolActions     int64   `json:"tool_actions"`
	ToolFailureRate float64 `json:"tool_failure_rate"`
}

// Parity verdicts for a Delta. Closed vocabulary so consumers can filter.
const (
	VerdictParity               = "parity"
	VerdictCandidateWorse       = "candidate_worse"
	VerdictCandidateBetter      = "candidate_better"
	VerdictInsufficientEvidence = "insufficient_evidence"
)

// Delta compares a candidate model against the highest-tier baseline in
// the same (project, turn-kind) group. Rates are reported with both
// sample sizes and a 95% confidence half-width; the verdict is on the
// error-rate difference and degrades to insufficient_evidence below the
// sample floor (§R7.2).
type Delta struct {
	ProjectID   int64            `json:"project_id"`
	ProjectRoot string           `json:"project_root,omitempty"`
	TurnKind    routing.TurnKind `json:"turn_kind"`

	BaselineModel  string       `json:"baseline_model"`
	BaselineTier   routing.Tier `json:"baseline_tier"`
	CandidateModel string       `json:"candidate_model"`
	CandidateTier  routing.Tier `json:"candidate_tier"`

	NBaseline  int64 `json:"n_baseline"`
	NCandidate int64 `json:"n_candidate"`
	// Graded denominators: error rates are observable only on
	// proxy-captured rows, tool-failure rates only where actions were
	// attributable — the verdict gates on these, not on raw turn counts.
	ErrorGradedBaseline  int64 `json:"error_graded_baseline"`
	ErrorGradedCandidate int64 `json:"error_graded_candidate"`
	ToolGradedBaseline   int64 `json:"tool_graded_baseline"`
	ToolGradedCandidate  int64 `json:"tool_graded_candidate"`

	// DeltaErrorRate is candidate − baseline in percentage points;
	// ErrorCI95 is the ± half-width of its 95% confidence interval.
	DeltaErrorRate float64 `json:"delta_error_rate_pp"`
	ErrorCI95      float64 `json:"error_ci95_pp"`

	DeltaToolFailureRate float64 `json:"delta_tool_failure_rate_pp"`
	ToolFailureCI95      float64 `json:"tool_failure_ci95_pp"`

	CostPerTurnBaseline  float64 `json:"cost_per_turn_baseline_usd"`
	CostPerTurnCandidate float64 `json:"cost_per_turn_candidate_usd"`

	// Verdict is the parity grading; VerdictBasis names which dimension
	// graded it ("error_rate" when proxy-observed status rows passed the
	// sample floor on both sides, else "tool_failure_rate" when action
	// attribution did, else empty with insufficient_evidence).
	Verdict      string `json:"verdict"`
	VerdictBasis string `json:"verdict_basis,omitempty"`
}

// Caveat is the attribution-honesty statement every Report carries
// verbatim (§R7.2): the numbers are observational, not causal.
const Caveat = "Attribution is correlational: harder tasks are routed to bigger models, " +
	"so per-model outcome differences are NOT causal quality claims. Deltas ship with " +
	"sample sizes and 95% confidence bands; cells below the sample floor are marked " +
	"insufficient_evidence."

// Report is the Model Value Report.
type Report struct {
	GeneratedAt time.Time `json:"generated_at"`
	WindowDays  int       `json:"window_days"`
	Caveat      string    `json:"caveat"`
	// Cells are the per-project aggregates; GlobalCells roll the same
	// turns up across projects (project_id 0).
	Cells       []Cell  `json:"cells"`
	GlobalCells []Cell  `json:"global_cells"`
	Deltas      []Delta `json:"deltas"`
	// Subagents carries the §R10.3 per-sub-agent recommendations from
	// the sidechain emitter. Empty when no sidechain evidence exists in
	// the window. Read-only in P0 — nothing writes agent config.
	Subagents []routing.SubagentRecommendation `json:"subagents,omitempty"`
}

// GlobalDeltasFor returns the report's global-group (project_id 0)
// parity deltas whose candidate is the given model — the §R7.2
// evidence rows a Channel A apply consults before moving work onto
// that model. Order follows the report's delta ordering; an empty
// result means no graded parity rows exist for the candidate in the
// window (the caller states that honestly rather than inventing one).
func (r *Report) GlobalDeltasFor(candidate string) []Delta {
	out := []Delta{}
	for _, d := range r.Deltas {
		if d.ProjectID == 0 && d.ProjectRoot == "" && d.CandidateModel == candidate {
			out = append(out, d)
		}
	}
	return out
}
