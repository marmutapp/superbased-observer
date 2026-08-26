package mistralcode

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestLiveVerify_RealStore is a manual audit tool, not part of the regular
// suite: it re-parses a REAL ~/.vibe/logs/session tree (never a fixture) and
// logs what came out, so a session-level regression against real vibe output
// is visible without hand-copying real data into testdata/. Skipped unless
// MISTRALCODE_LIVE_ROOT points at a real `.../logs/session` directory (never
// committed, never required for `go test ./...`).
func TestLiveVerify_RealStore(t *testing.T) {
	root := os.Getenv("MISTRALCODE_LIVE_ROOT")
	if root == "" {
		t.Skip("set MISTRALCODE_LIVE_ROOT=$HOME/.vibe/logs/session to run against a real store")
	}
	a := NewWithOptions(nil, root)

	var sessions, actions, tokenRows int
	models := map[string]int{}

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !a.IsSessionFile(path) {
			return nil
		}
		sessions++
		res, perr := a.ParseSessionFile(context.Background(), path, 0)
		if perr != nil {
			t.Errorf("ParseSessionFile(%s): %v", path, perr)
			return nil
		}
		actions += len(res.ToolEvents)
		tokenRows += len(res.TokenEvents)
		for _, tk := range res.TokenEvents {
			models[tk.Model]++
			t.Logf("session=%s model=%q input(net)=%d output=%d cache_read=%d cost=$%.4f",
				tk.SessionID, tk.Model, tk.InputTokens, tk.OutputTokens, tk.CacheReadTokens, tk.EstimatedCostUSD)
		}
		// Idempotency: re-parsing from the returned offset must yield zero new
		// tool events against the SAME bytes.
		second, serr := a.ParseSessionFile(context.Background(), path, res.NewOffset)
		if serr != nil {
			t.Errorf("re-parse(%s): %v", path, serr)
			return nil
		}
		if len(second.ToolEvents) != 0 {
			t.Errorf("re-parse from offset %d yielded %d NEW tool events, want 0", res.NewOffset, len(second.ToolEvents))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("sessions=%d actions=%d token_rows=%d models=%v", sessions, actions, tokenRows, models)
	if sessions == 0 {
		t.Errorf("MISTRALCODE_LIVE_ROOT=%s matched zero session files", root)
	}
}
