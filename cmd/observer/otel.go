package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
	otelexp "github.com/marmutapp/superbased-observer/internal/exporter/otel"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// buildOTelExporter constructs the agent-side OpenTelemetry exporter (Teams M4)
// from the [exporter.otel] config. The exporter tails api_turns independently
// of the dashboard and org client (it needs M0 only and never couples to the
// org server), so it opens its own DB handle; the returned cleanup closes it.
//
// Returns a nil exporter with enabled=false when the section is disabled — the
// solo-local default — so no OTLP client is built and no network call is made.
func buildOTelExporter(ctx context.Context, configPath string) (e *otelexp.Exporter, cleanup func(), enabled bool, err error) {
	cfg, err := config.Load(config.LoadOptions{GlobalPath: configPath})
	if err != nil {
		return nil, func() {}, false, fmt.Errorf("load config: %w", err)
	}
	if !cfg.Exporter.OTel.Enabled {
		return nil, func() {}, false, nil
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Observer.DBPath), 0o755); err != nil {
		return nil, func() {}, false, fmt.Errorf("ensure db dir: %w", err)
	}
	database, err := db.Open(ctx, db.Options{Path: cfg.Observer.DBPath})
	if err != nil {
		return nil, func() {}, false, fmt.Errorf("open db %s: %w", cfg.Observer.DBPath, err)
	}

	st := store.New(database)
	oc := cfg.Exporter.OTel
	exp, err := otelexp.New(otelexp.Config{
		Endpoint:          oc.Endpoint,
		Insecure:          oc.Insecure,
		PollInterval:      time.Duration(oc.PollIntervalSeconds) * time.Second,
		EmitPromptContent: oc.EmitPromptContent,
		EmitUserEmail:     oc.EmitUserEmail,
		SemconvStability:  oc.SemconvStability,
	}, st, st, newLogger(cfg.Observer.LogLevel))
	if err != nil {
		_ = database.Close()
		return nil, func() {}, false, fmt.Errorf("build otel exporter: %w", err)
	}
	return exp, func() { _ = database.Close() }, true, nil
}
