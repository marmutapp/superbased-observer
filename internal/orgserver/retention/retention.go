// Package retention implements the org server's only content-retention sweep:
// an admin-configurable, default-OFF horizon that NULLs stored message bodies
// (otel_content) once they age past the configured number of days, while
// keeping each row's content_hash so audit / dedup / re-push remain intact.
//
// It mirrors the individual node's retention model (config-driven horizon, ≤0
// disables) but is scoped to the single content-bearing table the org server
// stores. The pure prune (PruneOTelContent) takes a *sql.DB and is independent
// of the scheduler so it can be unit-tested and invoked ad hoc.
package retention

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"
)

// PruneOTelContent NULLs the content body of every otel_content row whose event
// time (timestamp, falling back to pushed_at) is older than horizonDays before
// now, leaving content_hash and the row intact. A horizonDays ≤ 0 is a no-op
// (retention disabled — keep forever). Returns the number of bodies cleared.
//
// Idempotent: a second run clears nothing new because already-NULL bodies are
// excluded by the `content IS NOT NULL` guard.
func PruneOTelContent(ctx context.Context, db *sql.DB, horizonDays int, now time.Time) (int64, error) {
	if horizonDays <= 0 {
		return 0, nil
	}
	cutoff := now.UTC().AddDate(0, 0, -horizonDays).Format(time.RFC3339)
	// COALESCE(NULLIF(timestamp,''), pushed_at): event time when present, else
	// the (NOT NULL) push time, so a row with a missing timestamp still ages out.
	res, err := db.ExecContext(ctx, `
UPDATE otel_content
   SET content = NULL
 WHERE content IS NOT NULL
   AND COALESCE(NULLIF(timestamp,''), pushed_at) < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("retention.PruneOTelContent: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("retention.PruneOTelContent: rows: %w", err)
	}
	return n, nil
}

// PruneObsContent NULLs the content body of every obs_content row (obs-org-tier
// T3) whose event time (timestamp, falling back to pushed_at) is older than
// horizonDays before now, leaving content_hash + the row intact. Same horizon,
// same idempotency, same hash-preserving posture as PruneOTelContent — the org
// content-retention policy covers BOTH content stores under one knob.
func PruneObsContent(ctx context.Context, db *sql.DB, horizonDays int, now time.Time) (int64, error) {
	if horizonDays <= 0 {
		return 0, nil
	}
	cutoff := now.UTC().AddDate(0, 0, -horizonDays).Format(time.RFC3339)
	res, err := db.ExecContext(ctx, `
UPDATE obs_content
   SET content = NULL
 WHERE content IS NOT NULL
   AND COALESCE(NULLIF(timestamp,''), pushed_at) < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("retention.PruneObsContent: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("retention.PruneObsContent: rows: %w", err)
	}
	return n, nil
}

// Sweeper runs PruneOTelContent on a daily cadence. It mirrors the analytics
// schedulers' loop (run immediately, then on every interval tick, until ctx is
// cancelled; failures logged, never fatal). It is only constructed when the
// horizon is positive, so its mere existence means retention is enabled.
type Sweeper struct {
	db          *sql.DB
	horizonDays int
	interval    time.Duration
	logger      *slog.Logger
	now         func() time.Time
}

// NewSweeper builds a daily content-retention sweeper for the given horizon.
// Returns nil when horizonDays ≤ 0 (retention disabled — keep forever), so the
// caller can simply skip launching a nil sweeper.
func NewSweeper(db *sql.DB, horizonDays int, logger *slog.Logger) *Sweeper {
	if horizonDays <= 0 {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Sweeper{
		db:          db,
		horizonDays: horizonDays,
		interval:    24 * time.Hour,
		logger:      logger,
		now:         func() time.Time { return time.Now().UTC() },
	}
}

// Run prunes immediately, then on every interval tick, until ctx is cancelled.
func (s *Sweeper) Run(ctx context.Context) {
	s.logger.Info("content-retention sweeper started", "otel_content_days", s.horizonDays, "interval", s.interval.String())
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		now := s.now()
		if n, err := PruneOTelContent(ctx, s.db, s.horizonDays, now); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.logger.Warn("content-retention prune failed", "store", "otel_content", "err", err)
		} else if n > 0 {
			s.logger.Info("content-retention pruned otel bodies", "cleared", n)
		}
		if n, err := PruneObsContent(ctx, s.db, s.horizonDays, now); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.logger.Warn("content-retention prune failed", "store", "obs_content", "err", err)
		} else if n > 0 {
			s.logger.Info("content-retention pruned obs bodies", "cleared", n)
		}
		select {
		case <-ctx.Done():
			s.logger.Info("content-retention sweeper stopping")
			return
		case <-t.C:
		}
	}
}
