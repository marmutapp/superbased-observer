package dashboard

import (
	"net/http/httptest"
	"testing"
	"time"
)

// TestWindowRange exercises the shared time-window wire contract helper:
// param precedence (since/until > hours > days), fail-open fallback on
// malformed values, clamps, and byte-identical behavior when only `days`
// is supplied.
func TestWindowRange(t *testing.T) {
	// A fixed reference so we can assert the lower bound within a small
	// tolerance of "now - window". windowRange calls time.Now itself, so
	// we compare against a freshly-sampled now with a generous slack.
	const slack = 5 * time.Second

	approxSince := func(t *testing.T, got time.Time, wantAgo time.Duration) {
		t.Helper()
		if got.IsZero() {
			t.Fatalf("since is zero, want ~%s ago", wantAgo)
		}
		want := time.Now().UTC().Add(-wantAgo)
		if d := got.Sub(want); d > slack || d < -slack {
			t.Fatalf("since = %s (%s off from want %s)", got, d, want)
		}
	}

	t.Run("days only (default) — unchanged behavior", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?days=7", nil)
		since, until := windowRange(r, 30, 1, 36500)
		approxSince(t, since, 7*24*time.Hour)
		if !until.IsZero() {
			t.Fatalf("until = %s, want zero", until)
		}
	})

	t.Run("no params — falls back to defDays", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x", nil)
		since, until := windowRange(r, 30, 1, 36500)
		approxSince(t, since, 30*24*time.Hour)
		if !until.IsZero() {
			t.Fatalf("until = %s, want zero", until)
		}
	})

	t.Run("no params + minDays 0 — no lower bound", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x", nil)
		since, until := windowRange(r, 0, 0, 36500)
		if !since.IsZero() {
			t.Fatalf("since = %s, want zero (no filter)", since)
		}
		if !until.IsZero() {
			t.Fatalf("until = %s, want zero", until)
		}
	})

	t.Run("days clamps below minDays", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?days=0", nil)
		since, _ := windowRange(r, 30, 1, 36500)
		approxSince(t, since, 1*24*time.Hour) // clamped up to minDays=1
	})

	t.Run("days clamps above maxDays", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?days=100000", nil)
		since, _ := windowRange(r, 30, 1, 365)
		approxSince(t, since, 365*24*time.Hour)
	})

	t.Run("hours wins over days", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?hours=6&days=7", nil)
		since, _ := windowRange(r, 30, 1, 36500)
		approxSince(t, since, 6*time.Hour)
	})

	t.Run("sub-day hours are honored (no day truncation)", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?hours=1", nil)
		since, _ := windowRange(r, 30, 1, 36500)
		approxSince(t, since, 1*time.Hour)
	})

	t.Run("hours clamps to maxDays*24", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?hours=100000", nil)
		since, _ := windowRange(r, 30, 1, 365)
		approxSince(t, since, 365*24*time.Hour)
	})

	t.Run("hours below 1 falls through to days", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?hours=0&days=7", nil)
		since, _ := windowRange(r, 30, 1, 36500)
		approxSince(t, since, 7*24*time.Hour)
	})

	t.Run("malformed hours falls through to days", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?hours=abc&days=7", nil)
		since, _ := windowRange(r, 30, 1, 36500)
		approxSince(t, since, 7*24*time.Hour)
	})

	t.Run("explicit since wins over hours and days", func(t *testing.T) {
		want := "2026-01-02T03:04:05Z"
		r := httptest.NewRequest("GET", "/x?since="+want+"&hours=6&days=7", nil)
		since, _ := windowRange(r, 30, 1, 36500)
		wt, _ := time.Parse(time.RFC3339, want)
		if !since.Equal(wt) {
			t.Fatalf("since = %s, want %s", since, wt)
		}
	})

	t.Run("since accepts RFC3339Nano", func(t *testing.T) {
		want := "2026-01-02T03:04:05.123456789Z"
		r := httptest.NewRequest("GET", "/x?since="+want, nil)
		since, _ := windowRange(r, 30, 1, 36500)
		wt, _ := time.Parse(time.RFC3339Nano, want)
		if !since.Equal(wt) {
			t.Fatalf("since = %s, want %s", since, wt)
		}
	})

	t.Run("until parsed independently of lower-bound tier", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?days=7&until=2026-01-02T00:00:00Z", nil)
		since, until := windowRange(r, 30, 1, 36500)
		approxSince(t, since, 7*24*time.Hour)
		wt, _ := time.Parse(time.RFC3339, "2026-01-02T00:00:00Z")
		if !until.Equal(wt) {
			t.Fatalf("until = %s, want %s", until, wt)
		}
	})

	t.Run("since and until together", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?since=2026-01-01T00:00:00Z&until=2026-01-02T00:00:00Z", nil)
		since, until := windowRange(r, 30, 1, 36500)
		ws, _ := time.Parse(time.RFC3339, "2026-01-01T00:00:00Z")
		wu, _ := time.Parse(time.RFC3339, "2026-01-02T00:00:00Z")
		if !since.Equal(ws) || !until.Equal(wu) {
			t.Fatalf("since=%s until=%s, want %s / %s", since, until, ws, wu)
		}
	})

	t.Run("malformed since falls through to hours", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?since=not-a-time&hours=6", nil)
		since, _ := windowRange(r, 30, 1, 36500)
		approxSince(t, since, 6*time.Hour)
	})

	t.Run("malformed since with no lower tier falls to days default", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?since=not-a-time", nil)
		since, _ := windowRange(r, 30, 1, 36500)
		approxSince(t, since, 30*24*time.Hour)
	})

	t.Run("malformed until is ignored (no upper bound)", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?days=7&until=garbage", nil)
		_, until := windowRange(r, 30, 1, 36500)
		if !until.IsZero() {
			t.Fatalf("until = %s, want zero on malformed", until)
		}
	})

	t.Run("returned times are UTC", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?since=2026-01-01T00:00:00%2B05:00&until=2026-01-02T00:00:00%2B05:00", nil)
		since, until := windowRange(r, 30, 1, 36500)
		if since.Location() != time.UTC || until.Location() != time.UTC {
			t.Fatalf("since loc=%v until loc=%v, want UTC", since.Location(), until.Location())
		}
	})
}
