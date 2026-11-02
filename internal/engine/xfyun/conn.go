package xfyun

import (
	"context"
	"fmt"
	"time"

	"github.com/gorilla/websocket"
)

// dialWS opens an iFlytek speech WebSocket.
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
		// The URL carries the signature; do not log it.
		return nil, fmt.Errorf("xfyun: websocket dial: %w", err)
	}
	return conn, nil
}
