package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/arena"
	"github.com/marmutapp/superbased-observer/internal/models"
)

func TestReadArenaPatchRequiresExactRegularArtifact(t *testing.T) {
	dir := t.TempDir()
	expected := filepath.Join(dir, "candidate.patch")
	if err := os.WriteFile(expected, []byte("diff body"), 0o600); err != nil {
		t.Fatal(err)
	}
	if body, err := readArenaPatch(expected, expected); err != nil || string(body) != "diff body" {
		t.Fatalf("regular patch = %q err=%v", body, err)
	}
	if _, err := readArenaPatch(filepath.Join(dir, "elsewhere.patch"), expected); err == nil {
		t.Fatal("DB path outside the expected run artifact was trusted")
	}
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(expected); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, expected); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := readArenaPatch(expected, expected); err == nil {
		t.Fatal("symlink patch artifact was served")
	}
}

func seedArenaActionRows(t *testing.T, s *Server, runID, projectRoot string) {
	t.Helper()
	st := s.remoteManageStore()
	if err := st.InsertArenaRun(context.Background(), &models.ArenaRun{
		ID: runID, ProjectRoot: projectRoot, BaseBranch: "main", BaseSHA: "base",
		Prompt: "p", JudgeTool: "claude-code", Status: models.ArenaRunStatusComplete,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertArenaCandidate(context.Background(), &models.ArenaCandidate{
		ID: runID + "-claude-code", RunID: runID, Tool: "claude-code",
		Status: models.ArenaCandidateStatusJudged, BranchName: "arena/" + runID + "/claude-code",
	}); err != nil {
		t.Fatal(err)
	}
}

func arenaActionRequest(t *testing.T, h http.Handler, ck *http.Cookie, token, runID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/arena/runs/"+runID+"/action/"+runID+"-claude-code", strings.NewReader(body))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(remoteConfirmHeader, token)
	req.AddCookie(ck)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestArenaActionRejectsUnknownStrategy(t *testing.T) {
	s, h := newManageServer(t)
	seedArenaActionRows(t, s, "strict-strategy", t.TempDir())
	var ck *http.Cookie
	var echoValue string
	ck, echoValue = getConfirm(t, h)
	rec := arenaActionRequest(t, h, ck, echoValue, "strict-strategy", `{"action":"keep","strategy":"typo"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "strategy must") {
		t.Fatalf("unknown strategy = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestArenaActionsSerializeByProject(t *testing.T) {
	s, h := newManageServer(t)
	projectRoot := t.TempDir()
	seedArenaActionRows(t, s, "serialized", projectRoot)
	var ck *http.Cookie
	var echoValue string
	ck, echoValue = getConfirm(t, h)
	arenaMu.Lock()
	arenaActionInFlight[projectRoot] = true
	arenaMu.Unlock()
	t.Cleanup(func() {
		arenaMu.Lock()
		delete(arenaActionInFlight, projectRoot)
		arenaMu.Unlock()
	})
	rec := arenaActionRequest(t, h, ck, echoValue, "serialized", `{"action":"discard"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already mutating") {
		t.Fatalf("concurrent action = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestArenaActionWaitsForTerminalRun(t *testing.T) {
	s, h := newManageServer(t)
	projectRoot := t.TempDir()
	seedArenaActionRows(t, s, "still-judging", projectRoot)
	if err := s.remoteManageStore().UpdateArenaRunStatus(context.Background(), "still-judging", models.ArenaRunStatusJudging); err != nil {
		t.Fatal(err)
	}
	ck, token := getConfirm(t, h)
	rec := arenaActionRequest(t, h, ck, token, "still-judging", `{"action":"discard"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "still active") {
		t.Fatalf("active-run action = %d: %s", rec.Code, rec.Body.String())
	}
	row, err := s.remoteManageStore().ArenaCandidate(context.Background(), "still-judging-claude-code")
	if err != nil || row == nil || row.Status != models.ArenaCandidateStatusJudged {
		t.Fatalf("active-run action mutated candidate: %+v err=%v", row, err)
	}
}

func TestArenaCreateRejectsOutOfRangeTimeoutBeforeMutation(t *testing.T) {
	s, h := newManageServer(t)
	ck, token := getConfirm(t, h)
	for _, tc := range []struct {
		name    string
		runID   string
		timeout int
	}{
		{name: "negative", runID: "bad-negative-timeout", timeout: -1},
		{name: "above max", runID: "bad-large-timeout", timeout: int(arena.MaxTimeout/time.Second) + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"run_id":%q,"timeout_sec":%d}`, tc.runID, tc.timeout)
			req := httptest.NewRequest(http.MethodPost, "/api/arena/runs", strings.NewReader(body))
			req.Host = "127.0.0.1:8080"
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(remoteConfirmHeader, token)
			req.AddCookie(ck)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "timeout_sec") {
				t.Fatalf("invalid timeout = %d: %s", rec.Code, rec.Body.String())
			}
			run, err := s.remoteManageStore().ArenaRun(context.Background(), tc.runID)
			if err != nil || run != nil {
				t.Fatalf("invalid timeout persisted run: %+v err=%v", run, err)
			}
		})
	}
}
