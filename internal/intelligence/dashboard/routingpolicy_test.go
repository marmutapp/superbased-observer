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

// newRoutingPolicyServer seeds an isolated config with one custom rule
// plus the secret-bearing file-only shapes whose survival the rules
// path must never threaten.
func newRoutingPolicyServer(t *testing.T) (*Server, string) {
	t.Helper()
	tdir := t.TempDir()
	cfgPath := filepath.Join(tdir, "config.toml")
	seed := `[routing]
enabled = true
mode = "advise"
policy = "value"

[routing.key_pool]
anthropic = ["sk-ant-prior-1"]

[[routing.rules]]
name = "pin-plan"
when.turn_kind = "plan"
action.pin_tier = "opus-class"
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
	return server, cfgPath
}

const cleanRulesFragment = `[[routing.rules]]
name = "cheap-reads"
when.turn_kind = "read_only"
action.route_to_tier = "haiku-class"
action.reason = "overpowered_read"

[[routing.rules]]
name = "exempt-secret-proj"
when.project = "secret-proj"
action.no_route = true
`

// TestHandleConfigSection_RoutingRulesTOML pins the R2.2 one-writer
// extension of the R1.1 routing case: RulesTOML absent = rules
// preserved wholesale (the original contract); RulesTOML present =
// replace, gated by parse + config.Load shape checks + error-severity
// compiler lint, with every refusal leaving the file untouched; an
// explicit empty string clears the rules. key_pool survives every arm.
func TestHandleConfigSection_RoutingRulesTOML(t *testing.T) {
	server, cfgPath := newRoutingPolicyServer(t)

	put := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		rr := httptest.NewRecorder()
		server.Handler().ServeHTTP(rr,
			httptest.NewRequest(http.MethodPut, "/api/config/section/routing", strings.NewReader(body)))
		return rr
	}
	baseKnobs := `"Enabled":true,"Mode":"advise","Policy":"value",` +
		`"Stickiness":{"RespectCache":true,"MinTurnsBetweenSwitches":5},` +
		`"RateLimitWindow":{"HeadroomPct":15},"Calibration":{"MinSamples":50}`
	reload := func() config.RoutingConfig {
		t.Helper()
		cfg, err := config.Load(config.LoadOptions{GlobalPath: cfgPath})
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		return cfg.Routing
	}

	// Replace via a clean fragment.
	body, _ := json.Marshal(cleanRulesFragment)
	rr := put(`{` + baseKnobs + `,"RulesTOML":` + string(body) + `}`)
	if rr.Code != 200 {
		t.Fatalf("rules replace: %d body=%s", rr.Code, rr.Body.String())
	}
	rc := reload()
	if len(rc.Rules) != 2 || rc.Rules[0].Name != "cheap-reads" || rc.Rules[1].Name != "exempt-secret-proj" {
		t.Fatalf("rules not replaced: %+v", rc.Rules)
	}
	if len(rc.KeyPool["anthropic"]) != 1 {
		t.Errorf("key_pool must survive a rules save: %+v", rc.KeyPool)
	}

	// Every refusal class: 400 + file untouched.
	for name, frag := range map[string]string{
		"toml_parse":      `not [[ toml`,
		"stray_key":       "[routing]\nenabled = false\n\n[[routing.rules]]\nname = \"r\"\nwhen.turn_kind = \"read_only\"\naction.route_to_tier = \"haiku-class\"\n",
		"wrong_shape":     "[[rules]]\nname = \"r\"\n",
		"load_validation": "[[routing.rules]]\nname = \"r\"\nwhen.turn_kind = \"read_only\"\naction.set_effort = \"turbo\"\n",
		"lint_error":      "[[routing.rules]]\nname = \"r\"\nwhen.turn_kind = \"read_only\"\naction.route_to_tier = \"super-class\"\n",
	} {
		b, _ := json.Marshal(frag)
		rr = put(`{` + baseKnobs + `,"RulesTOML":` + string(b) + `}`)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: code = %d, want 400 (%s)", name, rr.Code, rr.Body.String())
		}
		if got := reload(); len(got.Rules) != 2 || got.Rules[0].Name != "cheap-reads" {
			t.Fatalf("%s: refused save mutated the file: %+v", name, got.Rules)
		}
	}

	// RulesTOML absent (JSON Rules zeroing included) = preserved
	// wholesale — the R1.1 contract restated with rules present.
	rr = put(`{` + baseKnobs + `,"Rules":[]}`)
	if rr.Code != 200 {
		t.Fatalf("knob save: %d body=%s", rr.Code, rr.Body.String())
	}
	if got := reload(); len(got.Rules) != 2 {
		t.Fatalf("absent RulesTOML must preserve rules: %+v", got.Rules)
	}

	// Explicit empty string clears all custom rules.
	rr = put(`{` + baseKnobs + `,"RulesTOML":""}`)
	if rr.Code != 200 {
		t.Fatalf("rules clear: %d body=%s", rr.Code, rr.Body.String())
	}
	if got := reload(); len(got.Rules) != 0 {
		t.Errorf("empty RulesTOML must clear rules: %+v", got.Rules)
	}
}

// TestAPIRoutingPolicy pins the editor's read + validate surfaces: the
// GET payload round-trips the on-disk rules in the encoder dialect,
// and the lint endpoint returns ok/problems/lint without ever writing.
func TestAPIRoutingPolicy(t *testing.T) {
	server, _ := newRoutingPolicyServer(t)

	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/routing/policy", nil))
	if rr.Code != 200 {
		t.Fatalf("GET policy: %d body=%s", rr.Code, rr.Body.String())
	}
	var view struct {
		RulesTOML string `json:"rules_toml"`
		Rules     int    `json:"rules"`
		Policy    string `json:"policy"`
		Note      string `json:"note"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Rules != 1 || !strings.Contains(view.RulesTOML, "pin-plan") || !strings.Contains(view.RulesTOML, "[[routing.rules]]") {
		t.Errorf("policy view = %+v", view)
	}
	if view.Note == "" {
		t.Error("honesty note missing")
	}
	// The served TOML must itself pass the gate (round-trip fidelity).
	rules, problems := parseRoutingRulesTOML(view.RulesTOML)
	if len(problems) != 0 || len(rules) != 1 {
		t.Errorf("served rules_toml does not re-parse cleanly: %v %v", rules, problems)
	}

	lint := func(frag string) (code int, out struct {
		OK       bool     `json:"ok"`
		Problems []string `json:"problems"`
		Rules    int      `json:"rules"`
		Lint     []any    `json:"lint"`
	},
	) {
		t.Helper()
		b, _ := json.Marshal(map[string]string{"rules_toml": frag})
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec,
			httptest.NewRequest(http.MethodPost, "/api/routing/policy/lint", strings.NewReader(string(b))))
		if rec.Code == 200 {
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
		}
		return rec.Code, out
	}

	code, ok := lint(cleanRulesFragment)
	if code != 200 || !ok.OK || ok.Rules != 2 || len(ok.Problems) != 0 {
		t.Errorf("clean fragment: code=%d %+v", code, ok)
	}
	code, bad := lint("not [[ toml")
	if code != 200 || bad.OK || len(bad.Problems) == 0 {
		t.Errorf("parse error must be ok=false with problems: code=%d %+v", code, bad)
	}
	_, stray := lint("[routing]\nenabled = false\n")
	if stray.OK || len(stray.Problems) == 0 {
		t.Errorf("stray keys must be ok=false: %+v", stray)
	}
	_, tier := lint("[[routing.rules]]\nname = \"r\"\nwhen.turn_kind = \"read_only\"\naction.route_to_tier = \"super-class\"\n")
	if tier.OK || len(tier.Problems) == 0 {
		t.Errorf("lint-error fragment must be ok=false: %+v", tier)
	}
	// Empty fragment = "clear all rules" — valid.
	_, empty := lint("")
	if !empty.OK || empty.Rules != 0 {
		t.Errorf("empty fragment must be ok=true rules=0: %+v", empty)
	}

	// Method discipline.
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/routing/policy", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT policy = %d, want 405 (the write rides the config section seam)", rec.Code)
	}
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/routing/policy/lint", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET lint = %d, want 405", rec.Code)
	}
}
