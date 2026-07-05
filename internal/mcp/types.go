package mcp

import "encoding/json"

// Protocol version negotiated in initialize. We announce the latest supported
// version; if the client asks for an older one, we respond with the version
// we understand and let the client decide whether to continue.
const protocolVersion = "2025-06-18"

// rpcRequest is a JSON-RPC 2.0 request envelope. A request with no ID is a
// notification (no response expected).
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// rpcResponse is a JSON-RPC 2.0 response envelope. Exactly one of Result or
// Error is populated.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Standard JSON-RPC error codes.
const (
	errParseError     = -32700
	errInvalidRequest = -32600
	errMethodNotFound = -32601
	errInvalidParams  = -32602
	errInternalError  = -32603
)

// initializeParams mirrors the MCP initialize request body. We only read the
// fields we care about; unknown keys are ignored.
type initializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"clientInfo"`
}

// initializeResult is the server's handshake response.
type initializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    serverCapabilities `json:"capabilities"`
	ServerInfo      serverInfo         `json:"serverInfo"`
}

type serverCapabilities struct {
	Tools *toolsCapability `json:"tools,omitempty"`
}

type toolsCapability struct {
	ListChanged bool `json:"listChanged"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// toolDescriptor is the shape of a tools/list entry.
type toolDescriptor struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// toolsListResult is the response to tools/list.
type toolsListResult struct {
	Tools []toolDescriptor `json:"tools"`
}

// toolCallParams is the request body for tools/call.
type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// toolContent is a single content block in a tool/call response. We always
// return a single JSON content block — the MCP spec permits multiple (e.g.
// text + image) but our tools are strictly structured data.
type toolContent struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

// toolCallResult is the response to tools/call. Tools return arbitrary JSON;
// the server serializes it into a single "text" content block (pretty JSON)
// so the host can both render it for humans and machine-parse it.
type toolCallResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}
