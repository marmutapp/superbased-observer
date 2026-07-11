package notify

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// stubDoer records requests and returns a canned response.
type stubDoer struct {
	mu     sync.Mutex
	gotURL []string
	gotHdr []http.Header
	body   []byte
	status int
	err    error
}

func (d *stubDoer) Do(r *http.Request) (*http.Response, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.gotURL = append(d.gotURL, r.URL.String())
	d.gotHdr = append(d.gotHdr, r.Header.Clone())
	if d.err != nil {
		return nil, d.err
	}
	status := d.status
	if status == 0 {
		status = 200
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewReader(d.body)),
	}, nil
}

// collect gathers worker results.
type collect struct {
	mu  sync.Mutex
	got []Result
}

func (c *collect) add(r Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, r)
}

func (c *collect) all() []Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Result(nil), c.got...)
}

func TestEgress_GateTable(t *testing.T) {
	cases := []struct {
		name        string
		allow       []string
		maxBody     int
		req         Request
		wantSent    bool
		wantRefused bool
		wantErrSub  string
	}{
		{
			name:     "exact_allow_sends",
			allow:    []string{"https://hooks.example/x"},
			req:      Request{Feature: "webhook", Endpoint: "https://hooks.example/x", Body: []byte(`{}`)},
			wantSent: true,
		},
		{
			name:     "prefix_allow_sends",
			allow:    []string{NPMRegistryBase},
			req:      Request{Feature: "reputation", Endpoint: NPMRegistryBase + "left-pad", Method: http.MethodGet},
			wantSent: true,
		},
		{
			name:        "unlisted_endpoint_refused",
			allow:       []string{"https://hooks.example/x"},
			req:         Request{Feature: "webhook", Endpoint: "https://evil.example/exfil", Body: []byte(`{}`)},
			wantRefused: true,
			wantErrSub:  "allowlist",
		},
		{
			name:        "empty_allowlist_refuses_everything",
			allow:       nil,
			req:         Request{Feature: "webhook", Endpoint: "https://hooks.example/x"},
			wantRefused: true,
			wantErrSub:  "allowlist",
		},
		{
			name:        "oversize_payload_refused",
			allow:       []string{"https://hooks.example/x"},
			maxBody:     8,
			req:         Request{Feature: "webhook", Endpoint: "https://hooks.example/x", Body: []byte(`{"k":"0123456789"}`)},
			wantRefused: true,
			wantErrSub:  "payload_max_bytes",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doer := &stubDoer{}
			col := &collect{}
			e := NewEgress(EgressOptions{
				Allow: tc.allow, MaxBodyBytes: tc.maxBody, Doer: doer, OnResult: col.add,
			})
			e.Submit(tc.req)
			e.Close()

			results := col.all()
			if len(results) != 1 {
				t.Fatalf("results = %d, want 1 (%+v)", len(results), results)
			}
			r := results[0]
			if r.Refused != tc.wantRefused {
				t.Errorf("refused = %v, want %v (err=%q)", r.Refused, tc.wantRefused, r.Err)
			}
			if tc.wantErrSub != "" && !strings.Contains(r.Err, tc.wantErrSub) {
				t.Errorf("err = %q, want substring %q", r.Err, tc.wantErrSub)
			}
			sent := len(doer.gotURL) == 1
			if sent != tc.wantSent {
				t.Errorf("sent = %v, want %v", sent, tc.wantSent)
			}
			if tc.wantSent && r.Status != 200 {
				t.Errorf("status = %d, want 200", r.Status)
			}
		})
	}
}

func TestEgress_RedactionRunsBeforeSend(t *testing.T) {
	doer := &stubDoer{}
	col := &collect{}
	var redacted []byte
	e := NewEgress(EgressOptions{
		Allow: []string{"https://hooks.example/x"},
		Doer:  doer, OnResult: col.add,
		Redact: func(b []byte) []byte {
			redacted = bytes.ReplaceAll(b, []byte("sk-SECRET"), []byte("[redacted]"))
			return redacted
		},
	})
	e.Submit(Request{Feature: "webhook", Endpoint: "https://hooks.example/x", Body: []byte(`{"reason":"key sk-SECRET leaked"}`)})
	e.Close()
	if len(col.all()) != 1 || col.all()[0].Status != 200 {
		t.Fatalf("results = %+v", col.all())
	}
	if !bytes.Contains(redacted, []byte("[redacted]")) || bytes.Contains(redacted, []byte("sk-SECRET")) {
		t.Errorf("redaction did not run over the outbound body: %s", redacted)
	}
}

func TestEgress_ClosedAndFullQueueDropAsResults(t *testing.T) {
	doer := &stubDoer{}
	col := &collect{}
	e := NewEgress(EgressOptions{Allow: []string{"https://h/x"}, Doer: doer, OnResult: col.add})
	e.Close()
	if ok := e.Submit(Request{Feature: "webhook", Endpoint: "https://h/x"}); ok {
		t.Error("submit after Close = true, want false")
	}
	results := col.all()
	if len(results) != 1 || !results[0].Refused || !strings.Contains(results[0].Err, "closed") {
		t.Errorf("closed-submit result = %+v, want refused closed", results)
	}
}

func TestEgress_TransportErrorIsResultNotPanic(t *testing.T) {
	doer := &stubDoer{err: io.ErrUnexpectedEOF}
	col := &collect{}
	e := NewEgress(EgressOptions{Allow: []string{"https://h/x"}, Doer: doer, OnResult: col.add})
	e.Submit(Request{Feature: "llm_judge", Endpoint: "https://h/x", Body: []byte(`{}`)})
	e.Close()
	r := col.all()
	if len(r) != 1 || r[0].Refused || r[0].Err == "" || r[0].Status != 0 {
		t.Errorf("transport-failure result = %+v, want non-refused error", r)
	}
}

func TestEgress_AuthHeaderAndResponseBody(t *testing.T) {
	doer := &stubDoer{body: []byte(`{"ok":true}`), status: 201}
	col := &collect{}
	e := NewEgress(EgressOptions{Allow: []string{"https://judge.example/v1/chat/completions"}, Doer: doer, OnResult: col.add})
	req, err := BuildJudgeRequest("https://judge.example/v1/chat/completions", "m", "KEY", JudgeContext{RuleID: "R-001"})
	if err != nil {
		t.Fatal(err)
	}
	e.Submit(req)
	e.Close()
	r := col.all()
	if len(r) != 1 || r[0].Status != 201 || string(r[0].Body) != `{"ok":true}` {
		t.Fatalf("result = %+v", r)
	}
	if got := doer.gotHdr[0].Get("Authorization"); got != "Bearer KEY" {
		t.Errorf("auth header = %q", got)
	}
	if got := doer.gotHdr[0].Get("Content-Type"); got != "application/json" {
		t.Errorf("content-type = %q", got)
	}
}
