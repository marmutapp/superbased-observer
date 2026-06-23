package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestScanRun pins the P4.13 full-rescan entry: POST /api/scan/run
// spawns `scan --force` through the shared job registry, the adapter
// filter is allow-listed against the injected tool catalog (nothing
// user-typed reaches argv), and the job is pollable like any backfill.
func TestScanRun(t *testing.T) {
	type sawArgs struct{ args []string }
	seen := make(chan sawArgs, 2)
	fake := func(ctx context.Context, args []string, onChunk func([]byte)) (int, error) {
		seen <- sawArgs{args: append([]string(nil), args...)}
		onChunk([]byte("rescan (from zero) complete: files_processed=3 errors=0\n"))
		return 0, nil
	}
	server := newServerWithFakeExec(t, fake)
	server.opts.ToolCatalog = []ToolCatalogEntry{{Tool: "codex"}, {Tool: "claude-code"}}

	post := func(t *testing.T, body string) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		rr := httptest.NewRecorder()
		server.Handler().ServeHTTP(rr,
			httptest.NewRequest(http.MethodPost, "/api/scan/run", strings.NewReader(body)))
		var got map[string]any
		if rr.Code == http.StatusOK {
			if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
		}
		return rr, got
	}

	t.Run("all adapters", func(t *testing.T) {
		rr, got := post(t, `{}`)
		if rr.Code != 200 {
			t.Fatalf("status %d body=%s", rr.Code, rr.Body.String())
		}
		if got["mode"] != "scan" {
			t.Errorf("mode: %v", got["mode"])
		}
		var args sawArgs
		select {
		case args = <-seen:
		case <-time.After(5 * time.Second):
			t.Fatal("subprocess never invoked")
		}
		if len(args.args) < 2 || args.args[0] != "scan" || args.args[1] != "--force" {
			t.Errorf("args: %v", args.args)
		}
		if slices.Contains(args.args, "--adapter") {
			t.Errorf("no-adapter run must not pass --adapter: %v", args.args)
		}
	})

	t.Run("adapter scoped", func(t *testing.T) {
		rr, got := post(t, `{"adapter":"codex"}`)
		if rr.Code != 200 {
			t.Fatalf("status %d body=%s", rr.Code, rr.Body.String())
		}
		if got["mode"] != "scan:codex" {
			t.Errorf("mode: %v", got["mode"])
		}
		var args sawArgs
		select {
		case args = <-seen:
		case <-time.After(5 * time.Second):
			t.Fatal("subprocess never invoked")
		}
		want := []string{"scan", "--force", "--adapter", "codex"}
		if len(args.args) < 4 || !slices.Equal(args.args[:4], want) {
			t.Errorf("args: got %v want prefix %v", args.args, want)
		}
	})

	t.Run("unknown adapter rejected", func(t *testing.T) {
		rr, _ := post(t, `{"adapter":"rm -rf /"}`)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("bogus adapter: got %d want 400", rr.Code)
		}
		select {
		case extra := <-seen:
			t.Errorf("rejected adapter must not spawn: %v", extra.args)
		case <-time.After(100 * time.Millisecond):
		}
	})

	t.Run("method guard", func(t *testing.T) {
		rr := httptest.NewRecorder()
		server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/scan/run", nil))
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET: got %d want 405", rr.Code)
		}
	})
}
