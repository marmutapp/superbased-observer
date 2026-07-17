package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const privacyNote = "Contains SAMPLES of YOUR prompt/response — redact the *_sample / *_raw / *_head / request_body fields before sharing; only STRUCTURE + PATHS are needed to validate parsers.js."

// responseDump is the sanitized response surface: truncated frame/segment
// samples + a total-length note, never the full text.
type responseDump struct {
	Transport    string   `json:"transport"`
	TotalLength  int      `json:"total_length"`
	FrameSamples []string `json:"frame_samples,omitempty"`
	RawHead      string   `json:"raw_head,omitempty"`
	GetBodyError string   `json:"get_body_error,omitempty"`
}

// wsDump is the sanitized second-leg WebSocket surface.
type wsDump struct {
	URL      string   `json:"url"`
	Sent     []string `json:"sent"`
	Received []string `json:"received"`
}

// dumpFile is the on-disk per-turn JSON. It lines up field-for-field with the
// Go CapturedTurn struct (captured_turn_mapping) + the harness dump so a diff
// against parsers.js is direct. STRUCTURE + PATHS are the deliverable.
type dumpFile struct {
	Privacy                  string                 `json:"// PRIVACY"`
	SchemaVersion            int                    `json:"schema_version"`
	Site                     string                 `json:"site"`
	Transport                string                 `json:"transport"`
	CapturedAt               string                 `json:"captured_at"`
	RequestURL               string                 `json:"request_url"`
	Method                   string                 `json:"method"`
	LatencyMs                int64                  `json:"latency_ms"`
	RequestHeadersOfInterest map[string]string      `json:"request_headers_of_interest,omitempty"`
	RequestBodyKeyStructure  interface{}            `json:"request_body_key_structure"`
	RequestBodyParseError    string                 `json:"request_body_parse_error,omitempty"`
	Response                 *responseDump          `json:"response,omitempty"`
	WSFrames                 *wsDump                `json:"ws_frames,omitempty"`
	CapturedTurnMapping      Extraction             `json:"captured_turn_mapping"`
	Flags                    map[string]interface{} `json:"flags"`
}

// buildDump assembles the sanitized dump for an SSE / batchexecute turn.
func buildDump(s *site, rec *reqRecord, body, getErr string, latency int64) dumpFile {
	structure, parseErr := describeBody(rec.postData)

	headersOfInterest := map[string]string{}
	for _, h := range s.HeadersOfInterest {
		if v, ok := rec.headers[strings.ToLower(h)]; ok {
			headersOfInterest[strings.ToLower(h)] = v
		}
	}
	if len(headersOfInterest) == 0 {
		headersOfInterest = nil
	}

	resp := &responseDump{Transport: string(s.Transport), TotalLength: len(body), GetBodyError: getErr}
	flags := map[string]interface{}{}
	switch s.Transport {
	case transportSSE:
		resp.FrameSamples = sseFrameSamples(body, flags)
		// ChatGPT thinking-tier handoff canaries (a conduit_token / stream_handoff
		// on the SSE leg means the completion text streams over the conduit leg).
		if strings.Contains(body, "conduit_token") {
			flags["conduit_token_seen"] = true
		}
		if strings.Contains(body, "stream_handoff") {
			flags["stream_handoff_seen"] = true
		}
	case transportBatchExecute:
		// Keep the RAW )]}'-prefixed head but truncated.
		resp.RawHead = truncStr(body, 1200)
		flags["batchexecute_prefix"] = strings.HasPrefix(strings.TrimLeft(body, " \t\r\n"), ")]}'")
		flags["utf16_frame_count"] = len(geminiFrames(body))
	}

	ex := Extract(s.Site, rec.url, rec.postData, body, rec.headers)

	return dumpFile{
		Privacy:                  privacyNote,
		SchemaVersion:            1,
		Site:                     s.Site,
		Transport:                string(s.Transport),
		CapturedAt:               time.Now().UTC().Format(time.RFC3339),
		RequestURL:               rec.url,
		Method:                   rec.method,
		LatencyMs:                latency,
		RequestHeadersOfInterest: headersOfInterest,
		RequestBodyKeyStructure:  structure,
		RequestBodyParseError:    parseErr,
		Response:                 resp,
		CapturedTurnMapping:      ex,
		Flags:                    flags,
	}
}

// buildWSDump assembles the sanitized dump for a captured second-leg WS.
func buildWSDump(s *site, rec *wsRecord) dumpFile {
	raw := rec.rawBuf.String()
	flags := map[string]interface{}{
		"received_frame_count": len(rec.received),
		"sent_frame_count":     len(rec.sent),
		"raw_total_length":     len(raw),
	}
	if strings.Contains(raw, "encoded_item") {
		flags["chatgpt_encoded_item"] = true
	}
	if strings.Contains(raw, "message_stream_complete") {
		flags["chatgpt_message_stream_complete"] = true
	}
	// Thinking-tier conduit second-leg canaries.
	if strings.Contains(raw, "conduit_token") {
		flags["conduit_token_seen"] = true
	}
	if strings.Contains(raw, "stream_handoff") {
		flags["stream_handoff_seen"] = true
	}
	return dumpFile{
		Privacy:       privacyNote,
		SchemaVersion: 1,
		Site:          s.Site,
		Transport:     string(transportWebSocket),
		CapturedAt:    time.Now().UTC().Format(time.RFC3339),
		RequestURL:    rec.url,
		Method:        "WS",
		WSFrames:      &wsDump{URL: rec.url, Sent: rec.sent, Received: rec.received},
		CapturedTurnMapping: Extraction{
			Note: "Second-leg WebSocket (ChatGPT Pro/Thinking handoff or Copilot). Decode the received frames' payload shape here.",
		},
		Flags: flags,
	}
}

// sseFrameSamples returns up to maxFrames truncated non-empty SSE lines and
// records an event-type histogram + a stream_handoff canary into flags.
func sseFrameSamples(body string, flags map[string]interface{}) []string {
	var out []string
	eventTypes := map[string]int{}
	parseErrors := 0
	for _, line := range sseLines(body) {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "event:") {
			ev := strings.TrimSpace(t[len("event:"):])
			eventTypes[ev]++
		}
		if strings.Contains(t, "stream_handoff") || strings.Contains(t, "resume_") {
			flags["possible_second_stream"] = true
		}
		if payload, ok := dataPayload(t); ok {
			if _, decoded := jsonDecode(payload); !decoded {
				parseErrors++
			}
		}
		if len(out) < maxFrames {
			out = append(out, truncStr(t, maxFrameChars))
		}
	}
	if len(eventTypes) > 0 {
		flags["event_types"] = eventTypes
	}
	if parseErrors > 0 {
		flags["data_parse_errors"] = parseErrors
	}
	return out
}

// writeDump writes a dump file to <outDir>/<prefix>-<timestamp>.json and
// returns the path.
func writeDump(outDir, prefix string, d dumpFile) (string, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", fmt.Errorf("writeDump mkdir: %w", err)
	}
	name := fmt.Sprintf("%s-%s.json", prefix, time.Now().UTC().Format("20060102T150405.000"))
	path := filepath.Join(outDir, name)
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return "", fmt.Errorf("writeDump marshal: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("writeDump write: %w", err)
	}
	return path, nil
}
