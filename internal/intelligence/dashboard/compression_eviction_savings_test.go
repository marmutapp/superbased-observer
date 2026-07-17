package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// seedDropOnlyTurn inserts an api_turn whose entire apparent compression
// "saving" (original 50000 → compressed 100) is the result of a lossy
// drop, plus the matching compression_events drop row that evicted those
// bytes. It returns the day bucket string (YYYY-MM-DD) and the evicted
// byte count so callers can assert the eviction is NOT counted as saving.
func seedDropOnlyTurn(t *testing.T, s *Server, now time.Time) (string, int64) {
	t.Helper()
	const evicted int64 = 49900 // 50000 original - 100 compressed
	res, err := s.opts.DB.Exec(
		`INSERT INTO api_turns (session_id, timestamp, provider, model, input_tokens, output_tokens,
		    compression_original_bytes, compression_compressed_bytes, compression_dropped_count)
		 VALUES ('sDrop', ?, 'anthropic', 'claude-sonnet-4-6', 1000, 500, 50000, 100, 1)`,
		now.Format(time.RFC3339Nano),
	)
	if err != nil {
		t.Fatalf("seed api_turn: %v", err)
	}
	turnID, _ := res.LastInsertId()
	if _, err := s.opts.DB.Exec(
		`INSERT INTO compression_events (api_turn_id, timestamp, mechanism, original_bytes, compressed_bytes, msg_index, importance_score)
		 VALUES (?, ?, 'drop', ?, 0, 0, 0.1)`,
		turnID, now.Format(time.RFC3339Nano), evicted,
	); err != nil {
		t.Fatalf("seed drop event: %v", err)
	}
	return now.Format("2006-01-02"), evicted
}

// TestTimeseriesCostDropEvictionNotCountedAsSaving pins 2a: a day whose
// only compression activity is a lossy drop contributes 0 to the
// cost-timeseries compression savings (bytes/tokens/USD), and the
// evicted bytes surface additively in compression_evicted_bytes.
func TestTimeseriesCostDropEvictionNotCountedAsSaving(t *testing.T) {
	s, _ := newTestServer(t)
	now := time.Now().UTC()
	day, evicted := seedDropOnlyTurn(t, s, now)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/timeseries/cost?days=2&bucket=day", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Series []struct {
			Bucket           string  `json:"bucket"`
			CompBytesSaved   int64   `json:"compression_bytes_saved"`
			CompTokensSaved  int64   `json:"compression_tokens_saved_est"`
			CompCostUSDSaved float64 `json:"compression_cost_saved_usd_est"`
			CompEvicted      int64   `json:"compression_evicted_bytes"`
		} `json:"series"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, p := range got.Series {
		if p.Bucket != day {
			continue
		}
		found = true
		if p.CompBytesSaved != 0 {
			t.Errorf("compression_bytes_saved: got %d want 0 (drop is eviction, not saving)", p.CompBytesSaved)
		}
		if p.CompTokensSaved != 0 {
			t.Errorf("compression_tokens_saved_est: got %d want 0", p.CompTokensSaved)
		}
		if p.CompCostUSDSaved != 0 {
			t.Errorf("compression_cost_saved_usd_est: got %f want 0", p.CompCostUSDSaved)
		}
		if p.CompEvicted != evicted {
			t.Errorf("compression_evicted_bytes: got %d want %d", p.CompEvicted, evicted)
		}
	}
	if !found {
		t.Fatalf("no series point for day %q in %+v", day, got.Series)
	}
}

// TestReportMonthlyDropEvictionNotCountedAsSaving pins 2b: the monthly
// report's "compression trimmed" excludes lossy-evicted bytes and
// surfaces them additively as compression_evicted_bytes.
func TestReportMonthlyDropEvictionNotCountedAsSaving(t *testing.T) {
	s, _ := newTestServer(t)
	now := time.Now().UTC()
	_, evicted := seedDropOnlyTurn(t, s, now)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/report/monthly", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Savings struct {
			CompressionBytes   int64 `json:"compression_bytes"`
			CompressionTokens  int64 `json:"compression_tokens"`
			CompressionEvicted int64 `json:"compression_evicted_bytes"`
		} `json:"savings"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Savings.CompressionBytes != 0 {
		t.Errorf("savings.compression_bytes: got %d want 0 (drop is eviction, not saving)", got.Savings.CompressionBytes)
	}
	if got.Savings.CompressionTokens != 0 {
		t.Errorf("savings.compression_tokens: got %d want 0", got.Savings.CompressionTokens)
	}
	if got.Savings.CompressionEvicted != evicted {
		t.Errorf("savings.compression_evicted_bytes: got %d want %d", got.Savings.CompressionEvicted, evicted)
	}
}
