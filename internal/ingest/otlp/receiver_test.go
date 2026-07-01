package otlp

import (
	"bytes"
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// sample builds a minimal export with one record carrying a marker attribute.
func sample(marker string) *collogspb.ExportLogsServiceRequest {
	return &collogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{{
			ScopeLogs: []*logspb.ScopeLogs{{
				LogRecords: []*logspb.LogRecord{{
					EventName: marker,
				}},
			}},
		}},
	}
}

type capture struct {
	mu   sync.Mutex
	reqs []*collogspb.ExportLogsServiceRequest
}

func (c *capture) handler(_ context.Context, req *collogspb.ExportLogsServiceRequest) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, req)
	return nil
}

func (c *capture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reqs)
}

func TestReceiver_HTTPAndGRPCReachHandler(t *testing.T) {
	cap := &capture{}
	r, err := New(Options{
		GRPCAddr: "127.0.0.1:0",
		HTTPAddr: "127.0.0.1:0",
		Handler:  cap.handler,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.Start()
	defer func() { _ = r.Shutdown(context.Background()) }()

	// HTTP path.
	raw, _ := proto.Marshal(sample("http-marker"))
	resp, err := http.Post("http://"+r.HTTPAddr()+"/v1/logs", "application/x-protobuf", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("http post: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("http status = %d, want 200", resp.StatusCode)
	}

	// gRPC path.
	conn, err := grpc.NewClient(r.GRPCAddr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := collogspb.NewLogsServiceClient(conn).Export(context.Background(), sample("grpc-marker")); err != nil {
		t.Fatalf("grpc export: %v", err)
	}

	// Both deliveries should have reached the handler.
	deadline := time.Now().Add(2 * time.Second)
	for cap.count() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if cap.count() != 2 {
		t.Fatalf("handler saw %d exports, want 2", cap.count())
	}
}

// sampleTrace builds a minimal trace export with one named span.
func sampleTrace(name string) *coltracepb.ExportTraceServiceRequest {
	return &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			ScopeSpans: []*tracepb.ScopeSpans{{
				Spans: []*tracepb.Span{{TraceId: []byte{0x01}, SpanId: []byte{0x02}, Name: name}},
			}},
		}},
	}
}

// TestReceiver_TraceHTTPAndGRPCReachHandler confirms the trace receiver serves
// /v1/traces (HTTP) + the gRPC TraceService and routes to the injected
// TraceHandler — the P2 mirror of the logs path.
func TestReceiver_TraceHTTPAndGRPCReachHandler(t *testing.T) {
	var mu sync.Mutex
	var n int
	th := func(_ context.Context, _ *coltracepb.ExportTraceServiceRequest) error {
		mu.Lock()
		n++
		mu.Unlock()
		return nil
	}
	r, err := New(Options{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", TraceHandler: th})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.Start()
	defer func() { _ = r.Shutdown(context.Background()) }()

	raw, _ := proto.Marshal(sampleTrace("http-span"))
	resp, err := http.Post("http://"+r.HTTPAddr()+"/v1/traces", "application/x-protobuf", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("http post: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("http status = %d, want 200", resp.StatusCode)
	}

	conn, err := grpc.NewClient(r.GRPCAddr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := coltracepb.NewTraceServiceClient(conn).Export(context.Background(), sampleTrace("grpc-span")); err != nil {
		t.Fatalf("grpc export: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for func() int { mu.Lock(); defer mu.Unlock(); return n }() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	got := n
	mu.Unlock()
	if got != 2 {
		t.Fatalf("trace handler saw %d exports, want 2", got)
	}
}

func TestReceiver_HTTPRejectsBadProto(t *testing.T) {
	r, err := New(Options{HTTPAddr: "127.0.0.1:0", Handler: func(context.Context, *collogspb.ExportLogsServiceRequest) error { return nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.Start()
	defer func() { _ = r.Shutdown(context.Background()) }()

	resp, err := http.Post("http://"+r.HTTPAddr()+"/v1/logs", "application/x-protobuf", bytes.NewReader([]byte("not-proto")))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestNew_RefusesNonLoopback(t *testing.T) {
	_, err := New(Options{
		GRPCAddr: "0.0.0.0:4317",
		Handler:  func(context.Context, *collogspb.ExportLogsServiceRequest) error { return nil },
	})
	if err == nil {
		t.Fatal("expected ErrNonLoopback for 0.0.0.0 bind")
	}
}

func TestNew_AllowNonLoopbackOptIn(t *testing.T) {
	// With the explicit opt-in, a non-loopback bind is permitted (we still bind
	// to a loopback addr here so the test doesn't actually expose a port).
	r, err := New(Options{
		HTTPAddr:         "127.0.0.1:0",
		AllowNonLoopback: true,
		Handler:          func(context.Context, *collogspb.ExportLogsServiceRequest) error { return nil },
	})
	if err != nil {
		t.Fatalf("New with AllowNonLoopback: %v", err)
	}
	_ = r.Shutdown(context.Background())
}
