package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
)

// TestHandleConfigSection_Observability pins the [observability] toggle
// the Settings page writes (PART 5 #2 / the obs follow-up arc). The
// defect it closes: before this section existed the dashboard had no
// way to turn the obs subsystem on, so the only obs line a save ever
// produced was the zero-valued `enabled = false` the full re-marshal
// emits. The case must (a) persist Enabled=true, and (b) preserve the
// [observability.eval] sub-config (judge model / online sampling) —
// the same selective-copy invariant the org/intelligence cases carry,
// so a section save can never clobber the hand-edited judge config.
func TestHandleConfigSection_Observability(t *testing.T) {
	tdir := t.TempDir()
	cfgPath := filepath.Join(tdir, "config.toml")
	seed := `[observer]
log_level = "info"

[observability]
enabled = false

[observability.eval]
judge_model = "openrouter/anthropic/claude-3.5-sonnet"
online_sample_rate = 0.25
`
	if err := os.WriteFile(cfgPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(tdir, "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	server, err := New(Options{DB: database, ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}

	// The draft a real UI sends is the full Observability object. This
	// body sets Enabled=true and deliberately ZEROES Eval — a buggy or
	// stale form might omit/blank it, and the selective decode must keep
	// the file's eval config regardless.
	body := `{"Enabled":true,"Eval":{"JudgeModel":"","OnlineSampleRate":0,"OnlineScorers":null}}`
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPut, "/api/config/section/observability", strings.NewReader(body)))
	if rr.Code != 200 {
		t.Fatalf("PUT observability: %d body=%s", rr.Code, rr.Body.String())
	}

	reloaded, err := config.Load(config.LoadOptions{GlobalPath: cfgPath})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	ob := reloaded.Observability
	// The on/off gate the section owns landed.
	if !ob.Enabled {
		t.Errorf("observability.enabled must persist true, got %v", ob.Enabled)
	}
	// The eval sub-config survived the zeroing body (selective decode).
	if ob.Eval.JudgeModel != "openrouter/anthropic/claude-3.5-sonnet" {
		t.Errorf("eval.judge_model must survive a section save, got %q", ob.Eval.JudgeModel)
	}
	if ob.Eval.OnlineSampleRate != 0.25 {
		t.Errorf("eval.online_sample_rate must survive a section save, got %v", ob.Eval.OnlineSampleRate)
	}

	// A toggle back to false must also round-trip (no asymmetry).
	off := `{"Enabled":false}`
	rr = httptest.NewRecorder()
	server.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPut, "/api/config/section/observability", strings.NewReader(off)))
	if rr.Code != 200 {
		t.Fatalf("PUT observability off: %d body=%s", rr.Code, rr.Body.String())
	}
	reloaded, err = config.Load(config.LoadOptions{GlobalPath: cfgPath})
	if err != nil {
		t.Fatalf("reload off: %v", err)
	}
	if reloaded.Observability.Enabled {
		t.Errorf("observability.enabled must persist false after toggle-off")
	}
	if reloaded.Observability.Eval.JudgeModel != "openrouter/anthropic/claude-3.5-sonnet" {
		t.Errorf("eval.judge_model must survive the toggle-off too, got %q", reloaded.Observability.Eval.JudgeModel)
	}

	// editable_sections advertises the new section.
	rr = httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	var got struct {
		EditableSections []string `json:"editable_sections"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, s := range got.EditableSections {
		have[s] = true
	}
	if !have["observability"] {
		t.Errorf("editable_sections missing observability: %v", got.EditableSections)
	}
}
