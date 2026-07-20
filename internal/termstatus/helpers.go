package termstatus

import (
	"strconv"
	"time"
)

// itoa formats an int (small helper to keep the classifier evidence strings
// dependency-light).
func itoa(i int) string { return strconv.Itoa(i) }

// dur renders a duration as a short human string for evidence text ("3s",
// "5m"). It is approximate — evidence copy, not a precise metric.
func dur(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	case d >= time.Minute:
		return strconv.Itoa(int(d.Minutes())) + "m"
	default:
		return strconv.Itoa(int(d.Seconds())) + "s"
	}
}
