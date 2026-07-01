//go:build no_obs

package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"

	"github.com/marmutapp/superbased-observer/internal/config"
	otlpingest "github.com/marmutapp/superbased-observer/internal/ingest/otlp"
	"github.com/marmutapp/superbased-observer/internal/intelligence/dashboard"
	"github.com/marmutapp/superbased-observer/internal/store"
)

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
