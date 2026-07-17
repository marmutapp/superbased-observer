package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// TestToolFromHeaders pins the L1 header → tool signature table (D20
// fallback) row by row: every verified client signal resolves its
// normalized tool, unbranded SDK traffic (the cline-cli shape) stays a
// clean miss, and kilo rows shadow the opencode rows it forked from.
func TestToolFromHeaders(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		headers  map[string]string
		wantTool string
		wantOK   bool
	}{
		{
			name:     "claude-code UA",
			headers:  map[string]string{"User-Agent": "claude-cli/2.1.167 (external, cli)"},
			wantTool: models.ToolClaudeCode,
			wantOK:   true,
		},
		{
			name:     "codex UA",
			headers:  map[string]string{"User-Agent": "codex_cli_rs/0.133.0 (Windows 11; x86_64) WindowsTerminal"},
			wantTool: models.ToolCodex,
			wantOK:   true,
		},
		{
			name:     "codex Originator header with generic UA",
			headers:  map[string]string{"User-Agent": "reqwest/0.12", "Originator": "codex_cli_rs"},
			wantTool: models.ToolCodex,
			wantOK:   true,
		},
		{
			name:     "kilo anthropic-path UA",
			headers:  map[string]string{"User-Agent": "Kilo-Code/7.3.40"},
			wantTool: models.ToolKiloCodeCLI,
			wantOK:   true,
		},
		{
			name:     "kilo gateway X-Title with Bun default UA",
			headers:  map[string]string{"User-Agent": "Bun/1.2.5", "X-Title": "Kilo Code"},
			wantTool: models.ToolKiloCodeCLI,
			wantOK:   true,
		},
		{
			name: "kilo X-Title shadows inherited opencode UA",
			headers: map[string]string{
				"User-Agent": "opencode/0.6.3",
				"X-Title":    "Kilo Code",
			},
			wantTool: models.ToolKiloCodeCLI,
			wantOK:   true,
		},
		{
			name:     "opencode UA",
			headers:  map[string]string{"User-Agent": "opencode/0.6.3"},
			wantTool: models.ToolOpenCode,
			wantOK:   true,
		},
		{
			name:     "opencode X-Title",
			headers:  map[string]string{"User-Agent": "Bun/1.2.5", "X-Title": "opencode"},
			wantTool: models.ToolOpenCode,
			wantOK:   true,
		},
		{
			name:     "case-insensitive prefix",
			headers:  map[string]string{"User-Agent": "CLAUDE-CLI/2.0.0"},
			wantTool: models.ToolClaudeCode,
			wantOK:   true,
		},
		{
			name:    "stock Anthropic SDK UA stays a miss (cline-cli shape)",
			headers: map[string]string{"User-Agent": "anthropic-sdk-typescript/0.39.0"},
		},
		{
			name:    "axios UA stays a miss",
			headers: map[string]string{"User-Agent": "axios/1.7.2"},
		},
		{
			name:    "no headers at all",
			headers: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := http.Header{}
			for k, v := range tc.headers {
				h.Set(k, v)
			}
			tool, ok := toolFromHeaders(h)
			if tool != tc.wantTool || ok != tc.wantOK {
				t.Errorf("toolFromHeaders(%v) = (%q, %v), want (%q, %v)", tc.headers, tool, ok, tc.wantTool, tc.wantOK)
			}
		})
	}
}

// TestProxy_RequestClass_HeaderToolFallback pins the D20 L1 contract
// end-to-end through the real proxy assembly: a pidbridge hit always
// wins over header evidence, a bridge miss resolves the tool from a
// recognized signature, and a request with neither degrades to the
// per-provider tier (Tool "") exactly like before.
func TestProxy_RequestClass_HeaderToolFallback(t *testing.T) {
	cases := []struct {
		name       string
		bridgeTool string
		headers    map[string]string
		wantTool   string
	}{
		{
			name:       "bridge entry wins over header signature",
			bridgeTool: models.ToolClaudeCode,
			headers:    map[string]string{"User-Agent": "codex_cli_rs/0.133.0 (linux; x86_64)"},
			wantTool:   models.ToolClaudeCode,
		},
		{
			name:     "bridge miss resolves tool from UA",
			headers:  map[string]string{"User-Agent": "codex_cli_rs/0.133.0 (linux; x86_64)"},
			wantTool: models.ToolCodex,
		},
		{
			name:     "bridge miss resolves tool from X-Title",
			headers:  map[string]string{"User-Agent": "Bun/1.2.5", "X-Title": "Kilo Code"},
			wantTool: models.ToolKiloCodeCLI,
		},
		{
			name:     "bridge miss + unrecognized UA degrades to provider tier",
			headers:  map[string]string{"User-Agent": "anthropic-sdk-typescript/0.39.0"},
			wantTool: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			anth := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"m","model":"claude-opus-4-8","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
			})
			anthUp := httptest.NewServer(anth)
			defer anthUp.Close()

			comp := &fakeClassAwareCompressor{}
			p, err := New(Options{
				AnthropicUpstream: anthUp.URL,
				OpenAIUpstream:    anthUp.URL,
				Sink:              &fakeSink{},
				Compressor:        comp,
				SessionResolver:   &fakeToolResolver{sid: "bridge-sess", tool: tc.bridgeTool},
			})
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}
			srv := httptest.NewServer(p.Handler())
			defer srv.Close()

			body := `{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hi"}]}`
			req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			_ = resp.Body.Close()

			comp.mu.Lock()
			defer comp.mu.Unlock()
			if comp.calls != 1 {
				t.Fatalf("compressor calls: %d, want 1", comp.calls)
			}
			if comp.class.Tool != tc.wantTool {
				t.Errorf("tool: got %q want %q", comp.class.Tool, tc.wantTool)
			}
		})
	}
}
