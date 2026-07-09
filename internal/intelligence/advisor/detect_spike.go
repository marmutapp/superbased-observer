package advisor

import (
	"fmt"
	"sort"
)

// detectSpendSpike (usability arc P6.12 / review §8.1) is the
// advisor's "something changed TODAY" reflex: today's spend is
// compared against the trailing daily average over the window's
// PRIOR active days (days with any spend; zero-activity days don't
// dilute the baseline — weekends off shouldn't make Monday look like
// a spike). Fires at the SpikeMultiple threshold with at least
// SpikeMinPriorDays of history and a $ floor so fresh installs and
// idle days never nag. Severity escalates with the multiple. Pure
// read-layer math over already-loaded facts — no new queries.
func detectSpendSpike(f *Facts, th Thresholds) []Suggestion {
	today := f.Now.Format("2006-01-02")
	byDay := map[string]float64{}
	for i := range f.Sessions {
		s := &f.Sessions[i]
		for _, r := range s.Rows {
			if c, ok := rowCost(f, s, r); ok {
				byDay[r.TS.UTC().Format("2006-01-02")] += c
			}
		}
	}
	todayUSD := byDay[today]
	var prior []float64
	for day, usd := range byDay {
		if day != today && usd > 0 {
			prior = append(prior, usd)
		}
	}
	minDays := th.SpikeMinPriorDays
	if minDays <= 0 {
		minDays = DefaultThresholds().SpikeMinPriorDays
	}
	multiple := th.SpikeMultiple
	if multiple <= 0 {
		multiple = DefaultThresholds().SpikeMultiple
	}
	if len(prior) < minDays {
		return nil
	}
	var sum float64
	for _, v := range prior {
		sum += v
	}
	avg := sum / float64(len(prior))
	// Floors: a "3×" over a $0.20 average is noise, not news.
	if avg <= 0 || todayUSD < 5 || todayUSD < avg*multiple {
		return nil
	}
	ratio := todayUSD / avg
	sev := SeverityInfo
	if ratio >= 2*multiple {
		sev = SeverityWarning
	}
	// Name today's biggest spender so the nudge is actionable.
	type ses struct {
		id   string
		tool string
		usd  float64
	}
	var todays []ses
	for i := range f.Sessions {
		s := &f.Sessions[i]
		var u float64
		for _, r := range s.Rows {
			if r.TS.UTC().Format("2006-01-02") != today {
				continue
			}
			if c, ok := rowCost(f, s, r); ok {
				u += c
			}
		}
		if u > 0 {
			todays = append(todays, ses{id: s.ID, tool: s.Tool, usd: u})
		}
	}
	sort.Slice(todays, func(i, j int) bool { return todays[i].usd > todays[j].usd })
	top := ""
	if len(todays) > 0 {
		top = fmt.Sprintf(" The biggest contributor is a %s session at $%.2f.", todays[0].tool, todays[0].usd)
	}
	return []Suggestion{{
		DedupKey: "spend_spike|" + today, Detector: "spend_spike",
		Category: CategoryCost, Scope: ScopeGlobal, Severity: sev,
		Title: fmt.Sprintf("Today is %.1f× your daily average ($%.2f vs $%.2f)", ratio, todayUSD, avg),
		Nudge: fmt.Sprintf(
			"Spend today is $%.2f against a $%.2f average over your last %d active days.%s Nothing is wrong by itself — this is the \"did I mean to do that?\" check while the day is still going.",
			todayUSD, avg, len(prior), top,
		),
		// The $ figure is the EXCESS over baseline — the spike's
		// magnitude, and what a user catching a runaway day early can
		// still avoid. The Math line states the framing.
		SavingsUSD: round2(todayUSD - avg), Confidence: 0.6,
		Action: &Action{Kind: "page", Target: "/live", Label: "Watch live sessions"},
		Evidence: Evidence{
			Numbers: map[string]float64{
				"today_usd":  round2(todayUSD),
				"avg_usd":    round2(avg),
				"prior_days": float64(len(prior)),
				"ratio":      round2(ratio),
			},
			Math: fmt.Sprintf("$%.2f today − $%.2f avg (over %d prior active days) = $%.2f excess at %.1f× (fires at ≥%.0f×, $5 floor); the excess is today-above-baseline, not guaranteed waste",
				todayUSD, avg, len(prior), todayUSD-avg, ratio, multiple),
		},
	}}
}
