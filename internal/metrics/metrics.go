package metrics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/diag"
	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
)

// Options configures a Renderer or Server.
type Options struct {
	// DB is the observer database. Required.
	DB *sql.DB
	// DBPath is surfaced via the db_path label on observer_db_size_bytes.
	DBPath string
	// CostEngine prices token summaries. Defaults to baked-in pricing.
	CostEngine *cost.Engine
	// CostWindowMinutes caps the cost rollup window. A short window keeps
	// each scrape bounded; Prometheus users get per-scrape deltas via
	// recording rules. Zero → 5 minutes.
	CostWindowMinutes int
	// Logger receives operational messages. Zero value → slog.Default().
	Logger *slog.Logger
	// Now overrides time.Now for tests.
	Now func() time.Time
}

// Render writes one scrape's worth of Prometheus text exposition to w. Any
// individual query failure is absorbed — we emit what we have and surface
// the failure as observer_metrics_scrape_ok = 0.
func Render(ctx context.Context, w io.Writer, opts Options) error {
	if opts.DB == nil {
		return errors.New("metrics.Render: DB is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.CostEngine == nil {
		opts.CostEngine = cost.NewEngine(config.IntelligenceConfig{})
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	window := opts.CostWindowMinutes
	if window <= 0 {
		window = 5
	}

	start := opts.Now()
	out := &buf{}
	scrapeOK := true

	if err := writeSnapshotMetrics(ctx, out, opts); err != nil {
		opts.Logger.Warn("metrics.Render snapshot failed", "err", err)
		scrapeOK = false
	}
	if err := writeBridgeMetrics(ctx, out, opts); err != nil {
		opts.Logger.Warn("metrics.Render bridge count failed", "err", err)
		scrapeOK = false
	}
	if err := writeCostMetrics(ctx, out, opts, window); err != nil {
		opts.Logger.Warn("metrics.Render cost failed", "err", err)
		scrapeOK = false
	}

	dur := opts.Now().Sub(start).Seconds()
	writeGauge(out, "observer_metrics_scrape_duration_seconds",
		"Duration of the most recent /metrics scrape, in seconds.", dur)
	ok := 1.0
	if !scrapeOK {
		ok = 0
	}
	writeGauge(out, "observer_metrics_scrape_ok",
		"1 if every query backing this scrape succeeded, 0 if any failed.", ok)

	if _, err := w.Write(out.buf); err != nil {
		return fmt.Errorf("metrics.Render write: %w", err)
	}
	return nil
}

// writeSnapshotMetrics emits gauges sourced from diag.Snapshot.
func writeSnapshotMetrics(ctx context.Context, out *buf, opts Options) error {
	snap, err := diag.Snapshot(ctx, opts.DB, opts.DBPath)
	if err != nil {
		return fmt.Errorf("diag.Snapshot: %w", err)
	}

	writeGaugeLabeled(out, "observer_db_size_bytes",
		"Size of the observer SQLite file on disk, in bytes.",
		[]labeled{{labels: [][2]string{{"path", snap.DBPath}}, value: float64(snap.DBSizeBytes)}})
	writeGauge(out, "observer_db_schema_version",
		"Applied schema migration version.", float64(snap.SchemaVersion))

	writeGauge(out, "observer_projects_total",
		"Total rows in the projects table.", float64(snap.Counts.Projects))
	writeGauge(out, "observer_sessions_total",
		"Total rows in the sessions table.", float64(snap.Counts.Sessions))
	writeGauge(out, "observer_actions_total",
		"Total rows in the actions table.", float64(snap.Counts.Actions))
	writeGauge(out, "observer_api_turns_total",
		"Total rows in the api_turns table (proxy-logged LLM turns).",
		float64(snap.Counts.APITurns))
	writeGauge(out, "observer_file_state_total",
		"Total rows in the file_state table (freshness snapshots).",
		float64(snap.Counts.FileState))
	writeGauge(out, "observer_failure_context_total",
		"Total rows in the failure_context table (lifetime).",
		float64(snap.Counts.FailureContext))
	writeGauge(out, "observer_action_excerpts_total",
		"Total rows in the FTS5 action_excerpts table.",
		float64(snap.Counts.ActionExcerpts))
	writeGauge(out, "observer_token_usage_rows_total",
		"Total rows in the token_usage table (JSONL-derived).",
		float64(snap.Counts.TokenUsageRows))
	writeGauge(out, "observer_failures_24h",
		"Rows in failure_context with timestamp within the last 24 hours.",
		float64(snap.RecentFailures24))

	if !snap.LastActionAt.IsZero() {
		writeGauge(out, "observer_last_action_timestamp_seconds",
			"Unix timestamp of the most recently ingested action, regardless of tool.",
			float64(snap.LastActionAt.Unix()))
	}

	if len(snap.PerToolLastSeen) > 0 {
		actions := make([]labeled, 0, len(snap.PerToolLastSeen))
		seen := make([]labeled, 0, len(snap.PerToolLastSeen))
		for _, t := range snap.PerToolLastSeen {
			actions = append(actions, labeled{
				labels: [][2]string{{"tool", t.Tool}},
				value:  float64(t.ActionCount),
			})
			if !t.LastSeenAt.IsZero() {
				seen = append(seen, labeled{
					labels: [][2]string{{"tool", t.Tool}},
					value:  float64(t.LastSeenAt.Unix()),
				})
			}
		}
		writeGaugeLabeled(out, "observer_tool_actions_total",
			"Actions observed per tool, lifetime.", actions)
		if len(seen) > 0 {
			writeGaugeLabeled(out, "observer_tool_last_seen_timestamp_seconds",
				"Unix timestamp of the most recently ingested action per tool.", seen)
		}
	}
	return nil
}

// writeBridgeMetrics emits the pid→session_id bridge row count.
func writeBridgeMetrics(ctx context.Context, out *buf, opts Options) error {
	var n int
	if err := opts.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_pid_bridge`).Scan(&n); err != nil {
		return fmt.Errorf("pidbridge count: %w", err)
	}
	writeGauge(out, "observer_pidbridge_entries",
		"Live entries in the session_pid_bridge table (populated by SessionStart hooks).",
		float64(n))
	return nil
}

// writeCostMetrics emits cost / token / compression gauges rolled up over
// the last windowMinutes.
func writeCostMetrics(ctx context.Context, out *buf, opts Options, windowMinutes int) error {
	since := opts.Now().Add(-time.Duration(windowMinutes) * time.Minute)
	summary, err := opts.CostEngine.Summary(ctx, opts.DB, cost.Options{
		Since:   since,
		GroupBy: cost.GroupByModel,
		Source:  cost.SourceAuto,
		Now:     opts.Now,
	})
	if err != nil {
		return fmt.Errorf("cost.Summary: %w", err)
	}

	windowLabel := strconv.Itoa(windowMinutes) + "m"

	writeGaugeLabeled(out, "observer_cost_usd_window",
		"Total USD cost aggregated over the scrape window.",
		[]labeled{{labels: [][2]string{{"window", windowLabel}}, value: summary.TotalCost}})
	writeGaugeLabeled(out, "observer_turns_window",
		"Total LLM turns aggregated over the scrape window.",
		[]labeled{{labels: [][2]string{{"window", windowLabel}}, value: float64(summary.TurnCount)}})

	if len(summary.Rows) > 0 {
		costs := make([]labeled, 0, len(summary.Rows))
		turns := make([]labeled, 0, len(summary.Rows))
		var tokens []labeled
		for _, r := range summary.Rows {
			modelLabels := [][2]string{
				{"model", r.Key},
				{"window", windowLabel},
			}
			costs = append(costs, labeled{labels: modelLabels, value: r.CostUSD})
			turns = append(turns, labeled{labels: modelLabels, value: float64(r.TurnCount)})
			for _, kv := range tokenKinds(r) {
				tokens = append(tokens, labeled{
					labels: [][2]string{
						{"model", r.Key},
						{"kind", kv.kind},
						{"window", windowLabel},
					},
					value: float64(kv.value),
				})
			}
		}
		writeGaugeLabeled(out, "observer_cost_usd",
			"USD cost per model over the scrape window.", costs)
		writeGaugeLabeled(out, "observer_turns_per_model",
			"LLM turns per model over the scrape window.", turns)
		writeGaugeLabeled(out, "observer_tokens",
			"Token counts per model and kind (input|output|cache_read|cache_creation) over the scrape window.",
			tokens)
	}

	c := summary.TotalCompression
	writeGaugeLabeled(out, "observer_compression_original_bytes",
		"Sum of pre-compression request body sizes over the scrape window.",
		[]labeled{{labels: [][2]string{{"window", windowLabel}}, value: float64(c.OriginalBytes)}})
	writeGaugeLabeled(out, "observer_compression_compressed_bytes",
		"Sum of post-compression request body sizes over the scrape window.",
		[]labeled{{labels: [][2]string{{"window", windowLabel}}, value: float64(c.CompressedBytes)}})
	writeGaugeLabeled(out, "observer_compression_saved_bytes",
		"Bytes saved by conversation compression over the scrape window.",
		[]labeled{{labels: [][2]string{{"window", windowLabel}}, value: float64(c.SavedBytes())}})
	writeGaugeLabeled(out, "observer_compression_compressed_count",
		"Tool-result bodies rewritten by per-type compression over the scrape window.",
		[]labeled{{labels: [][2]string{{"window", windowLabel}}, value: float64(c.CompressedCount)}})
	writeGaugeLabeled(out, "observer_compression_dropped_count",
		"Original messages replaced by markers over the scrape window.",
		[]labeled{{labels: [][2]string{{"window", windowLabel}}, value: float64(c.DroppedCount)}})
	writeGaugeLabeled(out, "observer_compression_marker_count",
		"Marker messages emitted by the compression pipeline over the scrape window.",
		[]labeled{{labels: [][2]string{{"window", windowLabel}}, value: float64(c.MarkerCount)}})
	writeGaugeLabeled(out, "observer_compression_turns",
		"Turns in the window that carried any compression metadata.",
		[]labeled{{labels: [][2]string{{"window", windowLabel}}, value: float64(c.Turns)}})
	return nil
}

type tokenKV struct {
	kind  string
	value int64
}

func tokenKinds(r cost.Row) []tokenKV {
	return []tokenKV{
		{"input", r.Tokens.Input},
		{"output", r.Tokens.Output},
		{"cache_read", r.Tokens.CacheRead},
		{"cache_creation", r.Tokens.CacheCreation},
	}
}

// labeled is one labeled sample.
type labeled struct {
	labels [][2]string
	value  float64
}

// buf is a thin bytes.Buffer replacement that avoids the extra import and
// tracks write errors cleanly. Methods always succeed (they write to a byte
// slice); the surrounding Render call decides how to surface failures.
type buf struct {
	buf []byte
}

func (b *buf) writeString(s string) { b.buf = append(b.buf, s...) }
func (b *buf) writeByte(c byte)     { b.buf = append(b.buf, c) }

// writeGauge emits HELP + TYPE + a single unlabeled sample.
func writeGauge(w *buf, name, help string, value float64) {
	writeHeader(w, name, help, "gauge")
	w.writeString(name)
	w.writeByte(' ')
	w.writeString(formatValue(value))
	w.writeByte('\n')
}

// writeGaugeLabeled emits HELP + TYPE + one sample per label set. Empty
// input is a no-op (absent metric families are valid in Prometheus).
func writeGaugeLabeled(w *buf, name, help string, samples []labeled) {
	if len(samples) == 0 {
		return
	}
	writeHeader(w, name, help, "gauge")
	// Deterministic order: sort by the formatted label string so tests are
	// stable regardless of map iteration.
	sort.SliceStable(samples, func(i, j int) bool {
		return formatLabels(samples[i].labels) < formatLabels(samples[j].labels)
	})
	for _, s := range samples {
		w.writeString(name)
		w.writeString(formatLabels(s.labels))
		w.writeByte(' ')
		w.writeString(formatValue(s.value))
		w.writeByte('\n')
	}
}

func writeHeader(w *buf, name, help, typeStr string) {
	w.writeString("# HELP ")
	w.writeString(name)
	w.writeByte(' ')
	w.writeString(escapeHelp(help))
	w.writeByte('\n')
	w.writeString("# TYPE ")
	w.writeString(name)
	w.writeByte(' ')
	w.writeString(typeStr)
	w.writeByte('\n')
}

// formatLabels renders `{k1="v1",k2="v2"}` with proper escaping. Returns
// an empty string when there are no labels.
func formatLabels(labels [][2]string) string {
	if len(labels) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, kv := range labels {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(kv[0])
		b.WriteString(`="`)
		b.WriteString(escapeLabel(kv[1]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// escapeLabel escapes `\`, `"`, and newlines per Prometheus text format.
func escapeLabel(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 4)
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// escapeHelp escapes `\` and newlines in HELP text (no `"` escape needed —
// HELP is terminated by end-of-line, not a quote).
func escapeHelp(s string) string {
	if !strings.ContainsAny(s, "\\\n") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 4)
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// formatValue renders a float using Go's 'g' verb with full precision —
// Prometheus accepts decimal, exponential, and the three special tokens.
func formatValue(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// Server wraps Render in an http.Handler for long-running scrapers.
type Server struct {
	opts Options
}

// New returns a Server. DB is required.
func New(opts Options) (*Server, error) {
	if opts.DB == nil {
		return nil, errors.New("metrics.New: DB is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.CostEngine == nil {
		opts.CostEngine = cost.NewEngine(config.IntelligenceConfig{})
	}
	return &Server{opts: opts}, nil
}

// Handler returns an http.Handler that serves /metrics in Prometheus text
// format. Any other path 404s; GET is the only accepted method.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = io.WriteString(w, "observer metrics exporter\nGET /metrics\n")
			return
		}
		http.NotFound(w, r)
	})
	return mux
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		return
	}
	if err := Render(r.Context(), w, s.opts); err != nil {
		s.opts.Logger.Error("metrics.Render failed", "err", err)
	}
}

// ListenAndServe runs the metrics server on addr until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
