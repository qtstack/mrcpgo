// Package wsutil provides a small serialised writer around a gorilla
// WebSocket connection. Every speech engine writes control frames from more
// than one goroutine (request, audio pump, teardown), while the WebSocket
// protocol permits only one concurrent writer.
package wsutil

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Conn serialises writes and bounds each one with a timeout.
type Conn struct {
	conn    *websocket.Conn
	timeout time.Duration

	mu sync.Mutex
}

// New wraps conn. timeout bounds every write; a non-positive value leaves the
// deadline untouched.
func New(conn *websocket.Conn, timeout time.Duration) *Conn {
	return &Conn{conn: conn, timeout: timeout}
}

// WriteJSON encodes v and writes it as a text frame.
func (c *Conn) WriteJSON(v any) error {
	return c.WriteJSONTimeout(v, c.timeout)
}

// WriteJSONTimeout is WriteJSON with an explicit timeout, used on teardown
// paths that must not wait for the configured timeout.
func (c *Conn) WriteJSONTimeout(v any, timeout time.Duration) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("wsutil: encode frame: %w", err)
	}
	return c.WriteMessage(websocket.TextMessage, payload, timeout)
}

// WriteText writes a text frame.
func (c *Conn) WriteText(text string) error {
	return c.WriteMessage(websocket.TextMessage, []byte(text), c.timeout)
}

// WriteBinary writes a binary frame.
func (c *Conn) WriteBinary(data []byte) error {
	return c.WriteMessage(websocket.BinaryMessage, data, c.timeout)
}

// WriteMessage serialises a frame write.
func (c *Conn) WriteMessage(messageType int, data []byte, timeout time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	deadline := timeout
	if c.timeout > 0 && (deadline <= 0 || c.timeout < deadline) {
		deadline = c.timeout
	}
	if deadline > 0 {
		if err := c.conn.SetWriteDeadline(time.Now().Add(deadline)); err != nil {
			return err
		}
	}
	return c.conn.WriteMessage(messageType, data)
}

// Read reads one frame. Only one goroutine may call Read.
func (c *Conn) Read() (messageType int, data []byte, err error) {
	return c.conn.ReadMessage()
}

// SetReadDeadline bounds the next read.
func (c *Conn) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }

// Close closes the underlying connection.
func (c *Conn) Close() error { return c.conn.Close() }
