package ccanalytics

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
)

func newSchedTestPoller(t *testing.T) (*Poller, *sync.Mutex, *[]string, func()) {
	t.Helper()
	d, err := orgdb.Open(context.Background(), orgdb.Options{Path: filepath.Join(t.TempDir(), "s.db")})
	if err != nil {
		t.Fatalf("orgdb.Open: %v", err)
	}
	var mu sync.Mutex
	var days []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		days = append(days, r.URL.Query().Get("starting_at"))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"data":[],"has_more":false,"next_page":null}`))
	}))
	p := &Poller{DB: d, HTTPClient: srv.Client(), BaseURL: srv.URL, APIKey: "k"}
	cleanup := func() { srv.Close(); _ = d.Close() }
	return p, &mu, &days, cleanup
}

func TestScheduler_PollRecentSkipsTodayWithinLag(t *testing.T) {
	p, mu, days, cleanup := newSchedTestPoller(t)
	defer cleanup()

	// "now" is 00:30 UTC — only 30min into today, inside the 2h lag → today skipped.
	now := time.Date(2026, 6, 16, 0, 30, 0, 0, time.UTC)
	s := NewScheduler(p, 24, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.now = func() time.Time { return now }

	s.pollRecent(context.Background())

	mu.Lock()
	got := append([]string(nil), *days...)
	mu.Unlock()
	sort.Strings(got)
	// recentDays=3, today (06-16) skipped → 06-14 and 06-15 only.
	want := []string{"2026-06-14", "2026-06-15"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("polled days = %v, want %v", got, want)
	}
}

func TestScheduler_PollRecentIncludesTodayPastLag(t *testing.T) {
	p, mu, days, cleanup := newSchedTestPoller(t)
	defer cleanup()

	// "now" is 06:00 UTC — well past the 2h lag → today included.
	now := time.Date(2026, 6, 16, 6, 0, 0, 0, time.UTC)
	s := NewScheduler(p, 24, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.now = func() time.Time { return now }

	s.pollRecent(context.Background())

	mu.Lock()
	got := append([]string(nil), *days...)
	mu.Unlock()
	if len(got) != recentDays {
		t.Fatalf("polled %d days, want %d (today included): %v", len(got), recentDays, got)
	}
}
