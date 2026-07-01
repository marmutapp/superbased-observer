package ccanalytics

import (
	"context"
	"log/slog"
	"time"
)

// recentDays is how many trailing UTC days each tick re-polls. The analytics
// API restates a day's running total (and the upsert overwrites), so re-polling
// the last few days catches late-settling activity within the ~1h freshness
// boundary without any delta bookkeeping.
const recentDays = 3

// Scheduler drives the Poller on a daily cadence. It mirrors
// budget.Evaluator's loop: poll immediately, then on every interval tick, until
// ctx is cancelled. Failures are logged, never fatal (P1 — an analytics hiccup
// must not affect the server).
type Scheduler struct {
	poller   *Poller
	interval time.Duration
	lag      time.Duration
	logger   *slog.Logger
	now      func() time.Time
}

// NewScheduler builds a scheduler. A non-positive interval defaults to 24h.
func NewScheduler(poller *Poller, intervalHours, lagHours int, logger *slog.Logger) *Scheduler {
	if logger == nil {
		logger = slog.Default()
	}
	interval := time.Duration(intervalHours) * time.Hour
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	lag := time.Duration(lagHours) * time.Hour
	if lag <= 0 {
		lag = 2 * time.Hour
	}
	return &Scheduler{
		poller:   poller,
		interval: interval,
		lag:      lag,
		logger:   logger,
		now:      func() time.Time { return time.Now().UTC() },
	}
}

// Run polls immediately, then on every interval tick, until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	s.logger.Info("cc-analytics scheduler started", "interval", s.interval.String(), "lag", s.lag.String())
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		s.pollRecent(ctx)
		select {
		case <-ctx.Done():
			s.logger.Info("cc-analytics scheduler stopping")
			return
		case <-t.C:
		}
	}
}

// pollRecent polls the trailing recentDays UTC days. The current day is included
// only once it is past the freshness lag, so we never request a day with no
// settled data yet.
func (s *Scheduler) pollRecent(ctx context.Context) {
	now := s.now()
	for i := 0; i < recentDays; i++ {
		day := now.AddDate(0, 0, -i)
		if i == 0 && now.Sub(day.Truncate(24*time.Hour)) < s.lag {
			continue // today hasn't accumulated past the freshness lag yet
		}
		ds := day.Format("2006-01-02")
		if n, err := s.poller.PollDay(ctx, ds); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.logger.Warn("cc-analytics poll failed", "day", ds, "err", err)
		} else {
			s.logger.Debug("cc-analytics polled", "day", ds, "rows", n)
		}
	}
}
