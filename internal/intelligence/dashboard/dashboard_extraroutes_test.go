package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/db"
)

// TestExtraRoutes_RegisteredIntoMux proves the D4 seam: a separable subsystem
// (e.g. internal/obs's trajectory API) registers its own /api/* handlers into
// the dashboard's shared mux via Options.ExtraRoutes, WITHOUT the dashboard
// importing that subsystem. Here a generic stand-in route is reachable and a
// built-in route still works alongside it.
func TestExtraRoutes_RegisteredIntoMux(t *testing.T) {
	database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(t.TempDir(), "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	s, err := New(Options{
		DB: database,
		ExtraRoutes: []ExtraRoute{{
			Pattern: "GET /api/obs/enabled",
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"enabled":true}`))
			},
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := s.Handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/obs/enabled", nil))
	if rr.Code != http.StatusOK || rr.Body.String() != `{"enabled":true}` {
		t.Errorf("extra route = %d %q, want 200 {\"enabled\":true}", rr.Code, rr.Body.String())
	}

	// A built-in route still resolves (no collision from the extra registration).
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rr2.Code != http.StatusOK {
		t.Errorf("/api/status = %d, want 200 (built-in route intact)", rr2.Code)
	}
}
