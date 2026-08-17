package main

import (
	"context"
	"log/slog"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/routing"
	"github.com/marmutapp/superbased-observer/internal/selfobs/run"
)

func TestBuildSelfObsSink_DisabledReturnsNop(t *testing.T) {
	t.Parallel()
	sink, cleanup, err := buildSelfObsSink(config.Config{}, slog.Default())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	defer cleanup()
	if sink == nil {
		t.Fatal("sink must not be nil")
	}
	if err := sink.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
	if err := sink.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestBuildSelfObsSink_EnabledEmptyEndpointNop(t *testing.T) {
	t.Parallel()
	cfg := config.Config{
		SelfObs: config.SelfObsConfig{
			Enabled:  true,
			Endpoint: "",
			Token:    "kid.secret",
		},
	}
	sink, cleanup, err := buildSelfObsSink(cfg, slog.Default())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	defer cleanup()
	if sink == nil {
		t.Fatal("sink must not be nil")
	}
}

func TestBuildSelfObsSink_BadSchemeErrors(t *testing.T) {
	t.Parallel()
	cfg := config.Config{
		SelfObs: config.SelfObsConfig{
			Enabled:  true,
			Endpoint: "ftp://gateway.example/v1/traces",
			Token:    "kid.secret",
		},
	}
	sink, cleanup, err := buildSelfObsSink(cfg, slog.Default())
	if err == nil {
		cleanup()
		t.Fatal("expected scheme error")
	}
	if sink != nil {
		t.Fatal("sink must be nil on error")
	}
}

func TestEmitSelfObs_SampleN(t *testing.T) {
	t.Parallel()
	rec := &recordingSink{}
	lr := &liveRouter{
		selfobs:        rec,
		selfobsSampleN: 3,
	}
	d := routing.Decision{RuleName: "r1"}
	for i := 0; i < 9; i++ {
		lr.emitSelfObs("sess-1", routing.TurnKind("readonly"), d, true)
	}
	if got := rec.n; got != 3 {
		t.Fatalf("emitted %d, want 3 (1-of-3 over 9)", got)
	}
}

func TestEmitSelfObs_SampleZeroOff(t *testing.T) {
	t.Parallel()
	rec := &recordingSink{}
	lr := &liveRouter{
		selfobs:        rec,
		selfobsSampleN: 0,
	}
	lr.emitSelfObs("sess-1", routing.TurnKind("readonly"), routing.Decision{}, true)
	if rec.n != 0 {
		t.Fatalf("emitted %d with sampleN=0, want 0", rec.n)
	}
}

type recordingSink struct {
	n int
}

func (r *recordingSink) Emit(_ context.Context, _ run.DecisionRun) { r.n++ }
func (r *recordingSink) ForceFlush(context.Context) error          { return nil }
func (r *recordingSink) Shutdown(context.Context) error            { return nil }
