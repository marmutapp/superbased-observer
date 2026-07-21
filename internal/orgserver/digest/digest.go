// Package digest is the org-server SCHEDULER + data-assembly boundary for
// scheduled report digests (gap-register G13). It mirrors the budget / obsalert
// evaluator pattern: a goroutine that ticks on an interval, composes a
// content-free org spend rollup for the most-recently-completed period, and
// delivers it through the shared fail-soft email notifier.
//
// The PURE composition lives in internal/notify/digest; this package owns the
// SQL (rollup reuse + the send-once de-dup marker in digest_state) and the
// clock. Fail-soft + P1 like every evaluator: a digest failure logs a warning
// and never affects the server.
package digest

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	notifydigest "github.com/marmutapp/superbased-observer/internal/notify/digest"
	"github.com/marmutapp/superbased-observer/internal/notify/email"
	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
	"github.com/marmutapp/superbased-observer/internal/orgserver/rollup"
)

// stateKind is the digest_state row key for the org digest.
const stateKind = "org"

// tickInterval is how often the scheduler re-evaluates whether a digest is due.
// Hourly gives the send_hour gate its granularity.
const tickInterval = time.Hour

// topMoversN caps the movers list and each spend breakdown in the email.
const topMoversN = 8

// Scheduler drives the org digest. Non-nil only when [digest].enabled AND the
// shared [email] notifier is present; the server launches Run alongside the
// budget evaluator.
type Scheduler struct {
	db        *sql.DB
	org       orgdb.Org
	cfg       notifydigest.Config
	notifier  *email.Notifier
	version   string
	defaultTo []string
	logger    *slog.Logger
	now       func() time.Time
	// interval is the tick cadence (hourly in production; overridable in tests).
	interval time.Duration
	// send delivers one composed message. Defaults to notifier.Send; tests
	// substitute a recorder (mirrors budget.Evaluator.Deliver).
	send func(context.Context, email.Message)
}

// NewScheduler builds the org digest scheduler. notifier must be non-nil (the
// caller only constructs a scheduler when [email] is enabled). defaultTo is the
// [email].to fallback used when [digest].to is empty.
func NewScheduler(db *sql.DB, org orgdb.Org, cfg notifydigest.Config, notifier *email.Notifier, version string, defaultTo []string, logger *slog.Logger) *Scheduler {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Scheduler{
		db: db, org: org, cfg: cfg, notifier: notifier, version: version,
		defaultTo: defaultTo, logger: logger,
		now:      func() time.Time { return time.Now().UTC() },
		interval: tickInterval,
	}
	s.send = func(ctx context.Context, m email.Message) { s.notifier.Send(ctx, m) }
	return s
}

// Run evaluates immediately, then on every interval tick until ctx is done.
func (s *Scheduler) Run(ctx context.Context) {
	s.logger.Info("digest scheduler started",
		"frequency", s.cfg.FrequencyOrDefault(), "send_hour", s.cfg.SendHourOrDefault())
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		s.tick(ctx)
		select {
		case <-ctx.Done():
			s.logger.Info("digest scheduler stopping")
			return
		case <-t.C:
		}
	}
}

// tick sends the digest for the most-recently-completed period exactly once,
// gated by send_hour and the persisted marker. Fail-soft throughout.
func (s *Scheduler) tick(ctx context.Context) {
	period, ready := notifydigest.DuePeriod(s.cfg.FrequencyOrDefault(), s.cfg.SendHourOrDefault(), s.now())
	if !ready {
		return
	}
	last, err := s.lastSentPeriod(ctx)
	if err != nil {
		s.logger.Warn("digest: read marker failed", "err", err)
		return
	}
	if last == period.Key {
		return // already sent this period
	}
	data, err := s.assemble(ctx, period)
	if err != nil {
		s.logger.Warn("digest: assemble failed", "period", period.Key, "err", err)
		return
	}
	s.send(ctx, notifydigest.Compose(data))
	if err := s.recordSent(ctx, period.Key); err != nil {
		s.logger.Warn("digest: persist marker failed", "period", period.Key, "err", err)
		return
	}
	s.logger.Info("digest sent", "period", period.Key, "total_usd", data.TotalUSD)
}

// Preview assembles the digest Message for the most-recently-completed period,
// ignoring the send_hour gate and the marker. It NEVER sends and NEVER touches
// the marker — the `observer-org digest send --dry-run` path. An empty period
// still composes a valid (zero-total) message.
func (s *Scheduler) Preview(ctx context.Context) (email.Message, error) {
	period, _ := notifydigest.DuePeriod(s.cfg.FrequencyOrDefault(), s.cfg.SendHourOrDefault(), s.now())
	data, err := s.assemble(ctx, period)
	if err != nil {
		return email.Message{}, err
	}
	return notifydigest.Compose(data), nil
}

// SendNow composes and delivers the digest for the most-recently-completed
// period immediately, bypassing the send_hour gate and the marker (an operator-
// triggered `observer-org digest send`). It DOES update the marker so a manual
// send suppresses the same period's scheduled send. Requires a notifier.
func (s *Scheduler) SendNow(ctx context.Context) error {
	period, _ := notifydigest.DuePeriod(s.cfg.FrequencyOrDefault(), s.cfg.SendHourOrDefault(), s.now())
	data, err := s.assemble(ctx, period)
	if err != nil {
		return err
	}
	s.send(ctx, notifydigest.Compose(data))
	if err := s.recordSent(ctx, period.Key); err != nil {
		s.logger.Warn("digest: persist marker failed", "period", period.Key, "err", err)
	}
	return nil
}

// assemble builds the content-free digest Data for a period: current + prior
// spend rollups (org-wide admin scope), ranked movers, and the alert count.
func (s *Scheduler) assemble(ctx context.Context, period notifydigest.Period) (notifydigest.Data, error) {
	scope := rollup.Scope{Admin: true}
	now := s.now()
	cur, err := rollup.WindowReport(ctx, s.db, scope, period.Start, period.End, now)
	if err != nil {
		return notifydigest.Data{}, err
	}
	priorStart := period.Start.Add(-period.End.Sub(period.Start))
	prior, err := rollup.WindowReport(ctx, s.db, scope, priorStart, period.Start, now)
	if err != nil {
		return notifydigest.Data{}, err
	}

	title := "Weekly org spend digest"
	if s.cfg.FrequencyOrDefault() == notifydigest.Monthly {
		title = "Monthly org spend digest"
	}
	data := notifydigest.Data{
		Title:         title,
		OrgName:       s.org.OrgName,
		PeriodLabel:   period.Label,
		TotalUSD:      cur.TotalUSD,
		PriorTotalUSD: prior.TotalUSD,
		HasPrior:      true,
		Breakdowns: []notifydigest.Breakdown{
			{Title: "Spend by developer", Items: toItems(cur.ByDeveloper, topMoversN)},
			{Title: "Spend by model", Items: toItems(cur.ByModel, topMoversN)},
			{Title: "Spend by project", Items: toItems(cur.ByProject, topMoversN)},
		},
		Movers:  notifydigest.RankMovers(toItems(cur.ByModel, 0), toItems(prior.ByModel, 0), topMoversN),
		Version: s.version,
		To:      s.cfg.To,
	}
	if len(data.To) == 0 {
		data.To = s.defaultTo
	}

	if n, err := s.alertCount(ctx, period.Start, period.End); err != nil {
		s.logger.Warn("digest: alert count failed", "err", err)
	} else {
		data.AlertCount = n
		data.HasAlertCount = true
	}
	return data, nil
}

// toItems converts rollup KeyCost rows to digest LineItems, optionally capping
// to the first limit rows (they arrive already sorted by cost desc). limit ≤ 0
// keeps all rows.
func toItems(rows []rollup.KeyCost, limit int) []notifydigest.LineItem {
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	out := make([]notifydigest.LineItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, notifydigest.LineItem{Label: r.Key, CostUSD: r.CostUSD, Tokens: r.Tokens})
	}
	return out
}

// alertCount returns the number of obs_alert_events fired in [start, end) for
// this org.
func (s *Scheduler) alertCount(ctx context.Context, start, end time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM obs_alert_events WHERE org_id = ? AND fired_at >= ? AND fired_at < ?`,
		s.org.OrgID, start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("digest.alertCount: %w", err)
	}
	return n, nil
}

// lastSentPeriod returns the Period.Key of the last-sent org digest, or "" when
// none has been sent.
func (s *Scheduler) lastSentPeriod(ctx context.Context) (string, error) {
	var key string
	err := s.db.QueryRowContext(ctx, `SELECT last_period FROM digest_state WHERE kind = ?`, stateKind).Scan(&key)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("digest.lastSentPeriod: %w", err)
	}
	return key, nil
}

// recordSent upserts the send-once marker for period.
func (s *Scheduler) recordSent(ctx context.Context, periodKey string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO digest_state (kind, last_period, sent_at) VALUES (?, ?, ?)
ON CONFLICT(kind) DO UPDATE SET last_period = excluded.last_period, sent_at = excluded.sent_at`,
		stateKind, periodKey, s.now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("digest.recordSent: %w", err)
	}
	return nil
}
