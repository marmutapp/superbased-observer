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

// TestAttachSessionsLiveDaemonOwnedRows pins the core filter: any LIVE,
// non-setup daemon-owned PTY row of a VALID terminal_run kind (fresh / handoff /
// attach / resume) is joinable and returned; an exited row, a local-only setup
// row, and a kindless (legacy) row are all excluded. The test is a table so the
// per-row include/exclude decision is one case each.
func TestAttachSessionsLiveDaemonOwnedRows(t *testing.T) {
	rows := []struct {
		row  LaunchInfo
		want bool // expected to appear in /api/attach/sessions
	}{
		{LaunchInfo{ID: "attach-live", Kind: "attach", Tool: "claude-code", Subcommand: "claude"}, true},
		{LaunchInfo{ID: "fresh-live", Kind: "fresh", Subcommand: "claude"}, true},
		{LaunchInfo{ID: "handoff-live", Kind: "handoff", Subcommand: "codex", SessionID: "sess-h"}, true},
		{LaunchInfo{ID: "resume-live", Kind: "resume", Tool: "codex", Subcommand: "codex", SessionID: "sess-r"}, true},
		{LaunchInfo{ID: "attach-exited", Kind: "attach", Tool: "codex", Subcommand: "codex", Exited: true, ExitCode: 0}, false},
		{LaunchInfo{ID: "setup-live", Kind: "fresh", Subcommand: "claude", Setup: true}, false},
		{LaunchInfo{ID: "legacy-nokind", Subcommand: "claude"}, false}, // predates run-identity wiring
	}

	lm := newRecordingLaunchManager(nil)
	for _, tc := range rows {
		lm.snapshot = append(lm.snapshot, tc.row)
	}
	s := newLaunchTestServer(t, lm)

	byID := idsOf(getAttachSessions(t, s, false))
	for _, tc := range rows {
		_, present := byID[tc.row.ID]
		if present != tc.want {
			t.Errorf("row %q present=%v, want %v", tc.row.ID, present, tc.want)
		}
	}
	// The included rows are the LaunchInfo JSON verbatim — Kind + Tool ride the wire.
	if got, ok := byID["attach-live"]; ok {
		if got.Kind != "attach" || got.Tool != "claude-code" || got.Subcommand != "claude" {
			t.Fatalf("attach-live wire shape = %+v, want kind=attach tool=claude-code subcommand=claude", got)
		}
	}
	if got, ok := byID["resume-live"]; ok {
		if got.Kind != "resume" || got.SessionID != "sess-r" {
			t.Fatalf("resume-live wire shape = %+v, want kind=resume session_id=sess-r", got)
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
		// A fresh run carries session_id only once correlation links it
		// (SessionForRun); the populated id must ride the wire verbatim too.
		{ID: "fresh-corr", Kind: "fresh", Tool: "claude-code", Subcommand: "claude", SessionID: "sess-fresh", RunID: "run-2"},
	}
	s := newLaunchTestServer(t, lm)
	byID := idsOf(getAttachSessions(t, s, false))
	attach, ok := byID["attach-corr"]
	if !ok || attach.SessionID != "sess-xyz" {
		t.Fatalf("attach-corr = %+v, want session_id=sess-xyz", attach)
	}
	if attach.RunID != "run-1" {
		t.Fatalf("attach-corr run_id = %q, want run-1", attach.RunID)
	}
	fresh, ok := byID["fresh-corr"]
	if !ok || fresh.SessionID != "sess-fresh" {
		t.Fatalf("fresh-corr = %+v, want session_id=sess-fresh", fresh)
	}
	if fresh.RunID != "run-2" {
		t.Fatalf("fresh-corr run_id = %q, want run-2", fresh.RunID)
	}
}

// TestAttachSessionsHandoffKeysByForkedSession pins the P2 fix: a handoff PTY's
// spec-set SessionID is the SOURCE session (the session forked FROM), but on
// /api/attach/sessions session_id must mean "the session this PTY is driving" —
// the FORKED session. When correlation has linked the run, the response row
// carries the FORK id, never the source id the snapshot stamped.
func TestAttachSessionsHandoffKeysByForkedSession(t *testing.T) {
	lm := newRecordingLaunchManager(nil)
	// Snapshot stamps the SOURCE session id (sess-source) as the spec set it at
	// spawn; correlation has since linked run run-h to the forked session.
	lm.snapshot = []LaunchInfo{
		{ID: "handoff-linked", Kind: "handoff", Subcommand: "codex", SessionID: "sess-source", RunID: "run-h"},
	}
	lm.sessionForRun = map[string]string{"run-h": "sess-fork"}
	s := newLaunchTestServer(t, lm)

	byID := idsOf(getAttachSessions(t, s, false))
	got, ok := byID["handoff-linked"]
	if !ok {
		t.Fatalf("handoff-linked must be listed, got %+v", byID)
	}
	if got.SessionID != "sess-fork" {
		t.Fatalf("handoff row session_id = %q, want sess-fork (the FORK, not the source)", got.SessionID)
	}
}

// TestAttachSessionsHandoffPreLinkEmptySessionID pins that a handoff run with no
// correlation link yet is STILL listed but carries an EMPTY session_id — it
// matches no session-detail page (so the source's page never claims the fork's
// PTY) until the fork is known (~10–30s). The spec's source id must NOT leak.
func TestAttachSessionsHandoffPreLinkEmptySessionID(t *testing.T) {
	lm := newRecordingLaunchManager(nil)
	lm.snapshot = []LaunchInfo{
		{ID: "handoff-unlinked", Kind: "handoff", Subcommand: "codex", SessionID: "sess-source", RunID: "run-h"},
	}
	// sessionForRun left nil ⇒ no link for run-h.
	s := newLaunchTestServer(t, lm)

	byID := idsOf(getAttachSessions(t, s, false))
	got, ok := byID["handoff-unlinked"]
	if !ok {
		t.Fatalf("pre-link handoff row must still be listed, got %+v", byID)
	}
	if got.SessionID != "" {
		t.Fatalf("pre-link handoff row session_id = %q, want empty (source id must not leak)", got.SessionID)
	}
}

// TestAttachSessionsRemoteDenied pins the remote-VIEW redaction of the
// remote-sensitive kinds (§3.2): with [remote].allow_terminal_view OFF (the
// nil-controller state of newLaunchTestServer), a REMOTE-exposed caller sees
// ONLY the non-sensitive floor — fresh + handoff dashboard terminals — while the
// attach + resume rows (whose TUI can echo secrets) are redacted by
// visibleSnapshot on run KIND. An owner-local caller still sees all four.
func TestAttachSessionsRemoteDenied(t *testing.T) {
	lm := newRecordingLaunchManager(nil)
	lm.snapshot = []LaunchInfo{
		{ID: "attach-a", Kind: "attach", Tool: "claude-code", Subcommand: "claude"},
		{ID: "resume-a", Kind: "resume", Tool: "codex", Subcommand: "codex", SessionID: "sess-r"},
		{ID: "fresh-a", Kind: "fresh", Subcommand: "claude"},
		{ID: "handoff-a", Kind: "handoff", Subcommand: "codex", SessionID: "sess-h"},
	}
	s := newLaunchTestServer(t, lm)

	// Owner-local caller sees every live row.
	local := idsOf(getAttachSessions(t, s, false))
	for _, id := range []string{"attach-a", "resume-a", "fresh-a", "handoff-a"} {
		if _, ok := local[id]; !ok {
			t.Errorf("local caller must see %q", id)
		}
	}

	// Remote-exposed caller with the view gate OFF: only the non-sensitive
	// fresh + handoff floor is disclosed; attach + resume are redacted.
	remote := idsOf(getAttachSessions(t, s, true))
	if len(remote) != 2 {
		t.Fatalf("remote caller must see exactly the fresh+handoff floor, got %+v", remote)
	}
	for _, id := range []string{"fresh-a", "handoff-a"} {
		if _, ok := remote[id]; !ok {
			t.Errorf("remote caller must see non-sensitive row %q", id)
		}
	}
	for _, id := range []string{"attach-a", "resume-a"} {
		if _, ok := remote[id]; ok {
			t.Errorf("remote caller must NOT see remote-sensitive row %q (view gate off)", id)
		}
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
