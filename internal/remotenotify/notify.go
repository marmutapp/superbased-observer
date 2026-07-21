package remotenotify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Event kinds. Phase 0 wires the coarse session-exit signal available today
// (session_finished); the richer session_blocked event lands when the cockpit
// F4 status feature exists (plan §7 Phase 0 Note).
const (
	// EventSessionFinished fires when a tracked session's process exits.
	EventSessionFinished = "session_finished"
	// EventSessionBlocked fires when a session is waiting on the operator
	// (a future F4-status signal; defined now so config/subscription is
	// stable).
	EventSessionBlocked = "session_blocked"
)

// Kinds of transport.
const (
	KindWebhook = "webhook"
	KindNtfy    = "ntfy"
)

// Event is a notification-worthy lifecycle transition. It carries only
// metadata (ids, enums, exit code) — never conversation content.
type Event struct {
	Type       string
	SessionID  string
	Tool       string
	Subcommand string
	ExitCode   int
	Detail     string
	Time       time.Time
}

// Config is the subset of the [remote.notify] block the notifier needs.
type Config struct {
	Enabled bool
	Kind    string
	URL     string
	Events  []string
}

// Request is the transport-agnostic delivery an injected Sender fulfils: an
// HTTP POST of Body to URL with the given Headers. Keeping it a plain struct
// (not an *http.Request) is what lets this package avoid importing net/http.
type Request struct {
	URL     string
	Body    []byte
	Headers map[string]string
}

// Sender delivers a built Request. Injected so the pure core never imports
// net/http; the cmd wiring supplies an http.Client-backed implementation.
type Sender interface {
	Send(ctx context.Context, req Request) error
}

// Notifier fires notifications for subscribed events over the configured
// transport. The zero value (and a disabled config) is a safe no-op.
type Notifier struct {
	cfg    Config
	sender Sender
	now    func() time.Time
}

// New builds a Notifier. A nil sender or disabled config yields a no-op
// Notifier (Notify returns nil without building anything).
func New(cfg Config, sender Sender) *Notifier {
	return &Notifier{cfg: cfg, sender: sender, now: func() time.Time { return time.Now().UTC() }}
}

// SetClock overrides the clock (test hook).
func (n *Notifier) SetClock(now func() time.Time) {
	if now != nil {
		n.now = now
	}
}

// Enabled reports whether the rail is on with a usable transport + URL.
func (n *Notifier) Enabled() bool {
	return n != nil && n.cfg.Enabled && n.sender != nil && strings.TrimSpace(n.cfg.URL) != ""
}

// subscribed reports whether eventType is in the configured Events set. An
// empty set means "all known events".
func (n *Notifier) subscribed(eventType string) bool {
	if len(n.cfg.Events) == 0 {
		return true
	}
	for _, e := range n.cfg.Events {
		if strings.EqualFold(strings.TrimSpace(e), eventType) {
			return true
		}
	}
	return false
}

// Notify builds the payload for ev and delivers it through the sender. It is a
// no-op (returns nil) when the rail is disabled or ev.Type is not subscribed —
// so callers can invoke it unconditionally on every lifecycle transition.
func (n *Notifier) Notify(ctx context.Context, ev Event) error {
	req, ok := n.BuildRequest(ev)
	if !ok {
		return nil
	}
	if err := n.sender.Send(ctx, req); err != nil {
		return fmt.Errorf("remotenotify.Notify: %w", err)
	}
	return nil
}

// BuildRequest is the pure payload builder. It returns the delivery Request and
// true when the event should fire, or a zero Request and false when the rail is
// disabled / the event is not subscribed / the kind is unknown. Exposed so the
// payload shape is unit-testable without a Sender.
func (n *Notifier) BuildRequest(ev Event) (Request, bool) {
	if !n.Enabled() || !n.subscribed(ev.Type) {
		return Request{}, false
	}
	if ev.Time.IsZero() {
		ev.Time = n.now()
	}
	switch strings.ToLower(strings.TrimSpace(n.cfg.Kind)) {
	case KindNtfy:
		return n.buildNtfy(ev), true
	case KindWebhook, "":
		return n.buildWebhook(ev), true
	default:
		return Request{}, false
	}
}

// webhookPayload is the JSON body of a webhook delivery — metadata only.
type webhookPayload struct {
	Event      string `json:"event"`
	SessionID  string `json:"session_id,omitempty"`
	Tool       string `json:"tool,omitempty"`
	Subcommand string `json:"subcommand,omitempty"`
	ExitCode   int    `json:"exit_code"`
	Detail     string `json:"detail,omitempty"`
	Time       string `json:"time"`
	Source     string `json:"source"`
}

func (n *Notifier) buildWebhook(ev Event) Request {
	body, _ := json.Marshal(webhookPayload{
		Event:      ev.Type,
		SessionID:  ev.SessionID,
		Tool:       ev.Tool,
		Subcommand: ev.Subcommand,
		ExitCode:   ev.ExitCode,
		Detail:     ev.Detail,
		Time:       ev.Time.UTC().Format(time.RFC3339),
		Source:     "superbased-observer",
	})
	return Request{
		URL:     n.cfg.URL,
		Body:    body,
		Headers: map[string]string{"Content-Type": "application/json"},
	}
}

// buildNtfy builds a plain-text ntfy publish: the body is the human message and
// Title/Priority/Tags ride as headers (ntfy's documented publish contract).
func (n *Notifier) buildNtfy(ev Event) Request {
	title, msg, tag, priority := ntfyMessage(ev)
	return Request{
		URL:  n.cfg.URL,
		Body: []byte(msg),
		Headers: map[string]string{
			"Content-Type": "text/plain; charset=utf-8",
			"Title":        title,
			"Priority":     priority,
			"Tags":         tag,
		},
	}
}

// ntfyMessage renders the human-facing title/body/tag/priority for an event.
func ntfyMessage(ev Event) (title, body, tag, priority string) {
	who := ev.Tool
	if ev.Subcommand != "" {
		who = strings.TrimSpace(ev.Tool + " " + ev.Subcommand)
	}
	if who == "" {
		who = "session"
	}
	switch ev.Type {
	case EventSessionBlocked:
		title = "Agent needs you"
		body = fmt.Sprintf("%s is blocked and waiting for input", who)
		tag = "warning"
		priority = "high"
	default: // session_finished
		title = "Agent finished"
		if ev.ExitCode == 0 {
			body = fmt.Sprintf("%s finished successfully", who)
			tag = "white_check_mark"
		} else {
			body = fmt.Sprintf("%s exited with code %d", who, ev.ExitCode)
			tag = "x"
		}
		priority = "default"
	}
	if ev.Detail != "" {
		body += " — " + ev.Detail
	}
	return title, body, tag, priority
}
