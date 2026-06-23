package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRouter is a scriptable ModelRouter seam double.
type fakeRouter struct {
	mu       sync.Mutex
	verdict  RouterVerdict
	panics   bool
	decides  []RouterShape
	sessions []RouterSession
	served   []servedRecord
}

type servedRecord struct {
	token  int64
	turnID int64
	model  string
}

func (f *fakeRouter) Decide(shape RouterShape, sess RouterSession) RouterVerdict {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.panics {
		panic("router exploded")
	}
	f.decides = append(f.decides, shape)
	f.sessions = append(f.sessions, sess)
	return f.verdict
}

func (f *fakeRouter) RecordServed(token, turnID int64, model string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.served = append(f.served, servedRecord{token, turnID, model})
}

func (f *fakeRouter) snapshot() ([]RouterShape, []RouterSession, []servedRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]RouterShape{}, f.decides...),
		append([]RouterSession{}, f.sessions...),
		append([]servedRecord{}, f.served...)
}

const routerReqBody = `{"model":"claude-opus-4-8","max_tokens":64,"stream":false,` +
	`"metadata":{"user_id":"{\"session_id\":\"sess-router\"}"},` +
	`"system":[{"type":"text","text":"be brief","cache_control":{"type":"ephemeral"}}],` +
	`"messages":[{"role":"user","content":"the word model: \"claude-fake\" appears in text"}]}`

const routerRespBody = `{"id":"msg_r1","model":"claude-haiku-4-5","stop_reason":"end_turn",` +
	`"usage":{"input_tokens":10,"output_tokens":5}}`

// routerTestProxy wires a proxy with the fake router against a
// capture-everything Anthropic upstream.
func routerTestProxy(t *testing.T, router ModelRouter, status int) (*httptest.Server, *fakeSink, *string, func()) {
	t.Helper()
	var seenBody string
	anthUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seenBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(routerRespBody))
	}))
	sink := &fakeSink{}
	p, err := New(Options{
		AnthropicUpstream: anthUp.URL,
		OpenAIUpstream:    anthUp.URL,
		Sink:              sink,
		ModelRouter:       router,
	})
	if err != nil {
		anthUp.Close()
		t.Fatalf("proxy.New: %v", err)
	}
	ts := httptest.NewServer(p.Handler())
	return ts, sink, &seenBody, func() { ts.Close(); anthUp.Close() }
}

func postRouterRequest(t *testing.T, ts *httptest.Server, auth string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", ts.URL+"/v1/messages", strings.NewReader(routerReqBody))
	req.Header.Set("Content-Type", "application/json")
	if strings.HasPrefix(auth, "Bearer") {
		req.Header.Set("Authorization", auth)
	} else if auth != "" {
		req.Header.Set("X-Api-Key", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	return resp
}

// waitServed polls for the detached RecordServed call.
func waitServed(t *testing.T, fr *fakeRouter, want int) []servedRecord {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, _, served := fr.snapshot()
		if len(served) >= want {
			return served
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, _, served := fr.snapshot()
	t.Fatalf("RecordServed calls = %d, want %d", len(served), want)
	return nil
}

// TestRouter_NilRouterByteIdenticalPassThrough pins the enabled=false /
// unwired contract: with no router set, the upstream receives the
// EXACT original bytes (the routing code path doesn't run at all).
func TestRouter_NilRouterByteIdenticalPassThrough(t *testing.T) {
	ts, _, seenBody, cleanup := routerTestProxy(t, nil, 200)
	defer cleanup()
	resp := postRouterRequest(t, ts, "sk-ant-api-key")
	defer resp.Body.Close()
	if *seenBody != routerReqBody {
		t.Errorf("upstream body diverged with nil router:\n got %q\nwant %q", *seenBody, routerReqBody)
	}
}

// TestRouter_AdviseModeTouchesNothing pins mode=off/advise: the router
// is consulted (decision rows are its side), the request is forwarded
// byte-identical, and the landed turn links back via RecordServed.
func TestRouter_AdviseModeTouchesNothing(t *testing.T) {
	fr := &fakeRouter{verdict: RouterVerdict{Apply: false, SelectedModel: "claude-haiku-4-5", Token: 7}}
	ts, sink, seenBody, cleanup := routerTestProxy(t, fr, 200)
	defer cleanup()
	resp := postRouterRequest(t, ts, "sk-ant-api-key")
	defer resp.Body.Close()

	if *seenBody != routerReqBody {
		t.Errorf("advise mode mutated the body:\n got %q\nwant %q", *seenBody, routerReqBody)
	}
	shapes, sessions, _ := fr.snapshot()
	if len(shapes) != 1 || shapes[0].Model != "claude-opus-4-8" || shapes[0].ToolUseCount != 0 {
		t.Errorf("Decide shape = %+v", shapes)
	}
	if sessions[0].Provider != "anthropic" || sessions[0].SessionID != "sess-router" ||
		sessions[0].Entitlement != "api_key" {
		t.Errorf("Decide session = %+v", sessions[0])
	}
	served := waitServed(t, fr, 1)
	if served[0].token != 7 || served[0].turnID == 0 || served[0].model != "claude-haiku-4-5" {
		// model from the RESPONSE body — the model that actually served.
		t.Errorf("RecordServed = %+v", served[0])
	}
	if turns := sink.all(); len(turns) != 1 {
		t.Fatalf("turns = %d", len(turns))
	}
}

// TestRouter_EnforceRewritesModelOnly pins the §R11.1/§R11.2 enforce
// path: the upstream body carries the selected model, every other
// byte of the document is identical (metadata, session ids,
// cache_control, stream flag, the in-content "model:" text), the
// response passes through untouched, and api_turns.model records the
// SERVED model.
func TestRouter_EnforceRewritesModelOnly(t *testing.T) {
	fr := &fakeRouter{verdict: RouterVerdict{Apply: true, SelectedModel: "claude-haiku-4-5", Token: 9}}
	ts, sink, seenBody, cleanup := routerTestProxy(t, fr, 200)
	defer cleanup()
	resp := postRouterRequest(t, ts, "sk-ant-api-key")
	defer resp.Body.Close()
	gotResp, _ := io.ReadAll(resp.Body)
	if string(gotResp) != routerRespBody {
		t.Errorf("response mutated: %q", gotResp)
	}

	wantBody, ok := rewriteTopLevelModel([]byte(routerReqBody), "claude-haiku-4-5")
	if !ok {
		t.Fatal("reference splice failed")
	}
	if *seenBody != string(wantBody) {
		t.Errorf("upstream body:\n got %q\nwant %q", *seenBody, wantBody)
	}
	// Integrity: everything except the model value is byte-identical.
	if !strings.Contains(*seenBody, `"metadata":{"user_id":"{\"session_id\":\"sess-router\"}"}`) ||
		!strings.Contains(*seenBody, `"cache_control":{"type":"ephemeral"}`) ||
		!strings.Contains(*seenBody, `"stream":false`) ||
		!strings.Contains(*seenBody, `the word model: \"claude-fake\" appears in text`) {
		t.Errorf("non-model fields not preserved: %q", *seenBody)
	}
	if strings.Contains(*seenBody, `"model":"claude-opus-4-8"`) {
		t.Error("original model survived the rewrite")
	}

	turns := sink.all()
	if len(turns) != 1 || turns[0].Model != "claude-haiku-4-5" {
		t.Fatalf("api_turn model = %+v, want served claude-haiku-4-5", turns)
	}
	served := waitServed(t, fr, 1)
	if served[0].token != 9 || served[0].model != "claude-haiku-4-5" {
		t.Errorf("RecordServed = %+v", served[0])
	}
}

// TestRouter_ErrorTurnRecordsServedModel pins §R11.1 on the error
// path: a 429 from upstream lands an error turn carrying the model
// the request was SERVED as (the rewrite target).
func TestRouter_ErrorTurnRecordsServedModel(t *testing.T) {
	fr := &fakeRouter{verdict: RouterVerdict{Apply: true, SelectedModel: "claude-haiku-4-5", Token: 3}}
	ts, sink, _, cleanup := routerTestProxy(t, fr, 429)
	defer cleanup()
	resp := postRouterRequest(t, ts, "sk-ant-api-key")
	defer resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Fatalf("status = %d, want the upstream 429 passed through", resp.StatusCode)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(sink.all()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	turns := sink.all()
	if len(turns) != 1 || turns[0].Model != "claude-haiku-4-5" || turns[0].HTTPStatus != 429 {
		t.Fatalf("error turn = %+v, want served model + 429", turns)
	}
}

// TestRouter_PanicFailsOpen is the §R25 fault-injection row: a router
// implementation that panics must not break the turn — the original
// request forwards, the client gets the upstream response, the turn
// records.
func TestRouter_PanicFailsOpen(t *testing.T) {
	fr := &fakeRouter{panics: true}
	ts, sink, seenBody, cleanup := routerTestProxy(t, fr, 200)
	defer cleanup()
	resp := postRouterRequest(t, ts, "sk-ant-api-key")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (fail-open)", resp.StatusCode)
	}
	if *seenBody != routerReqBody {
		t.Errorf("panicking router still mutated the body")
	}
	if turns := sink.all(); len(turns) != 1 || turns[0].Model != "claude-haiku-4-5" {
		t.Errorf("turn after panic = %+v", turns)
	}
}

// TestRouter_ApplyWithoutChangeIsNoop: Apply=true but the selected
// model equals the original — no splice, byte-identical forward.
func TestRouter_ApplyWithoutChangeIsNoop(t *testing.T) {
	fr := &fakeRouter{verdict: RouterVerdict{Apply: true, SelectedModel: "claude-opus-4-8", Token: 1}}
	ts, _, seenBody, cleanup := routerTestProxy(t, fr, 200)
	defer cleanup()
	resp := postRouterRequest(t, ts, "sk-ant-api-key")
	defer resp.Body.Close()
	if *seenBody != routerReqBody {
		t.Errorf("no-change verdict still mutated the body")
	}
}

// TestRouter_SubscriptionEntitlementResolved pins §R11.3 boundary
// resolution rows: OAuth tokens and ChatGPT JWTs classify as
// subscription; platform keys as api_key; bare requests unknown.
func TestRouter_SubscriptionEntitlementResolved(t *testing.T) {
	cases := []struct {
		name string
		auth string
		want string
	}{
		{"anthropic_api_key_header", "sk-ant-api-key", "api_key"},
		{"claude_oauth", "Bearer sk-ant-oat01-abcdef", "subscription"},
		{"platform_key", "Bearer sk-proj-abc", "api_key"},
		{"chatgpt_jwt", "Bearer eyJhbGciOi", "subscription"},
		{"no_auth", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fr := &fakeRouter{}
			ts, _, _, cleanup := routerTestProxy(t, fr, 200)
			defer cleanup()
			resp := postRouterRequest(t, ts, tc.auth)
			resp.Body.Close()
			_, sessions, _ := fr.snapshot()
			if len(sessions) != 1 || sessions[0].Entitlement != tc.want {
				t.Errorf("entitlement = %+v, want %q", sessions, tc.want)
			}
		})
	}
}

// TestRewriteTopLevelModel_Rows — one row per splice behavior (§R11.2).
func TestRewriteTopLevelModel_Rows(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		body  string
		ok    bool
		check func(t *testing.T, out string)
	}{
		{
			name: "basic_first_key",
			body: `{"model":"a","x":1}`,
			ok:   true,
			check: func(t *testing.T, out string) {
				if out != `{"model":"new-model","x":1}` {
					t.Errorf("out = %q", out)
				}
			},
		},
		{
			name: "model_not_first_key",
			body: `{"stream":true,"model":"a","x":[1,2]}`,
			ok:   true,
			check: func(t *testing.T, out string) {
				if out != `{"stream":true,"model":"new-model","x":[1,2]}` {
					t.Errorf("out = %q", out)
				}
			},
		},
		{
			name: "nested_model_key_untouched",
			body: `{"messages":[{"model":"inner","content":"hi"}],"model":"a"}`,
			ok:   true,
			check: func(t *testing.T, out string) {
				if !strings.Contains(out, `{"model":"inner","content":"hi"}`) {
					t.Errorf("nested model touched: %q", out)
				}
				if !strings.HasSuffix(out, `"model":"new-model"}`) {
					t.Errorf("top-level model not rewritten: %q", out)
				}
			},
		},
		{
			name: "model_as_value_string_elsewhere",
			body: `{"a":"model","model":"a"}`,
			ok:   true,
			check: func(t *testing.T, out string) {
				if out != `{"a":"model","model":"new-model"}` {
					t.Errorf("out = %q", out)
				}
			},
		},
		{
			name: "whitespace_form",
			body: "{\n  \"model\" : \"a\",\n  \"x\": 1\n}",
			ok:   true,
			check: func(t *testing.T, out string) {
				var m map[string]any
				if err := json.Unmarshal([]byte(out), &m); err != nil {
					t.Fatalf("output unparseable: %v\n%q", err, out)
				}
				if m["model"] != "new-model" || m["x"] != float64(1) {
					t.Errorf("out = %q", out)
				}
			},
		},
		{name: "missing_model", body: `{"x":1}`, ok: false},
		{name: "model_value_not_string", body: `{"model":{"nested":true}}`, ok: false},
		{name: "malformed", body: `{"model":"a"`, ok: false},
		{name: "array_document", body: `[{"model":"a"}]`, ok: false},
		{name: "empty", body: ``, ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, ok := rewriteTopLevelModel([]byte(tc.body), "new-model")
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (out=%q)", ok, tc.ok, out)
			}
			if ok {
				var m any
				if err := json.Unmarshal(out, &m); err != nil {
					t.Fatalf("rewritten body unparseable: %v\n%q", err, out)
				}
				if tc.check != nil {
					tc.check(t, string(out))
				}
			}
		})
	}
}
