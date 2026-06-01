package budget

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	gen "github.com/marmutapp/superbased-observer/internal/orgserver/dashboard/gen"
	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
	"github.com/marmutapp/superbased-observer/internal/orgserver/rollup"
)

const (
	defaultInterval = 60 * time.Second
	maxDeliveryAge  = time.Hour // give up retrying a webhook after this
	initialBackoff  = 2 * time.Second
	maxBackoff      = 2 * time.Minute
	deliveryTimeout = 10 * time.Second // per HTTP attempt
)

// Evaluator polls budgets and fires webhook alerts on threshold crossings.
type Evaluator struct {
	db       *sql.DB
	org      orgdb.Org
	interval time.Duration
	client   *http.Client
	logger   *slog.Logger
	now      func() time.Time
	wg       sync.WaitGroup

	// Deliver sends one alert. It defaults to deliverWithBackoff (async,
	// retrying up to an hour); tests substitute a synchronous recorder.
	Deliver func(ctx context.Context, url string, alert gen.BudgetAlert)
}

// NewEvaluator builds an evaluator over the server DB. A non-positive interval
// defaults to 60s.
func NewEvaluator(db *sql.DB, org orgdb.Org, interval time.Duration, logger *slog.Logger) *Evaluator {
	if logger == nil {
		logger = slog.Default()
	}
	if interval <= 0 {
		interval = defaultInterval
	}
	e := &Evaluator{
		db:       db,
		org:      org,
		interval: interval,
		client:   &http.Client{Timeout: deliveryTimeout},
		logger:   logger,
		now:      func() time.Time { return time.Now().UTC() },
	}
	e.Deliver = e.deliverWithBackoff
	return e
}

// Run evaluates immediately, then on every interval tick, until ctx is
// cancelled. It then waits for in-flight webhook deliveries to finish (bounded
// by their own deadline). Errors are logged, never returned — a failing
// evaluation must not take down the server (P1).
func (e *Evaluator) Run(ctx context.Context) {
	e.logger.Info("budget evaluator started", "interval", e.interval.String())
	t := time.NewTicker(e.interval)
	defer t.Stop()
	for {
		if err := e.EvaluateOnce(ctx); err != nil && ctx.Err() == nil {
			e.logger.Error("budget evaluate", "err", err)
		}
		select {
		case <-ctx.Done():
			e.logger.Info("budget evaluator stopping; draining deliveries")
			e.wg.Wait()
			return
		case <-t.C:
		}
	}
}

// Wait blocks until all in-flight async webhook deliveries finish. Intended for
// tests and graceful shutdown bookkeeping.
func (e *Evaluator) Wait() { e.wg.Wait() }

type budgetRow struct {
	id         string
	scope      string
	scopeID    string
	cap        float64
	webhook    string
	thresholds []float64
	lastFired  float64
}

// EvaluateOnce evaluates every budget once: compute spend, detect a new
// threshold crossing, persist the high-water mark, and fire on a crossing.
func (e *Evaluator) EvaluateOnce(ctx context.Context) error {
	rows, err := e.db.QueryContext(ctx, `
SELECT id, scope, scope_id, monthly_usd_cap, COALESCE(alert_webhook_url,''),
       alert_thresholds, last_fired_threshold
  FROM budgets`)
	if err != nil {
		return fmt.Errorf("budget.EvaluateOnce: list: %w", err)
	}
	var budgets []budgetRow
	for rows.Next() {
		var b budgetRow
		var thresholds string
		if err := rows.Scan(&b.id, &b.scope, &b.scopeID, &b.cap, &b.webhook, &thresholds, &b.lastFired); err != nil {
			_ = rows.Close()
			return fmt.Errorf("budget.EvaluateOnce: scan: %w", err)
		}
		b.thresholds = rollup.ParseThresholds(thresholds)
		budgets = append(budgets, b)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	now := e.now()
	for _, b := range budgets {
		if b.cap <= 0 {
			continue
		}
		spend, err := rollup.ScopeSpend(ctx, e.db, b.scope, b.scopeID, rollup.BudgetWindowDays, now)
		if err != nil {
			e.logger.Error("budget spend", "budget", b.id, "err", err)
			continue
		}
		ratio := spend / b.cap
		fire, newHigh := crossing(b.thresholds, ratio, b.lastFired)

		if newHigh != b.lastFired {
			if _, err := e.db.ExecContext(ctx,
				`UPDATE budgets SET last_fired_threshold = ?, updated_at = ? WHERE id = ?`,
				newHigh, now.Format(time.RFC3339), b.id); err != nil {
				e.logger.Error("budget persist threshold", "budget", b.id, "err", err)
				continue
			}
		}
		if fire && b.webhook != "" {
			e.Deliver(ctx, b.webhook, e.alert(ctx, b, newHigh, spend, ratio, now))
		} else if fire {
			e.logger.Info("budget threshold crossed (no webhook configured)",
				"budget", b.id, "scope", b.scope, "scope_id", b.scopeID, "threshold", newHigh)
		}
	}
	return nil
}

// crossing reports whether to alert and the resulting high-water threshold.
// newHigh is the greatest threshold at or below ratio (0 if none). A fire is a
// strict increase over lastFired; a decrease lowers the mark without firing.
func crossing(thresholds []float64, ratio, lastFired float64) (fire bool, newHigh float64) {
	sorted := append([]float64(nil), thresholds...)
	sort.Float64s(sorted)
	for _, t := range sorted {
		if ratio >= t {
			newHigh = t
		}
	}
	return newHigh > lastFired, newHigh
}

func (e *Evaluator) alert(ctx context.Context, b budgetRow, threshold, spend, ratio float64, now time.Time) gen.BudgetAlert {
	label := rollup.ScopeLabel(ctx, e.db, b.scope, b.scopeID)
	return gen.BudgetAlert{
		BudgetId:        b.id,
		Scope:           gen.BudgetAlertScope(b.scope),
		ScopeId:         b.scopeID,
		ScopeLabel:      &label,
		Threshold:       threshold,
		MonthlyUsdCap:   b.cap,
		CurrentSpendUsd: spend,
		CurrentRatio:    ratio,
		OrgId:           e.org.OrgID,
		OrgName:         e.org.OrgName,
		FiredAt:         now,
	}
}

// deliverWithBackoff launches an async delivery that POSTs the alert JSON with
// exponential backoff until it succeeds, the context is cancelled, or one hour
// elapses (then it is dropped with a logged error). Tracked in the WaitGroup so
// Run drains on shutdown.
func (e *Evaluator) deliverWithBackoff(ctx context.Context, url string, alert gen.BudgetAlert) {
	body, err := json.Marshal(alert)
	if err != nil {
		e.logger.Error("budget alert marshal", "err", err)
		return
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		deadline := e.now().Add(maxDeliveryAge)
		backoff := initialBackoff
		for attempt := 1; ; attempt++ {
			if err := e.postOnce(ctx, url, body); err == nil {
				e.logger.Info("budget alert delivered", "budget", alert.BudgetId, "threshold", alert.Threshold, "attempt", attempt)
				return
			} else {
				e.logger.Warn("budget alert delivery failed", "budget", alert.BudgetId, "attempt", attempt, "err", err)
			}
			if e.now().After(deadline) {
				e.logger.Error("budget alert dropped after retries", "budget", alert.BudgetId, "threshold", alert.Threshold)
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}()
}

// postOnce makes a single webhook POST attempt; any non-2xx or transport error
// is an error.
func (e *Evaluator) postOnce(ctx context.Context, url string, body []byte) error {
	reqCtx, cancel := context.WithTimeout(ctx, deliveryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "observer-org-budget/1")
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("budget webhook status %d", resp.StatusCode)
	}
	return nil
}
