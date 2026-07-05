package notify

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Egress worker (guard spec §15): the ONE owner for all guard cloud
// network I/O. Every webhook, LLM-judge call and reputation lookup is
// submitted here; nothing else under internal/guard opens a socket.
// The worker is fail-soft by contract — a cloud outage degrades to
// core (local) behaviour and never blocks a caller: Submit is
// non-blocking, sends happen on a single background goroutine, and
// every outcome (sent, refused, failed, dropped) is reported through
// OnResult so the composition layer can record it as a guard event
// with the §15 `source` attribution.
//
// Outbound-data posture (§15.1): the worker enforces the endpoint
// allowlist (the set of CONFIG-DECLARED endpoints — webhook URLs, the
// judge endpoint, the reputation registry; an unconfigured destination
// cannot be reached even by a coding error), the payload_max_bytes
// cap, and a redaction pass over every body before it leaves the
// process. Refusals are results, not errors.

// egressTimeout bounds one outbound HTTP call.
const egressTimeout = 10 * time.Second

// egressMaxResponse bounds how much of a response body is retained
// for the caller (the judge needs the body; nothing needs megabytes).
const egressMaxResponse = 64 << 10

// defaultQueueSize bounds the submit queue; the queue full case drops
// the NEW request (alerting is best-effort, and the oldest queued
// items are closest to delivery).
const defaultQueueSize = 64

// Request is one outbound cloud call.
type Request struct {
	// Feature attributes the call for logging ("webhook" |
	// "llm_judge" | "reputation") — it becomes the guard event's
	// source field at the composition layer.
	Feature string
	// Endpoint is the absolute URL. It must match the allowlist.
	Endpoint string
	// Method defaults to POST when empty.
	Method string
	// Headers are added verbatim (Content-Type defaults to
	// application/json for bodied requests).
	Headers map[string]string
	// Body is the payload; it passes the redaction hook and the
	// size cap before sending.
	Body []byte
	// Tag is an opaque caller correlation id echoed back on the
	// Result (the judge dispatcher keys pending reviews by it).
	Tag string
}

// Result reports one settled egress call (delivered, failed, refused
// or dropped). Err is empty on HTTP delivery regardless of status —
// the status code is the caller's signal.
type Result struct {
	Request Request
	// Status is the HTTP status (0 when the call never went out).
	Status int
	// Body is the bounded response body (judge responses ride here).
	Body []byte
	// Err is the transport/refusal description ("" on delivery).
	Err string
	// Refused is true when the gate (allowlist, size cap, queue
	// full, closed worker) stopped the request BEFORE any network
	// I/O — the §15 "endpoint allowlist" and fail-soft contracts
	// made observable.
	Refused bool
}

// Doer is the injectable HTTP transport seam.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// EgressOptions configures NewEgress.
type EgressOptions struct {
	// Allow is the endpoint allowlist: a request is sendable when
	// its URL equals an entry exactly OR starts with an entry that
	// ends in "/" (prefix form, for path-carrying registries like
	// https://registry.npmjs.org/). Empty list = nothing sendable.
	Allow []string
	// MaxBodyBytes is [guard.cloud].payload_max_bytes (≤0 = the
	// config default 4096).
	MaxBodyBytes int
	// Redact, when non-nil, runs over every body before sending
	// (§15.1 — the composition layer wires the scrub pass).
	Redact func([]byte) []byte
	// OnResult, when non-nil, receives every settled Result on the
	// worker goroutine (callers must not block long).
	OnResult func(Result)
	// Doer overrides the HTTP client (tests). nil = a client with
	// the production timeout.
	Doer Doer
	// QueueSize overrides the submit-queue bound (≤0 = default).
	QueueSize int
}

// Egress is the background sender. Construct with NewEgress; Close
// stops the worker after draining in-flight work.
type Egress struct {
	opts  EgressOptions
	queue chan Request

	mu     sync.Mutex
	closed bool
	done   chan struct{}
}

// NewEgress starts the single worker goroutine.
func NewEgress(opts EgressOptions) *Egress {
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = 4096
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = defaultQueueSize
	}
	if opts.Doer == nil {
		opts.Doer = &http.Client{Timeout: egressTimeout}
	}
	e := &Egress{
		opts:  opts,
		queue: make(chan Request, opts.QueueSize),
		done:  make(chan struct{}),
	}
	go e.run()
	return e
}

// Submit enqueues one request, never blocking: a full queue or a
// closed worker drops the request with a Refused result. The boolean
// mirrors that result for callers that care inline.
func (e *Egress) Submit(r Request) bool {
	e.mu.Lock()
	closed := e.closed
	e.mu.Unlock()
	if closed {
		e.report(Result{Request: r, Refused: true, Err: "egress closed"})
		return false
	}
	select {
	case e.queue <- r:
		return true
	default:
		e.report(Result{Request: r, Refused: true, Err: "egress queue full"})
		return false
	}
}

// Close stops accepting work, drains the queue, and waits for the
// worker to exit.
func (e *Egress) Close() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		<-e.done
		return
	}
	e.closed = true
	e.mu.Unlock()
	close(e.queue)
	<-e.done
}

// run is the worker loop.
func (e *Egress) run() {
	defer close(e.done)
	for r := range e.queue {
		e.report(e.send(r))
	}
}

// send performs the gate checks and the HTTP call for one request.
func (e *Egress) send(r Request) Result {
	if !endpointAllowed(e.opts.Allow, r.Endpoint) {
		return Result{Request: r, Refused: true, Err: "endpoint not in the configured allowlist"}
	}
	body := r.Body
	if e.opts.Redact != nil && len(body) > 0 {
		body = e.opts.Redact(body)
	}
	if len(body) > e.opts.MaxBodyBytes {
		return Result{Request: r, Refused: true, Err: "payload exceeds payload_max_bytes"}
	}
	method := r.Method
	if method == "" {
		method = http.MethodPost
	}
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, r.Endpoint, reader)
	if err != nil {
		return Result{Request: r, Err: "build request: " + err.Error()}
	}
	if len(body) > 0 && r.Headers["Content-Type"] == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range r.Headers {
		req.Header.Set(k, v)
	}
	resp, err := e.opts.Doer.Do(req)
	if err != nil {
		return Result{Request: r, Err: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, egressMaxResponse))
	return Result{Request: r, Status: resp.StatusCode, Body: respBody}
}

// report delivers a result to OnResult when wired.
func (e *Egress) report(res Result) {
	if e.opts.OnResult != nil {
		e.opts.OnResult(res)
	}
}

// endpointAllowed implements the allowlist match: exact URL, or
// prefix when the allow entry ends in "/".
func endpointAllowed(allow []string, endpoint string) bool {
	for _, a := range allow {
		if a == "" {
			continue
		}
		if strings.HasSuffix(a, "/") {
			if strings.HasPrefix(endpoint, a) {
				return true
			}
			continue
		}
		if endpoint == a {
			return true
		}
	}
	return false
}
