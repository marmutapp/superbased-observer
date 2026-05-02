package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/compression/conversation"
	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/pidbridge"
	"github.com/marmutapp/superbased-observer/internal/proxy"
	"github.com/marmutapp/superbased-observer/internal/scrub"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// newProxyCmd returns the `proxy` subcommand group. Currently only hosts
// `proxy start`; room for `proxy status` / `proxy stop` if we adopt a
// long-running daemon model later.
func newProxyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "proxy",
		Short: "Run the API reverse proxy for accurate token capture",
	}
	cmd.AddCommand(newProxyStartCmd())
	return cmd
}

func newProxyStartCmd() *cobra.Command {
	var (
		configPath string
		port       int
		bindAddr   string
	)
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the proxy on localhost (point ANTHROPIC_BASE_URL / OPENAI_BASE_URL here)",
		Long: "Starts the API reverse proxy on localhost:<port>.\n" +
			"Claude Code: ANTHROPIC_BASE_URL=http://localhost:<port>\n" +
			"Codex:       OPENAI_BASE_URL=http://localhost:<port>/v1",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			p, cleanup, addr, err := buildProxy(ctx, configPath, port, bindAddr)
			if err != nil {
				return err
			}
			defer cleanup()

			fmt.Fprintf(cmd.OutOrStdout(), "proxy listening on %s — ctrl-c to stop\n", addr)
			if err := p.ListenAndServe(ctx, addr); err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	cmd.Flags().IntVar(&port, "port", 0, "Override [proxy].port")
	cmd.Flags().StringVar(&bindAddr, "bind", "127.0.0.1", "Bind address (default localhost only)")
	return cmd
}

// buildProxy loads config, opens the DB, constructs the Proxy wired to the
// storage layer, and returns the Proxy + cleanup closure + resolved listen
// address. The db is closed by the cleanup.
func buildProxy(ctx context.Context, configPath string, portOverride int, bindAddr string) (*proxy.Proxy, func(), string, error) {
	cfg, err := config.Load(config.LoadOptions{GlobalPath: configPath})
	if err != nil {
		return nil, nil, "", fmt.Errorf("load config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Observer.DBPath), 0o755); err != nil {
		return nil, nil, "", fmt.Errorf("ensure db dir: %w", err)
	}
	database, err := db.Open(ctx, db.Options{Path: cfg.Observer.DBPath})
	if err != nil {
		return nil, nil, "", fmt.Errorf("open db %s: %w", cfg.Observer.DBPath, err)
	}
	s := store.New(database)

	port := cfg.Proxy.Port
	if portOverride > 0 {
		port = portOverride
	}
	if port <= 0 || port > 65535 {
		_ = database.Close()
		return nil, nil, "", fmt.Errorf("proxy: port %d out of range", port)
	}

	logger := newLogger(cfg.Observer.LogLevel)

	// Wire the pid → session_id bridge. The SessionStart hook writes
	// entries; the proxy resolves a client's TCP remote addr through
	// /proc into an ancestor pid that owns the bridge row. Prune once
	// at startup so rebooted machines don't carry stale entries.
	bridge := pidbridge.New(database)
	if _, err := bridge.Prune(ctx, 6*time.Hour); err != nil {
		logger.Warn("pidbridge prune", "err", err)
	}
	resolver := pidbridge.NewProcResolver(bridge, "", 30*time.Second)

	opts := proxy.Options{
		AnthropicUpstream: cfg.Proxy.AnthropicUpstream,
		OpenAIUpstream:    cfg.Proxy.OpenAIUpstream,
		Sink:              s,
		ObserverLog:       s,
		SessionResolver:   resolver,
		Logger:            logger,
	}
	if cfg.Compression.Conversation.Enabled {
		var scrubber *scrub.Scrubber
		if cfg.Observer.Secrets.EnableScrubbing {
			scrubber = scrub.NewWithExtra(cfg.Observer.Secrets.ExtraPatterns)
		} else {
			scrubber = scrub.New()
		}
		pipeline := conversation.NewPipeline(conversation.PipelineConfig{
			Enabled:       cfg.Compression.Conversation.Enabled,
			Mode:          cfg.Compression.Conversation.Mode,
			TargetRatio:   cfg.Compression.Conversation.TargetRatio,
			PreserveLastN: cfg.Compression.Conversation.PreserveLastN,
			CompressTypes: cfg.Compression.Conversation.CompressTypes,
		}, conversation.DefaultRegistry(), scrubber)
		opts.Compressor = pipelineAdapter{p: pipeline}
	}

	p, err := proxy.New(opts)
	if err != nil {
		_ = database.Close()
		return nil, nil, "", err
	}
	addr := net.JoinHostPort(bindAddr, strconv.Itoa(port))
	cleanup := func() { _ = database.Close() }
	return p, cleanup, addr, nil
}

// pipelineAdapter bridges conversation.Pipeline (no context parameter —
// the pipeline is a pure function by design) to proxy.Compressor which
// is context-aware. Lives in cmd/observer so the conversation package
// stays free of the proxy interface.
type pipelineAdapter struct {
	p *conversation.Pipeline
}

// Compress implements proxy.Compressor.
func (a pipelineAdapter) Compress(_ context.Context, provider string, body []byte) proxy.CompressionResult {
	r := a.p.Run(provider, body)
	events := make([]proxy.CompressionEvent, 0, len(r.Events))
	for _, e := range r.Events {
		events = append(events, proxy.CompressionEvent{
			Mechanism:       e.Mechanism,
			OriginalBytes:   e.OriginalBytes,
			CompressedBytes: e.CompressedBytes,
			MsgIndex:        e.MsgIndex,
			ImportanceScore: e.ImportanceScore,
		})
	}
	return proxy.CompressionResult{
		Body:              r.Body,
		Skipped:           r.Skipped,
		MessagePrefixHash: r.MessagePrefixHash,
		OriginalBytes:     r.OriginalBytes,
		CompressedBytes:   r.CompressedBytes,
		CompressedCount:   r.CompressedCount,
		DroppedCount:      r.DroppedCount,
		MarkerCount:       r.MarkerCount,
		Events:            events,
	}
}
