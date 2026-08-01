package oneshot

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// defaultWindowDays is the window used when spec is empty (no --since
// flag given), per plan §1.3.
const defaultWindowDays = 30

// Window parses a --since spec into a since time.Time (the lower bound of
// the reporting window) and a human-readable label for the report header.
// now is the caller's current time, injected so callers never depend on
// time.Now() directly and tests stay deterministic.
//
// Accepted specs:
//
//   - ""    → the default 30-day window, labeled "last 30 days".
//   - "Nd"  → the last N days, labeled "last N days", for any integer N > 0.
//     "7d"/"30d"/"90d" are the documented shortcuts, but any positive
//     integer works (e.g. "14d").
//   - "all" → the zero time.Time (no lower bound), labeled "all time".
//   - an RFC3339 timestamp → that instant as the lower bound, labeled
//     "since <yyyy-mm-dd>".
//
// A non-positive day count ("-1d", "0d"), or any spec that is neither
// "all" nor a well-formed "Nd" spec nor a valid RFC3339 timestamp, is
// rejected with an error naming the accepted forms.
func Window(spec string, now time.Time) (time.Time, string, error) {
	switch spec {
	case "":
		return windowDays(defaultWindowDays, now), fmt.Sprintf("last %d days", defaultWindowDays), nil
	case "all":
		return time.Time{}, "all time", nil
	}

	if strings.HasSuffix(spec, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(spec, "d"))
		if err != nil {
			return time.Time{}, "", fmt.Errorf("oneshot.Window: %q is not a recognized window (want 7d/30d/90d/all/RFC3339): %w", spec, err)
		}
		if n <= 0 {
			return time.Time{}, "", fmt.Errorf("oneshot.Window: window must be a positive number of days, got %q", spec)
		}
		return windowDays(n, now), fmt.Sprintf("last %d days", n), nil
	}

	t, err := time.Parse(time.RFC3339, spec)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("oneshot.Window: %q is not a recognized window (want 7d/30d/90d/all/RFC3339): %w", spec, err)
	}
	t = t.UTC()
	return t, "since " + t.Format("2006-01-02"), nil
}

// windowDays returns the since time for an N-day-lookback window anchored
// at now.
func windowDays(n int, now time.Time) time.Time {
	return now.UTC().Add(-time.Duration(n) * 24 * time.Hour)
}
