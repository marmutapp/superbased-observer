package benchmark

import "time"

// Status is an attempt's machine-readable terminal state. Every value is
// COUNTED in the denominator — silent drops are the cherry-picking the honesty
// doctrine forbids (plan §3.2 "Failure honesty").
type Status string

const (
	// StatusOK — the attempt completed and produced its session(s); the
	// success verdict is then the scorer's, not this field's.
	StatusOK Status = "ok"
	// StatusModelFail — the harness ran but the model refused / errored the
	// task (e.g. an auth-gated model 400). Never retried — it IS the
	// measurement.
	StatusModelFail Status = "model_fail"
	// StatusSetupError — clone/worktree/setup command failed; no session was
	// produced. Retryable as infra (plan §3.6).
	StatusSetupError Status = "setup_error"
	// StatusHarnessError — the launcher subprocess itself failed to run.
	StatusHarnessError Status = "harness_error"
	// StatusProxyError — the proxy was unreachable / the turn never landed.
	StatusProxyError Status = "proxy_error"
	// StatusTimeout — the attempt exceeded its wall-clock cap and was killed.
	StatusTimeout Status = "timeout"
	// StatusBudgetStop — a per-attempt or per-run USD/turn cap tripped.
	StatusBudgetStop Status = "budget_stop"
	// StatusScorerUnavailable — the run finished but no scorer could produce a
	// verdict (e.g. no recoverable final answer, or the judge was not wired).
	StatusScorerUnavailable Status = "scorer_unavailable"
	// StatusOrphaned — the correlation resolver could not attach exactly one
	// session to the attempt (0, >1, or a workspace mismatch). Fail loudly,
	// never guess (plan §3.3 fail-on-ambiguous).
	StatusOrphaned Status = "orphaned"
)

// TerminalStatuses is the closed set, for validation + the report's
// per-status census.
func TerminalStatuses() []Status {
	return []Status{
		StatusOK, StatusModelFail, StatusSetupError, StatusHarnessError,
		StatusProxyError, StatusTimeout, StatusBudgetStop,
		StatusScorerUnavailable, StatusOrphaned,
	}
}

// IsInfraFailure reports whether a status is an infra/setup failure that the
// pre-declared retry policy MAY retry (config-blind). Model/answer failures
// (model_fail, and a plain scorer verdict of "not passed") are NEVER retried.
func (s Status) IsInfraFailure() bool {
	switch s {
	case StatusSetupError, StatusHarnessError, StatusProxyError:
		return true
	default:
		return false
	}
}

// ModelEligible reports whether the attempt actually exercised the model (so it
// belongs in the model-eligible success denominator, plan §3.6). Infra
// failures and orphaned/budget stops did not fairly test the model.
func (s Status) ModelEligible() bool {
	switch s {
	case StatusOK, StatusModelFail, StatusTimeout:
		return true
	default:
		return false
	}
}

// MemberRole classifies a session attached to an attempt.
type MemberRole string

const (
	// RolePrimary — the harness's own top-level session.
	RolePrimary MemberRole = "primary"
	// RoleSubagent — a sub-agent session the harness spawned under the
	// attempt.
	RoleSubagent MemberRole = "subagent"
	// RoleJudge — an llm_judge scoring call routed through the proxy; billed
	// to judge_spend_usd, never to the tested cell (plan §3.4).
	RoleJudge MemberRole = "judge"
)

// RunRecord is the persisted run header (benchmark_runs).
type RunRecord struct {
	RunID               string
	SpecName            string
	SpecHash            string
	SpecJSON            string
	ManifestJSON        string
	PricingSnapshotJSON string
	StartedAt           time.Time
	FinishedAt          time.Time // zero = still running
	Status              string    // running|completed|budget_stop|aborted|error
	PlannedCells        int
	CompletedCells      int
	SpendUSD            float64
	JudgeSpendUSD       float64
	BudgetJSON          string
	Notes               string
}

// AttemptRecord is one persisted attempt (benchmark_attempts). It is a
// first-class row independent of its sessions.
type AttemptRecord struct {
	ID             int64
	RunID          string
	TaskID         string
	ConfigID       string
	Harness        string
	ModelRequested string
	RepeatIdx      int
	// AttemptNo is the physical retry index within the logical cell
	// (run_id, task_id, config_id, repeat_idx). 0 for the first attempt;
	// incremented for each pre-declared infra retry so retries persist as
	// distinct rows instead of colliding on the uniqueness key (migration 068).
	AttemptNo          int
	WorkspacePath      string
	WallMS             int64
	ExitCode           *int
	Status             Status
	FinalAnswerExcerpt string
	SpendUSD           float64
	Turns              int
	ErrorClass         string
	StartedAt          time.Time
	FinishedAt         time.Time
}

// SessionMember attaches one session to an attempt (benchmark_session_members).
type SessionMember struct {
	ID            int64
	AttemptID     int64
	RunID         string
	SessionID     string
	Role          MemberRole
	ModelReturned string
}

// ScoreRecord is one scorer's verdict on one attempt (benchmark_scores).
type ScoreRecord struct {
	ID         int64
	AttemptID  int64
	RunID      string
	Scorer     string
	Score      float64
	Passed     bool
	Rationale  string
	JudgeModel string
	RubricHash string
	Degraded   bool
}

// AttemptFact is the report substrate: one attempt joined to its derived
// billed tokens/cost (summed across its session members' api_turns) and its
// primary-scorer verdict. The store seam (LoadBenchmarkFacts) builds these; the
// pure report code (report.go) consumes them. Cost/tokens are NOT stored on the
// benchmark tables — they are derived here so the cost engine stays the one
// owner of cost.
type AttemptFact struct {
	TaskID    string
	ConfigID  string
	Harness   string
	Model     string // model_requested (the matrix key)
	RepeatIdx int
	Status    Status
	WallMS    int64
	Turns     int

	// Derived from the joined api_turns across all non-judge session members.
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
	// CostUSD is the run-time (snapshot) billed cost summed over the attempt's
	// success turns. The report can additionally reprice at current rates.
	CostUSD float64

	// Sessions is the count of NON-judge sessions attached to the attempt (the
	// "sessions" level in the N-at-every-level census). 0 for a pre-session
	// failure (setup_error).
	Sessions int

	// Scored is true when a primary scorer produced a verdict for this
	// attempt; Passed is that verdict. An unscored attempt (Scored=false) is
	// never counted as a pass.
	Scored bool
	Passed bool
}
