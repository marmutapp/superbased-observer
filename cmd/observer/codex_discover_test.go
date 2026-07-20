package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeRollout writes a codex rollout JSONL whose leading line is a session_meta
// record carrying id + cwd, under dir with a rollout-*.jsonl name.
func writeRollout(t *testing.T, dir, id, cwd string) string {
	t.Helper()
	name := fmt.Sprintf("rollout-2026-07-19T10-00-00-%s.jsonl", id)
	path := filepath.Join(dir, name)
	meta := fmt.Sprintf(
		`{"timestamp":"2026-07-19T10:00:00.000Z","type":"session_meta","payload":{"id":%q,"timestamp":"2026-07-19T10:00:00.000Z","cwd":%q}}`+"\n",
		id, cwd,
	)
	body := meta + `{"timestamp":"2026-07-19T10:00:01.000Z","type":"event_msg","payload":{"type":"user_message","message":"hi"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	return path
}

func TestSelectDiscoveredCodexSession(t *testing.T) {
	cwd := "/work/proj"
	cases := []struct {
		name      string
		cands     []codexRolloutCandidate
		targetCwd string
		wantID    string
		wantCount int
	}{
		{"zero candidates", nil, cwd, "", 0},
		{"single, no cwd filter", []codexRolloutCandidate{{sessionID: "a", cwd: cwd}}, "", "a", 1},
		{"single, cwd match", []codexRolloutCandidate{{sessionID: "a", cwd: cwd}}, cwd, "a", 1},
		{
			"two candidates → abstain",
			[]codexRolloutCandidate{{sessionID: "a", cwd: cwd}, {sessionID: "b", cwd: cwd}},
			cwd, "", 2,
		},
		{
			"two candidates, one cwd mismatch → single survives",
			[]codexRolloutCandidate{{sessionID: "a", cwd: cwd}, {sessionID: "b", cwd: "/other"}},
			cwd, "a", 1,
		},
		{
			"candidate with unknown cwd is kept (counts toward ambiguity)",
			[]codexRolloutCandidate{{sessionID: "a", cwd: cwd}, {sessionID: "b", cwd: ""}},
			cwd, "", 2,
		},
		{
			"empty session id skipped",
			[]codexRolloutCandidate{{sessionID: "", cwd: cwd}, {sessionID: "a", cwd: cwd}},
			cwd, "a", 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, count := selectDiscoveredCodexSession(tc.cands, tc.targetCwd)
			if id != tc.wantID || count != tc.wantCount {
				t.Fatalf("selectDiscoveredCodexSession = (%q,%d), want (%q,%d)", id, count, tc.wantID, tc.wantCount)
			}
		})
	}
}

func TestReadCodexRolloutMeta(t *testing.T) {
	dir := t.TempDir()

	t.Run("session_meta id + cwd", func(t *testing.T) {
		p := writeRollout(t, dir, "019e-aaaa", "/work/proj")
		id, cwd := readCodexRolloutMeta(p)
		if id != "019e-aaaa" || cwd != "/work/proj" {
			t.Fatalf("readCodexRolloutMeta = (%q,%q), want (019e-aaaa,/work/proj)", id, cwd)
		}
	})

	t.Run("legacy session_configured / session_id", func(t *testing.T) {
		p := filepath.Join(dir, "rollout-legacy.jsonl")
		body := `{"type":"session_configured","payload":{"session_id":"legacy-1","cwd":"/legacy"}}` + "\n"
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		id, cwd := readCodexRolloutMeta(p)
		if id != "legacy-1" || cwd != "/legacy" {
			t.Fatalf("readCodexRolloutMeta legacy = (%q,%q), want (legacy-1,/legacy)", id, cwd)
		}
	})

	t.Run("no meta → empty", func(t *testing.T) {
		p := filepath.Join(dir, "rollout-nometa.jsonl")
		body := `{"type":"event_msg","payload":{"type":"user_message","message":"hi"}}` + "\n"
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		id, cwd := readCodexRolloutMeta(p)
		if id != "" || cwd != "" {
			t.Fatalf("readCodexRolloutMeta nometa = (%q,%q), want empty", id, cwd)
		}
	})
}

func TestScanNewCodexRollouts(t *testing.T) {
	root := t.TempDir()
	start := time.Now()

	// A pre-existing rollout (snapshotted before start) must be excluded even
	// though it lives under the root.
	pre := writeRollout(t, root, "old-1", "/work/proj")
	preexisting := map[string]struct{}{pre: {}}

	// A genuinely new rollout appears after start.
	writeRollout(t, root, "new-1", "/work/proj")

	got := scanNewCodexRollouts([]string{root}, preexisting, start)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 new rollout, got %d: %+v", len(got), got)
	}
	if got[0].sessionID != "new-1" || got[0].cwd != "/work/proj" {
		t.Fatalf("scan candidate = %+v, want id=new-1 cwd=/work/proj", got[0])
	}

	// A file whose ModTime predates start is rejected by the defensive guard.
	future := writeRollout(t, root, "new-2", "/work/proj")
	old := start.Add(-1 * time.Hour)
	if err := os.Chtimes(future, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	got2 := scanNewCodexRollouts([]string{root}, preexisting, start)
	for _, c := range got2 {
		if c.sessionID == "new-2" {
			t.Fatalf("rollout with ModTime before start must be excluded, got %+v", got2)
		}
	}
}

func TestRunCodexDiscovery(t *testing.T) {
	// A short window keeps the tests fast while still exercising the
	// observe-the-whole-window-then-announce behaviour (R2-1).
	cfg := codexDiscoverConfig{window: 120 * time.Millisecond, poll: 5 * time.Millisecond}
	cwd := "/work/proj"

	t.Run("single new rollout announces at window END (not mid-window)", func(t *testing.T) {
		root := t.TempDir()
		start := time.Now().Add(-time.Second)
		writeRollout(t, root, "disco-1", cwd)

		var got string
		var calls int
		begin := time.Now()
		runCodexDiscovery(context.Background(), []string{root}, map[string]struct{}{}, start, cwd, cfg,
			func(id string) { got = id; calls++ })
		elapsed := time.Since(begin)
		if calls != 1 || got != "disco-1" {
			t.Fatalf("announce calls=%d id=%q, want 1 / disco-1", calls, got)
		}
		// It must NOT announce at ~2 polls (the pre-fix bug) — it waits the window
		// so a late second candidate can still force abstention.
		if elapsed < cfg.window/2 {
			t.Fatalf("announced after %v — must observe the whole window before announcing (>= %v)", elapsed, cfg.window/2)
		}
	})

	t.Run("two new rollouts → no announcement (never guess)", func(t *testing.T) {
		root := t.TempDir()
		start := time.Now().Add(-time.Second)
		writeRollout(t, root, "amb-1", cwd)
		writeRollout(t, root, "amb-2", cwd)

		var calls int
		runCodexDiscovery(context.Background(), []string{root}, map[string]struct{}{}, start, cwd, cfg,
			func(string) { calls++ })
		if calls != 0 {
			t.Fatalf("ambiguous window must not announce, got %d calls", calls)
		}
	})

	t.Run("late second candidate BEFORE close → abstain (R2-1)", func(t *testing.T) {
		root := t.TempDir()
		start := time.Now().Add(-time.Second)
		// One candidate is present from the start; a SECOND (same cwd, so cwd
		// corroboration can't disambiguate) appears partway through the window.
		// The pre-fix loop would have announced the first at ~2 polls and returned
		// before the second ever showed; the whole-window observation catches it.
		writeRollout(t, root, "first-1", cwd)
		go func() {
			time.Sleep(cfg.window / 3)
			writeRollout(t, root, "second-1", cwd)
		}()

		var got string
		var calls int
		runCodexDiscovery(context.Background(), []string{root}, map[string]struct{}{}, start, cwd, cfg,
			func(id string) { got = id; calls++ })
		if calls != 0 {
			t.Fatalf("a second candidate appearing before window close must force abstention, got %d calls (id=%q)", calls, got)
		}
	})

	t.Run("cwd mismatch disambiguates to the matching session", func(t *testing.T) {
		root := t.TempDir()
		start := time.Now().Add(-time.Second)
		writeRollout(t, root, "mine-1", cwd)
		writeRollout(t, root, "theirs-1", "/some/other/dir")

		var got string
		var calls int
		runCodexDiscovery(context.Background(), []string{root}, map[string]struct{}{}, start, cwd, cfg,
			func(id string) { got = id; calls++ })
		if calls != 1 || got != "mine-1" {
			t.Fatalf("announce calls=%d id=%q, want 1 / mine-1", calls, got)
		}
	})

	t.Run("no rollout in window → no announcement", func(t *testing.T) {
		root := t.TempDir()
		start := time.Now().Add(-time.Second)
		cfg2 := codexDiscoverConfig{window: 60 * time.Millisecond, poll: 5 * time.Millisecond}

		var calls int
		runCodexDiscovery(context.Background(), []string{root}, map[string]struct{}{}, start, cwd, cfg2,
			func(string) { calls++ })
		if calls != 0 {
			t.Fatalf("empty window must not announce, got %d calls", calls)
		}
	})

	t.Run("cancelled ctx stops without announcing", func(t *testing.T) {
		root := t.TempDir()
		start := time.Now().Add(-time.Second)
		writeRollout(t, root, "disco-1", cwd)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		var calls int
		runCodexDiscovery(ctx, []string{root}, map[string]struct{}{}, start, cwd, cfg,
			func(string) { calls++ })
		if calls != 0 {
			t.Fatalf("cancelled discovery must not announce, got %d calls", calls)
		}
	})
}

func TestSnapshotCodexRollouts(t *testing.T) {
	root := t.TempDir()
	a := writeRollout(t, root, "a", "/x")
	b := writeRollout(t, root, "b", "/x")
	// A non-rollout file must be ignored.
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	snap := snapshotCodexRollouts([]string{root})
	if _, ok := snap[a]; !ok {
		t.Fatalf("snapshot missing %s", a)
	}
	if _, ok := snap[b]; !ok {
		t.Fatalf("snapshot missing %s", b)
	}
	if len(snap) != 2 {
		t.Fatalf("snapshot size = %d, want 2 (rollouts only): %+v", len(snap), snap)
	}
}
