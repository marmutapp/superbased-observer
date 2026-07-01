package store

import (
	"context"
	"testing"
)

// codex token_count rate_limits envelope, as the adapter marshals it into
// actions.raw_tool_input. primary = 5h window, secondary = weekly.
const codexRateLimitRawJSON = `{"limit_id":"codex","primary":{"used_percent":18,"window_minutes":300,"resets_at":1778867450},"secondary":{"used_percent":3,"window_minutes":10080,"resets_at":1779454250},"plan_type":"plus","rate_limit_reached_type":null}`

func TestParseRateLimitWindows(t *testing.T) {
	t.Parallel()
	w, ok := parseRateLimitWindows(codexRateLimitRawJSON)
	if !ok {
		t.Fatal("parseRateLimitWindows: ok=false, want true")
	}
	if w.Window5hUtil == nil || *w.Window5hUtil != 0.18 {
		t.Errorf("5h util = %v, want 0.18", w.Window5hUtil)
	}
	if w.Window7dUtil == nil || *w.Window7dUtil != 0.03 {
		t.Errorf("7d util = %v, want 0.03", w.Window7dUtil)
	}
	if w.Window5hReset == nil || *w.Window5hReset != 1778867450 {
		t.Errorf("5h reset = %v, want 1778867450", w.Window5hReset)
	}
	if w.PlanType != "plus" {
		t.Errorf("plan = %q, want plus", w.PlanType)
	}

	// Garbage / non-rate_limit body → ok=false, never panics.
	if _, ok := parseRateLimitWindows("not json"); ok {
		t.Error("garbage body parsed ok=true, want false")
	}
	if _, ok := parseRateLimitWindows(`{"limit_id":"codex"}`); ok {
		t.Error("window-less body parsed ok=true, want false")
	}
}

func TestLatestRateLimitWindows(t *testing.T) {
	t.Parallel()
	st, db := newTestStore(t)
	ctx := context.Background()

	pid, err := st.UpsertProject(ctx, "/tmp/rl", "")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, tool, started_at) VALUES (?,?,?,?)`,
		"sCodex", pid, "codex", "2026-05-15T12:00:00Z"); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	insertRL := func(ts, raw string) {
		t.Helper()
		if _, err := db.ExecContext(ctx,
			`INSERT INTO actions (session_id, project_id, timestamp, action_type, tool, raw_tool_input, source_file, source_event_id)
			 VALUES (?,?,?,?,?,?,?,?)`,
			"sCodex", pid, ts, "rate_limit", "codex", raw,
			"rollout.jsonl", "ratelimit:rollout.jsonl:"+ts); err != nil {
			t.Fatalf("insert rate_limit action: %v", err)
		}
	}

	// No rows yet → ok=false (every non-codex tool stays here forever).
	if _, ok, err := st.LatestRateLimitWindows(ctx, "codex", "sCodex"); err != nil || ok {
		t.Fatalf("empty: ok=%v err=%v, want ok=false", ok, err)
	}

	// Older then newer; the newest by timestamp must win.
	insertRL("2026-05-15T12:30:00Z", codexRateLimitRawJSON)
	newer := `{"limit_id":"codex","primary":{"used_percent":55,"window_minutes":300,"resets_at":1778870000},"secondary":{"used_percent":9,"window_minutes":10080,"resets_at":1779454250},"plan_type":"plus","rate_limit_reached_type":null}`
	insertRL("2026-05-15T12:45:00Z", newer)

	got, ok, err := st.LatestRateLimitWindows(ctx, "codex", "sCodex")
	if err != nil || !ok {
		t.Fatalf("latest: ok=%v err=%v", ok, err)
	}
	if got.Window5hUtil == nil || *got.Window5hUtil != 0.55 {
		t.Errorf("latest 5h util = %v, want 0.55 (newest)", got.Window5hUtil)
	}
	if got.ObservedAt.IsZero() {
		t.Error("ObservedAt not set")
	}

	// Account-wide fallback: a session with no rate_limit row of its own
	// still gets the tool's most-recent window.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, tool, started_at) VALUES (?,?,?,?)`,
		"sFresh", pid, "codex", "2026-05-15T13:00:00Z"); err != nil {
		t.Fatalf("insert fresh session: %v", err)
	}
	got2, ok, err := st.LatestRateLimitWindows(ctx, "codex", "sFresh")
	if err != nil || !ok {
		t.Fatalf("fallback: ok=%v err=%v", ok, err)
	}
	if got2.Window5hUtil == nil || *got2.Window5hUtil != 0.55 {
		t.Errorf("fallback 5h util = %v, want 0.55", got2.Window5hUtil)
	}
}
