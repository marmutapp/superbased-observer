package otel

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/marmutapp/superbased-observer/internal/models"
)

func quietLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

type fakeSource struct{ turns []models.APITurn }

func (f *fakeSource) SubscribeAPITurns(_ context.Context, _ time.Duration, _ *slog.Logger) <-chan models.APITurn {
	ch := make(chan models.APITurn, len(f.turns))
	for _, t := range f.turns {
		ch <- t
	}
	close(ch)
	return ch
}

type fakeResolver struct{ roots map[int64]string }

func (f *fakeResolver) ProjectRootByID(_ context.Context, id int64) (string, error) {
	return f.roots[id], nil
}

// newTestExporter wires an Exporter around a synchronous in-memory provider so
// span emission is deterministic and inspectable without a network endpoint.
func newTestExporter(cfg Config, src TurnSource, res ProjectResolver) (*Exporter, *tracetest.InMemoryExporter) {
	inMem := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(inMem),
		sdktrace.WithResource(newResource("test")),
	)
	return newExporter(cfg.withEnvOverrides(), src, res, quietLogger(), tp), inMem
}

func TestExporter_EmitsSpanPerTurn(t *testing.T) {
	src := &fakeSource{turns: []models.APITurn{
		{
			ID: 1, Provider: "anthropic", Model: "model-a", RequestID: "r1",
			InputTokens: 10, OutputTokens: 5, ProjectID: 7, SessionID: "s1",
			StopReason: "end_turn", TotalResponseMS: 1200, Timestamp: time.Now(),
		},
		{
			ID: 2, Provider: "openai", Model: "model-b", HTTPStatus: 429,
			ErrorClass: "rate_limit_error", ErrorMessage: "slow down", ProjectID: 7,
		},
	}}
	res := &fakeResolver{roots: map[int64]string{7: "/home/me/proj"}}

	e, inMem := newTestExporter(Config{}, src, res)
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Capture before Stop: tracetest's in-memory exporter resets on Shutdown.
	spans := inMem.GetSpans()
	if err := e.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2", len(spans))
	}

	ok := spans[0]
	if ok.Name != "chat model-a" {
		t.Errorf("span 0 name = %q, want %q", ok.Name, "chat model-a")
	}
	m := attrMap(ok.Attributes)
	if m[keyGenAIRequestModel].AsString() != "model-a" {
		t.Errorf("request model = %q", m[keyGenAIRequestModel].AsString())
	}
	if m[keySBOProjectRoot].AsString() != "/home/me/proj" {
		t.Errorf("project root not resolved/attached: %q", m[keySBOProjectRoot].AsString())
	}
	if ok.Status.Code != codes.Unset {
		t.Errorf("ok span status = %v, want Unset", ok.Status.Code)
	}
	// 1200ms duration from TotalResponseMS.
	if d := ok.EndTime.Sub(ok.StartTime); d != 1200*time.Millisecond {
		t.Errorf("span duration = %v, want 1.2s", d)
	}

	errSpan := spans[1]
	if errSpan.Status.Code != codes.Error {
		t.Errorf("error span status = %v, want Error", errSpan.Status.Code)
	}

	// Prompt content OFF by default → no inference-details event on either span.
	for _, s := range spans {
		for _, ev := range s.Events {
			if ev.Name == eventInferenceDetails {
				t.Errorf("inference-details event must be absent when EmitPromptContent=false")
			}
		}
	}
}

func TestExporter_PromptContentEventWhenEnabled(t *testing.T) {
	src := &fakeSource{turns: []models.APITurn{
		{
			ID: 1, Provider: "anthropic", Model: "m", SystemPromptHash: "abc123",
			MessagePrefixHash: "def456", Timestamp: time.Now(),
		},
	}}
	e, inMem := newTestExporter(Config{EmitPromptContent: true}, src, &fakeResolver{})
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	spans := inMem.GetSpans() // capture before Stop (resets on Shutdown)
	_ = e.Stop(context.Background())
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	var found bool
	for _, ev := range spans[0].Events {
		if ev.Name == eventInferenceDetails {
			found = true
			m := attrMap(ev.Attributes)
			if m["sbo.system_prompt.hash"].AsString() != "abc123" {
				t.Errorf("event missing system prompt hash: %v", ev.Attributes)
			}
		}
	}
	if !found {
		t.Error("inference-details event must be present when EmitPromptContent=true")
	}
}

func TestExporter_RootCacheReusesLookup(t *testing.T) {
	src := &fakeSource{turns: []models.APITurn{
		{ID: 1, Provider: "anthropic", Model: "m", ProjectID: 9, Timestamp: time.Now()},
		{ID: 2, Provider: "anthropic", Model: "m", ProjectID: 9, Timestamp: time.Now()},
	}}
	res := &countingResolver{roots: map[int64]string{9: "/p"}}
	e, _ := newTestExporter(Config{}, src, res)
	_ = e.Start(context.Background())
	_ = e.Stop(context.Background())
	if res.calls != 1 {
		t.Errorf("resolver called %d times for one project id, want 1 (cache miss only)", res.calls)
	}
}

type countingResolver struct {
	roots map[int64]string
	calls int
}

func (c *countingResolver) ProjectRootByID(_ context.Context, id int64) (string, error) {
	c.calls++
	return c.roots[id], nil
}

func TestNewResource_StampsEchoGuardAttribute(t *testing.T) {
	r := newResource("test")
	var found bool
	for _, kv := range r.Attributes() {
		if string(kv.Key) == "sbo.emitted_by" && kv.Value.AsString() == "observer" {
			found = true
		}
	}
	if !found {
		t.Fatal("newResource must stamp sbo.emitted_by=observer for the §3.2 echo guard")
	}
}
