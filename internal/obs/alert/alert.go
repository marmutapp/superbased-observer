package alert

import "time"

// Metric names — the closed vocabulary an alert rule evaluates. These MIRROR
// the org-server alert dialect (internal/orgserver/obsalert Metric* constants)
// exactly, so a node operator authoring [observability.alerts] rules and an org
// admin authoring obs_alert_rules use the same metric vocabulary.
const (
	MetricErrorRate    = "error_rate"     // error traces / total traces, 0..1
	MetricCostUSD      = "cost_usd"       // summed span cost over the window
	MetricLatencyP95Ms = "latency_p95_ms" // p95 span wall-duration, milliseconds
)

// Comparator names — how a value is tested against the threshold. Mirrors the
// org dialect (comparator column: gt | gte); an empty/unknown comparator is
// treated as gt.
const (
	ComparatorGT  = "gt"
	ComparatorGTE = "gte"
)

// Metrics is the closed set of metric names, for validation at the boundary.
var Metrics = []string{MetricErrorRate, MetricCostUSD, MetricLatencyP95Ms}

// ValidMetric reports whether m is one of the known metric names.
func ValidMetric(m string) bool {
	for _, k := range Metrics {
		if k == m {
			return true
		}
	}
	return false
}

// Rule is one locally-authored threshold rule. It mirrors the org
// obs_alert_rules row shape (name / metric / comparator / threshold / window /
// cooldown / last-fired), but the node's window/cooldown are expressed in
// MINUTES (finer-grained than the org's day windows — a node reacts to its own
// live traffic in near real time). LastFired is the boundary-supplied moment
// this rule last fired (zero = never); the evaluator suppresses a re-fire while
// within cooldown of it.
type Rule struct {
	Name            string
	Metric          string
	Comparator      string
	Threshold       float64
	WindowMinutes   int
	CooldownMinutes int
	LastFired       time.Time
}

// Summary is a content-free metric snapshot computed over one rule's window by
// the store boundary (internal/obs/store.AlertSummary). Only the fields a rule
// can reference are present; a metric a rule doesn't use is simply ignored.
type Summary struct {
	ErrorRate    float64 // error traces / total traces, 0..1
	CostUSD      float64 // summed span cost over the window
	LatencyP95Ms int64   // p95 span wall-duration, milliseconds
}

// Input pairs a rule with the Summary computed over ITS window. The boundary
// resolves each rule's window and hands the pure evaluator the matched
// snapshot, so this package never reads a clock-window or a table.
type Input struct {
	Rule    Rule
	Summary Summary
}

// Fired is one threshold crossing — the value delivered to the webhook and
// persisted as an obs_alert_events row. Mirrors the org obsalert.Alert payload
// shape (rule name / metric / threshold / observed value / window / fired-at),
// minus the org identity fields (a node alert has no org/tenant).
type Fired struct {
	RuleName      string    `json:"rule_name"`
	Metric        string    `json:"metric"`
	Comparator    string    `json:"comparator"`
	Threshold     float64   `json:"threshold"`
	Value         float64   `json:"value"`
	WindowMinutes int       `json:"window_minutes"`
	FiredAt       time.Time `json:"fired_at"`
}

// Evaluate returns a Fired for every input whose metric crosses its threshold
// AND is not within its cooldown at now. Pure: rules + snapshots in, fired
// values out — no I/O and no clock read beyond the passed now. A rule naming an
// unknown metric is skipped (it cannot crash the loop).
func Evaluate(inputs []Input, now time.Time) []Fired {
	var out []Fired
	for _, in := range inputs {
		val, ok := MetricValue(in.Summary, in.Rule.Metric)
		if !ok {
			continue
		}
		if !Crossed(in.Rule.Comparator, val, in.Rule.Threshold) {
			continue
		}
		if InCooldown(in.Rule.LastFired, EffectiveCooldownMinutes(in.Rule), now) {
			continue
		}
		out = append(out, Fired{
			RuleName:      in.Rule.Name,
			Metric:        in.Rule.Metric,
			Comparator:    normalizeComparator(in.Rule.Comparator),
			Threshold:     in.Rule.Threshold,
			Value:         val,
			WindowMinutes: in.Rule.WindowMinutes,
			FiredAt:       now,
		})
	}
	return out
}

// MetricValue extracts the rule's metric from a snapshot. ok=false for an
// unknown metric name.
func MetricValue(s Summary, metric string) (float64, bool) {
	switch metric {
	case MetricErrorRate:
		return s.ErrorRate, true
	case MetricCostUSD:
		return s.CostUSD, true
	case MetricLatencyP95Ms:
		return float64(s.LatencyP95Ms), true
	default:
		return 0, false
	}
}

// Crossed reports whether val crosses threshold under the comparator. gte uses
// >=; every other (incl. the default gt and any unknown value) uses >. Mirrors
// obsalert.crossed.
func Crossed(comparator string, val, threshold float64) bool {
	if comparator == ComparatorGTE {
		return val >= threshold
	}
	return val > threshold
}

// InCooldown reports whether a rule that last fired at lastFired is still
// within cooldownMin minutes of now. A zero lastFired (never fired) or a
// non-positive cooldown is never in cooldown. Mirrors obsalert.inCooldown but
// takes a time.Time (the boundary already parsed the stored timestamp).
func InCooldown(lastFired time.Time, cooldownMin int, now time.Time) bool {
	if lastFired.IsZero() || cooldownMin <= 0 {
		return false
	}
	return now.Sub(lastFired) < time.Duration(cooldownMin)*time.Minute
}

// EffectiveCooldownMinutes resolves the cooldown that applies to a rule:
// the rule's CooldownMinutes when set (>0), else the window_minutes default
// (gap-audit dedup requirement #6 — don't re-fire every tick while the
// condition persists; a full window must elapse before the same rule fires
// again). Falls back to 5 when neither is set.
func EffectiveCooldownMinutes(r Rule) int {
	switch {
	case r.CooldownMinutes > 0:
		return r.CooldownMinutes
	case r.WindowMinutes > 0:
		return r.WindowMinutes
	default:
		return 5
	}
}

// normalizeComparator maps an empty/unknown comparator to the gt default so the
// persisted/delivered value is honest about what was applied.
func normalizeComparator(c string) string {
	if c == ComparatorGTE {
		return ComparatorGTE
	}
	return ComparatorGT
}
