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

// TestHandleConfigSection_OrgAndOTel pins the P4.12 sections. The org
// case carries the arc's hardest invariant: a section save copies
// share/scope/cadence ONLY — the enrolment identity (enabled,
// org_server_url, keychain_id) written by `observer enroll` must
// survive a PUT whose body zeroes or omits those fields, so the
// dashboard can never detach or re-point an enrolment.
func TestHandleConfigSection_OrgAndOTel(t *testing.T) {
	tdir := t.TempDir()
	cfgPath := filepath.Join(tdir, "config.toml")
	seed := `[observer]
log_level = "info"

[org_client]
enabled = true
org_server_url = "https://org.example.com"
keychain_id = "sbo-org-default"
push_interval_seconds = 900
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

	// Body deliberately zeroes the enrolment fields (a real UI draft
	// would carry them — a hostile/buggy one might not; either way
	// they must not land).
	orgBody := `{"Enabled":false,"OrgServerURL":"","KeychainID":"",` +
		`"PushIntervalSeconds":300,"MaxPushBytes":2097152,` +
		`"Share":{"FullContent":true,"TargetActionAllowlist":["read_file"]},` +
		`"Scope":{"ProjectRootAllowlist":["/work/repo"],"ProjectRootDenylist":[]}}`
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPut, "/api/config/section/org", strings.NewReader(orgBody)))
	if rr.Code != 200 {
		t.Fatalf("PUT org: %d body=%s", rr.Code, rr.Body.String())
	}

	otelBody := `{"Enabled":true,"Endpoint":"collector:4318","Insecure":false,` +
		`"PollIntervalSeconds":5,"EmitPromptContent":false,"EmitUserEmail":true}`
	rr = httptest.NewRecorder()
	server.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPut, "/api/config/section/otel", strings.NewReader(otelBody)))
	if rr.Code != 200 {
		t.Fatalf("PUT otel: %d body=%s", rr.Code, rr.Body.String())
	}

	reloaded, err := config.Load(config.LoadOptions{GlobalPath: cfgPath})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	oc := reloaded.OrgClient
	// Enrolment identity survived the zeroing body.
	if !oc.Enabled || oc.OrgServerURL != "https://org.example.com" || oc.KeychainID != "sbo-org-default" {
		t.Errorf("enrolment fields must survive an org section save: %+v", oc)
	}
	// The knobs the section owns did land.
	if !oc.Share.FullContent || len(oc.Share.TargetActionAllowlist) != 1 {
		t.Errorf("share not persisted: %+v", oc.Share)
	}
	if len(oc.Scope.ProjectRootAllowlist) != 1 || oc.Scope.ProjectRootAllowlist[0] != "/work/repo" {
		t.Errorf("scope not persisted: %+v", oc.Scope)
	}
	if oc.PushIntervalSeconds != 300 || oc.MaxPushBytes != 2097152 {
		t.Errorf("cadence not persisted: interval=%d max=%d", oc.PushIntervalSeconds, oc.MaxPushBytes)
	}

	ot := reloaded.Exporter.OTel
	if !ot.Enabled || ot.Endpoint != "collector:4318" || ot.PollIntervalSeconds != 5 || !ot.EmitUserEmail {
		t.Errorf("otel not persisted: %+v", ot)
	}
	if ot.EmitPromptContent {
		t.Errorf("otel EmitPromptContent=false must persist false")
	}

	// editable_sections advertises both.
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
	if !have["org"] || !have["otel"] {
		t.Errorf("editable_sections missing org/otel: %v", got.EditableSections)
	}
}
