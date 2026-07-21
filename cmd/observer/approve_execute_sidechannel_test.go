package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// TestApproveExecuteDoesNotExposeSecretsInArgvURLOrHistory pins §8.1 item 5 for
// the CLI leg of the execute-tier approval: `observer remote approve-execute`
// (and the HTTP it drives) never place the minted capability / confirm in a URL,
// query string, header, Referer, or process argv — the secrets cross the wire
// ONLY in the loopback response body and reach the operator ONLY on stdout.
//
// A recording fake daemon returns canary secrets in its approve-execute
// response; the test then canary-searches every outbound request the CLI made
// (path, query, all headers incl. Referer) and the process argv, and confirms
// the canaries surface ONLY on the command's stdout.
func TestApproveExecuteDoesNotExposeSecretsInArgvURLOrHistory(t *testing.T) {
	const (
		canaryCap     = "CANARY-CAPABILITY-7f3a91"
		canaryConfirm = "CANARY-CONFIRM-2b8e0d"
	)

	var (
		mu       sync.Mutex
		captured []string // one "METHOD URI :: header dump" line per request
	)
	record := func(r *http.Request) {
		var b strings.Builder
		b.WriteString(r.Method + " " + r.URL.RequestURI())
		b.WriteString(" Referer=" + r.Header.Get("Referer"))
		for k, vs := range r.Header {
			b.WriteString(" " + k + "=" + strings.Join(vs, ","))
		}
		mu.Lock()
		captured = append(captured, b.String())
		mu.Unlock()
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record(r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/remote/config":
			http.SetCookie(w, &http.Cookie{Name: remoteConfirmCookieName, Value: "confirm-cookie-val"})
			_ = json.NewEncoder(w).Encode(map[string]any{"confirm_token": "confirm-token-val"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/remote/approve-execute":
			// The ONLY place the secrets ever appear: the loopback response body.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":         true,
				"capability": canaryCap,
				"confirm":    canaryConfirm,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")

	var out bytes.Buffer
	cmd := newRemoteApproveExecuteCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	args := []string{"--addr", addr, "--device", "device-fp-abc", "--handle", "TERM-xyz"}
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("approve-execute command failed: %v", err)
	}

	// The secrets MUST reach the operator on stdout.
	stdout := out.String()
	if !strings.Contains(stdout, canaryCap) || !strings.Contains(stdout, canaryConfirm) {
		t.Fatalf("stdout is missing the minted secrets it must surface:\n%s", stdout)
	}

	// The secrets must be ABSENT from every outbound request (URL/query/header/
	// Referer) — no side channel.
	mu.Lock()
	defer mu.Unlock()
	if len(captured) < 2 {
		t.Fatalf("expected the CLI to make >=2 requests (config + approve), got %d", len(captured))
	}
	for _, line := range captured {
		for _, secret := range []string{canaryCap, canaryConfirm} {
			if strings.Contains(line, secret) {
				t.Errorf("a request leaked a secret in its URL/query/header/Referer: %q", line)
			}
		}
	}

	// The secrets must never appear in process argv (they arrive by response, not
	// flags) — neither in the command's own args nor in os.Args.
	for _, a := range append(append([]string{}, args...), os.Args...) {
		if strings.Contains(a, canaryCap) || strings.Contains(a, canaryConfirm) {
			t.Errorf("a secret appeared in argv: %q", a)
		}
	}
}
