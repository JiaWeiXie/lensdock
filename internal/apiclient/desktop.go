package apiclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/coder/websocket"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/auth"
)

// Close codes of the control socket when the lease behind it ended. They
// mirror the runtime's vnc package.
const (
	closeLeaseEnded        websocket.StatusCode = 4001
	closeGenerationChanged websocket.StatusCode = 4002
)

const (
	// desktopReadLimit bounds one WebSocket message; the runtime relays RFB
	// in chunks far below it.
	desktopReadLimit = 1 << 20
	// desktopBusyWait is how long DialDesktop retries while the runtime
	// still counts the previous control socket of the same lease.
	desktopBusyWait = 5 * time.Second
	desktopBusyStep = 100 * time.Millisecond
)

// ErrDesktopLeaseEnded reports that the control socket was closed because
// its lease ended: released, expired, revoked, paused, fenced by a takeover
// or capture, or replaced by a new browser generation.
var ErrDesktopLeaseEnded = errors.New("apiclient: the lease behind the desktop connection ended")

// DesktopLeaseEnded reports whether err is the close of a control socket
// whose lease ended, as opposed to a transport failure.
func DesktopLeaseEnded(err error) bool {
	if errors.Is(err, ErrDesktopLeaseEnded) {
		return true
	}
	switch websocket.CloseStatus(err) {
	case closeLeaseEnded, closeGenerationChanged:
		return true
	}
	return false
}

// DialDesktop opens the lease-fenced RFB control socket of the workspace
// desktop (GET vnc/control) with the lease token and the lease's current epoch
// and generation. The returned connection carries raw RFB bytes; closing it
// closes the WebSocket. It ends, with an error for which DesktopLeaseEnded is
// true, when the lease ends. A runtime that still counts the previous socket
// of the same lease is retried for a few seconds.
func (c *Client) DialDesktop(ctx context.Context, ws string, tok auth.Secret, epoch uint64, generation string) (net.Conn, error) {
	u := *c.base
	u.RawPath = workspacePath(ws, "vnc", "control")
	p, err := url.PathUnescape(u.RawPath)
	if err != nil {
		return nil, errors.New("apiclient: invalid request path")
	}
	u.Path = p
	q := url.Values{}
	q.Set("epoch", strconv.FormatUint(epoch, 10))
	q.Set("generation", generation)
	u.RawQuery = q.Encode()

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+tok.Reveal())
	deadline := time.Now().Add(desktopBusyWait)
	for {
		conn, resp, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
			HTTPClient:      c.hc,
			HTTPHeader:      hdr,
			Subprotocols:    []string{"binary"},
			CompressionMode: websocket.CompressionDisabled,
		})
		if err == nil {
			conn.SetReadLimit(desktopReadLimit)
			return websocket.NetConn(ctx, conn, websocket.MessageBinary), nil
		}
		if resp == nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("%w: desktop connection failed", ErrUnreachable)
		}
		apiErr := decodeError(resp)
		_ = resp.Body.Close()
		var ae *APIError
		if errors.As(apiErr, &ae) && ae.Status == http.StatusTooManyRequests && ae.Code == apiwire.CodeTooManyConnections && time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(desktopBusyStep):
				continue
			}
		}
		return nil, apiErr
	}
}
