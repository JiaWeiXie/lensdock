package cdpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const maxDiscoveryBody = 4 << 20

// HTTPError is a non-2xx answer (or refused WebSocket upgrade). Body is the
// raw response body when one was read; for the LensDock proxy it is an
// apiwire.Error JSON document.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("cdpc: HTTP status %d", e.Status) }

// Target is one entry of /json/list.
type Target struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	Title                string `json:"title"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// Version is /json/version.
type Version struct {
	Browser              string `json:"Browser"`
	ProtocolVersion      string `json:"Protocol-Version"`
	UserAgent            string `json:"User-Agent"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// Discovery talks to a CDP HTTP endpoint, either the LensDock runtime proxy
// (Base = {api}/api/v1/workspaces/{ws}/cdp, Token = AGENT lease token) or a
// local bridge (Base is the bridge discovery URL, with the secret prefix in
// --path-secret mode; Token empty).
type Discovery struct {
	Base  string
	Token string
	HC    *http.Client
}

func (d *Discovery) do(ctx context.Context, method, path, rawQuery string) ([]byte, error) {
	u, err := url.Parse(d.Base)
	if err != nil {
		return nil, fmt.Errorf("cdpc: bad base URL: %w", err)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	u.RawQuery = rawQuery
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("cdpc: build request: %w", err)
	}
	if d.Token != "" {
		req.Header.Set("Authorization", "Bearer "+d.Token)
	}
	hc := d.HC
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cdpc: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDiscoveryBody))
	if err != nil {
		return nil, fmt.Errorf("cdpc: read %s: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &HTTPError{Status: resp.StatusCode, Body: string(body)}
	}
	return body, nil
}

// Version fetches /json/version.
func (d *Discovery) Version(ctx context.Context) (Version, error) {
	var v Version
	body, err := d.do(ctx, http.MethodGet, "/json/version", "")
	if err != nil {
		return v, err
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return v, fmt.Errorf("cdpc: decode /json/version: %w", err)
	}
	return v, nil
}

// List fetches /json/list.
func (d *Discovery) List(ctx context.Context) ([]Target, error) {
	body, err := d.do(ctx, http.MethodGet, "/json/list", "")
	if err != nil {
		return nil, err
	}
	var ts []Target
	if err := json.Unmarshal(body, &ts); err != nil {
		return nil, fmt.Errorf("cdpc: decode /json/list: %w", err)
	}
	return ts, nil
}

// NewTarget opens a new tab at pageURL (PUT /json/new?<url>).
func (d *Discovery) NewTarget(ctx context.Context, pageURL string) (Target, error) {
	var t Target
	body, err := d.do(ctx, http.MethodPut, "/json/new", pageURL)
	if err != nil {
		return t, err
	}
	if err := json.Unmarshal(body, &t); err != nil {
		return t, fmt.Errorf("cdpc: decode /json/new: %w", err)
	}
	return t, nil
}

// CloseTarget closes a tab.
func (d *Discovery) CloseTarget(ctx context.Context, id string) error {
	_, err := d.do(ctx, http.MethodGet, "/json/close/"+url.PathEscape(id), "")
	return err
}

// WebSocketURL re-roots the path of a reported webSocketDebuggerUrl onto the
// scheme and host of Base. The runtime proxy rewrites discovery documents to
// its public prefix, so only the host part (which may be the container-internal
// address when a test runs in a sidecar) needs adjusting.
func (d *Discovery) WebSocketURL(reported string) (string, error) {
	r, err := url.Parse(reported)
	if err != nil {
		return "", fmt.Errorf("cdpc: bad WebSocket URL: %w", err)
	}
	b, err := url.Parse(d.Base)
	if err != nil {
		return "", fmt.Errorf("cdpc: bad base URL: %w", err)
	}
	if !strings.HasPrefix(r.Path, b.Path+"/devtools/") {
		return "", fmt.Errorf("cdpc: WebSocket path %q is outside the endpoint prefix", r.Path)
	}
	scheme := "ws"
	if b.Scheme == "https" {
		scheme = "wss"
	}
	return (&url.URL{Scheme: scheme, Host: b.Host, Path: r.Path}).String(), nil
}

// Dial opens the WebSocket of a target (or of the browser, via Version).
func (d *Discovery) Dial(ctx context.Context, reportedWS string) (*Client, error) {
	ws, err := d.WebSocketURL(reportedWS)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	if d.Token != "" {
		h.Set("Authorization", "Bearer "+d.Token)
	}
	return Dial(ctx, ws, DialOptions{Header: h})
}
