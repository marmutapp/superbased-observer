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
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
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
