package digest

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/notify/email"
)

// LineItem is one (label → observed spend) row in a digest breakdown. Content-
// free: the label is a model id, a tool name, a hash-derived project id, or a
// developer email — never raw request/response content.
type LineItem struct {
	Label   string
	CostUSD float64
	Tokens  int64
}

// Mover is one dimension's spend change versus the prior period.
type Mover struct {
	Label      string
	CurrentUSD float64
	PriorUSD   float64
}

// Delta returns the signed spend change (current − prior).
func (m Mover) Delta() float64 { return m.CurrentUSD - m.PriorUSD }

// Breakdown is one titled group of line items (e.g. "Spend by model").
type Breakdown struct {
	Title string
	Items []LineItem
}

// Data is the fully-assembled, content-free input to Compose. Both the node
// daemon and the org server populate it from their own rollups, then hand it to
// the shared composer. Every dollar figure is OBSERVED spend.
type Data struct {
	// Title is the digest headline, e.g. "Weekly cost digest".
	Title string
	// OrgName is set for the org digest (blank for the node digest); when set it
	// prefixes the subject.
	OrgName string
	// PeriodLabel is the human window label, e.g. "Jun 30 – Jul 6, 2026".
	PeriodLabel string
	// TotalUSD is the observed spend over the period.
	TotalUSD float64
	// PriorTotalUSD is the prior period's observed spend; only rendered when
	// HasPrior is true.
	PriorTotalUSD float64
	HasPrior      bool
	// Breakdowns are the ordered spend sub-tables (by model / tool / project /
	// developer — whichever the caller assembled).
	Breakdowns []Breakdown
	// Movers are the largest spend changes vs the prior period (already ranked
	// + truncated by the caller). Empty when no prior data.
	Movers []Mover
	// AlertCount is the number of alerts fired in the period; rendered only when
	// HasAlertCount is true (the alert store may be absent, e.g. obs compiled
	// out).
	AlertCount    int
	HasAlertCount bool
	// Version stamps the email footer.
	Version string
	// To is the recipient override; empty leaves Message.To nil so the Notifier
	// applies the configured [email].to defaults.
	To []string
}

// Compose renders Data into an email.Message through the shared composer. Pure:
// no I/O. The subject follows the "[Observer] …" convention; the heading frames
// the total as observed spend with a prior-period delta.
func Compose(d Data) email.Message {
	subject := fmt.Sprintf("[Observer] %s — %s", d.Title, d.PeriodLabel)
	if strings.TrimSpace(d.OrgName) != "" {
		subject = fmt.Sprintf("[Observer] %s: %s — %s", d.OrgName, d.Title, d.PeriodLabel)
	}

	heading := fmt.Sprintf("Observed spend for %s: %s.", d.PeriodLabel, money(d.TotalUSD))
	if d.HasPrior {
		heading = fmt.Sprintf("Observed spend for %s: %s (%s vs the prior period).",
			d.PeriodLabel, money(d.TotalUSD), signedDelta(d.TotalUSD, d.PriorTotalUSD))
	}

	fields := []email.Field{
		{Label: "Period", Value: d.PeriodLabel},
		{Label: "Observed spend", Value: money(d.TotalUSD)},
	}
	if d.HasPrior {
		fields = append(
			fields,
			email.Field{Label: "Prior period", Value: money(d.PriorTotalUSD)},
			email.Field{Label: "Change", Value: signedDelta(d.TotalUSD, d.PriorTotalUSD)},
		)
	}
	if d.HasAlertCount {
		fields = append(fields, email.Field{Label: "Alerts fired", Value: fmt.Sprintf("%d", d.AlertCount)})
	}

	var sections []email.Section
	for _, b := range d.Breakdowns {
		if len(b.Items) == 0 {
			continue
		}
		sec := email.Section{Title: b.Title}
		for _, it := range b.Items {
			sec.Fields = append(sec.Fields, email.Field{Label: labelOrNone(it.Label), Value: money(it.CostUSD)})
		}
		sections = append(sections, sec)
	}
	if len(d.Movers) > 0 {
		sec := email.Section{Title: "Top movers vs prior period"}
		for _, m := range d.Movers {
			sec.Fields = append(sec.Fields, email.Field{
				Label: labelOrNone(m.Label),
				Value: fmt.Sprintf("%s (was %s, %s)", money(m.CurrentUSD), money(m.PriorUSD), signedDelta(m.CurrentUSD, m.PriorUSD)),
			})
		}
		sections = append(sections, sec)
	}

	return email.Compose(email.ComposeParams{
		Subject:  subject,
		Heading:  heading,
		Fields:   fields,
		Sections: sections,
		Version:  d.Version,
		To:       d.To,
	})
}

// money formats a USD amount with two decimals.
func money(v float64) string { return fmt.Sprintf("$%.2f", v) }

// labelOrNone renders an empty label as a readable placeholder so a blank
// dimension key never produces a bare "" cell.
func labelOrNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(unattributed)"
	}
	return s
}

// signedDelta renders the change from prior→current as an absolute dollar delta
// with a percentage. A prior of 0 renders "new" (no finite percentage).
func signedDelta(current, prior float64) string {
	delta := current - prior
	sign := "+"
	if delta < 0 {
		sign = "-"
	}
	abs := math.Abs(delta)
	if prior == 0 {
		if current == 0 {
			return "no change"
		}
		return fmt.Sprintf("%s%s (new)", sign, money(abs))
	}
	pct := delta / prior * 100
	return fmt.Sprintf("%s%s (%+.0f%%)", sign, money(abs), pct)
}

// RankMovers builds the top-N movers by absolute spend change from a current
// and prior breakdown (keyed by label). It is a caller convenience kept in the
// pure package so both the node and org schedulers rank identically. Labels
// present in either period are considered; ties break by current spend.
func RankMovers(current, prior []LineItem, topN int) []Mover {
	priorByLabel := make(map[string]float64, len(prior))
	for _, p := range prior {
		priorByLabel[p.Label] += p.CostUSD
	}
	curByLabel := make(map[string]float64, len(current))
	order := make([]string, 0, len(current)+len(prior))
	seen := map[string]bool{}
	add := func(label string) {
		if !seen[label] {
			seen[label] = true
			order = append(order, label)
		}
	}
	for _, c := range current {
		curByLabel[c.Label] += c.CostUSD
		add(c.Label)
	}
	for _, p := range prior {
		add(p.Label)
	}

	movers := make([]Mover, 0, len(order))
	for _, label := range order {
		movers = append(movers, Mover{Label: label, CurrentUSD: curByLabel[label], PriorUSD: priorByLabel[label]})
	}
	// Sort by absolute delta desc, then current spend desc, then label for
	// determinism.
	sortMovers(movers)
	if topN > 0 && len(movers) > topN {
		movers = movers[:topN]
	}
	return movers
}

// sortMovers orders movers by |delta| desc, current desc, label asc.
func sortMovers(m []Mover) {
	sort.SliceStable(m, func(i, j int) bool { return lessMover(m[i], m[j]) })
}

// lessMover reports whether a should sort before b under the mover ordering.
func lessMover(a, b Mover) bool {
	da, db := math.Abs(a.Delta()), math.Abs(b.Delta())
	if da != db {
		return da > db
	}
	if a.CurrentUSD != b.CurrentUSD {
		return a.CurrentUSD > b.CurrentUSD
	}
	return a.Label < b.Label
}
