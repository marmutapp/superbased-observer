package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

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
