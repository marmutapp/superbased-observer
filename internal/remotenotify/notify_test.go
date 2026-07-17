package remotenotify

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type captureSender struct {
	reqs []Request
	err  error
}

func (c *captureSender) Send(_ context.Context, req Request) error {
	c.reqs = append(c.reqs, req)
	return c.err
}

func TestNotifierGatingAndPayload(t *testing.T) {
	fixed := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		cfg       Config
		ev        Event
		wantFire  bool
		wantEvent string
	}{
		{
			name:     "disabled is a no-op",
			cfg:      Config{Enabled: false, Kind: KindWebhook, URL: "https://x/y"},
			ev:       Event{Type: EventSessionFinished},
			wantFire: false,
		},
		{
			name:     "no URL is a no-op",
			cfg:      Config{Enabled: true, Kind: KindWebhook, URL: ""},
			ev:       Event{Type: EventSessionFinished},
			wantFire: false,
		},
		{
			name:     "unsubscribed event does not fire",
			cfg:      Config{Enabled: true, Kind: KindWebhook, URL: "https://x/y", Events: []string{EventSessionBlocked}},
			ev:       Event{Type: EventSessionFinished},
			wantFire: false,
		},
		{
			name:      "subscribed webhook fires",
			cfg:       Config{Enabled: true, Kind: KindWebhook, URL: "https://x/y", Events: []string{EventSessionFinished}},
			ev:        Event{Type: EventSessionFinished, SessionID: "s1", Tool: "claude-code", ExitCode: 0},
			wantFire:  true,
			wantEvent: EventSessionFinished,
		},
		{
			name:      "empty events set means all",
			cfg:       Config{Enabled: true, Kind: KindWebhook, URL: "https://x/y"},
			ev:        Event{Type: EventSessionBlocked, SessionID: "s2"},
			wantFire:  true,
			wantEvent: EventSessionBlocked,
		},
		{
			name:     "unknown kind does not fire",
			cfg:      Config{Enabled: true, Kind: "carrier-pigeon", URL: "https://x/y"},
			ev:       Event{Type: EventSessionFinished},
			wantFire: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cs := &captureSender{}
			n := New(tc.cfg, cs)
			n.SetClock(func() time.Time { return fixed })
			if err := n.Notify(context.Background(), tc.ev); err != nil {
				t.Fatalf("Notify: %v", err)
			}
			if got := len(cs.reqs) == 1; got != tc.wantFire {
				t.Fatalf("fired=%v, want %v", got, tc.wantFire)
			}
			if !tc.wantFire {
				return
			}
			var p webhookPayload
			if err := json.Unmarshal(cs.reqs[0].Body, &p); err != nil {
				t.Fatalf("payload not JSON: %v", err)
			}
			if p.Event != tc.wantEvent {
				t.Errorf("event=%q want %q", p.Event, tc.wantEvent)
			}
			if p.Time != fixed.Format(time.RFC3339) {
				t.Errorf("time=%q want injected clock", p.Time)
			}
			if p.Source != "superbased-observer" {
				t.Errorf("source=%q", p.Source)
			}
		})
	}
}

func TestNtfyPayloadShape(t *testing.T) {
	cs := &captureSender{}
	n := New(Config{Enabled: true, Kind: KindNtfy, URL: "https://ntfy.sh/mytopic"}, cs)
	n.SetClock(func() time.Time { return time.Unix(0, 0).UTC() })
	if err := n.Notify(context.Background(), Event{Type: EventSessionFinished, Tool: "codex", ExitCode: 1}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if len(cs.reqs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(cs.reqs))
	}
	req := cs.reqs[0]
	if req.Headers["Title"] == "" || !strings.Contains(string(req.Body), "codex") {
		t.Errorf("ntfy request missing title/body: %+v", req)
	}
	if !strings.Contains(string(req.Body), "code 1") {
		t.Errorf("non-zero exit not surfaced: %q", req.Body)
	}
	if ct := req.Headers["Content-Type"]; !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("ntfy content-type=%q want text/plain", ct)
	}
}

func TestNotifyWrapsSenderError(t *testing.T) {
	cs := &captureSender{err: errors.New("boom")}
	n := New(Config{Enabled: true, Kind: KindWebhook, URL: "https://x/y"}, cs)
	err := n.Notify(context.Background(), Event{Type: EventSessionFinished})
	if err == nil || !strings.Contains(err.Error(), "remotenotify.Notify") {
		t.Fatalf("want wrapped sender error, got %v", err)
	}
}
