package hermes

import "time"

// unixFloatToTime converts a Hermes Unix-epoch-float timestamp (e.g.
// 1717500000.123) into a Go time.Time. Hermes writes both started_at
// and timestamp columns as REAL (Unix seconds since 1970-01-01 UTC
// with fractional milliseconds), distinct from observer's canonical
// ISO-8601 string timestamps elsewhere.
//
// Negative inputs are clamped to the zero time so a corrupt fixture
// can't crash the parser. NaN / +Inf land at zero too — Go's
// time.Unix accepts arbitrary int64 but the float conversion would
// truncate to MinInt64 / MaxInt64 first, yielding a non-meaningful
// time.
func unixFloatToTime(epoch float64) time.Time {
	// Inf / NaN protection: !(epoch == epoch) catches NaN; the
	// absolute bound catches both infinities and absurd magnitudes
	// that would overflow time.Time.
	if epoch != epoch || epoch < 0 || epoch > 1e15 {
		return time.Time{}
	}
	sec := int64(epoch)
	nsec := int64((epoch - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).UTC()
}
