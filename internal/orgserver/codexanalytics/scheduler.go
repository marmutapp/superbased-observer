package codexanalytics

import (
	"context"
	"log/slog"
	"time"
)

// recentDays is the trailing window each tick re-polls. Both surfaces restate a
// day's running total until it is past their freshness boundary, and the upsert
// overwrites, so re-polling the last few days converges without delta bookkeeping.
const recentDays = 3

// Scheduler drives the Poller on a daily cadence, mirroring ccanalytics.Scheduler
// (poll immediately, then on every interval tick, until ctx is cancelled).
// Failures are logged, never fatal — an analytics hiccup must not affect the server.
type Scheduler struct {
	poller   *Poller
	interval time.Duration
	lag      time.Duration
	logger   *slog.Logger
	now      func() time.Time
}

// NewScheduler builds a scheduler. Non-positive interval defaults to 24h; the lag
// defaults to 13h — Codex-Enterprise analytics lags up to ~12h (findings Q-C5),
// generous margin so the trailing window's end never outruns settled data.
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
		lag = 13 * time.Hour
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
	s.logger.Info("codex-analytics scheduler started",
		"surface", string(s.poller.Surface()), "interval", s.interval.String(), "lag", s.lag.String())
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		s.pollRecent(ctx)
		select {
		case <-ctx.Done():
			s.logger.Info("codex-analytics scheduler stopping")
			return
		case <-t.C:
		}
	}
}

// pollRecent polls the trailing recentDays window, ending at now minus the
// freshness lag so the window never includes data that has not settled.
func (s *Scheduler) pollRecent(ctx context.Context) {
	now := s.now()
	end := now.Add(-s.lag)
	start := end.AddDate(0, 0, -recentDays)
	if !end.After(start) {
		return
	}
	if n, err := s.poller.PollWindow(ctx, start, end); err != nil {
		if ctx.Err() != nil {
			return
		}
		s.logger.Warn("codex-analytics poll failed",
			"surface", string(s.poller.Surface()), "start", start.Format(time.RFC3339), "err", err)
	} else {
		s.logger.Debug("codex-analytics polled", "surface", string(s.poller.Surface()), "rows", n)
	}
}
