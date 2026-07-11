package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
)

// TestHandleConfigSection_Guard pins the G1.1 guard section (security-
// routing usability arc). The hard invariants:
//
//  1. [guard.cloud] survives a PUT whose body zeroes or omits it —
//     network egress stays a hand-written config decision (D1), the
//     dashboard must never be able to flip it.
//  2. Rules.OrgBundle (org-client-owned) and Rules.CEL (v2 gate)
//     survive likewise.
//  3. Boundary lists: an empty list in the body preserves the prior
//     value (nil = engine defaults; explicit "none" is config-file
//     territory).
//  4. Closed enums (mode / egress_action / min_severity) are rejected
//     with 400 at the PUT, not at the next daemon start.
func TestHandleConfigSection_Guard(t *testing.T) {
	tdir := t.TempDir()
	cfgPath := filepath.Join(tdir, "config.toml")
	seed := `[observer]
log_level = "info"

[guard]
enabled = true
mode = "observe"

[guard.rules]
org_bundle = "/custom/org-bundle.json"

[guard.boundary]
allow_paths = ["../sibling/**"]

[guard.cloud]
enabled = true
payload_max_bytes = 2048

[guard.cloud.llm_judge]
enabled = true
endpoint = "http://localhost:9999/v1/chat/completions"
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

	// Body deliberately zeroes cloud + org_bundle + boundary (a real UI
	// draft never carries them — a hostile/buggy one might send zeros;
	// either way they must not land).
	body := `{"Enabled":true,"Mode":"enforce","Strict":false,"RetentionDays":180,` +
		`"Rules":{"Disable":["R-151"],"UserPolicy":"","ProjectPolicy":"","OrgBundle":"","CEL":false},` +
		`"Boundary":{"AllowPaths":[],"ProtectedBranches":[]},` +
		`"Taint":{"Enabled":true,"DecayTurns":12},` +
		`"Proxy":{"EgressScan":true,"EgressAction":"mask","EgressAllow":["TESTKEY-[0-9]+"],"ResponseScan":true,"InjectionHeuristics":true},` +
		`"MCP":{"Pinning":true,"PoisoningHeuristics":false},` +
		`"Budget":{"SessionUSD":5,"DailyUSD":40,"Hard":false},` +
		`"Alerts":{"Desktop":true,"MinSeverity":"warn"},` +
		`"Export":{"OTel":false},` +
		`"Dialects":{"Compile":true,"Targets":["claude-code"]},` +
		`"Cloud":{"Enabled":false,"PayloadMaxBytes":0}}`
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPut, "/api/config/section/guard", strings.NewReader(body)))
	if rr.Code != 200 {
		t.Fatalf("PUT guard: %d body=%s", rr.Code, rr.Body.String())
	}
	var saved struct {
		RestartRequired bool `json:"restart_required"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&saved); err != nil {
		t.Fatal(err)
	}
	if !saved.RestartRequired {
		t.Errorf("guard saves must report restart_required=true (no hot-reload seam)")
	}

	reloaded, err := config.Load(config.LoadOptions{GlobalPath: cfgPath})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	g := reloaded.Guard

	// 1. Cloud survived the zeroing body.
	if !g.Cloud.Enabled || g.Cloud.PayloadMaxBytes != 2048 || !g.Cloud.LLMJudge.Enabled {
		t.Errorf("cloud opt-in must survive a guard section save: %+v", g.Cloud)
	}
	// 2. Org bundle path survived.
	if g.Rules.OrgBundle != "/custom/org-bundle.json" {
		t.Errorf("org_bundle must survive: %q", g.Rules.OrgBundle)
	}
	// 3. Boundary allowlist survived the empty-list body.
	if len(g.Boundary.AllowPaths) != 1 || g.Boundary.AllowPaths[0] != "../sibling/**" {
		t.Errorf("boundary allow_paths must survive an empty-list save: %+v", g.Boundary.AllowPaths)
	}
	// The knobs the section owns did land.
	if g.Mode != "enforce" || g.RetentionDays != 180 {
		t.Errorf("mode/retention not persisted: mode=%q retention=%d", g.Mode, g.RetentionDays)
	}
	if len(g.Rules.Disable) != 1 || g.Rules.Disable[0] != "R-151" {
		t.Errorf("rules.disable not persisted: %+v", g.Rules.Disable)
	}
	if g.Taint.DecayTurns != 12 {
		t.Errorf("taint.decay_turns not persisted: %d", g.Taint.DecayTurns)
	}
	if len(g.Proxy.EgressAllow) != 1 || g.Alerts.MinSeverity != "warn" {
		t.Errorf("proxy/alerts not persisted: %+v %q", g.Proxy.EgressAllow, g.Alerts.MinSeverity)
	}
	if g.Budget.SessionUSD != 5 || g.Budget.DailyUSD != 40 {
		t.Errorf("budget not persisted: %+v", g.Budget)
	}

	// 4. Closed enums reject at the PUT.
	for _, bad := range []string{
		`{"Mode":"yolo","Proxy":{"EgressAction":"mask"},"Alerts":{"MinSeverity":"high"}}`,
		`{"Mode":"observe","Proxy":{"EgressAction":"shred"},"Alerts":{"MinSeverity":"high"}}`,
		`{"Mode":"observe","Proxy":{"EgressAction":"flag"},"Alerts":{"MinSeverity":"loud"}}`,
	} {
		rr = httptest.NewRecorder()
		server.Handler().ServeHTTP(rr,
			httptest.NewRequest(http.MethodPut, "/api/config/section/guard", strings.NewReader(bad)))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("invalid enum body must 400, got %d (%s)", rr.Code, bad)
		}
	}

	// editable_sections advertises guard.
	rr = httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	var got struct {
		EditableSections []string `json:"editable_sections"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range got.EditableSections {
		if s == "guard" {
			found = true
		}
	}
	if !found {
		t.Errorf("editable_sections must advertise guard: %v", got.EditableSections)
	}
}

// TestAPIGuardRulesEffective pins the G1.5 effective view: a custom
// user-layer rule resolves through /api/guard/rules?effective=1 with
// its source attributed, so the Security page's RuleCell can show its
// definition instead of degrading to a bare mono ID.
func TestAPIGuardRulesEffective(t *testing.T) {
	tdir := t.TempDir()
	policyPath := filepath.Join(tdir, "guard-policy.toml")
	userPolicy := `[[rule]]
id = "U-001"
category = "boundary"
severity = "high"
decision = "ask"
applies_to = ["shell_exec"]
match.command_base = "terraform"
match.arg_contains = "apply"
`
	if err := os.WriteFile(policyPath, []byte(userPolicy), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(tdir, "config.toml")
	seed := "[guard]\nenabled = true\nmode = \"observe\"\n\n[guard.rules]\nuser_policy = " +
		strconv.Quote(policyPath) + "\n"
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

	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/api/guard/rules?effective=1", nil))
	if rr.Code != 200 {
		t.Fatalf("GET effective rules: %d %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Rules []struct {
			ID     string `json:"id"`
			Doc    string `json:"doc"`
			Source string `json:"source"`
		} `json:"rules"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	var custom, builtins bool
	for _, r := range resp.Rules {
		if r.ID == "U-001" {
			custom = true
			if r.Source != "user" {
				t.Errorf("U-001 = %+v, want source=user", r)
			}
		}
		if r.ID == "R-101" {
			builtins = true
		}
	}
	if !custom {
		t.Errorf("effective view missing the user-layer rule U-001")
	}
	if !builtins {
		t.Errorf("effective view missing built-ins (R-101)")
	}
}
