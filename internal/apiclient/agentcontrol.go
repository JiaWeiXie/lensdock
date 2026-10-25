package apiclient

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/auth"
)

// WatchAgentControl delivers the current policy followed by changes. Losing the
// stream is an error, never permission to resume browser operations. The caller
// decides when to reconnect; cancellation stops the pending read immediately.
func (c *Client) WatchAgentControl(ctx context.Context, workspace string, token auth.Secret, consume func(apiwire.AgentControl) error) error {
	if consume == nil {
		return errors.New("apiclient: notification callback is required")
	}
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	watchdog := time.AfterFunc(callTimeout, cancel)
	defer watchdog.Stop()
	u := *c.base
	u.RawPath = workspacePath(workspace, "agent", "events")
	u.Path, _ = url.PathUnescape(u.RawPath)
	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return errors.New("apiclient: build notification request")
	}
	req.Header.Set("Authorization", "Bearer "+token.Reveal())
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: notification connection failed", ErrUnreachable)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return decodeError(resp)
	}
	contentType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || contentType != "text/event-stream" {
		return errors.New("apiclient: invalid notification content type")
	}
	watchdog.Reset(45 * time.Second)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), maxErrorBody)
	var event string
	var data []byte
	for scanner.Scan() {
		watchdog.Reset(45 * time.Second)
		line := scanner.Text()
		if line == "" {
			if event == "agent-control" && len(data) != 0 {
				var state apiwire.AgentControl
				if err := apiwire.Decode(bytes.NewReader(data), maxErrorBody, &state); err != nil ||
					(state.Mode != "AGENT" && state.Mode != "NOVNC") || state.Revision == 0 || state.UpdatedAt.IsZero() ||
					(state.Mode == "NOVNC" && state.Paused) {
					return errors.New("apiclient: malformed agent control notification")
				}
				if err := consume(state); err != nil {
					return err
				}
			}
			event, data = "", data[:0]
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			if len(data)+len(value)+1 > maxErrorBody {
				return errors.New("apiclient: notification exceeds size limit")
			}
			data = append(data, value...)
			data = append(data, '\n')
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if scanner.Err() != nil {
		return fmt.Errorf("%w: notification stream interrupted", ErrUnreachable)
	}
	return fmt.Errorf("%w: notification stream ended: %w", ErrUnreachable, io.ErrUnexpectedEOF)
}
