package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// reqRecord holds an in-flight completion request while its response streams.
type reqRecord struct {
	requestID string
	sessionID string // owning target session ("" = root page; set = worker)
	url       string
	method    string
	postData  string
	headers   map[string]string
	startedMs float64
	// streaming fallback (Gemini StreamGenerate): once streamResourceContent is
	// armed, dataReceived events carry base64 body chunks accumulated here.
	streaming bool
	streamBuf strings.Builder
}

// wsRecord accumulates WebSocket frames for a second-leg / chat socket.
type wsRecord struct {
	requestID string
	sessionID string // owning target session ("" = root page; set = worker)
	url       string
	sent      []string
	received  []string
	rawBuf    strings.Builder // accumulated inbound payloads (for extraction)
}

// tabWatcher listens on one page target's CDP connection and emits dump files
// for each captured completion turn.
type tabWatcher struct {
	site   *site
	tabURL string
	client *cdpClient
	coord  *coordinator

	mu        sync.Mutex
	reqs      map[string]*reqRecord
	wss       map[string]*wsRecord
	sessTypes map[string]string // sessionId → target type ("" = "page")
	stops     bool              // once mode: stop tracking new requests after first capture
}

func newTabWatcher(s *site, tabURL string, c *cdpClient, coord *coordinator) *tabWatcher {
	return &tabWatcher{
		site:      s,
		tabURL:    tabURL,
		client:    c,
		coord:     coord,
		reqs:      make(map[string]*reqRecord),
		wss:       make(map[string]*wsRecord),
		sessTypes: map[string]string{"": "page"},
	}
}

// sessionType returns the target type for a session id ("page" for the root,
// the worker/service-worker type for auto-attached children, "unknown" if we
// somehow saw an event before its attachedToTarget).
func (w *tabWatcher) sessionType(sessionID string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.sessTypes[sessionID]; ok {
		return t
	}
	return "unknown"
}

// run enables the Network domain and processes events until the connection
// closes or stop is signalled.
func (w *tabWatcher) run(stop <-chan struct{}) {
	// Large buffers reduce the chance a streamed body is evicted before
	// getResponseBody runs (a known CDP caveat for streaming SSE).
	if err := w.client.Send("Network.enable", map[string]interface{}{
		"maxTotalBufferSize":    200 * 1024 * 1024,
		"maxResourceBufferSize": 100 * 1024 * 1024,
	}, nil, 10*time.Second); err != nil {
		fmt.Printf("  [warn] %s: Network.enable failed: %v\n", w.site.Site, err)
		return
	}
	// Auto-attach to workers / service-workers (flatten mode) so a conduit
	// WebSocket opened from a worker target — e.g. ChatGPT's thinking-tier
	// second leg — is seen. Without this the page-target Network domain never
	// reports worker-opened sockets. Each attached child gets its own
	// Network.enable in onAttached; waitForDebuggerOnStart pauses the child so
	// we enable Network BEFORE it can open a socket (we always resume it).
	if err := w.client.Send("Target.setAutoAttach", map[string]interface{}{
		"autoAttach":             true,
		"flatten":                true,
		"waitForDebuggerOnStart": true,
	}, nil, 10*time.Second); err != nil {
		fmt.Printf("  [warn] %s: Target.setAutoAttach failed (worker sockets may be missed): %v\n", w.site.Site, err)
	}
	for {
		select {
		case <-stop:
			return
		case ev, ok := <-w.client.Events:
			if !ok {
				return
			}
			w.handle(ev)
		}
	}
}

func (w *tabWatcher) handle(ev cdpEvent) {
	switch ev.Method {
	case "Network.requestWillBeSent":
		w.onRequest(ev.SessionID, ev.Params)
	case "Network.responseReceived":
		w.onResponseReceived(ev.Params)
	case "Network.dataReceived":
		w.onDataReceived(ev.Params)
	case "Network.loadingFinished":
		w.onLoadingFinished(ev.Params)
	case "Network.loadingFailed":
		w.onLoadingFailed(ev.Params)
	case "Network.webSocketCreated":
		w.onWSCreated(ev.SessionID, ev.Params)
	case "Network.webSocketFrameSent":
		w.onWSFrame(ev.Params, true)
	case "Network.webSocketFrameReceived":
		w.onWSFrame(ev.Params, false)
	case "Network.webSocketClosed":
		w.onWSClosed(ev.Params)
	case "Target.attachedToTarget":
		w.onAttached(ev.Params)
	}
}

// onAttached wires a newly auto-attached child target (worker / service-worker)
// into the same capture: it records the target type, enables the Network domain
// on that session so worker-opened requests + WebSockets are reported, and
// propagates auto-attach to any nested workers. The child is paused
// (waitForDebuggerOnStart) — we ALWAYS resume it, even on error, so a stuck
// target can never hang the page.
func (w *tabWatcher) onAttached(params json.RawMessage) {
	var p struct {
		SessionID  string `json:"sessionId"`
		TargetInfo struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"targetInfo"`
		WaitingForDebugger bool `json:"waitingForDebugger"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.SessionID == "" {
		return
	}
	w.mu.Lock()
	w.sessTypes[p.SessionID] = p.TargetInfo.Type
	w.mu.Unlock()
	fmt.Printf("  [%s] ← attached %s target %s\n", w.site.Site, p.TargetInfo.Type, shorten(p.TargetInfo.URL, 70))

	if err := w.client.SendTo(p.SessionID, "Network.enable", map[string]interface{}{
		"maxTotalBufferSize":    200 * 1024 * 1024,
		"maxResourceBufferSize": 100 * 1024 * 1024,
	}, nil, 10*time.Second); err != nil {
		fmt.Printf("  [warn] %s: worker Network.enable failed: %v\n", w.site.Site, err)
	}
	// Nested workers: a worker can spawn its own children — propagate.
	_ = w.client.SendTo(p.SessionID, "Target.setAutoAttach", map[string]interface{}{
		"autoAttach":             true,
		"flatten":                true,
		"waitForDebuggerOnStart": true,
	}, nil, 10*time.Second)
	// Resume the paused target no matter what — never leave it waiting.
	_ = w.client.SendTo(p.SessionID, "Runtime.runIfWaitingForDebugger", nil, nil, 5*time.Second)
}

func (w *tabWatcher) onRequest(sessionID string, params json.RawMessage) {
	var p struct {
		RequestID string `json:"requestId"`
		Request   struct {
			URL         string            `json:"url"`
			Method      string            `json:"method"`
			PostData    string            `json:"postData"`
			HasPostData bool              `json:"hasPostData"`
			Headers     map[string]string `json:"headers"`
		} `json:"request"`
		Timestamp float64 `json:"timestamp"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	matched := w.site.matchesEndpoint(p.Request.URL)
	// Log EVERY request (matched or not) across page + worker targets.
	w.coord.urls.addRequest(w.site.Site, w.sessionType(sessionID), p.Request.Method, p.Request.URL, matched)
	if !matched {
		return
	}
	w.mu.Lock()
	if w.stops {
		w.mu.Unlock()
		return
	}
	rec := &reqRecord{
		requestID: p.RequestID,
		sessionID: sessionID,
		url:       p.Request.URL,
		method:    p.Request.Method,
		postData:  p.Request.PostData,
		headers:   lowerHeaders(p.Request.Headers),
		startedMs: p.Timestamp * 1000,
	}
	w.reqs[p.RequestID] = rec
	w.mu.Unlock()

	// Streaming sites (Gemini StreamGenerate): arm at the EARLIEST point so the
	// full body is accumulated even if getResponseBody later evicts it. This is
	// a best-effort early attempt — armStream is idempotent and onResponse
	// received retries it once headers arrive, in case the request is too fresh
	// for streamResourceContent here.
	if w.site.StreamBody {
		w.armStream(rec)
	}

	// Body may be omitted on the event for large posts — fetch it explicitly
	// (scoped to the owning session for worker requests).
	if rec.postData == "" && p.Request.HasPostData {
		var body struct {
			PostData string `json:"postData"`
		}
		if err := w.client.SendTo(sessionID, "Network.getRequestPostData",
			map[string]interface{}{"requestId": p.RequestID}, &body, 10*time.Second); err == nil {
			w.mu.Lock()
			rec.postData = body.PostData
			w.mu.Unlock()
		}
	}
	fmt.Printf("  [%s] ← request %s %s\n", w.site.Site, rec.method, shorten(rec.url, 90))
}

// onResponseReceived arms streamResourceContent for streaming-body sites once
// response headers arrive, so a long StreamGenerate envelope is accumulated even
// when getResponseBody later returns empty (buffer eviction). This is the
// second (guaranteed-valid) arm point: onRequest already tried at
// requestWillBeSent, but streamResourceContent can reject a request that has no
// response yet, so we retry here. armStream is idempotent. No-op for
// non-streaming sites.
func (w *tabWatcher) onResponseReceived(params json.RawMessage) {
	if !w.site.StreamBody {
		return
	}
	var p struct {
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	w.mu.Lock()
	rec := w.reqs[p.RequestID]
	w.mu.Unlock()
	if rec == nil {
		return
	}
	w.armStream(rec)
}

// armStream enables Network.streamResourceContent for a streaming request so
// dataReceived events start carrying base64 body chunks and any already-buffered
// bytes are captured up front. Idempotent (guarded by rec.streaming) and
// fail-soft: an error (unsupported build, or a request too fresh to stream)
// leaves streaming=false so a later call can retry — getResponseBody remains the
// primary path. Scoped to the request's owning session (worker-safe).
func (w *tabWatcher) armStream(rec *reqRecord) {
	w.mu.Lock()
	if rec.streaming {
		w.mu.Unlock()
		return
	}
	rec.streaming = true
	sessionID, reqID := rec.sessionID, rec.requestID
	w.mu.Unlock()

	var sr struct {
		BufferedData string `json:"bufferedData"`
	}
	if err := w.client.SendTo(sessionID, "Network.streamResourceContent",
		map[string]interface{}{"requestId": reqID}, &sr, 10*time.Second); err != nil {
		w.mu.Lock()
		rec.streaming = false
		w.mu.Unlock()
		return
	}
	if sr.BufferedData != "" {
		if dec, e := base64.StdEncoding.DecodeString(sr.BufferedData); e == nil {
			w.mu.Lock()
			rec.streamBuf.Write(dec)
			w.mu.Unlock()
		}
	}
}

// onDataReceived appends streamed body chunks for an armed streaming request.
// The base64 `data` field is only populated by Chrome once streamResourceContent
// has enabled streaming for that request.
func (w *tabWatcher) onDataReceived(params json.RawMessage) {
	if !w.site.StreamBody {
		return
	}
	var p struct {
		RequestID string `json:"requestId"`
		Data      string `json:"data"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Data == "" {
		return
	}
	w.mu.Lock()
	rec := w.reqs[p.RequestID]
	if rec == nil || !rec.streaming {
		w.mu.Unlock()
		return
	}
	if dec, e := base64.StdEncoding.DecodeString(p.Data); e == nil {
		rec.streamBuf.Write(dec)
	}
	w.mu.Unlock()
}

func (w *tabWatcher) onLoadingFinished(params json.RawMessage) {
	var p struct {
		RequestID string  `json:"requestId"`
		Timestamp float64 `json:"timestamp"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	w.mu.Lock()
	rec := w.reqs[p.RequestID]
	if rec != nil {
		delete(w.reqs, p.RequestID)
	}
	w.mu.Unlock()
	if rec == nil {
		return
	}

	var resp struct {
		Body          string `json:"body"`
		Base64Encoded bool   `json:"base64Encoded"`
	}
	err := w.client.SendTo(rec.sessionID, "Network.getResponseBody",
		map[string]interface{}{"requestId": p.RequestID}, &resp, 20*time.Second)
	body := ""
	getErr := ""
	if err != nil {
		// Streaming bodies can be evicted / unbuffered — flagged honestly.
		getErr = err.Error()
	} else if resp.Base64Encoded {
		if dec, e := base64.StdEncoding.DecodeString(resp.Body); e == nil {
			body = string(dec)
		} else {
			getErr = "base64 decode: " + e.Error()
		}
	} else {
		body = resp.Body
	}
	// Streaming fallback: if getResponseBody yielded nothing but we accumulated
	// the body over streamResourceContent/dataReceived, use that (long Gemini
	// StreamGenerate envelopes get evicted from the response buffer).
	w.mu.Lock()
	streamed := rec.streamBuf.String()
	w.mu.Unlock()
	if strings.TrimSpace(body) == "" && streamed != "" {
		body = streamed
		if getErr == "" {
			getErr = "getResponseBody empty — used streamResourceContent fallback"
		} else {
			getErr += "; used streamResourceContent fallback"
		}
	}
	latency := int64(0)
	if p.Timestamp*1000 > rec.startedMs {
		latency = int64(p.Timestamp*1000 - rec.startedMs)
	}
	w.emit(rec, body, getErr, latency)
}

func (w *tabWatcher) onLoadingFailed(params json.RawMessage) {
	var p struct {
		RequestID string `json:"requestId"`
		ErrorText string `json:"errorText"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	w.mu.Lock()
	rec := w.reqs[p.RequestID]
	if rec != nil {
		delete(w.reqs, p.RequestID)
	}
	w.mu.Unlock()
	if rec == nil {
		return
	}
	w.emit(rec, "", "loadingFailed: "+p.ErrorText, 0)
}

// ---- WebSocket second-leg (ChatGPT Pro handoff + Copilot) -----------------

func (w *tabWatcher) onWSCreated(sessionID string, params json.RawMessage) {
	var p struct {
		RequestID string `json:"requestId"`
		URL       string `json:"url"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	matched := w.site.matchesWS(p.URL)
	// Log EVERY WebSocket (matched or not) across page + worker targets — this
	// is what reveals the ChatGPT thinking-tier conduit host in _urls.json.
	w.coord.urls.addWebSocket(w.site.Site, w.sessionType(sessionID), p.URL, matched)
	if !matched {
		return
	}
	w.mu.Lock()
	w.wss[p.RequestID] = &wsRecord{requestID: p.RequestID, sessionID: sessionID, url: p.URL}
	w.mu.Unlock()
	fmt.Printf("  [%s] ← websocket [%s] %s\n", w.site.Site, w.sessionType(sessionID), shorten(p.URL, 90))
}

func (w *tabWatcher) onWSFrame(params json.RawMessage, sent bool) {
	var p struct {
		RequestID string `json:"requestId"`
		Response  struct {
			PayloadData string `json:"payloadData"`
			Opcode      int    `json:"opcode"`
		} `json:"response"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	w.mu.Lock()
	rec := w.wss[p.RequestID]
	w.mu.Unlock()
	if rec == nil {
		return
	}
	// Opcode 1 == text frame; skip binary (opcode 2) for the sample list.
	payload := p.Response.PayloadData
	if p.Response.Opcode != 0 && p.Response.Opcode != 1 {
		payload = fmt.Sprintf("<binary opcode=%d, %d bytes>", p.Response.Opcode, len(payload))
	}
	w.mu.Lock()
	if sent {
		if len(rec.sent) < 20 {
			rec.sent = append(rec.sent, truncStr(payload, maxFrameChars))
		}
	} else {
		if len(rec.received) < maxFrames {
			rec.received = append(rec.received, truncStr(payload, maxFrameChars))
		}
		if p.Response.Opcode == 0 || p.Response.Opcode == 1 {
			rec.rawBuf.WriteString(p.Response.PayloadData)
		}
	}
	w.mu.Unlock()
}

func (w *tabWatcher) onWSClosed(params json.RawMessage) {
	var p struct {
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	w.mu.Lock()
	rec := w.wss[p.RequestID]
	if rec != nil {
		delete(w.wss, p.RequestID)
	}
	w.mu.Unlock()
	if rec == nil || (len(rec.sent) == 0 && len(rec.received) == 0) {
		return
	}
	w.emitWS(rec)
}

// ---- emit -----------------------------------------------------------------

func (w *tabWatcher) emit(rec *reqRecord, body, getErr string, latency int64) {
	dump := buildDump(w.site, rec, body, getErr, latency)
	path, err := writeDump(w.coord.outDir, w.site.Site, dump)
	if err != nil {
		fmt.Printf("  [%s] [error] writing dump: %v\n", w.site.Site, err)
		return
	}
	fmt.Printf("  [%s] captured ✓  →  %s\n", w.site.Site, path)
	w.coord.notify(w.site.Site)
	if w.coord.once {
		w.mu.Lock()
		w.stops = true
		w.mu.Unlock()
	}
}

func (w *tabWatcher) emitWS(rec *wsRecord) {
	dump := buildWSDump(w.site, rec)
	path, err := writeDump(w.coord.outDir, w.site.Site+"-ws", dump)
	if err != nil {
		fmt.Printf("  [%s] [error] writing ws dump: %v\n", w.site.Site, err)
		return
	}
	fmt.Printf("  [%s] captured WS second-leg ✓  →  %s\n", w.site.Site, path)
}

func lowerHeaders(h map[string]string) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[strings.ToLower(k)] = v
	}
	return out
}

func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
