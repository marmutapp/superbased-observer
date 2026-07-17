package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/notify/email"
)

// obs_alerts.go is the NODE-SIDE observability alert evaluator loop, webhook
// client, and CLI (general-observability gap-audit item #9, §2.6). It imports
// NO internal/obs package — every obs access funnels through the build-tagged
// wrappers in obs_wire.go (obsEvaluateAlertsOnce / obsRecordAlert /
// obsAlertsStatusCLI), so the separability boundary (obs plan §2.3/§11,
// tests/invariant/obs_boundary_test.go) holds. The pure evaluation lives in
// internal/obs/alert; fired events persist to the node-local obs_alert_events
// table. There is deliberately no node-dashboard surface — obs is
// Plane-A/web2-only (docs/deployment-models.md); the CLI + webhook are the
// node's honest alert surfaces.

// alertWebhookPayload is the JSON body POSTed to the configured webhook on a
// threshold crossing. It mirrors the org obsalert.Alert shape (rule / metric /
// threshold / observed value / window / fired-at), minus the org identity
// fields a node alert has no notion of.
type alertWebhookPayload struct {
	RuleName      string    `json:"rule_name"`
	Metric        string    `json:"metric"`
	Comparator    string    `json:"comparator"`
	Threshold     float64   `json:"threshold"`
	Value         float64   `json:"value"`
	WindowMinutes int       `json:"window_minutes"`
	FiredAt       time.Time `json:"fired_at"`
}

// obsAlertLoop is the daemon-resident evaluator: it ticks every
// eval_interval_minutes, evaluates every configured rule against this node's
// local obs_* metrics, delivers a webhook per crossing, and records the fire.
// Fail-soft + P1 like every `observer start` sibling: it never propagates an
// error and a webhook outage degrades to local recording only. Returns
// immediately when alerting is off (or the obs subsystem is compiled out).
func obsAlertLoop(ctx context.Context, configPath string) {
	cfg, db, cleanup, err := loadConfigAndDB(ctx, configPath)
	if err != nil {
		return
	}
	defer cleanup()
	if !obsAlertsRuntimeEnabled(cfg) {
		return
	}
	logger := newLogger(cfg.Observer.LogLevel)
	every := cfg.Observability.Alerts.EvalIntervalMinutes
	if every <= 0 {
		every = 5
	}
	// Optional email channel (gap-register G9): built once when [email] is
	// enabled AND this consumer opted in ([observability.alerts].email). A bad
	// [email] block is WARN-only and leaves the notifier nil (email off) — it
	// must never block alerting.
	emailNotifier := buildAlertEmailNotifier(cfg, logger)

	logger.Info("obs-alert evaluator started",
		"interval_min", every, "rules", len(cfg.Observability.Alerts.Rules),
		"webhook", cfg.Observability.Alerts.WebhookURL != "",
		"email", emailNotifier != nil)

	obsRunAlertTick(ctx, cfg, db, logger, emailNotifier)
	ticker := time.NewTicker(time.Duration(every) * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			obsRunAlertTick(ctx, cfg, db, logger, emailNotifier)
		}
	}
}

// buildAlertEmailNotifier returns a fail-soft email notifier when [email] is
// enabled AND [observability.alerts].email is set, else nil (email off). A
// structurally-invalid [email] block logs a warning and yields nil so alerting
// is never blocked by a mail misconfiguration. EmailTo overrides [email].to.
func buildAlertEmailNotifier(cfg config.Config, logger *slog.Logger) *email.Notifier {
	if !cfg.Email.Enabled || !cfg.Observability.Alerts.Email {
		return nil
	}
	ec := cfg.Email
	if to := cfg.Observability.Alerts.EmailTo; len(to) > 0 {
		ec.To = to
	}
	n, err := email.NewNotifier(ec, logger)
	if err != nil {
		logger.Warn("obs-alert email disabled: invalid [email] config", "err", err)
		return nil
	}
	return n
}

// obsRunAlertTick performs one evaluation pass: evaluate → (deliver webhook) →
// record. The pure evaluator already applied cooldown/dedup, so every crossing
// returned here is one that should fire now.
func obsRunAlertTick(ctx context.Context, cfg config.Config, db *sql.DB, logger *slog.Logger, emailNotifier *email.Notifier) {
	fired, err := obsEvaluateAlertsOnce(ctx, cfg, db, logger)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			logger.Warn("obs-alert evaluate failed", "err", err)
		}
		return
	}
	webhook := cfg.Observability.Alerts.WebhookURL
	for _, f := range fired {
		delivered := true
		if webhook != "" {
			if derr := postAlertWebhook(ctx, webhook, f); derr != nil {
				delivered = false
				logger.Warn("obs-alert webhook failed", "rule", f.RuleName, "err", derr)
			}
		}
		// Email is an ADDITIONAL, fail-soft channel (Notifier.Send never
		// errors). It does not affect the recorded delivery status (that tracks
		// the webhook).
		if emailNotifier != nil {
			emailNotifier.Send(ctx, composeAlertEmail(f, version))
		}
		if rerr := obsRecordAlert(ctx, db, f, delivered); rerr != nil {
			logger.Error("obs-alert persist failed", "rule", f.RuleName, "err", rerr)
			continue
		}
		logger.Info("obs-alert fired",
			"rule", f.RuleName, "metric", f.Metric, "value", f.Value,
			"threshold", f.Threshold, "delivered", delivered)
	}
}

// postAlertWebhook POSTs one fired-alert payload to url with a bounded timeout.
// Mirrors obsalert.deliver (the org webhook client): a non-2xx/3xx status is an
// error so the caller records delivered=false.
func postAlertWebhook(ctx context.Context, url string, f obsFiredAlert) error {
	body, err := json.Marshal(alertWebhookPayload{
		RuleName:      f.RuleName,
		Metric:        f.Metric,
		Comparator:    f.Comparator,
		Threshold:     f.Threshold,
		Value:         f.Value,
		WindowMinutes: f.WindowMinutes,
		FiredAt:       f.FiredAt,
	})
	if err != nil {
		return err
	}
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned %d", resp.StatusCode)
	}
	return nil
}

// composeAlertEmail renders a fired node alert as an email Message from the
// SAME content-bounded fields the webhook payload carries. version stamps the
// footer. Pure — no I/O; the Notifier does the delivery.
func composeAlertEmail(f obsFiredAlert, version string) email.Message {
	subject := fmt.Sprintf("[Observer] Alert: %s — %s", f.RuleName, f.Metric)
	heading := fmt.Sprintf("Rule %q fired: %s is %g (%s %g) over the last %d minute(s).",
		f.RuleName, f.Metric, f.Value, comparatorSymbol(f.Comparator), f.Threshold, f.WindowMinutes)
	return email.Compose(email.ComposeParams{
		Subject: subject,
		Heading: heading,
		Fields: []email.Field{
			{Label: "Rule", Value: f.RuleName},
			{Label: "Metric", Value: f.Metric},
			{Label: "Observed value", Value: fmt.Sprintf("%g", f.Value)},
			{Label: "Condition", Value: fmt.Sprintf("%s %g", comparatorSymbol(f.Comparator), f.Threshold)},
			{Label: "Window", Value: fmt.Sprintf("%d minute(s)", f.WindowMinutes)},
			{Label: "Fired at", Value: f.FiredAt.Format(time.RFC3339)},
		},
		Version: version,
	})
}

// newObsAlertsCmd is `observer obs alerts`: print the per-rule live status
// (current value + whether it's currently breaching) and the recently fired
// alerts. It funnels through the build-tagged obs wrappers, so this file
// imports no internal/obs package (the separability boundary).
func newObsAlertsCmd() *cobra.Command {
	var configPath string
	var limit int
	cmd := &cobra.Command{
		Use:   "alerts",
		Short: "Node-side observability alerts: rule status + recently fired alerts",
		Long: "Show the node-side observability alert rules ([observability.alerts]),\n" +
			"each rule's current metric value and whether it is breaching now, and\n" +
			"the most-recently fired alerts (requires [observability] enabled).\n\n" +
			"Alerting evaluates THIS node's own local trajectory data — it works\n" +
			"even with org sharing off. Fired alerts POST to the configured\n" +
			"webhook_url (an outbound call, so alerting is opt-in and default-off).\n" +
			"There is no dashboard page: obs surfaces are admin/web2-only.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, database, cleanup, err := loadConfigAndDB(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			if err := requireObsEnabled(cfg); err != nil {
				return err
			}
			st, err := obsAlertsStatusCLI(cmd.Context(), cfg, database, slog.Default(), limit)
			if err != nil {
				return err
			}
			printObsAlertsStatus(cmd, st)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "path to config.toml")
	cmd.Flags().IntVar(&limit, "limit", 20, "max recently-fired alerts to show")
	return cmd
}

// printObsAlertsStatus renders the alerts status table.
func printObsAlertsStatus(cmd *cobra.Command, st obsAlertsStatus) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "alerting:         %s\n", enabledLabel(st.Enabled))
	fmt.Fprintf(out, "eval interval:    %dm\n", st.IntervalMinutes)
	fmt.Fprintf(out, "webhook:          %s\n", configuredLabel(st.WebhookConfigured))

	fmt.Fprintf(out, "\nrules (%d):\n", len(st.Rules))
	if len(st.Rules) == 0 {
		fmt.Fprintln(out, "  (none configured — add [[observability.alerts.rules]] entries)")
	}
	for _, r := range st.Rules {
		state := "ok"
		if r.Breaching {
			state = "BREACHING"
		}
		last := "never"
		if !r.LastFired.IsZero() {
			last = r.LastFired.Format(time.RFC3339)
		}
		fmt.Fprintf(out, "  %-18s %-15s %s %g  window=%dm cooldown=%dm  current=%g  [%s]  last-fired=%s\n",
			r.Name, r.Metric, comparatorSymbol(r.Comparator), r.Threshold,
			r.WindowMinutes, r.CooldownMinutes, r.CurrentValue, state, last)
	}

	fmt.Fprintf(out, "\nrecently fired (%d):\n", len(st.Recent))
	if len(st.Recent) == 0 {
		fmt.Fprintln(out, "  (none)")
	}
	for _, e := range st.Recent {
		fmt.Fprintf(out, "  %s  %-18s %-15s value=%g %s %g  delivered=%t\n",
			e.FiredAt.Format(time.RFC3339), e.RuleName, e.Metric,
			e.Value, comparatorSymbol(e.Comparator), e.Threshold, e.Delivered)
	}
}

func configuredLabel(b bool) string {
	if b {
		return "configured"
	}
	return "none (records only)"
}

func comparatorSymbol(c string) string {
	if c == "gte" {
		return ">="
	}
	return ">"
}
