package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTerminalRunsHistoryEmpty(t *testing.T) {
	_, h := newManageServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/terminal/runs", nil)
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/terminal/runs = %d", rec.Code)
	}
	var resp struct {
		Runs []any `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Runs == nil {
		t.Error("runs must be a (possibly empty) array, never null")
	}
}
