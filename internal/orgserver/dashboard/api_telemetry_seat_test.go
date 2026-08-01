package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	gen "github.com/marmutapp/superbased-observer/internal/orgserver/dashboard/gen"
	"github.com/marmutapp/superbased-observer/internal/orgserver/rollup"
)

// TestOrgTelemetrySeatPricingReachesJSON walks the WHOLE seat-pricing path —
// Options → API → rollup → serialized response — rather than calling
// rollup.Telemetry directly.
//
// The rollup-level test proves the arithmetic; this one proves that a price
// set on Options actually reaches the serialized response.
//
// SCOPE, precisely: it injects the price into Options, so it does NOT cover
// the Config → Options mapping in server.go — deleting that line leaves this
// test green. That seam has its own assertion in
// orgserver.TestDashboardOptionsCarriesCopilotSeatPrice, and the two together
// are what close the gap that let per_seat_price_usd sit configured-but-unread.
func TestOrgTelemetrySeatPricingReachesJSON(t *testing.T) {
	api := newAPIWithData(t)
	seedCopilotSeats(t, api)

	const price = 19.0
	priced := NewAPI(api.db, rollup.NewCache(0), Options{
		AdminEmails:            []string{"boss@acme.example"},
		CopilotPerSeatPriceUSD: price,
	}, nil)

	body := telemetryJSON(t, priced, "u-boss")
	seats := copilotSeats(t, body)

	if got := seats["monthly_usd"]; got != 50*price {
		t.Errorf("monthly_usd = %v, want %v (50 seats × $%v) — the configured price did not reach the handler",
			got, 50*price, price)
	}
	if got := seats["per_seat_price_usd"]; got != price {
		t.Errorf("per_seat_price_usd = %v, want %v", got, price)
	}
	// The unit trap, asserted on the wire: the monthly subscription must not
	// have been folded into the additive metered cost, which is 4.00 here.
	if got := vendorField(t, body, "cost_usd"); got != 4.00 {
		t.Errorf("cost_usd = %v, want 4.00 — the $%v subscription must not be summed into metered spend",
			got, 50*price)
	}

	// Unconfigured price: the same request must omit the priced keys entirely
	// rather than serialize them as 0.
	unpriced := NewAPI(api.db, rollup.NewCache(0), Options{
		AdminEmails: []string{"boss@acme.example"},
	}, nil)
	raw := telemetryRaw(t, unpriced, "u-boss")
	for _, key := range []string{"monthly_usd", "per_seat_price_usd"} {
		if strings.Contains(raw, key) {
			t.Errorf("unconfigured price still emits %q on the wire: %s", key, raw)
		}
	}
	if !strings.Contains(raw, `"total":50`) {
		t.Errorf("seat COUNTS must survive an unconfigured price: %s", raw)
	}
}

// seedCopilotSeats inserts one org-aggregate seat snapshot plus a metered
// overage, so the response carries both of Copilot's cost feeds at once.
func seedCopilotSeats(t *testing.T, api *API) {
	t.Helper()
	// One day captured ONCE and bound to every row. Evaluating date('now') per
	// INSERT lets a UTC midnight land mid-seed, after which
	// telemetryCopilotSeats reads only MAX(day) and gets a partial snapshot
	// (active/inactive but no total) — an intermittent failure that would look
	// like a real regression.
	day := time.Now().UTC().Format("2006-01-02")
	rows := [][]any{
		{"seats", "seats", "seats_total", 50.0},
		{"seats", "seats", "seats_active", 40.0},
		{"seats", "seats", "seats_inactive", 10.0},
		{"billing", "usd", "cost", 4.00},
	}
	for _, r := range rows {
		if _, err := api.db.Exec(
			`INSERT INTO copilot_analytics_daily (day, user_key, actor_type, surface, unit, metric, value, org_id, owner, pulled_at)
			 VALUES (?, '__org__', 'org', ?, ?, ?, ?, 'org1', 'acme', '2026-05-26T04:00:00Z')`,
			day, r[0], r[1], r[2], r[3],
		); err != nil {
			t.Fatalf("seed copilot %v: %v", r[2], err)
		}
	}
}

func telemetryRaw(t *testing.T, api *API, userID string) string {
	t.Helper()
	w := do(userID, http.MethodGet, "/api/org/telemetry", nil, func(rw http.ResponseWriter, r *http.Request) {
		api.OrgTelemetry(rw, r, gen.OrgTelemetryParams{})
	})
	if w.Code != http.StatusOK {
		t.Fatalf("OrgTelemetry status = %d, want 200: %s", w.Code, w.Body.String())
	}
	return w.Body.String()
}

func telemetryJSON(t *testing.T, api *API, userID string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(telemetryRaw(t, api, userID)), &out); err != nil {
		t.Fatalf("unmarshal telemetry: %v", err)
	}
	return out
}

// copilotVendor returns the copilot vendor object from a telemetry response.
func copilotVendor(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	vendors, _ := body["vendors"].([]any)
	for _, v := range vendors {
		m, ok := v.(map[string]any)
		if ok && m["vendor"] == "copilot" {
			return m
		}
	}
	t.Fatalf("copilot vendor absent from telemetry response: %+v", body)
	return nil
}

func copilotSeats(t *testing.T, body map[string]any) map[string]float64 {
	t.Helper()
	seats, ok := copilotVendor(t, body)["seats"].(map[string]any)
	if !ok {
		t.Fatalf("copilot seats absent: %+v", body)
	}
	out := map[string]float64{}
	for k, v := range seats {
		if f, ok := v.(float64); ok {
			out[k] = f
		}
	}
	return out
}

func vendorField(t *testing.T, body map[string]any, field string) float64 {
	t.Helper()
	f, ok := copilotVendor(t, body)[field].(float64)
	if !ok {
		t.Fatalf("copilot %s absent or not numeric: %+v", field, body)
	}
	return f
}
