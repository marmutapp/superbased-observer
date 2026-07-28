package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/diag"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/pidbridge"
	"github.com/marmutapp/superbased-observer/internal/processobs"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// seedDB opens a fresh DB and inserts one session + one action + one
// api_turn with compression stats. The returned time is the scrape's
// "now" — it sits inside the default 5m window so the cost rollup
// picks up the turn.
func seedDB(t *testing.T) (*sql.DB, string, time.Time) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "m.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	now := time.Date(2026, 4, 17, 10, 0, 0, 0, time.UTC)
	base := now.Add(-2 * time.Minute) // inside the 5m window
	root := t.TempDir()

	st := store.New(database)
	if _, err := st.Ingest(context.Background(), []models.ToolEvent{{
		SourceFile: "f", SourceEventID: "e1", SessionID: "sA",
		ProjectRoot: root, Timestamp: base, Tool: models.ToolClaudeCode,
		ActionType: models.ActionReadFile, Target: "a.go", Success: true,
	}}, nil, store.IngestOptions{}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	turn := models.APITurn{
		SessionID:                  "sA",
		Timestamp:                  base,
		Provider:                   models.ProviderAnthropic,
		Model:                      "claude-sonnet-4",
		InputTokens:                100,
		OutputTokens:               50,
		CacheReadTokens:            20,
		CacheCreationTokens:        10,
		CostUSD:                    0.0042,
		CompressionOriginalBytes:   10_000,
		CompressionCompressedBytes: 3_000,
		CompressionCount:           2,
		CompressionDroppedCount:    1,
		CompressionMarkerCount:     1,
	}
	if _, err := st.InsertAPITurn(context.Background(), turn); err != nil {
		t.Fatalf("InsertAPITurn: %v", err)
	}

	bridge := pidbridge.New(database)
	if err := bridge.Write(context.Background(), pidbridge.Entry{
		PID: 1234, SessionID: "sA", Tool: "claude-code", CWD: "/tmp",
	}); err != nil {
		t.Fatalf("bridge.Write: %v", err)
	}
	return database, path, now
}

func TestRender_EmitsCoreFamilies(t *testing.T) {
	database, path, now := seedDB(t)
	var out strings.Builder
	err := Render(context.Background(), &out, Options{
		DB: database, DBPath: path,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	body := out.String()
	for _, want := range []string{
		"# TYPE observer_db_size_bytes gauge",
		"# TYPE observer_db_schema_version gauge",
		"# TYPE observer_projects_total gauge",
		"# TYPE observer_sessions_total gauge",
		"# TYPE observer_actions_total gauge",
		"# TYPE observer_api_turns_total gauge",
		"# TYPE observer_tool_actions_total gauge",
		"# TYPE observer_pidbridge_entries gauge",
		"# TYPE observer_process_runs gauge",
		"# TYPE observer_cost_usd gauge",
		"# TYPE observer_tokens gauge",
		"# TYPE observer_compression_saved_bytes gauge",
		"# TYPE observer_metrics_scrape_ok gauge",
		"# TYPE observer_metrics_scrape_duration_seconds gauge",
		`observer_pidbridge_entries 1`,
		`observer_sessions_total 1`,
		`observer_actions_total 1`,
		`observer_api_turns_total 1`,
		`observer_tool_actions_total{tool="claude-code"} 1`,
		`observer_metrics_scrape_ok 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("output missing %q; body:\n%s", want, body)
		}
	}
}

// TestRender_EmitsProcessMetrics seeds a session (via seedDB) then a
// process_runs row attributed to it and asserts the DB-derived process
// gauges (docs/process-observability.md §15) render with the right split
// and per-tool counts.
func TestRender_EmitsProcessMetrics(t *testing.T) {
	database, path, now := seedDB(t)
	// seedDB created session "sA"; attach an attributed process run to it.
	if _, err := database.ExecContext(context.Background(),
		`INSERT INTO process_runs
		   (process_key, pid, session_id, tool, attribution_source, attribution_confidence, started_at, last_seen_at)
		 VALUES ('pk-1', 4242, 'sA', 'claude-code', 'bridge', 'high', ?, ?)`,
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("seed process_runs: %v", err)
	}

	var out strings.Builder
	if err := Render(context.Background(), &out, Options{
		DB: database, DBPath: path, Now: func() time.Time { return now },
	}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	body := out.String()
	for _, want := range []string{
		`observer_process_runs{attributed="true"} 1`,
		`observer_process_runs{attributed="false"} 0`,
		`observer_process_runs_by_tool{tool="claude-code"} 1`,
		`observer_metrics_scrape_ok 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("output missing %q; body:\n%s", want, body)
		}
	}
}

func TestRender_CostRolledUpOverWindow(t *testing.T) {
	database, path, now := seedDB(t)
	var out strings.Builder
	if err := Render(context.Background(), &out, Options{
		DB: database, DBPath: path,
		Now: func() time.Time { return now },
	}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	body := out.String()

	// The api_turn is inside the default 5m window, so we should see its
	// cost and token contribution.
	for _, want := range []string{
		`observer_cost_usd{model="claude-sonnet-4",window="5m"}`,
		`observer_tokens{model="claude-sonnet-4",kind="input",window="5m"} 100`,
		`observer_tokens{model="claude-sonnet-4",kind="output",window="5m"} 50`,
		`observer_tokens{model="claude-sonnet-4",kind="cache_read",window="5m"} 20`,
		`observer_tokens{model="claude-sonnet-4",kind="cache_creation",window="5m"} 10`,
		`observer_compression_original_bytes{window="5m"} 10000`,
		`observer_compression_compressed_bytes{window="5m"} 3000`,
		`observer_compression_saved_bytes{window="5m"} 7000`,
		`observer_compression_compressed_count{window="5m"} 2`,
		`observer_compression_dropped_count{window="5m"} 1`,
		`observer_compression_marker_count{window="5m"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("output missing %q; body:\n%s", want, body)
		}
	}
}

func TestRender_WindowExcludesStaleTurns(t *testing.T) {
	database, path, now := seedDB(t)
	// Push the scrape forward an hour so the seeded turn falls out of the
	// 5m window. We still expect table-count metrics, but cost/compression
	// should be zero-valued families — the sample is still emitted with
	// value 0, not omitted, because the window=5m label set is always
	// present for those families.
	future := now.Add(time.Hour)
	var out strings.Builder
	if err := Render(context.Background(), &out, Options{
		DB: database, DBPath: path,
		Now: func() time.Time { return future },
	}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	body := out.String()

	// The per-model cost family should NOT contain claude-sonnet-4 (no
	// rows in window).
	if strings.Contains(body, `observer_cost_usd{model="claude-sonnet-4"`) {
		t.Errorf("stale model leaked into window; body:\n%s", body)
	}
	// But the aggregate window rows should still be present and zero.
	for _, want := range []string{
		`observer_cost_usd_window{window="5m"} 0`,
		`observer_compression_saved_bytes{window="5m"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("output missing %q; body:\n%s", want, body)
		}
	}
}

func TestRender_CustomWindow(t *testing.T) {
	database, path, now := seedDB(t)
	var out strings.Builder
	if err := Render(context.Background(), &out, Options{
		DB: database, DBPath: path,
		CostWindowMinutes: 10,
		Now:               func() time.Time { return now },
	}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	body := out.String()
	if !strings.Contains(body, `window="10m"`) {
		t.Errorf("expected window=10m label, body:\n%s", body)
	}
	if strings.Contains(body, `window="5m"`) {
		t.Errorf("default window leaked when custom set; body:\n%s", body)
	}
}

func TestHandler_MetricsEndpoint(t *testing.T) {
	database, path, now := seedDB(t)
	srv, err := New(Options{
		DB: database, DBPath: path,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("GET /metrics: %d", rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("Content-Type: %q", got)
	}
	body, _ := io.ReadAll(rr.Body)
	if !strings.Contains(string(body), "observer_sessions_total 1") {
		t.Errorf("/metrics body missing session count; body:\n%s", body)
	}

	// HEAD should set headers but not write a body.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodHead, "/metrics", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("HEAD /metrics: %d", rr.Code)
	}
	if rr.Body.Len() != 0 {
		t.Errorf("HEAD body should be empty, got %d bytes", rr.Body.Len())
	}

	// Non-GET/HEAD method should 405.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/metrics", strings.NewReader(""))
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /metrics: got %d want 405", rr.Code)
	}
}

func TestHandler_RootAndNotFound(t *testing.T) {
	database, path, now := seedDB(t)
	srv, err := New(Options{DB: database, DBPath: path, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	// Root: human-readable usage hint.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("GET /: %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "GET /metrics") {
		t.Errorf("root body missing usage hint: %s", rr.Body.String())
	}
	// Unknown path: 404.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/nope", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("GET /nope: %d want 404", rr.Code)
	}
}

func TestEscapeLabel_CoversSpecials(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{`with "quotes"`, `with \"quotes\"`},
		{`back\slash`, `back\\slash`},
		{"with\nnewline", `with\nnewline`},
		{`all "three"\n`, `all \"three\"\\n`},
	}
	for _, tc := range cases {
		if got := escapeLabel(tc.in); got != tc.want {
			t.Errorf("escapeLabel(%q): got %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatLabels_DeterministicKeyOrder(t *testing.T) {
	// formatLabels preserves the slice order exactly — the caller is
	// responsible for picking a stable key order. Samples are sorted by
	// the formatted output, so identical-key sets remain grouped.
	got := formatLabels([][2]string{
		{"model", "x"}, {"kind", "input"}, {"window", "5m"},
	})
	want := `{model="x",kind="input",window="5m"}`
	if got != want {
		t.Errorf("formatLabels: got %q want %q", got, want)
	}
	if formatLabels(nil) != "" {
		t.Error("formatLabels(nil) should be empty")
	}
}

// TestRender_ProcessHealthAbsentWithoutDaemon pins the honesty rule: with no
// daemon publishing health, the live families are ABSENT rather than zeroed —
// a fabricated `backend_up 0` would be indistinguishable from a real down
// backend.
func TestRender_ProcessHealthAbsentWithoutDaemon(t *testing.T) {
	database, path, now := seedDB(t)
	var out strings.Builder
	if err := Render(context.Background(), &out, Options{
		DB: database, DBPath: path, Now: func() time.Time { return now },
	}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	body := out.String()
	for _, absent := range []string{
		"observer_process_backend_up",
		"observer_process_network_accounting",
		"observer_process_health_timestamp_seconds",
	} {
		if strings.Contains(body, absent) {
			t.Errorf("family %q should be absent with no daemon reporting; body:\n%s", absent, body)
		}
	}
}

// TestRender_ProcessHealthModes asserts the /metrics surface the doctor check
// points operators at: one enum series per accounting mode, plus an info
// series carrying the daemon's verbatim reason.
func TestRender_ProcessHealthModes(t *testing.T) {
	tests := []struct {
		name       string
		health     diag.ProcessHealth
		want       []string
		wantAbsent []string
	}{
		{
			name: "off",
			health: diag.ProcessHealth{
				Backend: "poll", BackendUp: true, QueueDepth: 0,
				NetworkAccountingMode:   processobs.NetworkAccountingOff,
				NetworkAccountingReason: "not enabled ([observer.process.network].process_bytes)",
			},
			want: []string{
				`observer_process_backend_up{backend="poll"} 1`,
				`observer_process_queue_depth{backend="poll"} 0`,
				`observer_process_network_accounting{mode="off"} 1`,
				`observer_process_network_accounting{mode="unavailable"} 0`,
				`observer_process_network_accounting{mode="tcp"} 0`,
				`observer_process_network_accounting_info{mode="off",reason="not enabled ([observer.process.network].process_bytes)"} 1`,
			},
		},
		{
			name: "unavailable carries the untruncated reason",
			health: diag.ProcessHealth{
				Backend: "linux_ebpf+poll", BackendUp: true, QueueDepth: 7,
				NetworkAccountingMode:   processobs.NetworkAccountingUnavailable,
				NetworkAccountingReason: `load fentry/tcp_sendmsg: permission denied (missing CAP_BPF/CAP_PERFMON)`,
			},
			want: []string{
				`observer_process_backend_up{backend="linux_ebpf+poll"} 1`,
				`observer_process_queue_depth{backend="linux_ebpf+poll"} 7`,
				`observer_process_network_accounting{mode="unavailable"} 1`,
				`observer_process_network_accounting{mode="off"} 0`,
				`observer_process_network_accounting_info{mode="unavailable",reason="load fentry/tcp_sendmsg: permission denied (missing CAP_BPF/CAP_PERFMON)"} 1`,
			},
		},
		{
			name: "tcp live, backend down",
			health: diag.ProcessHealth{
				Backend: "", BackendUp: false,
				NetworkAccountingMode: processobs.NetworkAccountingTCP,
			},
			want: []string{
				`observer_process_backend_up{backend="unknown"} 0`,
				`observer_process_network_accounting{mode="tcp"} 1`,
				`observer_process_network_accounting_info{mode="tcp",reason=""} 1`,
			},
		},
		{
			// A mode outside this build's vocabulary is REPORTED (not folded
			// into "off" — the point of this surface is to not invent
			// certainty) but bucketed: the daemon learns its mode partly from
			// the cross-OS capturer's hello frame, and a label whose values a
			// remote picks is one retained Prometheus series per distinct
			// string. `observer doctor` prints the actual value.
			name: "an unrecognised mode is bucketed, never emitted as itself",
			health: diag.ProcessHealth{
				Backend: "etw", NetworkAccountingMode: "tcp+udp",
			},
			want: []string{
				`observer_process_network_accounting{mode="unrecognised"} 1`,
				`observer_process_network_accounting{mode="tcp"} 0`,
				`observer_process_network_accounting_info{mode="unrecognised",reason=""} 1`,
			},
			wantAbsent: []string{`mode="tcp+udp"`},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			database, path, now := seedDB(t)
			h := tc.health
			h.PID = os.Getpid()
			h.WrittenAt = now.Add(-10 * time.Second)
			if _, err := diag.WriteProcessHealth(filepath.Dir(path), h); err != nil {
				t.Fatalf("WriteProcessHealth: %v", err)
			}

			var out strings.Builder
			if err := Render(context.Background(), &out, Options{
				DB: database, DBPath: path, Now: func() time.Time { return now },
			}); err != nil {
				t.Fatalf("Render: %v", err)
			}
			body := out.String()
			want := append([]string{
				`observer_process_health_age_seconds 10`,
				// Timestamps render through the same 'g' formatter as every
				// other gauge (so they come out in exponential form).
				`observer_process_health_timestamp_seconds ` +
					formatValue(float64(h.WrittenAt.Unix())),
				`observer_metrics_scrape_ok 1`,
			}, tc.want...)
			for _, w := range want {
				if !strings.Contains(body, w) {
					t.Errorf("output missing %q; body:\n%s", w, body)
				}
			}
			for _, a := range tc.wantAbsent {
				if strings.Contains(body, a) {
					t.Errorf("output must not contain %q; body:\n%s", a, body)
				}
			}
		})
	}
}

// TestRender_ProcessTransportGauges pins the capturer-link gauges — the only
// scrape-side signal that distinguishes "the elevated capturer never started"
// from "every connection to it is being refused" from "the transport the
// operator asked for never came up at all".
//
// The absent-when-unconfigured row is the load-bearing one: emitting
// `observer_process_transport_connections 0` for the installs that have no
// dial-in transport would make every one of them look like a broken feed.
func TestRender_ProcessTransportGauges(t *testing.T) {
	connectAt := time.Date(2026, 7, 26, 11, 58, 0, 0, time.UTC)
	disconnectAt := time.Date(2026, 7, 26, 11, 59, 0, 0, time.UTC)
	tests := []struct {
		name       string
		health     diag.ProcessHealth
		want       []string
		wantAbsent []string
	}{
		{
			name: "no transport configured emits no transport family at all",
			health: diag.ProcessHealth{
				Backend: "poll", BackendUp: true,
				NetworkAccountingMode: processobs.NetworkAccountingOff,
			},
			wantAbsent: []string{"observer_process_transport_"},
		},
		{
			name: "configured but never connected reads as zeros with no timestamps",
			health: diag.ProcessHealth{
				Backend: "composite[poll+bridge-listen]", BackendUp: true,
				NetworkAccountingMode: processobs.NetworkAccountingOff,
				TransportState:        processobs.TransportStateConfigured, TransportAddr: "127.0.0.1:8823",
			},
			want: []string{
				`observer_process_transport_connected{addr="127.0.0.1:8823"} 0`,
				`observer_process_transport_connections{addr="127.0.0.1:8823"} 0`,
				`observer_process_transport_auth_failures{addr="127.0.0.1:8823"} 0`,
			},
			wantAbsent: []string{
				// Never-happened must be an absent series, not epoch 0.
				"observer_process_transport_last_connect_timestamp_seconds",
				"observer_process_transport_last_disconnect_timestamp_seconds",
			},
		},
		{
			name: "a refused capturer shows auth failures against zero connections",
			health: diag.ProcessHealth{
				Backend: "composite[poll+bridge-listen]", BackendUp: true,
				NetworkAccountingMode: processobs.NetworkAccountingOff,
				TransportState:        processobs.TransportStateConfigured, TransportAddr: "127.0.0.1:8823",
				TransportAuthFailures: 9,
			},
			want: []string{
				`observer_process_transport_auth_failures{addr="127.0.0.1:8823"} 9`,
				`observer_process_transport_connections{addr="127.0.0.1:8823"} 0`,
			},
			wantAbsent: []string{
				// No reason recorded → no info series. An empty reason label
				// would scrape as "refused for no reason". (The name appears
				// in the counter's HELP text, hence the "{" — this asserts
				// the absence of the SERIES, not of the cross-reference.)
				"observer_process_transport_auth_failure_info{",
			},
		},
		{
			// The counter names no cause, so the refusal's CLASS rides an
			// info-style always-1 series (the network_accounting_info
			// pattern). The class and not the verbatim reason: the reason
			// quotes whatever dialled the port, and one retained Prometheus
			// series per distinct string is unbounded cardinality. M4.
			name: "the refusal rides an info series as a BOUNDED class",
			health: diag.ProcessHealth{
				Backend: "composite[poll+bridge-listen]", BackendUp: true,
				NetworkAccountingMode: processobs.NetworkAccountingOff,
				TransportState:        processobs.TransportStateConfigured, TransportAddr: "127.0.0.1:8823",
				TransportAuthFailures:       9,
				TransportLastAuthError:      "processobs/bridge: malformed handshake: capturer speaks protocol v2, this daemon speaks v1",
				TransportLastAuthErrorClass: processobs.TransportAuthClassProtocolVersion,
			},
			want: []string{
				`observer_process_transport_auth_failure_info{addr="127.0.0.1:8823",class="protocol_version"} 1`,
			},
			wantAbsent: []string{
				// The verbatim reason must not reach a label.
				`speaks protocol v2, this daemon speaks v1"} 1`,
			},
		},
		{
			// Back-compat: a record written by a daemon that predates the
			// class field still emits the series (a refusal WAS recorded),
			// and says unknown rather than guessing one of the causes.
			name: "an older daemon's classless refusal reads as unknown",
			health: diag.ProcessHealth{
				Backend: "composite[poll+bridge-listen]", BackendUp: true,
				NetworkAccountingMode: processobs.NetworkAccountingOff,
				TransportState:        processobs.TransportStateConfigured, TransportAddr: "127.0.0.1:8823",
				TransportAuthFailures:  1,
				TransportLastAuthError: "processobs/bridge: invalid token",
			},
			want: []string{
				`observer_process_transport_auth_failure_info{addr="127.0.0.1:8823",class="unknown"} 1`,
			},
		},
		{
			// M3: requested-and-broken must not scrape identically to
			// never-requested (which emits nothing at all).
			name: "a requested transport that never started gets its own info series",
			health: diag.ProcessHealth{
				Backend: "composite[poll+bridge]", BackendUp: true,
				NetworkAccountingMode:      processobs.NetworkAccountingOff,
				TransportState:             processobs.TransportStateUnavailable,
				TransportUnavailableReason: "processobs/bridge: listen 127.0.0.1:8823: bind: address already in use",
			},
			want: []string{
				`observer_process_transport_unavailable_info{reason="processobs/bridge: listen 127.0.0.1:8823: bind: address already in use"} 1`,
			},
			wantAbsent: []string{
				// No transport exists, so it has no counters to report.
				"observer_process_transport_connected",
				"observer_process_transport_connections",
				"observer_process_transport_auth_failures",
			},
		},
		{
			name: "a live capturer carries both timestamps",
			health: diag.ProcessHealth{
				Backend: "composite[poll+bridge-listen]", BackendUp: true,
				NetworkAccountingMode: processobs.NetworkAccountingTCP,
				TransportState:        processobs.TransportStateConfigured, TransportAddr: "127.0.0.1:8823",
				TransportConnected: true, TransportConnections: 3,
				TransportLastConnectAt: connectAt, TransportLastDisconnectAt: disconnectAt,
			},
			want: []string{
				`observer_process_transport_connected{addr="127.0.0.1:8823"} 1`,
				`observer_process_transport_connections{addr="127.0.0.1:8823"} 3`,
				`observer_process_transport_last_connect_timestamp_seconds{addr="127.0.0.1:8823"} ` +
					formatValue(float64(connectAt.Unix())),
				`observer_process_transport_last_disconnect_timestamp_seconds{addr="127.0.0.1:8823"} ` +
					formatValue(float64(disconnectAt.Unix())),
			},
		},
		{
			name: "a transport without a resolved addr is labelled unknown, not blank",
			health: diag.ProcessHealth{
				Backend: "etw", BackendUp: true,
				NetworkAccountingMode: processobs.NetworkAccountingOff,
				TransportState:        processobs.TransportStateConfigured,
			},
			want: []string{`observer_process_transport_connected{addr="unknown"} 0`},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			database, path, now := seedDB(t)
			h := tc.health
			h.PID = os.Getpid()
			h.WrittenAt = now.Add(-10 * time.Second)
			if _, err := diag.WriteProcessHealth(filepath.Dir(path), h); err != nil {
				t.Fatalf("WriteProcessHealth: %v", err)
			}
			var out strings.Builder
			if err := Render(context.Background(), &out, Options{
				DB: database, DBPath: path, Now: func() time.Time { return now },
			}); err != nil {
				t.Fatalf("Render: %v", err)
			}
			body := out.String()
			for _, w := range tc.want {
				if !strings.Contains(body, w) {
					t.Errorf("output missing %q; body:\n%s", w, body)
				}
			}
			for _, a := range tc.wantAbsent {
				if strings.Contains(body, a) {
					t.Errorf("output unexpectedly contains %q; body:\n%s", a, body)
				}
			}
		})
	}
}

// TestRender_TransportAuthLabelCardinalityIsBounded is the M4 proof.
//
// The refusal reason quotes a fragment supplied by whatever dialled the
// listener, and under WSL's localhostForwarding that is any process on the
// Windows host. If that text is a metric label, a hostile local process mints
// a fresh label value per connection attempt and Prometheus retains every
// distinct series it has ever scraped — a growth the 240-byte clamp does
// nothing about, because it bounds each string's SIZE, not the NUMBER of
// distinct strings.
//
// So: feed the exporter a run of DISTINCT reasons (the shape an attacker
// produces) and assert the emitted label values all come from the closed
// class vocabulary, that the count of distinct series is bounded by that
// vocabulary rather than by the input, and that no reason text leaks.
func TestRender_TransportAuthLabelCardinalityIsBounded(t *testing.T) {
	seen := map[string]bool{}
	const attempts = 25
	database, path, now := seedDB(t)
	for i := 0; i < attempts; i++ {
		h := diag.ProcessHealth{
			PID: os.Getpid(), WrittenAt: now.Add(-time.Second),
			Backend: "composite[poll+bridge-listen]", BackendUp: true,
			NetworkAccountingMode: processobs.NetworkAccountingOff,
			TransportState:        processobs.TransportStateConfigured,
			TransportAddr:         "127.0.0.1:8823",
			TransportAuthFailures: int64(i + 1),
			// A fresh remote-chosen version fragment every time — exactly
			// what `unparsable protocol version %q` used to carry into the
			// label.
			TransportLastAuthError: fmt.Sprintf(
				"processobs/bridge: malformed handshake: unparsable protocol version %q", "v-"+strconv.Itoa(i),
			),
			TransportLastAuthErrorClass: processobs.TransportAuthClassProtocolVersion,
		}
		if _, err := diag.WriteProcessHealth(filepath.Dir(path), h); err != nil {
			t.Fatalf("WriteProcessHealth: %v", err)
		}
		var out strings.Builder
		if err := Render(context.Background(), &out, Options{
			DB: database, DBPath: path, Now: func() time.Time { return now },
		}); err != nil {
			t.Fatalf("Render: %v", err)
		}
		body := out.String()
		if strings.Contains(body, "v-"+strconv.Itoa(i)) {
			t.Fatalf("the remote-supplied fragment reached the exposition:\n%s", body)
		}
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "observer_process_transport_auth_failure_info{") {
				seen[line[:strings.LastIndex(line, "}")+1]] = true
			}
		}
	}
	if len(seen) == 0 {
		t.Fatal("no auth-failure info series was emitted at all")
	}
	if len(seen) > 1 {
		t.Fatalf("%d distinct label sets from %d attempts — the label tracks remote input: %v",
			len(seen), attempts, seen)
	}
}

// TestRender_TransportAuthClassIsNormalisedFromDisk closes the same hole one
// layer down: the health record is a FILE, so the exporter must not trust its
// class field either. Anything outside the closed vocabulary becomes
// "unknown" rather than becoming its own series.
func TestRender_TransportAuthClassIsNormalisedFromDisk(t *testing.T) {
	database, path, now := seedDB(t)
	if _, err := diag.WriteProcessHealth(filepath.Dir(path), diag.ProcessHealth{
		PID: os.Getpid(), WrittenAt: now.Add(-time.Second),
		Backend: "composite[poll+bridge-listen]", BackendUp: true,
		NetworkAccountingMode: processobs.NetworkAccountingOff,
		TransportState:        processobs.TransportStateConfigured,
		TransportAddr:         "127.0.0.1:8823",
		TransportAuthFailures: 1, TransportLastAuthError: "whatever",
		TransportLastAuthErrorClass: "not-a-class-this-build-defines",
	}); err != nil {
		t.Fatalf("WriteProcessHealth: %v", err)
	}
	var out strings.Builder
	if err := Render(context.Background(), &out, Options{
		DB: database, DBPath: path, Now: func() time.Time { return now },
	}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	body := out.String()
	if !strings.Contains(body, `observer_process_transport_auth_failure_info{addr="127.0.0.1:8823",class="unknown"} 1`) {
		t.Errorf("an undefined class must normalise to unknown; body:\n%s", body)
	}
	if strings.Contains(body, "not-a-class-this-build-defines") {
		t.Errorf("an undefined class became its own label value; body:\n%s", body)
	}
}

// TestEscapeLabel_ControlBytesAndInvalidUTF8 is L1. The text format defines
// exactly three escapes, so everything else needs a decision rather than a
// pass-through: a raw CR cannot be escaped (there is no `\r`; emitting one
// would be an undefined escape a conforming parser rejects) and a raw CRLF
// would end the sample line early, splicing a label's tail into what the
// scraper reads as the next metric. Invalid UTF-8 is not a legal label value
// at all.
func TestEscapeLabel_ControlBytesAndInvalidUTF8(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"carriage return is dropped, not passed through", "a\rb", "ab"},
		{"CRLF cannot end the sample line early", "a\r\nb", `a\nb`},
		{"other C0 controls are dropped", "a\x00\x07\x1bb", "ab"},
		{"DEL is dropped", "a\x7fb", "ab"},
		{"tab is a control too", "a\tb", "ab"},
		{"invalid UTF-8 is replaced, never emitted raw", "a\xffb", "a�b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := escapeLabel(tc.in)
			if got != tc.want {
				t.Fatalf("escapeLabel(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("escapeLabel(%q) is not valid UTF-8: %q", tc.in, got)
			}
			// Whatever the escaping, the result may never carry a byte that
			// can terminate or re-open the exposition line.
			for i := 0; i < len(got); i++ {
				if c := got[i]; c < 0x20 || c == 0x7f {
					t.Fatalf("escapeLabel(%q) kept control byte %#x", tc.in, c)
				}
			}
		})
	}
}

// TestRender_ExpositionSurvivesHostileLabels is the same property proven
// through a real scrape rather than through the escaper alone: a health
// record carrying quote / newline / CR / control bytes must still produce an
// exposition whose sample lines each stay one line, with balanced quotes.
func TestRender_ExpositionSurvivesHostileLabels(t *testing.T) {
	database, path, now := seedDB(t)
	hostile := "tcp\" injected=\"1\r\nobserver_forged_metric 99\n# HELP x\x00\x1b"
	if _, err := diag.WriteProcessHealth(filepath.Dir(path), diag.ProcessHealth{
		PID: os.Getpid(), WrittenAt: now.Add(-time.Second),
		Backend: "composite[poll+bridge-listen]", BackendUp: true,
		NetworkAccountingMode:   processobs.NetworkAccountingUnavailable,
		NetworkAccountingReason: hostile,
	}); err != nil {
		t.Fatalf("WriteProcessHealth: %v", err)
	}
	var out strings.Builder
	if err := Render(context.Background(), &out, Options{
		DB: database, DBPath: path, Now: func() time.Time { return now },
	}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	body := out.String()
	for _, line := range strings.Split(body, "\n") {
		// The forged name may legitimately appear INSIDE an escaped label
		// value; what must never happen is a line STARTING with it, which is
		// what a scraper would read as a metric of its own.
		if strings.HasPrefix(line, "observer_forged_metric") {
			t.Fatalf("a label value forged a metric line:\n%s", body)
		}
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		for i := 0; i < len(line); i++ {
			if c := line[i]; c < 0x20 || c == 0x7f {
				t.Fatalf("sample line carries control byte %#x: %q", c, line)
			}
		}
		// Unescaped quotes must be balanced, or the label set does not close.
		unescaped := 0
		for i := 0; i < len(line); i++ {
			if line[i] == '"' && (i == 0 || line[i-1] != '\\') {
				unescaped++
			}
		}
		if unescaped%2 != 0 {
			t.Fatalf("unbalanced quotes in sample line: %q", line)
		}
	}
}

// TestRender_CapturerDecodeGauges pins the alertable half of E6/E6b.
//
// Three rules, and the middle one is the whole point of E6b: absence is a
// missing family (never a zeroed one), a large IGNORE count on its own is not
// a fault and must not read as one, and the renumbered-provider conjunction is
// exported as its OWN derived series rather than left for each operator to
// re-derive from counters that are individually unalarming.
func TestRender_CapturerDecodeGauges(t *testing.T) {
	const addr = "127.0.0.1:8823"
	base := func() diag.ProcessHealth {
		return diag.ProcessHealth{
			Backend: "composite[poll+bridge-listen]", BackendUp: true,
			NetworkAccountingMode: processobs.NetworkAccountingTCP,
			TransportState:        processobs.TransportStateConfigured, TransportAddr: addr,
			TransportConnected: true, TransportConnections: 1,
		}
	}
	tests := []struct {
		name       string
		mutate     func(*diag.ProcessHealth)
		want       []string
		wantAbsent []string
	}{
		{
			name:   "never reported emits no decode family at all",
			mutate: func(*diag.ProcessHealth) {},
			wantAbsent: []string{
				`observer_process_capturer_decode_decoded{`,
				`observer_process_capturer_decode_ignored{`,
				`observer_process_capturer_decode_nothing_classified{`,
			},
		},
		{
			name: "a healthy busy capture: huge ignore count, and NOT flagged",
			mutate: func(h *diag.ProcessHealth) {
				h.TransportCapturerDecodeReported = true
				h.TransportCapturerIgnored = 1000000
				h.TransportCapturerDecoded = 4321
			},
			want: []string{
				// Rendered through formatValue: a large count is exposed in
				// Prometheus' own float formatting, not decimal digits.
				`observer_process_capturer_decode_ignored{addr="127.0.0.1:8823"} ` + formatValue(1000000),
				`observer_process_capturer_decode_decoded{addr="127.0.0.1:8823"} 4321`,
				`observer_process_capturer_decode_nothing_classified{addr="127.0.0.1:8823"} 0`,
				`observer_process_capturer_decode_dropped{addr="127.0.0.1:8823"} 0`,
			},
		},
		{
			name: "the renumbered-provider shape is flagged, though every refusal counter is clean",
			mutate: func(h *diag.ProcessHealth) {
				h.TransportCapturerDecodeReported = true
				h.TransportCapturerIgnored = 48211
			},
			want: []string{
				`observer_process_capturer_decode_ignored{addr="127.0.0.1:8823"} 48211`,
				`observer_process_capturer_decode_decoded{addr="127.0.0.1:8823"} 0`,
				`observer_process_capturer_decode_nothing_classified{addr="127.0.0.1:8823"} 1`,
				`observer_process_capturer_decode_dropped{addr="127.0.0.1:8823"} 0`,
				`observer_process_capturer_decode_unsupported_version{addr="127.0.0.1:8823"} 0`,
			},
		},
		{
			name: "a refusing decoder is not ALSO flagged as classifying nothing",
			mutate: func(h *diag.ProcessHealth) {
				h.TransportCapturerDecodeReported = true
				h.TransportCapturerIgnored = 900
				h.TransportCapturerDropped = 4
			},
			want: []string{
				`observer_process_capturer_decode_dropped{addr="127.0.0.1:8823"} 4`,
				`observer_process_capturer_decode_nothing_classified{addr="127.0.0.1:8823"} 0`,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			database, path, now := seedDB(t)
			h := base()
			tc.mutate(&h)
			h.PID = os.Getpid()
			h.WrittenAt = now.Add(-10 * time.Second)
			if _, err := diag.WriteProcessHealth(filepath.Dir(path), h); err != nil {
				t.Fatalf("WriteProcessHealth: %v", err)
			}
			var out strings.Builder
			if err := Render(context.Background(), &out, Options{
				DB: database, DBPath: path, Now: func() time.Time { return now },
			}); err != nil {
				t.Fatalf("Render: %v", err)
			}
			body := out.String()
			for _, w := range tc.want {
				if !strings.Contains(body, w) {
					t.Errorf("output missing %q; body:\n%s", w, body)
				}
			}
			for _, a := range tc.wantAbsent {
				if strings.Contains(body, a) {
					t.Errorf("output unexpectedly contains %q; body:\n%s", a, body)
				}
			}
		})
	}
}
