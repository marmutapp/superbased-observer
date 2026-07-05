package retention

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
)

var fixedNow = time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)

// openDB opens a migrated org-server DB (applies migrations incl. 007/011).
func openDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := orgdb.Open(context.Background(), orgdb.Options{Path: filepath.Join(t.TempDir(), "server.db")})
	if err != nil {
		t.Fatalf("orgdb.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// seedOTel inserts one otel_content row with the given event timestamp; a nil
// content pointer stores SQL NULL.
func seedOTel(t *testing.T, d *sql.DB, hash, ts string, content *string) {
	t.Helper()
	var c any
	if content != nil {
		c = *content
	}
	if _, err := d.ExecContext(context.Background(),
		`INSERT INTO otel_content (content_hash, user_id, org_id, request_id, session_id, tool_use_id, kind, content, timestamp, pushed_at, pushed_by_user_id)
		 VALUES (?, 'u1', 'org-1', '', 's1', '', 'prompt', ?, ?, ?, 'u1')`,
		hash, c, ts, ts); err != nil {
		t.Fatalf("seed otel_content: %v", err)
	}
}

func bodyOf(t *testing.T, d *sql.DB, hash string) (content sql.NullString) {
	t.Helper()
	if err := d.QueryRow(`SELECT content FROM otel_content WHERE content_hash = ?`, hash).Scan(&content); err != nil {
		t.Fatalf("read body %s: %v", hash, err)
	}
	return content
}

func TestPruneOTelContent_RespectsHorizonAndKeepsHash(t *testing.T) {
	d := openDB(t)
	old := "the old body"  // ~40 days before now
	recent := "fresh body" // ~1 day before now
	seedOTel(t, d, "h-old", "2026-05-14T12:00:00Z", &old)
	seedOTel(t, d, "h-recent", "2026-06-22T12:00:00Z", &recent)

	n, err := PruneOTelContent(context.Background(), d, 30, fixedNow)
	if err != nil {
		t.Fatalf("PruneOTelContent: %v", err)
	}
	if n != 1 {
		t.Fatalf("cleared = %d, want 1 (only the >30d body)", n)
	}
	// Old body NULLed; its row + hash survive.
	if b := bodyOf(t, d, "h-old"); b.Valid {
		t.Errorf("old body not cleared: %q", b.String)
	}
	var hashStillThere string
	if err := d.QueryRow(`SELECT content_hash FROM otel_content WHERE content_hash='h-old'`).Scan(&hashStillThere); err != nil {
		t.Errorf("old row/hash gone after prune (must keep hash): %v", err)
	}
	// Recent body untouched.
	if b := bodyOf(t, d, "h-recent"); !b.Valid || b.String != recent {
		t.Errorf("recent body changed: %+v", b)
	}
}

func TestPruneOTelContent_DisabledIsNoop(t *testing.T) {
	d := openDB(t)
	body := "keep me"
	seedOTel(t, d, "h1", "2020-01-01T00:00:00Z", &body) // ancient
	for _, horizon := range []int{0, -1} {
		n, err := PruneOTelContent(context.Background(), d, horizon, fixedNow)
		if err != nil {
			t.Fatalf("PruneOTelContent(%d): %v", horizon, err)
		}
		if n != 0 {
			t.Errorf("horizon %d cleared %d, want 0 (disabled)", horizon, n)
		}
	}
	if b := bodyOf(t, d, "h1"); !b.Valid {
		t.Errorf("disabled prune cleared a body")
	}
}

func TestPruneOTelContent_SecondRunNoop(t *testing.T) {
	d := openDB(t)
	body := "old"
	seedOTel(t, d, "h1", "2020-01-01T00:00:00Z", &body)
	if n, _ := PruneOTelContent(context.Background(), d, 30, fixedNow); n != 1 {
		t.Fatalf("first run cleared %d, want 1", n)
	}
	if n, err := PruneOTelContent(context.Background(), d, 30, fixedNow); err != nil || n != 0 {
		t.Errorf("second run cleared %d (err %v), want 0 (idempotent)", n, err)
	}
}

func TestNewSweeper_NilWhenDisabled(t *testing.T) {
	d := openDB(t)
	if s := NewSweeper(d, 0, nil); s != nil {
		t.Errorf("NewSweeper(0) = non-nil, want nil (retention off)")
	}
	if s := NewSweeper(d, 30, nil); s == nil {
		t.Errorf("NewSweeper(30) = nil, want a sweeper")
	}
}

// seedObs inserts one obs_content row with the given event timestamp; a nil
// content pointer stores SQL NULL (obs-org-tier T3).
func seedObs(t *testing.T, d *sql.DB, hash, ts string, content *string) {
	t.Helper()
	var c any
	if content != nil {
		c = *content
	}
	if _, err := d.ExecContext(context.Background(),
		`INSERT INTO obs_content (org_id, content_hash, kind, span_id, trace_id, content, timestamp, pushed_at, pushed_by_user_id)
		 VALUES ('org-1', ?, 'prompt', 'sp1', 'tr1', ?, ?, ?, 'u1')`,
		hash, c, ts, ts); err != nil {
		t.Fatalf("seed obs_content: %v", err)
	}
}

// TestPruneObsContent_RespectsHorizonAndKeepsHash mirrors the otel test for the
// T3 obs_content store: NULL old bodies past the horizon, keep the hash + row.
func TestPruneObsContent_RespectsHorizonAndKeepsHash(t *testing.T) {
	d := openDB(t)
	old := "the old body"
	recent := "fresh body"
	seedObs(t, d, "h-old", "2026-05-14T12:00:00Z", &old)
	seedObs(t, d, "h-recent", "2026-06-22T12:00:00Z", &recent)

	n, err := PruneObsContent(context.Background(), d, 30, fixedNow)
	if err != nil {
		t.Fatalf("PruneObsContent: %v", err)
	}
	if n != 1 {
		t.Fatalf("cleared = %d, want 1", n)
	}
	var b sql.NullString
	if err := d.QueryRow(`SELECT content FROM obs_content WHERE content_hash='h-old'`).Scan(&b); err != nil {
		t.Fatalf("read old: %v", err)
	}
	if b.Valid {
		t.Errorf("old obs body not cleared: %q", b.String)
	}
	var hash string
	if err := d.QueryRow(`SELECT content_hash FROM obs_content WHERE content_hash='h-old'`).Scan(&hash); err != nil {
		t.Errorf("old obs row/hash gone (must keep hash): %v", err)
	}
	// Disabled horizon is a no-op.
	if n, _ := PruneObsContent(context.Background(), d, 0, fixedNow); n != 0 {
		t.Errorf("horizon 0 cleared %d, want 0", n)
	}
}
