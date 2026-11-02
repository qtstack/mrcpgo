package tencent

import (
	"context"
	"fmt"
	"time"

	"github.com/gorilla/websocket"
)

// dialWS opens a Tencent Cloud speech WebSocket.
func dialWS(ctx context.Context, rawURL string, timeout time.Duration) (*websocket.Conn, error) {
	dialer := websocket.Dialer{
		HandshakeTimeout: timeout,
		ReadBufferSize:   4096,
		WriteBufferSize:  4096,
	}
	conn, resp, err := dialer.DialContext(ctx, rawURL, nil)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		// The URL carries the secret id and signature; do not log it.
		return nil, fmt.Errorf("tencent: websocket dial: %w", err)
	}
	return conn, nil
}
