package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// getAttachSessions GETs /api/attach/sessions (optionally through the
// remote-exposed provenance marker) and returns the decoded rows.
func getAttachSessions(t *testing.T, s *Server, remote bool) []LaunchInfo {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/attach/sessions", nil)
	if remote {
		req = req.WithContext(withRemoteExposed(req.Context()))
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/attach/sessions = %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Sessions []LaunchInfo `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	return body.Sessions
}

func idsOf(rows []LaunchInfo) map[string]LaunchInfo {
	m := make(map[string]LaunchInfo, len(rows))
	for _, r := range rows {
		m[r.ID] = r
	}
	return m
}

// TestAttachSessionsDisabledWhenNilManager pins the honest-disabled 503 when the
// embedded-terminal launcher is unwired.
func TestAttachSessionsDisabledWhenNilManager(t *testing.T) {
	s := newLaunchTestServer(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/attach/sessions", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil manager: status = %d, want 503", rec.Code)
	}
}

// TestAttachSessionsOnlyLiveAttachRows pins the core filter: only Kind=="attach"
// rows that have NOT exited are returned; handoff / fresh / kindless rows and an
// exited attach row are excluded.
func TestAttachSessionsOnlyLiveAttachRows(t *testing.T) {
	lm := newRecordingLaunchManager(nil)
	lm.snapshot = []LaunchInfo{
		{ID: "attach-live", Kind: "attach", Tool: "claude-code", Subcommand: "claude"},
		{ID: "attach-exited", Kind: "attach", Tool: "codex", Subcommand: "codex", Exited: true, ExitCode: 0},
		{ID: "handoff-1", Kind: "handoff", Subcommand: "codex", SessionID: "sess-h"},
		{ID: "fresh-1", Kind: "fresh", Subcommand: "claude"},
		{ID: "legacy-nokind", Subcommand: "claude"}, // predates run-identity wiring
	}
	s := newLaunchTestServer(t, lm)

	rows := getAttachSessions(t, s, false)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (only the live attach session): %+v", len(rows), rows)
	}
	got := rows[0]
	if got.ID != "attach-live" {
		t.Fatalf("returned row = %q, want attach-live", got.ID)
	}
	// The row is the LaunchInfo JSON verbatim — Kind + Tool ride the wire.
	if got.Kind != "attach" || got.Tool != "claude-code" || got.Subcommand != "claude" {
		t.Fatalf("row wire shape = %+v, want kind=attach tool=claude-code subcommand=claude", got)
	}
	// An exited attach + handoff/fresh/kindless rows never appear.
	byID := idsOf(rows)
	for _, absent := range []string{"attach-exited", "handoff-1", "fresh-1", "legacy-nokind"} {
		if _, ok := byID[absent]; ok {
			t.Errorf("row %q must not appear in the attach-sessions list", absent)
		}
	}
}

// TestAttachSessionsCarriesCorrelatedSessionID pins that a correlation-populated
// session_id flows through verbatim (the adapter fills it from SessionForRun
// before the row reaches the handler).
func TestAttachSessionsCarriesCorrelatedSessionID(t *testing.T) {
	lm := newRecordingLaunchManager(nil)
	lm.snapshot = []LaunchInfo{
		{ID: "attach-corr", Kind: "attach", Tool: "claude-code", Subcommand: "claude", SessionID: "sess-xyz", RunID: "run-1"},
	}
	s := newLaunchTestServer(t, lm)
	rows := getAttachSessions(t, s, false)
	if len(rows) != 1 || rows[0].SessionID != "sess-xyz" {
		t.Fatalf("rows = %+v, want one row with session_id=sess-xyz", rows)
	}
	if rows[0].RunID != "run-1" {
		t.Fatalf("run_id = %q, want run-1", rows[0].RunID)
	}
}

// TestAttachSessionsRemoteDenied pins the deny-by-default remote VIEW of attach
// sessions (§3.2, Phase-4 read gate pulled forward, P2-2): an owner-local caller
// sees every live attach row, but a REMOTE-exposed caller sees NONE — an
// external `observer <tool> --attach` PTY (which can echo secrets) is not even
// disclosed to a paired remote device until the Phase-4 [remote].allow_terminal_view
// opt-in exists. visibleSnapshot redacts attach rows on run KIND, so the deny is
// uniform across /api/attach/sessions, /api/terminal/sessions, /api/launch/sessions.
func TestAttachSessionsRemoteDenied(t *testing.T) {
	lm := newRecordingLaunchManager(nil)
	lm.snapshot = []LaunchInfo{
		{ID: "attach-a", Kind: "attach", Tool: "claude-code", Subcommand: "claude"},
		{ID: "attach-b", Kind: "attach", Tool: "codex", Subcommand: "codex"},
	}
	s := newLaunchTestServer(t, lm)

	// Owner-local caller sees both live attach rows.
	local := idsOf(getAttachSessions(t, s, false))
	if _, ok := local["attach-a"]; !ok {
		t.Error("local caller must see attach-a")
	}
	if _, ok := local["attach-b"]; !ok {
		t.Error("local caller must see attach-b")
	}

	// Remote-exposed caller: DENY — no attach row is disclosed at all.
	remote := getAttachSessions(t, s, true)
	if len(remote) != 0 {
		t.Fatalf("remote caller must see NO attach rows (deny-by-default §3.2), got %+v", remote)
	}
}

// TestAttachSessionsSetupStillRedactedRemotely keeps the pre-existing setup
// redaction pin honest under the new attach deny: a (contrived) non-attach setup
// row is still redacted from a remote caller while a non-attach regular row is
// still served — proving the attach deny is ADDITIVE to, not a replacement for,
// the setup redaction.
func TestAttachSessionsSetupStillRedactedRemotely(t *testing.T) {
	lm := newRecordingLaunchManager(nil)
	// These are terminal-session rows (not attach); assert via /api/terminal/sessions
	// through visibleSnapshot, which every remotely-visible snapshot shares.
	lm.snapshot = []LaunchInfo{
		{ID: "reg-1", Kind: "fresh", Subcommand: "claude"},
		{ID: "setup-1", Kind: "fresh", Subcommand: "claude", Setup: true},
	}
	s := newLaunchTestServer(t, lm)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/terminal/sessions", nil)
	req = req.WithContext(withRemoteExposed(req.Context()))
	s.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "reg-1") {
		t.Errorf("remote terminal snapshot must keep the regular row: %s", body)
	}
	if strings.Contains(body, "setup-1") {
		t.Errorf("remote terminal snapshot leaked a setup row: %s", body)
	}
}
