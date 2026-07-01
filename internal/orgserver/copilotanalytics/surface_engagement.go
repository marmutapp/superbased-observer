package copilotanalytics

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"
)

// pollEngagement is the TWO-STEP fetch unique to Copilot Rail C. For each day in
// the window: (1) GET the report endpoint → a JSON envelope of signed
// download_links; (2) fetch each link's NDJSON body (UN-authenticated, getRaw)
// and parse one engagement row per line. Engagement carries NO cost/token field.
//
// RESIDUAL (findings Q-C1): the exact per-row NDJSON keys + whether a row carries
// `user_login` are doc-reconstructed (copilot-analytics-samples.json); the report
// `?day=` param shape is doc-implied. The two-step envelope→NDJSON STRUCTURE is
// confirmed; lock the row keys against a live payload via the capture script.
func pollEngagement(ctx context.Context, p *Poller, win window) ([]DailyMetric, error) {
	var out []DailyMetric
	// Iterate whole-day buckets [Start, End). The 1-day report is fetched per day.
	for d := win.Start.UTC().Truncate(24 * time.Hour); d.Before(win.End); d = d.AddDate(0, 0, 1) {
		day := dayOf(d)
		env, err := p.fetchReportEnvelope(ctx, day)
		if err != nil {
			return nil, err
		}
		envDay := env.ReportDay
		if envDay == "" {
			envDay = day
		}
		for _, link := range env.DownloadLinks {
			body, err := p.getRaw(ctx, link)
			if err != nil {
				return nil, err
			}
			rows, err := parseEngagementNDJSON(body, envDay)
			if err != nil {
				return nil, err
			}
			out = append(out, rows...)
		}
	}
	return out, nil
}

// reportEnvelope is the JSON returned by the metrics report endpoint: signed
// links to the NDJSON bodies + the report day. 28-day reports use
// report_start_day/report_end_day instead — not used by the 1-day poller.
type reportEnvelope struct {
	DownloadLinks []string `json:"download_links"`
	ReportDay     string   `json:"report_day"`
}

// fetchReportEnvelope GETs the report endpoint for one day.
func (p *Poller) fetchReportEnvelope(ctx context.Context, day string) (reportEnvelope, error) {
	q := url.Values{}
	q.Set("day", day)
	rawURL := p.BaseURL + p.ownerPrefix() + "/copilot/metrics/reports/" + url.PathEscape(p.Report) + "?" + q.Encode()
	body, _, err := p.get(ctx, rawURL)
	if err != nil {
		return reportEnvelope{}, err
	}
	var env reportEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return reportEnvelope{}, fmt.Errorf("copilotanalytics: parse report envelope: %w", err)
	}
	return env, nil
}

// engagementRow is one NDJSON line. A per-user report carries user_login; an
// org-aggregate report carries total_active_users/total_engaged_users with no
// login. Both shapes are handled.
type engagementRow struct {
	ReportDay         string `json:"report_day"`
	UserLogin         string `json:"user_login"`
	TotalActiveUsers  *int   `json:"total_active_users"`
	TotalEngagedUsers *int   `json:"total_engaged_users"`
	CodeCompletions   struct {
		Editors []struct {
			Models []struct {
				Languages []struct {
					TotalCodeSuggestions    int `json:"total_code_suggestions"`
					TotalCodeAcceptances    int `json:"total_code_acceptances"`
					TotalCodeLinesSuggested int `json:"total_code_lines_suggested"`
					TotalCodeLinesAccepted  int `json:"total_code_lines_accepted"`
				} `json:"languages"`
			} `json:"models"`
		} `json:"editors"`
	} `json:"copilot_ide_code_completions"`
	IDEChat struct {
		TotalChats int `json:"total_chats"`
	} `json:"copilot_ide_chat"`
	DotcomChat struct {
		TotalChats int `json:"total_chats"`
	} `json:"copilot_dotcom_chat"`
}

// parseEngagementNDJSON parses one NDJSON body (one JSON object per line) into
// normalized metrics. Each line is independent; a malformed line fails the parse
// (we'd rather know than silently drop spend-adjacent data).
func parseEngagementNDJSON(body []byte, envDay string) ([]DailyMetric, error) {
	var out []DailyMetric
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 64*1024), 8<<20)
	line := 0
	for sc.Scan() {
		line++
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var row engagementRow
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, fmt.Errorf("copilotanalytics: parse engagement NDJSON line %d: %w", line, err)
		}
		out = append(out, engagementRowMetrics(row, envDay)...)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("copilotanalytics: scan engagement NDJSON: %w", err)
	}
	return out, nil
}

func engagementRowMetrics(row engagementRow, envDay string) []DailyMetric {
	day := row.ReportDay
	if day == "" {
		day = envDay
	}
	if day == "" {
		return nil
	}
	var sugg, acc, linesSug, linesAcc int
	for _, e := range row.CodeCompletions.Editors {
		for _, m := range e.Models {
			for _, l := range m.Languages {
				sugg += l.TotalCodeSuggestions
				acc += l.TotalCodeAcceptances
				linesSug += l.TotalCodeLinesSuggested
				linesAcc += l.TotalCodeLinesAccepted
			}
		}
	}
	chats := row.IDEChat.TotalChats + row.DotcomChat.TotalChats
	const s = SurfaceEngagement

	if row.UserLogin != "" {
		// Per-login row. Identity = GitHub login (NOT email).
		key := row.UserLogin
		actor := actorForLogin(key, "")
		out := []DailyMetric{
			emitMetric(day, key, actor, s, UnitCount, MetricCodeSuggestions, float64(sugg)),
			emitMetric(day, key, actor, s, UnitCount, MetricCodeAcceptances, float64(acc)),
			emitMetric(day, key, actor, s, UnitCount, MetricLinesSuggested, float64(linesSug)),
			emitMetric(day, key, actor, s, UnitCount, MetricLinesAccepted, float64(linesAcc)),
			emitMetric(day, key, actor, s, UnitCount, MetricChats, float64(chats)),
		}
		if row.TotalEngagedUsers != nil {
			out = append(out, emitMetric(day, key, actor, s, UnitCount, MetricEngagedUsers, float64(*row.TotalEngagedUsers)))
		}
		return out
	}

	// Org-aggregate row (organization-*-day report): no per-login attribution.
	out := []DailyMetric{
		emitMetric(day, orgAggregateKey, ActorOrg, s, UnitCount, MetricCodeSuggestions, float64(sugg)),
		emitMetric(day, orgAggregateKey, ActorOrg, s, UnitCount, MetricCodeAcceptances, float64(acc)),
		emitMetric(day, orgAggregateKey, ActorOrg, s, UnitCount, MetricLinesSuggested, float64(linesSug)),
		emitMetric(day, orgAggregateKey, ActorOrg, s, UnitCount, MetricLinesAccepted, float64(linesAcc)),
		emitMetric(day, orgAggregateKey, ActorOrg, s, UnitCount, MetricChats, float64(chats)),
	}
	if row.TotalActiveUsers != nil {
		out = append(out, emitMetric(day, orgAggregateKey, ActorOrg, s, UnitCount, MetricActiveUsers, float64(*row.TotalActiveUsers)))
	}
	if row.TotalEngagedUsers != nil {
		out = append(out, emitMetric(day, orgAggregateKey, ActorOrg, s, UnitCount, MetricEngagedUsers, float64(*row.TotalEngagedUsers)))
	}
	return out
}
