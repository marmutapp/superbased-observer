package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/provenance"
	"github.com/marmutapp/superbased-observer/internal/selfobs/emit"
	"github.com/marmutapp/superbased-observer/internal/selfobs/run"
)

// buildSelfObsSink constructs the P1-10 platform self-observability emit
// sink from [selfobs] (docs/plans/plane-a-p1-10-production-retrofit-plan.md
// Phase A). Returns emit.Nop() when disabled or when endpoint/credential
// are absent — never a nil interface. cleanup is ForceFlush+Shutdown; safe
// to call on the Nop sink.
func buildSelfObsSink(cfg config.Config, logger *slog.Logger) (sink emit.Sink, cleanup func(), err error) {
	if logger == nil {
		logger = slog.Default()
	}
	if !cfg.SelfObs.Enabled {
		return emit.Nop(), func() {}, nil
	}
	sink, err = emit.New(emit.Config{
		Endpoint:    cfg.SelfObs.Endpoint,
		KeyID:       cfg.SelfObs.KeyID,
		Secret:      cfg.SelfObs.Secret,
		Token:       cfg.SelfObs.Token,
		Insecure:    cfg.SelfObs.Insecure,
		ServiceName: cfg.SelfObs.ServiceName,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("buildSelfObsSink: %w", err)
	}
	logger.Info("selfobs: sink ready",
		"endpoint_set", cfg.SelfObs.Endpoint != "",
		"routing_sample_n", cfg.SelfObs.RoutingSampleN)
	cleanup = func() {
		ctx := context.Background()
		_ = sink.ForceFlush(ctx)
		_ = sink.Shutdown(ctx)
	}
	return sink, cleanup, nil
}

// emitSelfObsRun fire-and-forgets one DecisionRun when a sink is present.
// Nil/Nop sinks are safe; never blocks callers beyond the sink's async path.
func emitSelfObsRun(sink emit.Sink, r run.DecisionRun) {
	if sink == nil {
		return
	}
	if r.InitiatedBy == "" {
		r.InitiatedBy = provenance.ActorHuman
	}
	sink.Emit(context.Background(), r)
}
