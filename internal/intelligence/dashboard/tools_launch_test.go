package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/launch"
)

// stubLaunch pins a deterministic environment and captures spawns so
// tests never open a real terminal window (the spawn seam sibling of
// the pokeReload / setupWizardHome patterns).
func stubLaunch(t *testing.T, env launch.Environment, spawnErr error) *[]launch.Spec {
	t.Helper()
	var spawned []launch.Spec
	prevDetect, prevSpawn := launchDetect, launchSpawn
	launchDetect = func() launch.Environment { return env }
	launchSpawn = func(_ context.Context, spec launch.Spec) error {
		spawned = append(spawned, spec)
		return spawnErr
	}
	t.Cleanup(func() { launchDetect, launchSpawn = prevDetect, prevSpawn })
	return &spawned
}

func postLaunch(t *testing.T, server *Server, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tools/launch", strings.NewReader(body))
	server.Handler().ServeHTTP(rr, req)
	var got map[string]any
	if rr.Code == http.StatusOK {
		if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
	}
	return rr, got
}

func TestToolsLaunchSpawnSuccess(t *testing.T) {
	server, _ := wizardTestServer(t)
	spawned := stubLaunch(t, launch.Environment{
		GOOS: "linux", IsWSL: true, WSLDistro: "Ubuntu",
		WTPath: "/mnt/c/wt.exe", CmdPath: "/mnt/c/cmd.exe", ExePath: "/opt/observer",
	}, nil)

	rr, got := postLaunch(t, server, `{"tool":"claude-code"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", rr.Code, rr.Body.String())
	}
	if got["spawned"] != true {
		t.Errorf("spawned: got %v", got["spawned"])
	}
	if got["command"] == "" {
		t.Error("command must always be present")
	}
	if got["method"] != "wsl-wt" {
		t.Errorf("method: got %v", got["method"])
	}
	if len(*spawned) != 1 {
		t.Fatalf("spawn calls: got %d want 1", len(*spawned))
	}
}

func TestToolsLaunchSpawnFailureIsHonest(t *testing.T) {
	server, _ := wizardTestServer(t)
	stubLaunch(t, launch.Environment{
		GOOS: "windows", WTPath: `C:\wt.exe`, CmdPath: `C:\cmd.exe`, ExePath: `C:\obs.exe`,
	}, errors.New("interop socket gone"))

	rr, got := postLaunch(t, server, `{"tool":"codex"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", rr.Code, rr.Body.String())
	}
	if got["spawned"] != false {
		t.Errorf("spawned must be false on spawn error, got %v", got["spawned"])
	}
	if d, _ := got["detail"].(string); !strings.Contains(d, "interop socket gone") {
		t.Errorf("detail must carry the spawn error, got %v", got["detail"])
	}
	if c, _ := got["command"].(string); c == "" {
		t.Error("command (copy-paste fallback) must survive a failed spawn")
	}
}

func TestToolsLaunchHeadlessNoSpawnAttempt(t *testing.T) {
	server, _ := wizardTestServer(t)
	spawned := stubLaunch(t, launch.Environment{GOOS: "linux", ExePath: "/opt/observer"}, nil)

	rr, got := postLaunch(t, server, `{"tool":"claude-code"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", rr.Code, rr.Body.String())
	}
	if got["spawned"] != false || got["method"] != "none" {
		t.Errorf("headless: spawned=%v method=%v", got["spawned"], got["method"])
	}
	if d, _ := got["detail"].(string); d == "" {
		t.Error("headless response must explain why nothing spawned")
	}
	if len(*spawned) != 0 {
		t.Errorf("headless must not attempt a spawn, got %d", len(*spawned))
	}
}

func TestToolsLaunchGuards(t *testing.T) {
	server, _ := wizardTestServer(t)
	spawned := stubLaunch(t, launch.Environment{GOOS: "windows", WTPath: "wt", ExePath: "obs"}, nil)

	rr, _ := postLaunch(t, server, `{"tool":"cursor"}`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("unlisted tool: got %d want 400", rr.Code)
	}
	rr, _ = postLaunch(t, server, `{not json`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("bad body: got %d want 400", rr.Code)
	}
	rr = httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/tools/launch", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: got %d want 405", rr.Code)
	}
	if len(*spawned) != 0 {
		t.Errorf("guard paths must never spawn, got %d", len(*spawned))
	}
}
