package mcp

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/marmutapp/superbased-observer/internal/codegraph"
	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
)

// Tool is an MCP tool — a named callable with a JSON schema.
type Tool interface {
	Name() string
	Description() string
	InputSchema() map[string]any
	Invoke(ctx context.Context, args json.RawMessage) (any, error)
}

// Options configures a Server.
type Options struct {
	// DB is the observer's SQLite database (opened by internal/db.Open). The
	// built-in tools query it read-only for the most part.
	DB *sql.DB
	// Logger receives operational messages. stdout is reserved for the
	// JSON-RPC stream — the logger must write elsewhere (stderr by default).
	Logger *slog.Logger
	// ServerName, ServerVersion are echoed to clients during initialize.
	ServerName    string
	ServerVersion string
	// ExtraTools lets callers add custom tools on top of the built-in set.
	ExtraTools []Tool
	// CostEngine optionally overrides the built-in cost engine (pricing table
	// with baked-in defaults only). Pass a config-aware engine from the CLI
	// so user-configured pricing overrides flow into get_cost_summary.
	CostEngine *cost.Engine
	// CodegraphClient is the codegraph read-only client for structural
	// enrichment. When non-nil and Available(), check_file_freshness and
	// get_file_history include a `structure` block with functions, callers,
	// and imports.
	CodegraphClient *codegraph.Client
}

// Server is the MCP stdio JSON-RPC server. Safe for concurrent use after
// Run; Run itself is single-threaded (reads stdio serially).
type Server struct {
	db      *sql.DB
	logger  *slog.Logger
	name    string
	version string

	mu    sync.RWMutex
	tools map[string]Tool
}

// New constructs a Server and registers the built-in tools. DB is required.
func New(opts Options) (*Server, error) {
	if opts.DB == nil {
		return nil, errors.New("mcp.New: DB is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Server{
		db:      opts.DB,
		logger:  logger,
		name:    defaultIfEmpty(opts.ServerName, "observer"),
		version: defaultIfEmpty(opts.ServerVersion, "dev"),
		tools:   map[string]Tool{},
	}
	engine := opts.CostEngine
	if engine == nil {
		engine = cost.NewEngine(config.IntelligenceConfig{})
	}
	cg := opts.CodegraphClient
	if cg == nil {
		cg = codegraph.NewMissing()
	}
	for _, t := range builtinTools(opts.DB, cg) {
		s.Register(t)
	}
	for _, t := range extraBuiltinTools(opts.DB, engine) {
		s.Register(t)
	}
	for _, t := range opts.ExtraTools {
		s.Register(t)
	}
	return s, nil
}

// Register adds a tool by name. A subsequent Register with the same name
// replaces the earlier entry.
func (s *Server) Register(t Tool) {
	s.mu.Lock()
	s.tools[t.Name()] = t
	s.mu.Unlock()
}

// Run reads newline-delimited JSON-RPC 2.0 messages from r and writes
// responses to w until r is closed or ctx is done. The bufio.Scanner buffer
// is extended so large tool/call requests don't trip MaxScanTokenSize.
func (s *Server) Run(ctx context.Context, r io.Reader, w io.Writer) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)

	// Serialize writes — even though Run itself reads serially, a future
	// notification stream (e.g. tools/list_changed) would write from another
	// goroutine.
	var writeMu sync.Mutex
	writeResp := func(resp rpcResponse) {
		writeMu.Lock()
		defer writeMu.Unlock()
		if err := enc.Encode(resp); err != nil {
			s.logger.Warn("mcp: write response", "err", err)
		}
	}

	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			writeResp(rpcResponse{
				JSONRPC: "2.0",
				Error:   &rpcError{Code: errParseError, Message: "parse error: " + err.Error()},
			})
			continue
		}
		if req.JSONRPC != "2.0" {
			writeResp(rpcResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &rpcError{Code: errInvalidRequest, Message: "jsonrpc must be '2.0'"},
			})
			continue
		}
		isNotification := len(req.ID) == 0

		result, rpcErr := s.dispatch(ctx, req)

		if isNotification {
			// JSON-RPC notifications get no response.
			continue
		}
		resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
		if rpcErr != nil {
			resp.Error = rpcErr
		} else {
			resp.Result = result
		}
		writeResp(resp)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("mcp.Run: scan: %w", err)
	}
	return nil
}

// dispatch routes a parsed request to its handler. Returns either a result
// or an rpcError (never both).
func (s *Server) dispatch(ctx context.Context, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(req.Params)
	case "notifications/initialized":
		return nil, nil // no-op, notification
	case "ping":
		return struct{}{}, nil
	case "tools/list":
		return s.handleToolsList()
	case "tools/call":
		return s.handleToolsCall(ctx, req.Params)
	default:
		return nil, &rpcError{Code: errMethodNotFound, Message: "method not found: " + req.Method}
	}
}

func (s *Server) handleInitialize(params json.RawMessage) (any, *rpcError) {
	var p initializeParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: errInvalidParams, Message: "invalid initialize params: " + err.Error()}
		}
	}
	return initializeResult{
		ProtocolVersion: protocolVersion,
		Capabilities: serverCapabilities{
			Tools: &toolsCapability{ListChanged: false},
		},
		ServerInfo: serverInfo{Name: s.name, Version: s.version},
	}, nil
}

func (s *Server) handleToolsList() (any, *rpcError) {
	s.mu.RLock()
	names := make([]string, 0, len(s.tools))
	for n := range s.tools {
		names = append(names, n)
	}
	s.mu.RUnlock()
	sort.Strings(names)

	out := make([]toolDescriptor, 0, len(names))
	s.mu.RLock()
	for _, n := range names {
		t := s.tools[n]
		out = append(out, toolDescriptor{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: t.InputSchema(),
		})
	}
	s.mu.RUnlock()
	return toolsListResult{Tools: out}, nil
}

func (s *Server) handleToolsCall(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p toolCallParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: errInvalidParams, Message: "invalid tools/call params: " + err.Error()}
	}
	s.mu.RLock()
	tool, ok := s.tools[p.Name]
	s.mu.RUnlock()
	if !ok {
		return nil, &rpcError{Code: errMethodNotFound, Message: "unknown tool: " + p.Name}
	}
	result, err := tool.Invoke(ctx, p.Arguments)
	if err != nil {
		// Tool errors are returned as in-band tool-result errors (isError=true),
		// not JSON-RPC transport errors — per MCP spec, so the model can see
		// the failure and recover.
		return toolCallResult{
			Content: []toolContent{{Type: "text", Text: err.Error()}},
			IsError: true,
		}, nil
	}
	body, mErr := json.MarshalIndent(result, "", "  ")
	if mErr != nil {
		return nil, &rpcError{Code: errInternalError, Message: "marshal tool result: " + mErr.Error()}
	}
	return toolCallResult{
		Content: []toolContent{{Type: "text", Text: string(body)}},
	}, nil
}

func defaultIfEmpty(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
