package capture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"

	"github.com/coder/websocket"
)

// cdpReadLimit disables the WebSocket message size limit (coder/websocket
// treats -1 as unlimited): a full-page PNG arrives as one base64 message.
// Screenshot size is bounded only by the optional Config limits.
const cdpReadLimit = -1

// cdpConn is a minimal serial CDP client: one command in flight, events are
// discarded. It exists for the runtime's own short capture and probe
// sessions, so it needs no subscription machinery.
type cdpConn struct {
	ws     *websocket.Conn
	nextID atomic.Int64
}

// cdpError is a CDP error response.
type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *cdpError) Error() string { return fmt.Sprintf("cdp error %d: %s", e.Code, e.Message) }

// dialCDP opens ws://host/path. Dial sends no Origin header, which Chromium
// requires for DevTools WebSockets, and sets Host to the IP literal.
func dialCDP(ctx context.Context, client *http.Client, host, path string) (*cdpConn, error) {
	ws, _, err := websocket.Dial(ctx, "ws://"+host+path, &websocket.DialOptions{
		HTTPClient:      client,
		HTTPHeader:      http.Header{},
		Host:            host,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return nil, fmt.Errorf("dial devtools: %w", err)
	}
	ws.SetReadLimit(cdpReadLimit)
	return &cdpConn{ws: ws}, nil
}

func (c *cdpConn) close() { _ = c.ws.CloseNow() }

// call sends one command and decodes its result into out (which may be nil).
func (c *cdpConn) call(ctx context.Context, method string, params, out any) error {
	id := c.nextID.Add(1)
	body, err := json.Marshal(struct {
		ID     int64  `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params,omitempty"`
	}{id, method, params})
	if err != nil {
		return fmt.Errorf("marshal %s: %w", method, err)
	}
	if err := c.ws.Write(ctx, websocket.MessageText, body); err != nil {
		return fmt.Errorf("write %s: %w", method, err)
	}
	for {
		_, data, err := c.ws.Read(ctx)
		if err != nil {
			return fmt.Errorf("read %s: %w", method, err)
		}
		var env struct {
			ID     *int64          `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *cdpError       `json:"error"`
		}
		if json.Unmarshal(data, &env) != nil || env.ID == nil || *env.ID != id {
			continue // an event or an unrelated reply
		}
		if env.Error != nil {
			return fmt.Errorf("%s: %w", method, env.Error)
		}
		if out != nil && len(env.Result) > 0 {
			if err := json.Unmarshal(env.Result, out); err != nil {
				return fmt.Errorf("decode %s: %w", method, err)
			}
		}
		return nil
	}
}

// isCDPError reports whether err wraps a CDP error response.
func isCDPError(err error) bool {
	var ce *cdpError
	return errors.As(err, &ce)
}
