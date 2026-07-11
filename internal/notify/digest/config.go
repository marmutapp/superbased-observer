package digest

import (
	"fmt"
	"strings"
	"time"
)

// Frequency is the digest cadence.
type Frequency string

const (
	// Weekly reports the most-recently-completed 7-day window.
	Weekly Frequency = "weekly"
	// Monthly reports the most-recently-completed calendar month.
	Monthly Frequency = "monthly"
)

// Config is the [digest] block shared by the node (~/.observer/config.toml) and
// the org server (/etc/observer-org/config.toml). Like email.Config it is the
// SINGLE owner of the [digest] shape — both config packages embed this type as
// their [digest] surface, so the shape and its validation live in one place.
//
// LOCAL-ONLY, never distributed over the org wire. Zero value = disabled: a
// default install schedules no digest and makes no SMTP connection. A digest
// send additionally requires [email].enabled — the digest rides the same
// fail-soft email channel the alert evaluators use.
type Config struct {
	// Enabled gates the scheduler. Default false (opt-in; a fired digest makes
	// an outbound SMTP call).
	Enabled bool `toml:"enabled"`
	// Frequency ∈ weekly | monthly. Empty defaults to weekly.
	Frequency string `toml:"frequency"`
	// SendHour is the UTC hour (0–23) at/after which a due digest is sent on the
	// first tick of a new period. Default 8.
	SendHour int `toml:"send_hour"`
	// To overrides the recipient list for digests. Empty falls back to
	// [email].to (resolved by the caller before sending).
	To []string `toml:"to"`
}

// Validate checks the block only when Enabled — a stale disabled section never
// fails the daemon (digests are opt-in). It never touches I/O.
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(c.Frequency)) {
	case "", string(Weekly), string(Monthly):
	default:
		return fmt.Errorf("digest: frequency %q not in {weekly, monthly}", c.Frequency)
	}
	if c.SendHour < 0 || c.SendHour > 23 {
		return fmt.Errorf("digest: send_hour %d out of range 0..23", c.SendHour)
	}
	return nil
}

// FrequencyOrDefault returns the effective cadence, defaulting to weekly.
func (c Config) FrequencyOrDefault() Frequency {
	switch strings.ToLower(strings.TrimSpace(c.Frequency)) {
	case string(Monthly):
		return Monthly
	default:
		return Weekly
	}
}

// SendHourOrDefault returns the effective send hour, defaulting to 8.
func (c Config) SendHourOrDefault() int {
	if c.SendHour < 0 || c.SendHour > 23 {
		return 8
	}
	if c.SendHour == 0 {
		// The zero value is indistinguishable from an unset block, so 0 is
		// treated as the default 8 (mid-morning UTC) rather than midnight. Set
		// any hour 1–23 for a specific send time.
		return 8
	}
	return c.SendHour
}

// Period identifies one completed reporting window: a stable string key (used
// for the send-once de-dup marker) plus the [Start, End) bounds and a human
// label for the email.
type Period struct {
	Key   string    // stable de-dup key, e.g. "2026-W28" or "2026-06"
	Start time.Time // inclusive
	End   time.Time // exclusive
	Label string    // human label for the email, e.g. "Jun 30 – Jul 6, 2026"
}

// DuePeriod returns the most-recently-COMPLETED reporting window as of now, and
// whether the send-hour gate has passed. The window is always closed (it never
// includes the in-progress period), so a digest reports settled data.
//
// ready is true when now.Hour() >= sendHour. Combined with the caller's
// persisted de-dup marker (send once per Key), this yields at-most-once
// delivery per period that is also restart-safe: the first tick of a new period
// at/after the send hour fires; every later tick sees Key already sent.
func DuePeriod(freq Frequency, sendHour int, now time.Time) (Period, bool) {
	now = now.UTC()
	var p Period
	switch freq {
	case Monthly:
		curMonthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		p.Start = curMonthStart.AddDate(0, -1, 0)
		p.End = curMonthStart
		p.Key = p.Start.Format("2006-01")
		p.Label = p.Start.Format("January 2006")
	default: // Weekly
		curWeekStart := startOfISOWeek(now)
		p.Start = curWeekStart.AddDate(0, 0, -7)
		p.End = curWeekStart
		y, wk := p.Start.ISOWeek()
		p.Key = fmt.Sprintf("%04d-W%02d", y, wk)
		lastDay := p.End.AddDate(0, 0, -1)
		p.Label = fmt.Sprintf("%s – %s", p.Start.Format("Jan 2"), lastDay.Format("Jan 2, 2006"))
	}
	return p, now.Hour() >= sendHour
}

// startOfISOWeek returns 00:00 UTC of the Monday of t's ISO week.
func startOfISOWeek(t time.Time) time.Time {
	t = t.UTC()
	// Go's Weekday: Sunday=0 … Saturday=6. ISO week starts Monday.
	offset := (int(t.Weekday()) + 6) % 7 // days since Monday
	monday := t.AddDate(0, 0, -offset)
	return time.Date(monday.Year(), monday.Month(), monday.Day(), 0, 0, 0, 0, time.UTC)
}
