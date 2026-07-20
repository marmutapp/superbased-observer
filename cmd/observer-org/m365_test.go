package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// the sentinel client secret used across the doctor tests: no output or returned
// error may ever contain it.
const m365Secret = "SUPER-SECRET-CLIENT-VALUE"

// writeM365Config writes a config with the given [m365_copilot] body appended to
// a minimal (validation-irrelevant for doctor) base and returns its path.
func writeM365Config(t *testing.T, m365Body string) string {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	body := "[server]\n" +
		"external_url = \"https://org.example\"\n" +
		"db_path = \"" + filepath.ToSlash(filepath.Join(dir, "server.db")) + "\"\n" +
		m365Body
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

// tokenServer returns an httptest server that plays the Entra token endpoint:
// invalid_client (401) when fail is set, else a valid access_token.
func tokenServer(t *testing.T, fail bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"bad secret"}`))
			return
		}
		_, _ = w.Write([]byte(`{"token_type":"Bearer","expires_in":3600,"access_token":"probe.token"}`))
	}))
}

// graphServer returns an httptest server that plays getAllEnterpriseInteractions
// with a single interaction on the first page.
func graphServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"value":[{"id":"i1"}]}`))
	}))
}

func assertNoSecret(t *testing.T, out string, err error) {
	t.Helper()
	if strings.Contains(out, m365Secret) {
		t.Fatalf("client secret leaked in doctor output:\n%s", out)
	}
	if err != nil && strings.Contains(err.Error(), m365Secret) {
		t.Fatalf("client secret leaked in returned error: %v", err)
	}
}

func TestM365Doctor_Disabled(t *testing.T) {
	cfg := writeM365Config(t, "[m365_copilot]\nenabled = false\n")
	out, err := runCmd(t, "m365", "doctor", "--config", cfg)
	if err == nil {
		t.Fatal("expected non-zero exit when the rail is disabled")
	}
	if !strings.Contains(out, "disabled") || !strings.Contains(out, "enabled = true") {
		t.Errorf("disabled message missing/unclear:\n%s", out)
	}
	assertNoSecret(t, out, err)
}

func TestM365Doctor_IncompleteConfig(t *testing.T) {
	// Enabled but tenant_id / client_id / secret all absent (env unset).
	t.Setenv("M365_COPILOT_CLIENT_SECRET", "")
	cfg := writeM365Config(t, "[m365_copilot]\nenabled = true\n")
	out, err := runCmd(t, "m365", "doctor", "--config", cfg)
	if err == nil {
		t.Fatal("expected non-zero exit for incomplete config")
	}
	for _, want := range []string{"configuration incomplete", "tenant_id", "client_id", "client secret", "M365_COPILOT_CLIENT_SECRET"} {
		if !strings.Contains(out, want) {
			t.Errorf("incomplete message missing %q:\n%s", want, out)
		}
	}
	assertNoSecret(t, out, err)
}

func TestM365Doctor_InvalidClient(t *testing.T) {
	tok := tokenServer(t, true)
	defer tok.Close()
	t.Setenv("M365_COPILOT_CLIENT_SECRET", m365Secret)
	cfg := writeM365Config(t, "[m365_copilot]\n"+
		"enabled = true\n"+
		"tenant_id = \"tenant-x\"\n"+
		"client_id = \"client-y\"\n"+
		"login_base_url = \""+tok.URL+"\"\n")

	out, err := runCmd(t, "m365", "doctor", "--config", cfg, "--user", "dev@acme.com")
	if err == nil {
		t.Fatal("expected non-zero exit on invalid_client")
	}
	if !strings.Contains(out, "Entra token request failed") || !strings.Contains(out, "invalid_client") {
		t.Errorf("token-failure message missing the machine code:\n%s", out)
	}
	if !strings.Contains(out, "regenerate the client secret") {
		t.Errorf("token-failure fix hint missing:\n%s", out)
	}
	assertNoSecret(t, out, err)
}

func TestM365Doctor_Success(t *testing.T) {
	tok := tokenServer(t, false)
	defer tok.Close()
	graph := graphServer(t)
	defer graph.Close()
	t.Setenv("M365_COPILOT_CLIENT_SECRET", m365Secret)
	cfg := writeM365Config(t, "[m365_copilot]\n"+
		"enabled = true\n"+
		"tenant_id = \"tenant-x\"\n"+
		"client_id = \"client-y\"\n"+
		"surfaces = [\"graph\", \"purview\"]\n"+
		"login_base_url = \""+tok.URL+"\"\n"+
		"graph_base_url = \""+graph.URL+"\"\n")

	out, err := runCmd(t, "m365", "doctor", "--config", cfg, "--user", "dev@acme.com")
	if err != nil {
		t.Fatalf("expected success, got err=%v\n%s", err, out)
	}
	if !strings.Contains(out, "token acquired") {
		t.Errorf("success output missing token line:\n%s", out)
	}
	if !strings.Contains(out, "reached Graph") || !strings.Contains(out, "1 interaction(s)") {
		t.Errorf("success output missing Graph probe line:\n%s", out)
	}
	if !strings.Contains(out, "purview") || !strings.Contains(out, "scaffolded") {
		t.Errorf("success output missing purview scaffold note:\n%s", out)
	}
	assertNoSecret(t, out, err)
}
