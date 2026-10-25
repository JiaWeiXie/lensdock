// Package cdpc is a small Chrome DevTools Protocol client for the LensDock
// end-to-end tests: JSON-RPC over a WebSocket, event waiting, a few Page,
// Runtime, DOM and CSS helpers, and the HTTP discovery endpoints.
//
// Unlike the product bridge it is meant to observe the proxy, so it exposes
// the WebSocket close status and never retries or reconnects.
package cdpc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"
)

const readLimit = 64 << 20 // screenshots arrive base64 encoded in one frame

// Error is a CDP error response.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("cdp error %d: %s", e.Code, e.Message) }

type envelope struct {
	ID     *int64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *Error          `json:"error"`
}

type reply struct {
	result json.RawMessage
	err    error
}

type subscription struct {
	method string
	ch     chan json.RawMessage
}

// Client is one CDP WebSocket connection. All methods are safe for concurrent
// use.
type Client struct {
	conn   *websocket.Conn
	nextID atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan reply
	subs    map[*subscription]struct{}
	readErr error

	done chan struct{}
}

// DialOptions customise Dial.
type DialOptions struct {
	// Header is sent with the upgrade request, for example Authorization.
	Header http.Header
}

// Dial opens a CDP WebSocket. On a refused upgrade the returned error wraps a
// *HTTPError carrying the HTTP status.
func Dial(ctx context.Context, wsURL string, opts DialOptions) (*Client, error) {
	conn, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader:      opts.Header,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		if resp != nil {
			var body []byte
			if resp.Body != nil {
				body, _ = io.ReadAll(io.LimitReader(resp.Body, 4096))
			}
			return nil, fmt.Errorf("cdpc: dial: %w", &HTTPError{Status: resp.StatusCode, Body: string(body)})
		}
		return nil, fmt.Errorf("cdpc: dial: %w", err)
	}
	conn.SetReadLimit(readLimit)
	c := &Client{
		conn:    conn,
		pending: make(map[int64]chan reply),
		subs:    make(map[*subscription]struct{}),
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

func (c *Client) readLoop() {
	defer close(c.done)
	for {
		_, data, err := c.conn.Read(context.Background())
		if err != nil {
			c.mu.Lock()
			c.readErr = err
			for id, ch := range c.pending {
				ch <- reply{err: fmt.Errorf("cdpc: connection closed: %w", err)}
				delete(c.pending, id)
			}
			c.mu.Unlock()
			return
		}
		var env envelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue // not CDP JSON; nothing to dispatch
		}
		c.mu.Lock()
		if env.ID != nil {
			if ch, ok := c.pending[*env.ID]; ok {
				delete(c.pending, *env.ID)
				if env.Error != nil {
					ch <- reply{err: env.Error}
				} else {
					ch <- reply{result: env.Result}
				}
			}
		} else if env.Method != "" {
			for s := range c.subs {
				if s.method == env.Method {
					select {
					case s.ch <- env.Params:
					default: // a slow subscriber loses events rather than stalling the reader
					}
				}
			}
		}
		c.mu.Unlock()
	}
}

// Done is closed when the connection ends for any reason.
func (c *Client) Done() <-chan struct{} { return c.done }

// CloseStatus reports how the connection ended. It returns -1 until Done is
// closed and also -1 when the connection ended without a close frame.
func (c *Client) CloseStatus() websocket.StatusCode {
	select {
	case <-c.done:
	default:
		return -1
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return websocket.CloseStatus(c.readErr)
}

// Close terminates the connection without waiting for the peer.
func (c *Client) Close() {
	_ = c.conn.CloseNow()
	<-c.done
}

// Call sends one command and decodes its result into result (which may be
// nil). A CDP error response is returned as *Error.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	id := c.nextID.Add(1)
	req := struct {
		ID     int64  `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params,omitempty"`
	}{id, method, params}
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("cdpc: marshal %s: %w", method, err)
	}
	ch := make(chan reply, 1)
	c.mu.Lock()
	if c.readErr != nil {
		err := c.readErr
		c.mu.Unlock()
		return fmt.Errorf("cdpc: connection closed: %w", err)
	}
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.conn.Write(ctx, websocket.MessageText, data); err != nil {
		c.forget(id)
		return fmt.Errorf("cdpc: write %s: %w", method, err)
	}
	select {
	case r := <-ch:
		if r.err != nil {
			return r.err
		}
		if result != nil && len(r.result) > 0 {
			if err := json.Unmarshal(r.result, result); err != nil {
				return fmt.Errorf("cdpc: decode %s result: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		c.forget(id)
		return fmt.Errorf("cdpc: %s: %w", method, ctx.Err())
	}
}

func (c *Client) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// Subscribe returns a channel receiving the params of every event named
// method until cancel is called. Subscribe before the action that triggers the
// event.
func (c *Client) Subscribe(method string) (events <-chan json.RawMessage, cancel func()) {
	s := &subscription{method: method, ch: make(chan json.RawMessage, 64)}
	c.mu.Lock()
	c.subs[s] = struct{}{}
	c.mu.Unlock()
	return s.ch, func() {
		c.mu.Lock()
		delete(c.subs, s)
		c.mu.Unlock()
	}
}

// ErrNavigation reports a failed Page.navigate.
var ErrNavigation = errors.New("cdpc: navigation failed")

// Navigate loads url and waits for the load event.
func (c *Client) Navigate(ctx context.Context, url string) error {
	if err := c.Call(ctx, "Page.enable", nil, nil); err != nil {
		return err
	}
	loaded, cancel := c.Subscribe("Page.loadEventFired")
	defer cancel()
	var res struct {
		ErrorText string `json:"errorText"`
	}
	if err := c.Call(ctx, "Page.navigate", map[string]string{"url": url}, &res); err != nil {
		return err
	}
	if res.ErrorText != "" {
		return fmt.Errorf("%w: %s", ErrNavigation, res.ErrorText)
	}
	select {
	case <-loaded:
		return nil
	case <-c.done:
		return fmt.Errorf("cdpc: connection closed while loading: %w", c.readError())
	case <-ctx.Done():
		return fmt.Errorf("cdpc: waiting for load event: %w", ctx.Err())
	}
}

func (c *Client) readError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readErr
}

// BringToFront activates the page.
func (c *Client) BringToFront(ctx context.Context) error {
	return c.Call(ctx, "Page.bringToFront", nil, nil)
}

// ErrException reports a JavaScript exception from Runtime.evaluate.
var ErrException = errors.New("cdpc: javascript exception")

// Evaluate runs a JavaScript expression (awaiting promises) and decodes the
// by-value result into out, which may be nil.
func (c *Client) Evaluate(ctx context.Context, expression string, out any) error {
	var res struct {
		Result struct {
			Type  string          `json:"type"`
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	err := c.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    expression,
		"returnByValue": true,
		"awaitPromise":  true,
	}, &res)
	if err != nil {
		return err
	}
	if res.ExceptionDetails != nil {
		msg := res.ExceptionDetails.Text
		if e := res.ExceptionDetails.Exception; e != nil && e.Description != "" {
			msg = e.Description
		}
		return fmt.Errorf("%w: %s", ErrException, msg)
	}
	if out == nil || len(res.Result.Value) == 0 {
		return nil
	}
	if err := json.Unmarshal(res.Result.Value, out); err != nil {
		return fmt.Errorf("cdpc: decode evaluate result: %w", err)
	}
	return nil
}

// Screenshot captures the page viewport as PNG bytes.
func (c *Client) Screenshot(ctx context.Context) ([]byte, error) {
	var res struct {
		Data string `json:"data"`
	}
	if err := c.Call(ctx, "Page.captureScreenshot", map[string]string{"format": "png"}, &res); err != nil {
		return nil, err
	}
	png, err := base64.StdEncoding.DecodeString(res.Data)
	if err != nil {
		return nil, fmt.Errorf("cdpc: decode screenshot: %w", err)
	}
	return png, nil
}

// EnableCSS enables the DOM and CSS domains, which CSS.getPlatformFontsForNode
// requires.
func (c *Client) EnableCSS(ctx context.Context) error {
	if err := c.Call(ctx, "DOM.enable", nil, nil); err != nil {
		return err
	}
	return c.Call(ctx, "CSS.enable", nil, nil)
}

// QuerySelectorAll returns the node ids matching selector in the main document.
func (c *Client) QuerySelectorAll(ctx context.Context, selector string) ([]int, error) {
	var doc struct {
		Root struct {
			NodeID int `json:"nodeId"`
		} `json:"root"`
	}
	if err := c.Call(ctx, "DOM.getDocument", map[string]any{"depth": 0}, &doc); err != nil {
		return nil, err
	}
	var res struct {
		NodeIDs []int `json:"nodeIds"`
	}
	err := c.Call(ctx, "DOM.querySelectorAll", map[string]any{"nodeId": doc.Root.NodeID, "selector": selector}, &res)
	if err != nil {
		return nil, err
	}
	return res.NodeIDs, nil
}

// PlatformFont is one entry of CSS.getPlatformFontsForNode.
type PlatformFont struct {
	FamilyName     string `json:"familyName"`
	PostScriptName string `json:"postScriptName"`
	IsCustomFont   bool   `json:"isCustomFont"`
	GlyphCount     int    `json:"glyphCount"`
}

// PlatformFonts lists the fonts that rendered the text of the node.
func (c *Client) PlatformFonts(ctx context.Context, nodeID int) ([]PlatformFont, error) {
	var res struct {
		Fonts []PlatformFont `json:"fonts"`
	}
	if err := c.Call(ctx, "CSS.getPlatformFontsForNode", map[string]int{"nodeId": nodeID}, &res); err != nil {
		return nil, err
	}
	return res.Fonts, nil
}

// DominantFont returns the font with the most glyphs, or false for an empty list.
func DominantFont(fonts []PlatformFont) (PlatformFont, bool) {
	var best PlatformFont
	found := false
	for _, f := range fonts {
		if !found || f.GlyphCount > best.GlyphCount {
			best, found = f, true
		}
	}
	return best, found
}
