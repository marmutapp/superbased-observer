//go:build no_obs

package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/diag"
	otlpingest "github.com/marmutapp/superbased-observer/internal/ingest/otlp"
	"github.com/marmutapp/superbased-observer/internal/intelligence/dashboard"
	"github.com/marmutapp/superbased-observer/internal/proxy"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// wireAdmission is a no-op in the no_obs build — admission lives inside the
// observability subsystem, which is compiled out here.
func wireAdmission(_ context.Context, _ config.Config, _ *sql.DB, _ *proxy.Options, _ *slog.Logger) {
}

// errObsCompiledOut is returned by the eval CLI wrappers in the no_obs build:
// the whole observability subsystem (incl. the eval plane) is compiled out.
var errObsCompiledOut = errors.New("this binary was built without observability (no_obs) — the eval plane is unavailable")

// Plain eval shapes the (non-build-tagged) eval command references. Mirror the
// !no_obs definitions so the command file compiles in both builds.
type obsDatasetInfo struct {
	ID          int64
	Name        string
	Description string
	CreatedAt   string
	ItemCount   int64
}

type obsEvalSummary struct {
	RunID     int64
	Total     int
	Passed    int
	MeanScore float64
	PassRate  float64
}

func obsEvalEnabled(_ config.Config) bool { return false }

func obsEvalScorerNames() []string { return nil }

func obsEvalCreateDatasetFromTraces(_ context.Context, _ config.Config, _ *sql.DB, _ *slog.Logger, _, _ string, _ int) (int64, int, error) {
	return 0, 0, errObsCompiledOut
}

func obsEvalListDatasets(_ context.Context, _ config.Config, _ *sql.DB, _ *slog.Logger) ([]obsDatasetInfo, error) {
	return nil, errObsCompiledOut
}

func obsEvalRun(_ context.Context, _ config.Config, _ *sql.DB, _ *slog.Logger, _ string, _ []string, _, _, _ string, _ float64) (obsEvalSummary, error) {
	return obsEvalSummary{}, errObsCompiledOut
}

// newObsTraceHandler is the no-op stub for the no_obs build: the generalized
// observability subsystem (internal/obs) is compiled out of the binary
// entirely (decision D2). Nothing here imports internal/obs, and /v1/traces is
// never served.
func newObsTraceHandler(_ context.Context, _ config.Config, _ *sql.DB, _ *slog.Logger) otlpingest.TraceHandler {
	return nil
}

// obsDashboardRoutes is the no_obs stub: no obs trajectory endpoints exist.
func obsDashboardRoutes(_ context.Context, _ config.Config, _ *sql.DB, _ *slog.Logger) []dashboard.ExtraRoute {
	return nil
}

// obsOrgProviders is the no_obs stub: the org-tier observability provider seam
// is empty (every tier no-ops; obs is compiled out).
func obsOrgProviders(_ context.Context, _ config.Config, _ *sql.DB, _ *slog.Logger) store.ObsOrgProviders {
	return store.ObsOrgProviders{}
}

// Plain admission shapes the (non-build-tagged) admission command references.
// Mirror the !no_obs definitions so the command file compiles in both builds.
type obsAdmissionStatusInfo struct {
	Enabled       bool
	Mode          string
	JudgeHosting  string
	CriteriaCount int
	PolicyHash    string
	Decisions24h  map[string]int
	ChainRows     int
	ChainOK       bool
}

type obsAdmissionTestResult struct {
	Disabled        bool
	Decision        string
	Severity        string
	Criterion       string
	Reason          string
	Degraded        string
	JudgeUsed       bool
	EnforceDecision string
	LatencyMS       int
}

func obsAdmissionStatusCLI(_ context.Context, _ config.Config, _ *sql.DB, _ *slog.Logger) (obsAdmissionStatusInfo, error) {
	return obsAdmissionStatusInfo{Mode: "off", Decisions24h: map[string]int{}}, errObsCompiledOut
}

type obsBudgetSpender struct {
	User     string
	FiveHour float64
	Weekly   float64
	Monthly  float64
}

type obsAdmissionBudgetStatus struct {
	Enabled     bool
	FiveHourUSD float64
	WeeklyUSD   float64
	MonthlyUSD  float64
	UserHeader  string
	Breaches24h int
	TopSpenders []obsBudgetSpender
}

func obsAdmissionBudgetStatusCLI(_ context.Context, _ config.Config, _ *sql.DB, _ *slog.Logger, _ int) (obsAdmissionBudgetStatus, error) {
	return obsAdmissionBudgetStatus{}, errObsCompiledOut
}

func obsAdmissionTestCLI(_ context.Context, _ config.Config, _ *sql.DB, _ *slog.Logger, _ string) (obsAdmissionTestResult, error) {
	return obsAdmissionTestResult{Disabled: true}, errObsCompiledOut
}

func obsAdmissionLintCLI(_ config.Config) (issues []string, fatal bool) {
	return nil, false
}

// Plain setup-wizard shapes + wrappers for the no_obs build.
type obsAdmissionTemplate struct {
	Key          string
	Title        string
	Description  string
	NeedsPurpose bool
	NeedsTopics  bool
}

type obsAdmissionProbeJudgeResult struct {
	Hosting   string
	Model     string
	Off       bool
	OK        bool
	LatencyMS int64
	Err       string
}

func obsAdmissionStarterTemplates() []obsAdmissionTemplate { return nil }

func obsAdmissionRenderTemplate(_, _ string, _ []string) (config.AdmissionCriterionConfig, bool) {
	return config.AdmissionCriterionConfig{}, false
}

func obsAdmissionProbeJudge(_ context.Context, _ config.Config) obsAdmissionProbeJudgeResult {
	return obsAdmissionProbeJudgeResult{Off: true}
}

type obsAdmissionSimulateResult struct {
	Disabled     bool
	Replayed     int
	PolicyHash   string
	JudgeCalls   int
	WouldBlock   int
	Decisions    map[string]int
	PerCriterion map[string]int
}

func obsAdmissionSimulateCLI(_ context.Context, _ config.Config, _ *sql.DB, _ *slog.Logger, _ int) (obsAdmissionSimulateResult, error) {
	return obsAdmissionSimulateResult{Disabled: true, Decisions: map[string]int{}, PerCriterion: map[string]int{}}, errObsCompiledOut
}

// obsAdmissionDoctorChecks is a no-op in the no_obs build — the admission
// health checks live inside the observability subsystem, compiled out here.
func obsAdmissionDoctorChecks(_ context.Context, _ config.Config, _ *sql.DB, _ *slog.Logger) []diag.Check {
	return nil
}

// Plain calibration shapes the (non-build-tagged) admission command references.
type obsCalibrateProbe struct {
	Label     string
	Decision  string
	JudgeUsed bool
	Degraded  string
	LatencyMS int
}

type obsAdmissionCalibrateResult struct {
	Off       bool
	Reason    string
	Model     string
	Hosting   string
	TargetMS  int64
	Probes    int
	P50MS     int
	P95MS     int
	MaxMS     int
	Degraded  int
	JudgeUsed int
	Decisions map[string]int
	PerProbe  []obsCalibrateProbe
	Recommend bool
	Verdict   string
}

func obsAdmissionCalibrateCLI(_ context.Context, _ config.Config, _ *sql.DB, _ *slog.Logger, _ string, _ int64) (obsAdmissionCalibrateResult, error) {
	return obsAdmissionCalibrateResult{Off: true, Decisions: map[string]int{}}, errObsCompiledOut
}
