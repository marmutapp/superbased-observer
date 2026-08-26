package orgcontract

// benchmark.go is the W3.4 org-parity wire for the node's benchmark harness
// (docs/plans/org-parity-full-depth-plan-2026-08-24.md §4 "W3.4"). The node
// source is internal/db/migrations/061_benchmark.sql +
// internal/store/benchmark.go: benchmark_runs -> benchmark_attempts ->
// benchmark_scores, explicitly documented there as "NODE-LOCAL — these
// tables hold repo paths, prompts, and judge rationales. They MUST NOT leave
// this machine" under the pre-org-parity (teams) posture. Per §0 of the
// org-parity plan (enterprise-first — supersedes that framing), these two
// rows carry those same repo paths / prompts / judge rationales RAW, exactly
// like SessionProcessRow (W2.2) and OTelContentRow: they ship ONLY under
// ShareOptions.shipsRawContent() (full_content, or the enterprise
// admin_managed default) — never under a teams-tier opt-in flag.
//
// Two granularities, mirroring the runs->attempts node relationship:
//   - BenchmarkRunRow is one row per (run, config) — a run can exercise
//     several configs (harness/model combinations) in one spec, and the
//     leaderboard needs per-config aggregates, so the row is deliberately
//     NOT one-per-run. RunKey = run_id + ":" + config_id.
//   - BenchmarkAttemptRow is one row per TERMINAL attempt (the same
//     dedup the node's own internal/store::LoadBenchmarkFacts performs: the
//     highest attempt_no per (task_id, config_id, repeat_idx) cell — retries
//     only follow infra failures, so the max-attempt_no row is the cell's
//     terminal decision). AttemptKey = run_id + ":" + task_id + ":" +
//     config_id + ":" + repeat_idx + ":" + attempt_no.
//
// Cost/tokens are DERIVED, never denormalized: both row types carry the same
// api_turns-joined billing internal/store::LoadBenchmarkFacts already
// computes (success turns only), reusing that seam rather than
// re-implementing the join — "the cost engine stays the one owner of cost"
// (internal/store/benchmark.go's own doc comment).
//
// Repo path (Task.Repo) and task prompt (Task.Prompt) are not columns on
// benchmark_attempts at all — internal/benchmark/spec.go's Task struct is
// serialized whole into benchmark_runs.spec_json. The store-side selector
// (internal/store/benchmarkorgrows.go) unmarshals spec_json into
// internal/benchmark.Spec to recover them; this file just carries the
// resulting strings RAW.
type BenchmarkRunRow struct {
	// OrgID / UserEmail are the agent-stamped attribution (same stamping
	// rule as every other wire row — see ingest.go forcePusherOrgID /
	// forcePusherEmail).
	OrgID     string `json:"org_id,omitempty"`
	UserEmail string `json:"user_email,omitempty"`

	// RunKey is the server-side natural key: run_id + ":" + config_id. A run
	// with N configs produces N rows sharing RunID but distinct RunKey/
	// ConfigID — the leaderboard's per-config grouping key.
	RunKey string `json:"run_key"`
	RunID  string `json:"run_id"`

	// ConfigID / Harness / ModelRequested / ConfigHash are the "config
	// identity" fields: ConfigID is the raw config name from the spec
	// (internal/benchmark.Config.ID); Harness/ModelRequested are the launcher
	// + model actually recorded per attempt (benchmark_attempts.harness /
	// model_requested — not re-derived from spec_json, since the executed
	// value is authoritative). ConfigHash is a DERIVED stable identity —
	// sha256("harness|model|config_id") truncated to 8 bytes, hex-encoded —
	// for grouping the same logical config across runs/fleet, mirroring the
	// existing rollup.ProjectID sha256-truncation pattern
	// (internal/orgserver/rollup/cost.go::ProjectID). It is derived from real
	// node fields, not invented data.
	ConfigID       string `json:"config_id"`
	Harness        string `json:"harness"`
	ModelRequested string `json:"model_requested"`
	ConfigHash     string `json:"config_hash"`

	// SpecName / SpecHash identify the benchmark spec that produced this run
	// (benchmark_runs.spec_name / spec_hash).
	SpecName string `json:"spec_name"`
	SpecHash string `json:"spec_hash"`

	// RepoPathsJSON is a JSON-encoded array of distinct repo paths
	// (internal/benchmark.Task.Repo, RAW) exercised by the run's tasks —
	// recovered from benchmark_runs.spec_json, since Task.Repo is not its
	// own column. Enterprise-raw per §0: repo identity is exactly the kind
	// of content the admin_managed default ships.
	RepoPathsJSON string `json:"repo_paths_json"`

	// TaskCount is the number of distinct tasks declared in the run's spec
	// (internal/benchmark.Spec.Tasks length) — the planned breadth, distinct
	// from ExecutedCells below (which is scoped to this ConfigID).
	TaskCount int64 `json:"task_count"`

	// ExecutedCells / ScoredCells / PassedCells are this config's cell
	// counts within the run, counted over the TERMINAL attempt per
	// (task_id, repeat_idx) cell — the same dedup LoadBenchmarkFacts
	// performs. ScoredCells is cells with at least one scorer verdict;
	// PassedCells is cells whose primary verdict passed.
	ExecutedCells int64 `json:"executed_cells"`
	ScoredCells   int64 `json:"scored_cells"`
	PassedCells   int64 `json:"passed_cells"`

	// SpendUSD is this config's billed cost within the run, summed over the
	// same terminal-cell success-turn api_turns join LoadBenchmarkFacts
	// computes (internal/store/benchmark.go) — never the raw
	// benchmark_attempts.spend_usd estimate.
	SpendUSD float64 `json:"spend_usd"`

	// Status / StartedAt / FinishedAt are the RUN's own header fields
	// (benchmark_runs.status/started_at/finished_at) — identical across every
	// config row of the same run, carried per-row so a single BenchmarkRunRow
	// is self-contained on the server.
	Status     string `json:"status"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`
}

// BenchmarkAttemptRow is one TERMINAL benchmark attempt, RAW (org-parity plan
// §4 W3.4). See the package doc above for the dedup/derivation rules shared
// with BenchmarkRunRow.
type BenchmarkAttemptRow struct {
	// OrgID / UserEmail are the agent-stamped attribution.
	OrgID     string `json:"org_id,omitempty"`
	UserEmail string `json:"user_email,omitempty"`

	// RunKey joins back to the owning BenchmarkRunRow (run_id + ":" +
	// config_id). AttemptKey is the server natural key for this row: run_id +
	// ":" + task_id + ":" + config_id + ":" + repeat_idx + ":" + attempt_no —
	// unique even across retries, since a retry mints a new attempt_no
	// (migration 068) but only the terminal one is ever shipped.
	RunKey     string `json:"run_key"`
	AttemptKey string `json:"attempt_key"`

	RunID     string `json:"run_id"`
	TaskID    string `json:"task_id"`
	ConfigID  string `json:"config_id"`
	RepeatIdx int64  `json:"repeat_idx"`
	AttemptNo int64  `json:"attempt_no"`

	Harness        string `json:"harness"`
	ModelRequested string `json:"model_requested"`

	// TaskPrompt is the task's RAW prompt (internal/benchmark.Task.Prompt),
	// recovered from the owning run's spec_json — enterprise-raw per §0,
	// exactly the class of content admin_managed ships by default.
	TaskPrompt string `json:"task_prompt"`

	// Status is the attempt's terminal benchmark.Status (ok/model_fail/
	// setup_error/...). Scored/Passed are the primary-scorer verdict
	// (any score row exists / any score row passed — the same
	// pre-registered-primary-outcome convention LoadBenchmarkFacts uses).
	Status string `json:"status"`
	Scored bool   `json:"scored"`
	Passed bool   `json:"passed"`
	// Score is the numeric score from the scorer row picked for this
	// attempt (the row with a non-empty rationale when more than one scorer
	// ran, since v1 tasks declare a single primary scorer).
	Score float64 `json:"score,omitempty"`
	// JudgeModel / JudgeRationale are benchmark_scores.judge_model/rationale,
	// RAW — the LLM-judge verdict explanation. Empty when the task's scorer
	// isn't llm_judge (e.g. pattern/cmd scorers never populate rationale).
	JudgeModel     string `json:"judge_model,omitempty"`
	JudgeRationale string `json:"judge_rationale,omitempty"`

	// FinalAnswerExcerpt is benchmark_attempts.final_answer_excerpt, RAW —
	// already an excerpt on the node (never the full transcript).
	FinalAnswerExcerpt string `json:"final_answer_excerpt,omitempty"`

	// SpendUSD / WallMS / Turns / *Tokens are DERIVED the same way as
	// BenchmarkRunRow.SpendUSD — the LoadBenchmarkFacts api_turns join for
	// this attempt's logical cell, success turns only. Turns/WallMS on the
	// fact row (not the raw benchmark_attempts.turns/wall_ms estimate).
	SpendUSD        float64 `json:"spend_usd"`
	WallMS          int64   `json:"wall_ms"`
	Turns           int64   `json:"turns"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	CacheReadTokens int64   `json:"cache_read_tokens"`

	// StartedAt / FinishedAt / ExitCode are the attempt's own raw timing +
	// process exit fields (benchmark_attempts.started_at/finished_at/
	// exit_code). HasExitCode distinguishes a real 0 exit code from "the
	// process never reported one" (exit_code is nullable on the node).
	StartedAt   string `json:"started_at"`
	FinishedAt  string `json:"finished_at,omitempty"`
	ExitCode    int64  `json:"exit_code,omitempty"`
	HasExitCode bool   `json:"has_exit_code"`
}
