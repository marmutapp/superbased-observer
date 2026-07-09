package store

import (
	"context"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
)

func quietLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// TestTailAPITurns_CadenceAndRestartCursor exercises the pure timing loop with
// in-memory fakes (the orgclient.runLoop pattern) under testing/synctest, so it
// asserts the polling cadence and the restart-from-cursor seam on a fake clock
// with zero real I/O.
func TestTailAPITurns_CadenceAndRestartCursor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const interval = time.Second
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Restart-from-cursor: the loader reports a non-zero high-water mark,
		// so the very first fetch must ask for id > 5.
		load := func(context.Context) (int64, error) { return 5, nil }
		var saved int64
		save := func(_ context.Context, id int64) error { saved = id; return nil }

		firstAfterID := int64(-1)
		call := 0
		fetch := func(_ context.Context, afterID int64, _ int) ([]models.APITurn, error) {
			if firstAfterID < 0 {
				firstAfterID = afterID
			}
			defer func() { call++ }()
			switch call {
			case 0:
				return []models.APITurn{{ID: 6}, {ID: 7}}, nil
			case 2:
				return []models.APITurn{{ID: 8}}, nil
			default:
				return nil, nil // empty poll
			}
		}

		out := make(chan models.APITurn, 8)
		done := make(chan struct{})
		go func() {
			tailAPITurns(ctx, interval, quietLogger(), out, load, save, fetch)
			close(done)
		}()

		start := time.Now()
		var ids []int64
		var times []time.Duration
		for len(ids) < 3 {
			r := <-out
			ids = append(ids, r.ID)
			times = append(times, time.Since(start))
		}
		// Stop the tail and join it before reading the closure-shared vars, so
		// the goroutine-exit happens-before edge keeps the reads race-free.
		cancel()
		<-done

		if firstAfterID != 5 {
			t.Errorf("first fetch afterID = %d, want 5 (restart-from-cursor)", firstAfterID)
		}
		wantIDs := []int64{6, 7, 8}
		for i, id := range wantIDs {
			if ids[i] != id {
				t.Errorf("ids[%d] = %d, want %d", i, ids[i], id)
			}
		}
		// 6 and 7 publish on the immediate first drain (t=0); 8 arrives two
		// ticks later (one empty poll at t=interval, then the row at 2*interval).
		if times[0] != 0 || times[1] != 0 {
			t.Errorf("rows 6,7 should publish at t=0, got %v / %v", times[0], times[1])
		}
		if times[2] != 2*interval {
			t.Errorf("row 8 should publish at 2*interval (%v), got %v", 2*interval, times[2])
		}
		if saved != 8 {
			t.Errorf("persisted cursor = %d, want 8", saved)
		}
	})
}

// TestTailAPITurns_StopsOnContextCancel proves the loop closes the channel and
// returns when the context is cancelled mid-wait.
func TestTailAPITurns_StopsOnContextCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		load := func(context.Context) (int64, error) { return 0, nil }
		save := func(context.Context, int64) error { return nil }
		fetch := func(context.Context, int64, int) ([]models.APITurn, error) { return nil, nil }

		out := make(chan models.APITurn)
		done := make(chan struct{})
		go func() {
			tailAPITurns(ctx, time.Second, quietLogger(), out, load, save, fetch)
			close(done)
		}()

		synctest.Wait() // let the loop reach its first ticker wait
		cancel()
		<-done // tailAPITurns must return; if out weren't closed this would hang
		if _, ok := <-out; ok {
			t.Error("channel should be closed after cancel")
		}
	})
}

// TestSubscribeAPITurns_EndToEnd validates the real store path: rows are
// delivered in id order, the otel_cursor is persisted, and a restart resumes
// past the cursor (no re-delivery) — the cursor round-trip through schema_meta
// that the synctest fakes cannot cover.
func TestSubscribeAPITurns_EndToEnd(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	pid, err := s.UpsertProject(ctx, "/tmp/otelproj", "")
	if err != nil {
		t.Fatal(err)
	}

	insert := func(in int64) int64 {
		t.Helper()
		id, err := s.InsertAPITurn(ctx, models.APITurn{
			Provider: "anthropic", Model: "claude-test", ProjectID: pid,
			Timestamp: time.Now(), InputTokens: in, OutputTokens: 1,
		})
		if err != nil {
			t.Fatalf("InsertAPITurn: %v", err)
		}
		return id
	}
	id1, id2, id3 := insert(10), insert(20), insert(30)

	// First subscription: drains all three.
	cctx, cancel := context.WithCancel(ctx)
	ch := s.SubscribeAPITurns(cctx, 5*time.Millisecond, quietLogger())
	got := collect(t, ch, 3)
	cancel()
	drain(ch)

	if got[0].ID != id1 || got[1].ID != id2 || got[2].ID != id3 {
		t.Errorf("delivered ids %d,%d,%d want %d,%d,%d",
			got[0].ID, got[1].ID, got[2].ID, id1, id2, id3)
	}
	if got[0].Provider != "anthropic" || got[0].InputTokens != 10 {
		t.Errorf("row not fully populated: %+v", got[0])
	}
	if cur, _ := s.loadOTelCursor(ctx); cur != id3 {
		t.Errorf("persisted cursor = %d, want %d", cur, id3)
	}

	// Restart: a fresh subscription must resume past the cursor and deliver
	// only the new turn, never re-sending id1..id3.
	id4 := insert(40)
	cctx2, cancel2 := context.WithCancel(ctx)
	defer cancel2()
	ch2 := s.SubscribeAPITurns(cctx2, 5*time.Millisecond, quietLogger())
	got2 := collect(t, ch2, 1)
	cancel2()
	drain(ch2)
	if got2[0].ID != id4 {
		t.Errorf("restart delivered id %d, want only the new id %d", got2[0].ID, id4)
	}
}

func TestProjectRootByID(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	pid, err := s.UpsertProject(ctx, "/tmp/rootbyid", "")
	if err != nil {
		t.Fatal(err)
	}
	root, err := s.ProjectRootByID(ctx, pid)
	if err != nil || root != "/tmp/rootbyid" {
		t.Errorf("ProjectRootByID(%d) = %q, %v; want /tmp/rootbyid", pid, root, err)
	}
	if root, err := s.ProjectRootByID(ctx, 0); err != nil || root != "" {
		t.Errorf("ProjectRootByID(0) = %q, %v; want empty, nil", root, err)
	}
	if root, err := s.ProjectRootByID(ctx, 999999); err != nil || root != "" {
		t.Errorf("ProjectRootByID(unknown) = %q, %v; want empty, nil", root, err)
	}
}

func collect(t *testing.T, ch <-chan models.APITurn, n int) []models.APITurn {
	t.Helper()
	var out []models.APITurn
	deadline := time.After(3 * time.Second)
	for len(out) < n {
		select {
		case r, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed after %d rows, want %d", len(out), n)
			}
			out = append(out, r)
		case <-deadline:
			t.Fatalf("timeout: collected %d rows, want %d", len(out), n)
		}
	}
	return out
}

func drain(ch <-chan models.APITurn) {
	for range ch { //nolint:revive // intentionally draining until the producer closes
	}
}
