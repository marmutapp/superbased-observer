package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/adapter/browserchat"
	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/scrub"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// newBrowserCmd implements `observer browser hook <event>`. The browser
// extension's native-messaging host invokes this with a captured-turn JSON
// payload on STDIN — the sibling of `observer hook <tool> <event>` for the
// CLI adapters, but for the browser rail.
//
// Like the CLI hook path it opens db.Open directly (works even when the
// daemon isn't running) and MUST never fail hard: parse/DB errors are logged
// to stderr and swallowed so the browser's native-messaging port never sees
// a crash.
//
// `--config <path>` propagates the daemon's config so the DB write lands on
// the same observer.db the daemon is using.
func newBrowserCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "browser <subcommand>",
		Short: "Browser-extension capture bridge",
		Long: "Landing point for the opt-in MV3 browser extension. The\n" +
			"extension's native-messaging host invokes `observer browser hook\n" +
			"<event>` with a captured-turn JSON payload on stdin.",
	}

	hookCmd := &cobra.Command{
		Use:   "hook <event>",
		Short: "Ingest a captured browser-chatbot turn from stdin",
		Long: "Reads a captured-turn JSON payload (the browserchat wire\n" +
			"contract) from stdin, normalizes it, and writes it to the\n" +
			"observer DB via the unchanged store.Ingest seam. Errors are\n" +
			"logged to stderr and swallowed — never blocks the browser.",
		Args: cobra.ArbitraryArgs,
		Run: func(cmd *cobra.Command, args []string) {
			event := ""
			if len(args) > 0 {
				event = args[0]
			}
			handleBrowserHook(cmd.Context(), event, configPath)
		},
	}
	hookCmd.Flags().StringVar(&configPath, "config", "", "Path to observer config.toml — when set, hook DB writes land on the config's observer.db (matches the daemon)")
	cmd.AddCommand(hookCmd)

	var healthConfigPath string
	healthCmd := &cobra.Command{
		Use:   "health",
		Short: "Show the last health beacon relayed by the browser extension",
		Long: "Prints the per-site health the browser extension has relayed\n" +
			"through the native-messaging host (site, status, reason, age).\n" +
			"A stale or empty table means the extension isn't talking to the\n" +
			"host — the fastest way to see a broken manifest or degraded parser.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBrowserHealth(cmd.OutOrStdout(), healthConfigPath)
		},
	}
	healthCmd.Flags().StringVar(&healthConfigPath, "config", "", "Path to observer config.toml — resolves the observer dir the health file lives in")
	cmd.AddCommand(healthCmd)
	return cmd
}

// handleBrowserHook parses the captured-turn payload on stdin, normalizes it
// through internal/adapter/browserchat, and ingests the resulting events.
// The event argument is accepted for symmetry with the CLI hook path (a
// future capture/heartbeat/health event class) but is currently informational
// only — the payload's `site` field is the real discriminator.
func handleBrowserHook(ctx context.Context, event, configPath string) {
	label := "browser"
	if event != "" {
		label = "browser:" + event
	}

	body, _ := io.ReadAll(io.LimitReader(os.Stdin, 8*1024*1024))
	body = bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF})
	// Ack immediately on stdout — the native-messaging host relays this to
	// the extension; it must never wait on the DB path.
	_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "ok"})

	if len(body) == 0 {
		return
	}

	cfg, err := config.Load(config.LoadOptions{GlobalPath: configPath})
	if err != nil {
		fmt.Fprintf(os.Stderr, "observer-browser: %s config: %v\n", label, err)
		return
	}
	if !cfg.Browser.Enabled {
		return // rail disabled on the daemon side — drop the turn, fail-soft.
	}
	// A health beacon (event "health") is recorded to a small node-local file
	// so daemon-side failures are LEGIBLE (A6) — it never touches the DB /
	// store.Ingest path. host.js routes payload.type=="health" here.
	if event == "health" {
		if err := recordBrowserHealth(cfg, body); err != nil {
			fmt.Fprintf(os.Stderr, "observer-browser: %s health: %v\n", label, err)
		}
		return
	}
	database, err := db.Open(ctx, db.Options{Path: cfg.Observer.DBPath, SkipIntegrityCheck: true})
	if err != nil {
		fmt.Fprintf(os.Stderr, "observer-browser: %s db: %v\n", label, err)
		return
	}
	defer database.Close()

	insertCtx, cancel := context.WithTimeout(ctx, cfg.Observer.Hooks.HookTimeout())
	defer cancel()
	if err := ingestBrowserTurn(insertCtx, store.New(database), body, cfg.Browser); err != nil {
		fmt.Fprintf(os.Stderr, "observer-browser: %s ingest: %v\n", label, err)
	}
}

// ingestBrowserTurn is the ONE OWNER of the browser-capture normalize →
// store.Ingest path. Both the native-messaging hook (above) and the loopback
// listener (cmd/observer/start.go) funnel a raw captured-turn body through
// here, so the transport is a deployment detail, not a schema fork. It
// enforces the daemon's side of the §5.1 precedence — the per-site enable
// toggle and the granularity ceiling — before anything is stored.
//
// A site the daemon has toggled off is dropped silently (nil error). An
// unparseable / unknown-site body returns the normalize error for the caller
// to log; the caller never crashes on it.
func ingestBrowserTurn(ctx context.Context, st *store.Store, body []byte, bc config.BrowserConfig) error {
	if !browserSiteEnabled(body, bc) {
		return nil // per-site daemon toggle is off — drop, fail-soft.
	}
	toolEvents, tokenEvents, err := browserchat.NormalizeWith(body, browserchat.Options{
		Scrubber:           scrub.New(),
		GranularityCeiling: browserchat.Granularity(bc.GranularityCeiling),
	})
	if err != nil {
		return fmt.Errorf("normalize: %w", err)
	}
	if len(toolEvents) == 0 && len(tokenEvents) == 0 {
		return nil
	}
	if _, err := st.Ingest(ctx, toolEvents, tokenEvents, store.IngestOptions{}); err != nil {
		return fmt.Errorf("insert: %w", err)
	}
	return nil
}

// --- A6: browser-capture health beacon (node-local, file-backed) ----------

// browserHealthFileName is the node-local file the health beacons land in,
// next to the observer DB. Best-effort, bounded, 0600.
const browserHealthFileName = "browser-health.json"

// maxHealthSites bounds the health map so a hostile/looping extension can't
// grow the file unboundedly. Far above the five *-web sites in play.
const maxHealthSites = 32

// browserHealthBeacon is the compact health signal the service worker relays
// over the SAME native-messaging channel as captured turns (type=="health").
type browserHealthBeacon struct {
	Type   string `json:"type"`
	Site   string `json:"site"`
	Status string `json:"status"` // "ok" | "degraded"
	Reason string `json:"reason,omitempty"`
	TS     int64  `json:"ts,omitempty"` // client epoch millis
}

// browserHealthEntry is one site's last-seen health, as persisted.
type browserHealthEntry struct {
	Status     string `json:"status"`
	Reason     string `json:"reason,omitempty"`
	TS         int64  `json:"ts,omitempty"`          // client-reported epoch millis
	RecordedAt int64  `json:"recorded_at,omitempty"` // daemon-side epoch millis
}

// browserHealthFile is the on-disk shape.
type browserHealthFile struct {
	Sites map[string]browserHealthEntry `json:"sites"`
}

// browserObserverDir returns the observer dir (where the DB + node-local
// files live) for cfg — the DBPath's directory.
func browserObserverDir(cfg config.Config) string {
	return filepath.Dir(cfg.Observer.DBPath)
}

// recordBrowserHealth parses a health beacon and records it into the
// node-local health file (read-modify-write with an atomic rename). Best-
// effort: a malformed beacon or a write race is a soft no-op — health
// visibility must never block or crash the browser's native-messaging port.
func recordBrowserHealth(cfg config.Config, body []byte) error {
	var b browserHealthBeacon
	if err := json.Unmarshal(body, &b); err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	if b.Site == "" {
		return nil // nothing to key on — drop.
	}
	if b.Status == "" {
		b.Status = "ok"
	}
	path := filepath.Join(browserObserverDir(cfg), browserHealthFileName)
	hf := loadBrowserHealthFile(path)
	if hf.Sites == nil {
		hf.Sites = map[string]browserHealthEntry{}
	}
	hf.Sites[b.Site] = browserHealthEntry{
		Status:     b.Status,
		Reason:     b.Reason,
		TS:         b.TS,
		RecordedAt: time.Now().UnixMilli(),
	}
	evictOldestHealth(hf.Sites, maxHealthSites)
	return writeBrowserHealthFile(path, hf)
}

// loadBrowserHealthFile reads the health file, tolerating absence / bad JSON
// by returning an empty file (best-effort recovery).
func loadBrowserHealthFile(path string) browserHealthFile {
	var hf browserHealthFile
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path derived from the daemon's own config dir.
	if err != nil {
		return hf
	}
	_ = json.Unmarshal(raw, &hf)
	return hf
}

// writeBrowserHealthFile writes the health file 0600 via a temp file + rename
// so a concurrent reader never sees a half-written file.
func writeBrowserHealthFile(path string, hf browserHealthFile) error {
	raw, err := json.MarshalIndent(hf, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// evictOldestHealth trims the map to at most max entries, dropping the oldest
// recorded first (a bound on a hostile/looping extension).
func evictOldestHealth(sites map[string]browserHealthEntry, max int) {
	for len(sites) > max {
		var oldestKey string
		var oldest int64 = 1<<63 - 1
		for k, v := range sites {
			if v.RecordedAt < oldest {
				oldest, oldestKey = v.RecordedAt, k
			}
		}
		delete(sites, oldestKey)
	}
}

// runBrowserHealth prints the recorded per-site health — the honest,
// in-scope operator surface for A6 (a dashboard card / GET /api/browser/health
// is a noted follow-up). An empty/missing file means the extension hasn't
// relayed anything: the manifest may be broken or the extension not loaded.
func runBrowserHealth(out io.Writer, configPath string) error {
	cfg, err := config.Load(config.LoadOptions{GlobalPath: configPath})
	if err != nil {
		return fmt.Errorf("browser health: config: %w", err)
	}
	path := filepath.Join(browserObserverDir(cfg), browserHealthFileName)
	hf := loadBrowserHealthFile(path)
	if len(hf.Sites) == 0 {
		fmt.Fprintf(out, "no browser health beacons recorded yet (%s)\n", path)
		fmt.Fprintln(out, "  if you've loaded the extension and used a site, the native-messaging host")
		fmt.Fprintln(out, "  or manifest may be broken — check `observer init --browser` output.")
		return nil
	}
	names := make([]string, 0, len(hf.Sites))
	for s := range hf.Sites {
		names = append(names, s)
	}
	sort.Strings(names)
	now := time.Now().UnixMilli()
	fmt.Fprintf(out, "browser capture health (%s):\n", path)
	for _, s := range names {
		e := hf.Sites[s]
		age := "unknown age"
		if e.RecordedAt > 0 {
			age = fmt.Sprintf("%s ago", time.Duration(now-e.RecordedAt)*time.Millisecond)
		}
		line := fmt.Sprintf("  %-14s %-9s %s", s, e.Status, age)
		if e.Reason != "" {
			line += "  — " + e.Reason
		}
		fmt.Fprintln(out, line)
	}
	return nil
}

// --- A4: shared loopback-ingress token -------------------------------------

// resolveBrowserIngestToken returns the shared token the loopback browser
// receiver requires. Precedence: an explicit [browser.listener].token, else
// an auto-generated token persisted 0600 as browser-ingest-token in the
// observer dir (so the clientless, default-off listener is never
// unauthenticated without the operator having to set anything).
func resolveBrowserIngestToken(cfg config.Config) (string, error) {
	if t := cfg.Browser.Listener.Token; t != "" {
		return t, nil
	}
	path := filepath.Join(browserObserverDir(cfg), "browser-ingest-token")
	if raw, err := os.ReadFile(path); err == nil { //nolint:gosec // G304: path derived from the daemon's own config dir.
		if tok := string(bytes.TrimSpace(raw)); tok != "" {
			return tok, nil
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	tok := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("mkdir: %w", err)
	}
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("persist token: %w", err)
	}
	return tok, nil
}

// browserSiteEnabled reports whether the daemon accepts this turn's site.
// The extension's own per-site toggle is the primary control; this is the
// daemon backstop. A missing key means enabled (fail-open); a key set false
// drops the site. A body we can't peek at (bad JSON) is left to the
// normalizer to reject with a real error, so we return true here.
func browserSiteEnabled(body []byte, bc config.BrowserConfig) bool {
	if len(bc.Sites) == 0 {
		return true
	}
	var peek struct {
		Site string `json:"site"`
	}
	if err := json.Unmarshal(body, &peek); err != nil || peek.Site == "" {
		return true
	}
	enabled, ok := bc.Sites[peek.Site]
	if !ok {
		return true // no explicit toggle → enabled
	}
	return enabled
}
