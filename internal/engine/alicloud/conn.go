package alicloud

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gorilla/websocket"

	"github.com/qtstack/mrcpgo/internal/config"
)

// endpoint returns the NLS WebSocket URL.
func endpoint(cfg config.AliCloud) string {
	if cfg.Endpoint != "" {
		return cfg.Endpoint
	}
	return "wss://nls-gateway." + cfg.Region + ".aliyuncs.com/ws/v1"
}

// dial opens the NLS WebSocket carrying the token in the handshake header.
func dial(ctx context.Context, cfg config.AliCloud, token string) (*websocket.Conn, error) {
	header := http.Header{}
	header.Set(headerToken, token)

	dialer := websocket.Dialer{
		HandshakeTimeout: cfg.Timeout.Value(),
		ReadBufferSize:   4096,
		WriteBufferSize:  4096,
	}
	conn, resp, err := dialer.DialContext(ctx, endpoint(cfg), header)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, fmt.Errorf("alicloud: websocket dial %s: %w", endpoint(cfg), err)
	}
	return conn, nil
}
