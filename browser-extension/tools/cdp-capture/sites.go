package main

import (
	"net/url"
	"strings"
)

// transport describes how a site's completion response streams.
type transport string

const (
	transportSSE          transport = "sse"          // data: frames (ChatGPT/Claude/Perplexity)
	transportBatchExecute transport = "batchexecute" // )]}'-prefixed RPC (Gemini)
	transportWebSocket    transport = "websocket"    // WS frames (Copilot)
)

// site is one capture target. It mirrors the SITES table in capture-harness.js
// and content-main.js so the endpoint list stays in lock-step.
type site struct {
	Site      string    // the models.Tool*Web tag, e.g. "chatgpt-web"
	Transport transport // response transport
	HostMatch string    // substring of the tab URL host
	// PathSubstrings: a request matches if its URL path contains ANY of these.
	// (Substring match — robust to org-id / conversation-id path segments.)
	PathSubstrings []string
	// ExcludePathSuffixes: even after a PathSubstrings hit, reject the request
	// if its path ENDS in any of these (the ChatGPT /f/conversation/prepare +
	// /init debounce siblings are substrings of /f/conversation).
	ExcludePathSuffixes []string
	// HeadersOfInterest: request headers to record verbatim (Gemini's model id
	// rides x-goog-ext-525001261-jspb, not the body).
	HeadersOfInterest []string
	// WSHostMatch: for second-leg / WS transports, the WebSocket URL host to
	// capture (ChatGPT Pro handoff + Copilot).
	WSHostMatch []string
	// CaptureAllWS: capture EVERY WebSocket opened in this site's tab, not just
	// WSHostMatch hosts. Used for ChatGPT's thinking-tier conduit second leg,
	// whose host is not known ahead of time.
	CaptureAllWS bool
	// StreamBody: the completion response is a long stream whose full body
	// getResponseBody can evict/return-empty; when set, the watcher ALSO
	// accumulates the body via Network.streamResourceContent + dataReceived as
	// a fallback (Gemini StreamGenerate).
	StreamBody bool
}

// sites is the capture table. Copilot is a bonus (optional).
var sites = []site{
	{
		Site:           "chatgpt-web",
		Transport:      transportSSE,
		HostMatch:      "chatgpt.com",
		PathSubstrings: []string{"/backend-api/conversation", "/backend-api/f/conversation"},
		// …/f/conversation/prepare (debounce) and …/init are siblings that
		// contain the completion path as a substring — reject them so the
		// non-thinking completion POST is what satisfies "captured ✓".
		ExcludePathSuffixes: []string{"/prepare", "/init"},
		// Pro/Thinking tier hands off to a second-leg WebSocket (the "conduit").
		// Its host is not fixed, so capture every WS in the ChatGPT tab.
		WSHostMatch:  []string{"ws.chatgpt.com", "chatgpt.com"},
		CaptureAllWS: true,
	},
	{
		Site:           "claude-web",
		Transport:      transportSSE,
		HostMatch:      "claude.ai",
		PathSubstrings: []string{"/chat_conversations/", "/completion"},
	},
	{
		Site:           "perplexity-web",
		Transport:      transportSSE,
		HostMatch:      "perplexity.ai",
		PathSubstrings: []string{"/rest/sse/perplexity_ask"},
	},
	{
		Site:      "gemini-web",
		Transport: transportBatchExecute,
		HostMatch: "gemini.google.com",
		// Match ONLY the StreamGenerate chat RPC. The plain
		// batchexecute?rpcids=… calls (ESY5D bard_activity_enabled, settings,
		// etc.) are NOT the chat turn and must not satisfy "captured ✓".
		PathSubstrings:    []string{"StreamGenerate"},
		HeadersOfInterest: []string{"x-goog-ext-525001261-jspb"},
		// StreamGenerate is a long streamed response — getResponseBody can
		// return empty; accumulate the body via streamResourceContent too.
		StreamBody: true,
	},
	{
		Site:           "copilot-web",
		Transport:      transportWebSocket,
		HostMatch:      "copilot.microsoft.com",
		PathSubstrings: []string{"/c/api/", "/chat"},
		WSHostMatch:    []string{"copilot.microsoft.com"},
	},
}

// siteForURL returns the site whose HostMatch is a substring of the tab URL's
// host, or nil.
func siteForURL(rawURL string) *site {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	host := u.Hostname()
	for i := range sites {
		if strings.Contains(host, sites[i].HostMatch) {
			return &sites[i]
		}
	}
	return nil
}

// matchesEndpoint reports whether reqURL is one of the site's completion
// endpoints. Claude requires BOTH path markers (conversation + completion) so
// it doesn't over-match; every other site matches on ANY substring.
func (s *site) matchesEndpoint(reqURL string) bool {
	u, err := url.Parse(reqURL)
	if err != nil {
		return false
	}
	path := u.Path
	if s.Site == "claude-web" {
		return strings.Contains(path, "/chat_conversations/") && strings.Contains(path, "/completion")
	}
	matched := false
	for _, sub := range s.PathSubstrings {
		if strings.Contains(path, sub) {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	// Reject sibling debounce/init endpoints that contain the completion path
	// as a substring (ChatGPT /f/conversation/prepare + /init).
	for _, suf := range s.ExcludePathSuffixes {
		if strings.HasSuffix(path, suf) {
			return false
		}
	}
	return true
}

// matchesWS reports whether a WebSocket URL belongs to this site's second-leg
// / chat socket.
func (s *site) matchesWS(wsURL string) bool {
	if s.CaptureAllWS {
		return true
	}
	u, err := url.Parse(wsURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	for _, h := range s.WSHostMatch {
		if strings.Contains(host, h) {
			return true
		}
	}
	return false
}
