package antigravity

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/http2"

	"github.com/marmutapp/superbased-observer/internal/platform/protowire"
)

// callConvertTrajectory POSTs a gRPC ConvertTrajectoryToMarkdown
// request to the language_server at endpoint and returns the
// extracted Markdown body.
//
// Endpoint shape: "http://127.0.0.1:<port>" or "https://127.0.0.1:<port>".
// Auth: x-codeium-csrf-token header carries the CSRF token from the
// language_server's --csrf_token cmdline arg.
//
// Request shape (per descriptor in language_server_windows_x64.exe):
//
//	message ConvertTrajectoryToMarkdownRequest {
//	  Trajectory trajectory = 1 [deprecated = true];
//	  string conversation_id = 2;
//	}
//
// Response shape:
//
//	message ConvertTrajectoryToMarkdownResponse {
//	  string markdown = 1;
//	}
//
// Returns:
//   - markdown plaintext on grpc-status=0
//   - error on transport failure or non-zero grpc-status
//
// The HTTP/2 transport accepts both plaintext (h2c) and TLS with
// self-signed certs (the language_server uses a self-signed cert it
// generates at startup).
func callConvertTrajectory(ctx context.Context, endpoint, csrf, conversationID string, timeout time.Duration) (string, error) {
	respBody, err := callLanguageServerMethod(ctx, endpoint, csrf, "ConvertTrajectoryToMarkdown", 2, conversationID, timeout)
	if err != nil {
		return "", fmt.Errorf("antigravity.callConvertTrajectory: %w", err)
	}
	return extractMarkdownFromGRPCResponse(respBody)
}

// callGetCascadeTrajectory POSTs a gRPC GetCascadeTrajectory request
// and returns the raw response payload (5-byte gRPC frame header
// stripped). The structured-trajectory wire format is documented in
// docs/handovers/antigravity-path-b-implementation-handoff-2026-05-04.md.
//
// Conversation_id sits in field 1 (not field 2 like
// ConvertTrajectoryToMarkdown). Returning raw bytes lets the caller
// wire-walk the response without forcing a generated-bindings
// dependency.
//
// On WSL2 this is dead code (the routing fix in recoverViaLocalGRPC
// short-circuits to the bridge), but kept symmetric with
// callConvertTrajectory so native macOS / native Windows / non-WSL
// Linux can use it directly.
func callGetCascadeTrajectory(ctx context.Context, endpoint, csrf, conversationID string, timeout time.Duration) ([]byte, error) {
	respBody, err := callLanguageServerMethod(ctx, endpoint, csrf, "GetCascadeTrajectory", 1, conversationID, timeout)
	if err != nil {
		return nil, fmt.Errorf("antigravity.callGetCascadeTrajectory: %w", err)
	}
	if len(respBody) < 5 {
		return nil, fmt.Errorf("antigravity.callGetCascadeTrajectory: response body too short (%d bytes)", len(respBody))
	}
	return respBody[5:], nil
}

// callLanguageServerMethod is the shared HTTP/2 + gRPC call shape
// used by every per-conversation method on LanguageServerService.
// fieldNum parameterises which protobuf field carries the
// conversation_id (2 for most methods, 1 for GetCascadeTrajectory*
// per Path B discovery).
func callLanguageServerMethod(ctx context.Context, endpoint, csrf, method string, fieldNum int, conversationID string, timeout time.Duration) ([]byte, error) {
	if conversationID == "" {
		return nil, errors.New("conversationID empty")
	}
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		return nil, fmt.Errorf("endpoint must include scheme: %s", endpoint)
	}

	body := protowire.AppendBytesField(nil, fieldNum, []byte(conversationID))
	frame := framedGRPC(body)

	url := strings.TrimRight(endpoint, "/") + "/exa.language_server_pb.LanguageServerService/" + method
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/grpc+proto")
	req.Header.Set("Te", "trailers")
	req.Header.Set("x-codeium-csrf-token", csrf)
	req.ContentLength = int64(len(frame))
	req.Body = io.NopCloser(byteReader(frame))

	client := buildHTTP2Client(strings.HasPrefix(endpoint, "https://"))
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	grpcStatus := resp.Trailer.Get("Grpc-Status")
	grpcMessage := resp.Trailer.Get("Grpc-Message")
	if grpcStatus != "" && grpcStatus != "0" {
		return nil, fmt.Errorf("grpc-status=%s message=%q", grpcStatus, grpcMessage)
	}
	return respBody, nil
}

// extractMarkdownFromGRPCResponse strips the 5-byte gRPC frame
// header and parses the inner ConvertTrajectoryToMarkdownResponse
// for its `markdown` (field 1) string.
func extractMarkdownFromGRPCResponse(body []byte) (string, error) {
	if len(body) < 5 {
		return "", errors.New("response body too short")
	}
	// Skip 1-byte compression flag + 4-byte BE length.
	payload := body[5:]
	var markdown string
	err := protowire.Walk(payload, func(f protowire.Field) error {
		if f.Depth == 0 && f.FieldNumber == 1 && f.WireType == protowire.WireBytes {
			markdown = string(f.Bytes)
			return errStopWalk
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopWalk) {
		return "", fmt.Errorf("antigravity: parse response: %w", err)
	}
	if markdown == "" {
		return "", errors.New("antigravity: response missing markdown field")
	}
	return markdown, nil
}

var errStopWalk = errors.New("stop walk")

func framedGRPC(payload []byte) []byte {
	frame := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	return frame
}

// buildHTTP2Client constructs an http.Client that speaks HTTP/2,
// either over TLS (skipping cert verify — language_server uses a
// self-signed cert) or in plaintext h2c.
func buildHTTP2Client(useTLS bool) *http.Client {
	if useTLS {
		return &http.Client{Transport: &http2.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // language_server self-signs at runtime
		}}
	}
	return &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, network, addr)
		},
	}}
}

// byteReader wraps a byte slice as an io.Reader; we can't use
// bytes.NewReader because http.Request.Body must implement
// io.ReadCloser via NopCloser.
func byteReader(b []byte) io.Reader { return &simpleReader{b: b} }

type simpleReader struct {
	b []byte
	o int
}

func (r *simpleReader) Read(p []byte) (int, error) {
	if r.o >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.o:])
	r.o += n
	return n, nil
}
