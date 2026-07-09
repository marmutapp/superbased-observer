package copilotanalytics

import (
	"context"
	"log/slog"
	"time"
)

// recentDays is the trailing window each tick re-polls. The metrics reports
// restate a day until ~2 days past; the upsert overwrites, so re-polling the last
// few days converges without delta bookkeeping.
const recentDays = 3

// Scheduler drives one Poller on a daily cadence, mirroring
// ccanalytics/codexanalytics. Failures are logged, never fatal — an analytics
// hiccup must not affect the server.
type Scheduler struct {
	poller   *Poller
	interval time.Duration
	lag      time.Duration
	logger   *slog.Logger
	now      func() time.Time
}

// NewScheduler builds a scheduler. Non-positive interval defaults to 24h; the lag
// defaults to 48h — the usage-metrics reports land "within two full days"
// (findings §3), so a 48h margin keeps the trailing window's end on settled data.
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
		lag = 48 * time.Hour
	}
	return &Scheduler{
		poller:   poller,
		interval: interval,
		lag:      lag,
		logger:   logger,
		now:      func() time.Time { return time.Now().UTC() },
	}
}

// Surface reports the scheduler's surface (for logging/wiring).
func (s *Scheduler) Surface() Surface { return s.poller.Surface() }

// Run polls immediately, then on every interval tick, until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	s.logger.Info("copilot-analytics scheduler started",
		"surface", string(s.poller.Surface()), "interval", s.interval.String(), "lag", s.lag.String())
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		s.pollRecent(ctx)
		select {
		case <-ctx.Done():
			s.logger.Info("copilot-analytics scheduler stopping", "surface", string(s.poller.Surface()))
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
		s.logger.Warn("copilot-analytics poll failed",
			"surface", string(s.poller.Surface()), "start", start.Format(time.RFC3339), "err", err)
	} else {
		s.logger.Debug("copilot-analytics polled", "surface", string(s.poller.Surface()), "rows", n)
	}
}
