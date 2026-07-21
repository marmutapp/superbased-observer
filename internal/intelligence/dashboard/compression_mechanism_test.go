package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// TestMechanismIsLossy pins the lossy-eviction classification: `drop`
// evicts content (compressed_bytes == 0 by construction) and every
// compression mechanism that keeps a retrievable form is genuine
// compression.
func TestMechanismIsLossy(t *testing.T) {
	lossy := []string{"drop"}
	compressing := []string{
		"json", "code", "logs", "text", "diff", "html",
		"tools", "stash", "read_cache", "rolling_summary",
	}
	for _, m := range lossy {
		if !mechanismIsLossy(m) {
			t.Errorf("mechanismIsLossy(%q) = false, want true", m)
		}
	}
	for _, m := range compressing {
		if mechanismIsLossy(m) {
			t.Errorf("mechanismIsLossy(%q) = true, want false", m)
		}
	}
	if mechanismIsLossy("") {
		t.Errorf("mechanismIsLossy(\"\") = true, want false")
	}
}

// TestAPICompressionByModelLossy seeds one compressing (json) and one
// lossy-eviction (drop) event under the same model, then verifies the
// /api/compression/by-model rollup reports the drop row as EVICTED —
// no bytes saved, no dollars — while the json row keeps its real
// savings and priced $. Before the fix this endpoint rendered the drop
// row as "0 B compressed / 100% saved / $X saved (est)".
func TestAPICompressionByModelLossy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	st := store.New(database)
	now := time.Now().UTC()
	turn := models.APITurn{
		SessionID: "s1", Timestamp: now.Add(-time.Hour),
		Provider: models.ProviderAnthropic, Model: "claude-sonnet-4",
		InputTokens: 100, OutputTokens: 50,
		CompressionEvents: []models.CompressionEvent{
			{Mechanism: "json", Timestamp: now.Add(-time.Hour), OriginalBytes: 20000, CompressedBytes: 5000, MsgIndex: 3},
			{Mechanism: "drop", Timestamp: now.Add(-time.Hour), OriginalBytes: 10000, CompressedBytes: 0, MsgIndex: 5, ImportanceScore: 0.12},
		},
	}
	if _, err := st.InsertAPITurn(context.Background(), turn); err != nil {
		t.Fatalf("InsertAPITurn: %v", err)
	}

	srv, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/compression/by-model?days=30", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("/api/compression/by-model: %d body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	rows, _ := got["rows"].([]any)
	if len(rows) != 2 {
		t.Fatalf("rows: got %d want 2", len(rows))
	}
	byMech := map[string]map[string]any{}
	for _, r := range rows {
		m := r.(map[string]any)
		byMech[m["mechanism"].(string)] = m
	}

	drop := byMech["drop"]
	if drop == nil {
		t.Fatalf("missing drop row; rows=%v", rows)
	}
	if lossy, _ := drop["lossy"].(bool); !lossy {
		t.Errorf("drop lossy: got %v want true", drop["lossy"])
	}
	if int(drop["evicted_bytes"].(float64)) != 10000 {
		t.Errorf("drop evicted_bytes: got %v want 10000", drop["evicted_bytes"])
	}
	if int(drop["saved_bytes"].(float64)) != 0 {
		t.Errorf("drop saved_bytes: got %v want 0 (evicted, not saved)", drop["saved_bytes"])
	}
	if int(drop["saved_tokens_est"].(float64)) != 0 {
		t.Errorf("drop saved_tokens_est: got %v want 0", drop["saved_tokens_est"])
	}
	if usd, _ := drop["saved_usd_est"].(float64); usd != 0 {
		t.Errorf("drop saved_usd_est: got %v want 0 (evicted content is not priced)", usd)
	}

	jsonRow := byMech["json"]
	if jsonRow == nil {
		t.Fatalf("missing json row; rows=%v", rows)
	}
	if lossy, _ := jsonRow["lossy"].(bool); lossy {
		t.Errorf("json lossy: got true want false (json compresses)")
	}
	if int(jsonRow["saved_bytes"].(float64)) != 15000 {
		t.Errorf("json saved_bytes: got %v want 15000", jsonRow["saved_bytes"])
	}
	// claude-sonnet-4 baked-in pricing = $3 / 1M input. 15000/4 = 3750
	// tokens → $0.01125.
	wantJSONUSD := (15000.0 / 4) * 3.0 / 1_000_000
	if usd, _ := jsonRow["saved_usd_est"].(float64); !approxEqual(usd, wantJSONUSD, 1e-9) {
		t.Errorf("json saved_usd_est: got %v want %v", usd, wantJSONUSD)
	}
}
