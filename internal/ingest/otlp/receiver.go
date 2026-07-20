package otlp

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
)

// maxBodyBytes caps an OTLP/HTTP request body to bound memory on a malformed or
// hostile request to the loopback listener.
const maxBodyBytes = 16 << 20 // 16 MiB

// ErrNonLoopback is returned by New when an address is not loopback and
// AllowNonLoopback is false (the network-posture guard, §2.2 / L3).
var ErrNonLoopback = errors.New("ingest/otlp: refusing non-loopback bind without AllowNonLoopback")

// Handler ingests one decoded OTLP logs export. It is injected by the daemon
// (ccotel.ParseLogs → store.UpsertTurnByRequestID) so this package carries no
// dependency on the Claude Code schema or the store. A returned error is logged
// and surfaced to the client as an OTLP partial failure, but never panics the
// receiver.
type Handler func(ctx context.Context, req *collogspb.ExportLogsServiceRequest) error

// TraceHandler ingests one decoded OTLP trace export. Optional; injected by
// the generalized-observability subsystem (internal/obs) when [observability]
// is enabled, mirroring Handler's posture. Like Handler it carries NO schema
// or store dependency — the receiver stays generic. A nil TraceHandler means
// /v1/traces and the gRPC TraceService are simply not served.
type TraceHandler func(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) error

// Options configures a Receiver. At least one of Handler / TraceHandler must
// be set; each signal (logs / traces) is served only when its handler is.
type Options struct {
	// GRPCAddr / HTTPAddr are host:port binds. Empty disables that transport.
	GRPCAddr string
	HTTPAddr string
	// AllowNonLoopback permits a non-loopback bind (default false — see §2.2).
	AllowNonLoopback bool
	Handler          Handler
	TraceHandler     TraceHandler
	Logger           *slog.Logger
}

// Receiver owns the OTLP gRPC + HTTP listeners and their lifecycle.
type Receiver struct {
	opts       Options
	grpcServer *grpc.Server
	grpcLn     net.Listener
	httpServer *http.Server
	httpLn     net.Listener
}

// New validates the options, enforces the loopback posture, and opens the
// configured listeners (so a bind conflict fails fast at construction, before
// the daemon reports itself healthy). Call Start to begin serving.
func New(opts Options) (*Receiver, error) {
	if opts.Handler == nil && opts.TraceHandler == nil {
		return nil, errors.New("ingest/otlp: at least one of Handler / TraceHandler is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.GRPCAddr == "" && opts.HTTPAddr == "" {
		return nil, errors.New("ingest/otlp: at least one of GRPCAddr/HTTPAddr is required")
	}
	r := &Receiver{opts: opts}

	if opts.GRPCAddr != "" {
		if err := guardLoopback(opts.GRPCAddr, opts.AllowNonLoopback); err != nil {
			return nil, err
		}
		ln, err := net.Listen("tcp", opts.GRPCAddr)
		if err != nil {
			return nil, fmt.Errorf("ingest/otlp: grpc listen %s: %w", opts.GRPCAddr, err)
		}
		r.grpcLn = ln
		r.grpcServer = grpc.NewServer()
		if opts.Handler != nil {
			collogspb.RegisterLogsServiceServer(r.grpcServer, &logsService{handler: opts.Handler, logger: opts.Logger})
		}
		if opts.TraceHandler != nil {
			coltracepb.RegisterTraceServiceServer(r.grpcServer, &traceService{handler: opts.TraceHandler, logger: opts.Logger})
		}
	}

	if opts.HTTPAddr != "" {
		if err := guardLoopback(opts.HTTPAddr, opts.AllowNonLoopback); err != nil {
			r.closeListeners()
			return nil, err
		}
		ln, err := net.Listen("tcp", opts.HTTPAddr)
		if err != nil {
			r.closeListeners()
			return nil, fmt.Errorf("ingest/otlp: http listen %s: %w", opts.HTTPAddr, err)
		}
		r.httpLn = ln
		mux := http.NewServeMux()
		if opts.Handler != nil {
			mux.HandleFunc("/v1/logs", r.handleHTTPLogs)
		}
		if opts.TraceHandler != nil {
			mux.HandleFunc("/v1/traces", r.handleHTTPTraces)
		}
		r.httpServer = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	}
	return r, nil
}

// Start begins serving on the opened listeners in background goroutines and
// returns immediately. Use Shutdown to stop.
func (r *Receiver) Start() {
	if r.grpcServer != nil {
		go func() {
			if err := r.grpcServer.Serve(r.grpcLn); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
				r.opts.Logger.Warn("ingest/otlp: grpc serve ended", "err", err)
			}
		}()
		r.opts.Logger.Info("ingest/otlp: gRPC receiver listening", "addr", r.grpcLn.Addr().String(),
			"logs", r.opts.Handler != nil, "traces", r.opts.TraceHandler != nil)
	}
	if r.httpServer != nil {
		go func() {
			if err := r.httpServer.Serve(r.httpLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				r.opts.Logger.Warn("ingest/otlp: http serve ended", "err", err)
			}
		}()
		r.opts.Logger.Info("ingest/otlp: HTTP receiver listening", "addr", r.httpLn.Addr().String(),
			"logs", r.opts.Handler != nil, "traces", r.opts.TraceHandler != nil)
	}
}

// Shutdown gracefully stops both servers. It is safe to call once.
func (r *Receiver) Shutdown(ctx context.Context) error {
	if r.grpcServer != nil {
		r.grpcServer.GracefulStop()
	}
	if r.httpServer != nil {
		if err := r.httpServer.Shutdown(ctx); err != nil {
			return fmt.Errorf("ingest/otlp: http shutdown: %w", err)
		}
	}
	return nil
}

// GRPCAddr / HTTPAddr report the resolved bind addresses (useful when a :0
// ephemeral port was requested, e.g. in tests). Empty when that transport is
// disabled.
func (r *Receiver) GRPCAddr() string {
	if r.grpcLn == nil {
		return ""
	}
	return r.grpcLn.Addr().String()
}

func (r *Receiver) HTTPAddr() string {
	if r.httpLn == nil {
		return ""
	}
	return r.httpLn.Addr().String()
}

func (r *Receiver) closeListeners() {
	if r.grpcLn != nil {
		_ = r.grpcLn.Close()
	}
	if r.httpLn != nil {
		_ = r.httpLn.Close()
	}
}

// handleHTTPLogs decodes an OTLP/HTTP protobuf logs export (optionally gzipped),
// runs the handler, and replies with a marshaled ExportLogsServiceResponse.
func (r *Receiver) handleHTTPLogs(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body io.Reader = http.MaxBytesReader(w, req.Body, maxBodyBytes)
	if req.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(body)
		if err != nil {
			http.Error(w, "bad gzip", http.StatusBadRequest)
			return
		}
		defer func() { _ = gz.Close() }()
		body = gz
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var in collogspb.ExportLogsServiceRequest
	if err := proto.Unmarshal(raw, &in); err != nil {
		http.Error(w, "bad protobuf", http.StatusBadRequest)
		return
	}
	if err := r.opts.Handler(req.Context(), &in); err != nil {
		r.opts.Logger.Warn("ingest/otlp: http handler error", "err", err)
		http.Error(w, "ingest failed", http.StatusInternalServerError)
		return
	}
	out, err := proto.Marshal(&collogspb.ExportLogsServiceResponse{})
	if err != nil {
		http.Error(w, "marshal response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(out)
}

// handleHTTPTraces decodes an OTLP/HTTP protobuf trace export (optionally
// gzipped), runs the trace handler, and replies with a marshaled
// ExportTraceServiceResponse. Mirrors handleHTTPLogs exactly.
func (r *Receiver) handleHTTPTraces(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body io.Reader = http.MaxBytesReader(w, req.Body, maxBodyBytes)
	if req.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(body)
		if err != nil {
			http.Error(w, "bad gzip", http.StatusBadRequest)
			return
		}
		defer func() { _ = gz.Close() }()
		body = gz
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var in coltracepb.ExportTraceServiceRequest
	if err := proto.Unmarshal(raw, &in); err != nil {
		http.Error(w, "bad protobuf", http.StatusBadRequest)
		return
	}
	if err := r.opts.TraceHandler(req.Context(), &in); err != nil {
		r.opts.Logger.Warn("ingest/otlp: http trace handler error", "err", err)
		http.Error(w, "ingest failed", http.StatusInternalServerError)
		return
	}
	out, err := proto.Marshal(&coltracepb.ExportTraceServiceResponse{})
	if err != nil {
		http.Error(w, "marshal response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(out)
}

// traceService is the gRPC TraceService implementation; Export defers to the
// injected trace handler.
type traceService struct {
	coltracepb.UnimplementedTraceServiceServer
	handler TraceHandler
	logger  *slog.Logger
}

func (s *traceService) Export(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	if err := s.handler(ctx, req); err != nil {
		s.logger.Warn("ingest/otlp: grpc trace handler error", "err", err)
		return nil, err
	}
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

// logsService is the gRPC LogsService implementation; Export defers to the
// injected handler.
type logsService struct {
	collogspb.UnimplementedLogsServiceServer
	handler Handler
	logger  *slog.Logger
}

func (s *logsService) Export(ctx context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	if err := s.handler(ctx, req); err != nil {
		s.logger.Warn("ingest/otlp: grpc handler error", "err", err)
		return nil, err
	}
	return &collogspb.ExportLogsServiceResponse{}, nil
}

// guardLoopback enforces the network posture: a bind address must resolve to
// loopback unless the operator explicitly allowed otherwise.
func guardLoopback(addr string, allow bool) error {
	if allow {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("ingest/otlp: bad addr %q: %w", addr, err)
	}
	if host == "" || host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("%w: %q", ErrNonLoopback, addr)
	}
	return nil
}
