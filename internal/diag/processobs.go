package diag

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/marmutapp/superbased-observer/internal/config"
)

// checkProcessObservability reports the process-observability feature's
// posture in `observer doctor` (docs/process-observability.md §13.2).
//
// This is a one-shot CLI check over the DB + config — it sees DB-derived
// facts (enabled, backend, rows retained), NOT the running daemon's
// in-memory backend health (backend up/down, dropped events, queue
// backlog), which the daemon exposes on its /metrics endpoint instead. The
// feature is opt-in, so a disabled install reports a clean informational OK
// rather than a warning.
func checkProcessObservability(ctx context.Context, database *sql.DB, cfg config.Config) Check {
	p := cfg.Observer.Process
	if !p.Enabled {
		return Check{
			Name:    "process observability",
			Status:  StatusOK,
			Message: "[observer.process] disabled (opt-in; OS-level process capture off)",
		}
	}

	var runs int64
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM process_runs`).Scan(&runs); err != nil {
		return Check{
			Name:    "process observability",
			Status:  StatusFail,
			Message: "could not read process_runs table",
			Details: []string{err.Error()},
		}
	}

	details := []string{
		fmt.Sprintf("backend:       %s", p.Backend),
		fmt.Sprintf("argv mode:     %s", p.Argv.Mode),
		fmt.Sprintf("retention:     %d days", p.RetentionDays),
		fmt.Sprintf("rows retained: %d process runs", runs),
		"runtime health (backend up, dropped events, queue) is on the daemon /metrics endpoint",
	}

	// Enabled but no rows yet: not an error (the backend may be unsupported
	// on this host, or simply nothing has spawned). Nudge toward /metrics
	// and the backend-availability question, which only the daemon answers.
	if runs == 0 {
		return Check{
			Name:    "process observability",
			Status:  StatusWarn,
			Message: "enabled but no process runs captured yet — verify backend availability via the daemon /metrics endpoint",
			Details: details,
		}
	}
	return Check{
		Name:    "process observability",
		Status:  StatusOK,
		Message: fmt.Sprintf("enabled (%s backend), %d process runs retained", p.Backend, runs),
		Details: details,
	}
}
