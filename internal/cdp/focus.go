package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/auth"
)

const maxFocusTargets = 128

// ErrPageNotFocused means keyboard focus is outside every page target, for
// example in Chromium's address bar or another native browser control.
var ErrPageNotFocused = errors.New("keyboard focus is not in a page")

// FocusConfig identifies the lease-fenced CDP mount used by one focus read.
type FocusConfig struct {
	Upstream          *url.URL
	Token             auth.Secret
	Epoch             uint64
	RuntimeGeneration string
}

// FocusedElement is the non-sensitive page element description returned to
// the CLI. Callers own their public output type.
type FocusedElement struct {
	Role     string
	Name     string
	Selector string
}

// ReadFocusedElement reads the active DOM element and its computed
// accessibility role and name without activating or changing a page.
func ReadFocusedElement(ctx context.Context, cfg FocusConfig) (FocusedElement, error) {
	reader, err := newFocusReader(cfg)
	if err != nil {
		return FocusedElement{}, err
	}
	targets, err := reader.targets(ctx)
	if err != nil {
		return FocusedElement{}, err
	}
	var focused *focusConn
	defer func() {
		if focused != nil {
			focused.close()
		}
	}()
	for _, target := range targets {
		if target.Type != "page" {
			continue
		}
		if !validID(target.ID) {
			return FocusedElement{}, errors.New("cdp: malformed page target")
		}
		conn, err := reader.dial(ctx, target.ID)
		if err != nil {
			return FocusedElement{}, err
		}
		ownsFocus, err := conn.hasFocus(ctx)
		if err != nil {
			conn.close()
			return FocusedElement{}, err
		}
		if !ownsFocus {
			conn.close()
			continue
		}
		if focused != nil {
			conn.close()
			return FocusedElement{}, errors.New("cdp: several pages report keyboard focus")
		}
		focused = conn
	}
	if focused == nil {
		return FocusedElement{}, ErrPageNotFocused
	}
	result, err := focused.element(ctx)
	if err != nil {
		return FocusedElement{}, err
	}
	stillFocused, err := focused.hasFocus(ctx)
	if err != nil {
		return FocusedElement{}, err
	}
	if !stillFocused {
		return FocusedElement{}, errors.New("cdp: keyboard focus changed during inspection")
	}
	return result, nil
}

type focusReader struct {
	upstream   *url.URL
	token      auth.Secret
	epoch      string
	generation string
	client     *http.Client
}

func newFocusReader(cfg FocusConfig) (*focusReader, error) {
	u := cfg.Upstream
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("cdp: focus upstream must be an http(s) URL without credentials or query")
	}
	if cfg.Token == "" || cfg.Epoch == 0 || cfg.RuntimeGeneration == "" {
		return nil, errors.New("cdp: focus requires a complete lease")
	}
	up := *u
	up.Path = strings.TrimRight(u.Path, "/")
	up.RawPath = ""
	return &focusReader{
		upstream:   &up,
		token:      cfg.Token,
		epoch:      strconv.FormatUint(cfg.Epoch, 10),
		generation: cfg.RuntimeGeneration,
		client:     newUpstreamClient(),
	}, nil
}

type focusTarget struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

func (r *focusReader) targets(ctx context.Context) ([]focusTarget, error) {
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	u := *r.upstream
	u.Path += "/json/list"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("cdp: build focus discovery request")
	}
	r.headers(req.Header)
	resp, err := r.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("cdp: focus discovery failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, focusHTTPError(resp, "focus discovery")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDiscoveryBody+1))
	if err != nil || len(body) > maxDiscoveryBody {
		return nil, errors.New("cdp: focus discovery unreadable")
	}
	var targets []focusTarget
	if json.Unmarshal(body, &targets) != nil || len(targets) > maxFocusTargets {
		return nil, errors.New("cdp: focus discovery malformed")
	}
	return targets, nil
}

func (r *focusReader) dial(ctx context.Context, targetID string) (*focusConn, error) {
	u := *r.upstream
	u.Path += "/devtools/page/" + url.PathEscape(targetID)
	header := http.Header{}
	r.headers(header)
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	conn, resp, err := websocket.Dial(dialCtx, u.String(), &websocket.DialOptions{
		HTTPClient:      r.client,
		HTTPHeader:      header,
		CompressionMode: websocket.CompressionDisabled,
	})
	cancel()
	if err == nil {
		conn.SetReadLimit(maxClientMessage)
		return &focusConn{ws: conn}, nil
	}
	if resp != nil {
		defer resp.Body.Close()
		return nil, focusHTTPError(resp, "focus target")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if leaseClose(err) {
		return nil, fmt.Errorf("%w: focus connection closed", ErrLeaseLost)
	}
	return nil, errors.New("cdp: focus target unavailable")
}

func focusHTTPError(resp *http.Response, operation string) error {
	apiErr, ok := apiwire.ReadError(resp.Body, maxErrorBody)
	if ok && focusTerminalCode(apiErr.Code) {
		return fmt.Errorf("%w: %s rejected (HTTP %d %s)", ErrLeaseLost, operation, resp.StatusCode, apiErr.Code)
	}
	return fmt.Errorf("cdp: %s rejected (HTTP %d)", operation, resp.StatusCode)
}

func focusTerminalCode(code string) bool {
	switch code {
	case apiwire.CodeUnauthenticated,
		apiwire.CodeWrongLeaseMode,
		apiwire.CodeLeaseEnded,
		apiwire.CodeEpochMismatch,
		apiwire.CodeGenerationMismatch,
		apiwire.CodeAgentModeDisabled,
		apiwire.CodeAgentPaused,
		apiwire.CodeLeaseMismatch:
		return true
	}
	return false
}

func (r *focusReader) headers(header http.Header) {
	header.Set("Authorization", "Bearer "+r.token.Reveal())
	header.Set("X-LensDock-Lease-Epoch", r.epoch)
	header.Set("X-LensDock-Runtime-Generation", r.generation)
	header.Set("Accept", "application/json")
}

type focusConn struct {
	ws     *websocket.Conn
	nextID atomic.Int64
}

func (c *focusConn) close() { _ = c.ws.CloseNow() }

func (c *focusConn) call(ctx context.Context, method string, params, out any) error {
	id := c.nextID.Add(1)
	body, err := json.Marshal(struct {
		ID     int64  `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params,omitempty"`
	}{id, method, params})
	if err != nil {
		return fmt.Errorf("cdp: encode %s request", method)
	}
	if err := c.ws.Write(ctx, websocket.MessageText, body); err != nil {
		return focusConnectionError(ctx, err)
	}
	for {
		_, data, err := c.ws.Read(ctx)
		if err != nil {
			return focusConnectionError(ctx, err)
		}
		var reply struct {
			ID     *int64          `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(data, &reply) != nil || reply.ID == nil || *reply.ID != id {
			continue
		}
		if len(reply.Error) != 0 && string(reply.Error) != "null" {
			return fmt.Errorf("cdp: %s failed", method)
		}
		if out != nil && json.Unmarshal(reply.Result, out) != nil {
			return fmt.Errorf("cdp: decode %s response", method)
		}
		return nil
	}
}

func leaseClose(err error) bool {
	switch websocket.CloseStatus(err) {
	case CloseLeaseEnded, CloseGenerationChanged:
		return true
	}
	return false
}

func focusConnectionError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if leaseClose(err) {
		return fmt.Errorf("%w: focus connection closed", ErrLeaseLost)
	}
	return errors.New("cdp: focus connection closed")
}

type runtimeResult struct {
	Result struct {
		Type     string `json:"type"`
		ObjectID string `json:"objectId"`
		Value    bool   `json:"value"`
	} `json:"result"`
}

func (c *focusConn) hasFocus(ctx context.Context) (bool, error) {
	var result runtimeResult
	params := map[string]any{"expression": "document.hasFocus()", "returnByValue": true}
	if err := c.call(ctx, "Runtime.evaluate", params, &result); err != nil {
		return false, err
	}
	if result.Result.Type != "boolean" {
		return false, errors.New("cdp: document focus response is not boolean")
	}
	return result.Result.Value, nil
}

func (c *focusConn) element(ctx context.Context) (FocusedElement, error) {
	var active runtimeResult
	if err := c.call(ctx, "Runtime.evaluate", map[string]any{"expression": "document.activeElement"}, &active); err != nil {
		return FocusedElement{}, err
	}
	if active.Result.Type != "object" || active.Result.ObjectID == "" {
		return FocusedElement{}, errors.New("cdp: focused DOM element is unavailable")
	}

	var described struct {
		Node struct {
			NodeName      string   `json:"nodeName"`
			BackendNodeID int64    `json:"backendNodeId"`
			Attributes    []string `json:"attributes"`
		} `json:"node"`
	}
	if err := c.call(ctx, "DOM.describeNode", map[string]any{"objectId": active.Result.ObjectID}, &described); err != nil {
		return FocusedElement{}, err
	}
	if described.Node.NodeName == "" || described.Node.BackendNodeID == 0 || len(described.Node.Attributes)%2 != 0 {
		return FocusedElement{}, errors.New("cdp: focused DOM element is malformed")
	}

	var tree struct {
		Nodes []struct {
			BackendNodeID int64 `json:"backendDOMNodeId"`
			Ignored       bool  `json:"ignored"`
			Role          struct {
				Value string `json:"value"`
			} `json:"role"`
			Name struct {
				Value string `json:"value"`
			} `json:"name"`
		} `json:"nodes"`
	}
	params := map[string]any{"backendNodeId": described.Node.BackendNodeID, "fetchRelatives": false}
	if err := c.call(ctx, "Accessibility.getPartialAXTree", params, &tree); err != nil {
		return FocusedElement{}, err
	}
	for _, node := range tree.Nodes {
		if node.BackendNodeID == described.Node.BackendNodeID && !node.Ignored && node.Role.Value != "" {
			return FocusedElement{
				Role:     node.Role.Value,
				Name:     node.Name.Value,
				Selector: selector(described.Node.NodeName, described.Node.Attributes),
			}, nil
		}
	}
	return FocusedElement{}, errors.New("cdp: focused element has no accessibility node")
}

func selector(nodeName string, attributes []string) string {
	attrs := make(map[string]string, len(attributes)/2)
	for i := 0; i+1 < len(attributes); i += 2 {
		attrs[attributes[i]] = attributes[i+1]
	}
	if id := attrs["id"]; id != "" {
		return `[id="` + escapeCSSString(id) + `"]`
	}
	tag := strings.ToLower(nodeName)
	if name := attrs["name"]; name != "" {
		return tag + `[name="` + escapeCSSString(name) + `"]`
	}
	return tag
}

func escapeCSSString(s string) string {
	var b strings.Builder
	for len(s) > 0 {
		r, n := utf8.DecodeRuneInString(s)
		s = s[n:]
		switch {
		case r == utf8.RuneError && n == 1:
			b.WriteRune('\uFFFD')
		case r == 0:
			b.WriteRune('\uFFFD')
		case r == '"', r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\%x `, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
