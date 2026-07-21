package main

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// cdpTarget is one entry from http://127.0.0.1:<port>/json.
type cdpTarget struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	Title                string `json:"title"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// cdpEvent is a CDP protocol event (a message with a method but no id).
// SessionID is set for events that arrive from an auto-attached child target
// (worker / service-worker) in flatten mode; empty for the root page session.
type cdpEvent struct {
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
	SessionID string          `json:"sessionId"`
}

// cdpMessage is the union read off the socket: a command reply (has Id) or an
// event (has Method). SessionId is present on both events and replies that
// belong to an auto-attached child target (flatten mode).
type cdpMessage struct {
	ID        *int64          `json:"id"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
	Result    json.RawMessage `json:"result"`
	Error     *cdpError       `json:"error"`
	SessionID string          `json:"sessionId"`
}

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *cdpError) Error() string { return fmt.Sprintf("cdp error %d: %s", e.Code, e.Message) }

// cdpClient is a minimal raw Chrome DevTools Protocol client over one target's
// DevTools WebSocket. Commands block for their reply; events are delivered to
// the Events channel. It is flat (no sessionId) — one client per page target.
type cdpClient struct {
	conn    *websocket.Conn
	nextID  int64
	mu      sync.Mutex
	pending map[int64]chan cdpMessage
	Events  chan cdpEvent
	writeMu sync.Mutex
	closed  atomic.Bool
}

// dialCDP opens a DevTools WebSocket to the given target URL and starts the
// read loop.
func dialCDP(wsURL string) (*cdpClient, error) {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		// Chrome sends large frames for getResponseBody results.
		ReadBufferSize:  1 << 20,
		WriteBufferSize: 1 << 20,
	}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("dialCDP: %w", err)
	}
	conn.SetReadLimit(256 << 20) // response bodies can be large
	c := &cdpClient{
		conn:    conn,
		pending: make(map[int64]chan cdpMessage),
		Events:  make(chan cdpEvent, 4096),
	}
	go c.readLoop()
	return c, nil
}

func (c *cdpClient) readLoop() {
	defer func() {
		c.closed.Store(true)
		close(c.Events)
	}()
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		var msg cdpMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if msg.ID != nil {
			c.mu.Lock()
			ch := c.pending[*msg.ID]
			delete(c.pending, *msg.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- msg
			}
			continue
		}
		if msg.Method != "" {
			select {
			case c.Events <- cdpEvent{Method: msg.Method, Params: msg.Params, SessionID: msg.SessionID}:
			default:
				// Event buffer full — drop rather than block the read loop.
			}
		}
	}
}

// Send issues a CDP command on the root page session and waits (up to timeout)
// for its reply. result, when non-nil, is unmarshaled from the reply's result
// field.
func (c *cdpClient) Send(method string, params map[string]interface{}, result interface{}, timeout time.Duration) error {
	return c.SendTo("", method, params, result, timeout)
}

// SendTo issues a CDP command scoped to an auto-attached child target's session
// (flatten mode). An empty sessionID targets the root page session, so Send is
// SendTo(""). Replies are still matched by the connection-unique command id, so
// the child reply's own sessionId is irrelevant to routing.
func (c *cdpClient) SendTo(sessionID, method string, params map[string]interface{}, result interface{}, timeout time.Duration) error {
	if c.closed.Load() {
		return fmt.Errorf("cdp: connection closed")
	}
	id := atomic.AddInt64(&c.nextID, 1)
	ch := make(chan cdpMessage, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()

	payload := map[string]interface{}{"id": id, "method": method}
	if params != nil {
		payload["params"] = params
	}
	if sessionID != "" {
		payload["sessionId"] = sessionID
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	err = c.conn.WriteMessage(websocket.TextMessage, data)
	c.writeMu.Unlock()
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("cdp send %s: %w", method, err)
	}

	select {
	case msg := <-ch:
		if msg.Error != nil {
			return msg.Error
		}
		if result != nil && len(msg.Result) > 0 {
			return json.Unmarshal(msg.Result, result)
		}
		return nil
	case <-time.After(timeout):
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("cdp send %s: timeout after %s", method, timeout)
	}
}

// Close tears down the socket.
func (c *cdpClient) Close() {
	_ = c.conn.Close()
}
