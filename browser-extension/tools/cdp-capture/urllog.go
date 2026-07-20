package main

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// urlLog is the cross-target request/WebSocket diagnostic. It records EVERY
// request URL+method and EVERY WebSocket URL seen across page AND worker /
// service-worker targets — matched by a capture rule or not — so a run reveals
// the real conduit host/path (e.g. ChatGPT's thinking-tier second leg) and any
// worker-opened socket that our sites.go matcher missed. Query strings are
// STRIPPED before recording, so no auth tokens land in the diagnostic; a
// has_query flag preserves the fact a query was present.
type urlLog struct {
	mu         sync.Mutex
	requests   map[string]*urlSample
	websockets map[string]*urlSample
}

// urlSample is one deduped observed URL. Count folds repeat observations
// (polling / heartbeat sockets) so the dump stays small.
type urlSample struct {
	Site       string `json:"site"`
	TargetType string `json:"target_type"`
	Method     string `json:"method,omitempty"`
	URL        string `json:"url"`
	HasQuery   bool   `json:"has_query"`
	Matched    bool   `json:"matched"`
	Count      int    `json:"count"`
}

// urlLogCap bounds each map so a pathological page can't grow the dump without
// limit.
const urlLogCap = 4000

func newURLLog() *urlLog {
	return &urlLog{
		requests:   map[string]*urlSample{},
		websockets: map[string]*urlSample{},
	}
}

// addRequest records one HTTP(S) request observation.
func (l *urlLog) addRequest(site, targetType, method, rawURL string, matched bool) {
	l.add(l.requests, site, targetType, method, rawURL, matched)
}

// addWebSocket records one WebSocket-created observation.
func (l *urlLog) addWebSocket(site, targetType, rawURL string, matched bool) {
	l.add(l.websockets, site, targetType, "", rawURL, matched)
}

func (l *urlLog) add(m map[string]*urlSample, site, targetType, method, rawURL string, matched bool) {
	clean, hasQuery := sanitizeURL(rawURL)
	key := site + "\x00" + targetType + "\x00" + method + "\x00" + clean
	l.mu.Lock()
	defer l.mu.Unlock()
	if s, ok := m[key]; ok {
		s.Count++
		// A rule change or a later observation may flip matched to true; keep
		// the most permissive value.
		if matched {
			s.Matched = true
		}
		return
	}
	if len(m) >= urlLogCap {
		return
	}
	m[key] = &urlSample{
		Site:       site,
		TargetType: targetType,
		Method:     method,
		URL:        clean,
		HasQuery:   hasQuery,
		Matched:    matched,
		Count:      1,
	}
}

// sanitizeURL drops the query string and fragment (which can carry auth
// tokens) and reports whether one was present. Falls back to a naive '?' split
// if the URL doesn't parse.
func sanitizeURL(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		if i := strings.IndexByte(raw, '?'); i != -1 {
			return raw[:i], true
		}
		return raw, false
	}
	hasQuery := u.RawQuery != "" || u.Fragment != ""
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), hasQuery
}

// write dumps the collected observations to <dir>/_urls.json and returns the
// path. Safe to call once at shutdown.
func (l *urlLog) write(dir string) (string, error) {
	l.mu.Lock()
	reqs := make([]urlSample, 0, len(l.requests))
	for _, s := range l.requests {
		reqs = append(reqs, *s)
	}
	wss := make([]urlSample, 0, len(l.websockets))
	for _, s := range l.websockets {
		wss = append(wss, *s)
	}
	l.mu.Unlock()

	byURL := func(a, b urlSample) bool {
		if a.Site != b.Site {
			return a.Site < b.Site
		}
		if a.TargetType != b.TargetType {
			return a.TargetType < b.TargetType
		}
		return a.URL < b.URL
	}
	sort.Slice(reqs, func(i, j int) bool { return byURL(reqs[i], reqs[j]) })
	sort.Slice(wss, func(i, j int) bool { return byURL(wss[i], wss[j]) })

	out := struct {
		Privacy    string      `json:"// PRIVACY"`
		Note       string      `json:"note"`
		CapturedAt string      `json:"captured_at"`
		Requests   []urlSample `json:"requests"`
		WebSockets []urlSample `json:"websockets"`
	}{
		Privacy:    "URLs + methods + target-type only. Query strings STRIPPED (has_query flags their presence); no auth headers or tokens are recorded.",
		Note:       "Diagnostic: EVERY request + WebSocket seen across page AND worker/service-worker targets, matched by a sites.go rule or not. Use target_type=worker/service_worker WebSocket rows to discover the ChatGPT thinking-tier conduit host/path, then tune sites.go WSHostMatch / CaptureAllWS.",
		CapturedAt: time.Now().UTC().Format(time.RFC3339),
		Requests:   reqs,
		WebSockets: wss,
	}
	path := filepath.Join(dir, "_urls.json")
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}
