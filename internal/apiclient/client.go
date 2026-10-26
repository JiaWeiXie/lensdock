// Package apiclient is the Go client of the runtime API used by the lensdock
// CLI and the CDP bridge. It never logs or includes credentials in errors.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/auth"
	"github.com/JiaWeiXie/lensdock/internal/lease"
	"github.com/JiaWeiXie/lensdock/internal/netx"
)

const (
	callTimeout      = 15 * time.Second
	maxErrorBody     = 64 << 10
	clipboardContent = "text/plain; charset=utf-8"
)

// ErrUnreachable wraps transport failures: the runtime could not be reached
// or did not answer in time. The CLI maps it to its "upstream unreachable"
// exit code.
var ErrUnreachable = errors.New("apiclient: runtime unreachable")

// APIError is a non-2xx answer of the runtime API.
type APIError struct {
	Status    int
	Code      string
	Message   string
	RequestID string
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("runtime API: HTTP %d", e.Status)
	if e.Code != "" {
		s += " " + e.Code
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.RequestID != "" {
		s += " (request " + e.RequestID + ")"
	}
	return s
}

// Terminal reports whether retrying with the same credentials cannot succeed:
// the credential was refused (401, 403), the lease or workspace is gone (404),
// the lease ended (409 LEASE_ENDED) or a precondition failed (412).
func (e *APIError) Terminal() bool {
	switch e.Status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusPreconditionFailed:
		return true
	case http.StatusConflict:
		return e.Code == apiwire.CodeLeaseEnded || e.Code == apiwire.CodeAgentModeDisabled || e.Code == apiwire.CodeAgentPaused
	}
	return false
}

// Client talks to one runtime API endpoint.
type Client struct {
	base *url.URL
	hc   *http.Client
}

// New validates base and returns a Client. Plain http is only accepted for
// loopback hosts (127.0.0.1, localhost, [::1]) with an explicit port, so a
// bearer token is never sent in clear text over a network. hc may be nil;
// redirects are never followed because they would carry the Authorization
// header to another origin.
func New(base string, hc *http.Client) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, errors.New("apiclient: invalid base URL")
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("apiclient: base URL must be scheme://host[:port] without credentials, path or query")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if u.Port() == "" || !netx.IsLoopbackHost(u.Hostname()) {
			return nil, errors.New("apiclient: plain http base URL must be http://127.0.0.1|localhost|[::1]:port")
		}
	default:
		return nil, errors.New("apiclient: base URL scheme must be http or https")
	}
	var client http.Client
	if hc != nil {
		client = *hc
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if hc == nil {
		client.Transport = &http.Transport{Proxy: nil, DisableCompression: true}
	}
	return &Client{base: &url.URL{Scheme: u.Scheme, Host: u.Host}, hc: &client}, nil
}

// BaseURL returns the validated base URL without a trailing slash.
func (c *Client) BaseURL() string { return c.base.String() }

type request struct {
	method      string
	path        string
	token       auth.Secret
	idemKey     string
	contentType string
	body        []byte
	out         any
	epoch       uint64
	generation  string
	// noDeadline lifts the per-call timeout for a user operation whose size
	// is not limited (the caller's context still applies).
	noDeadline bool
}

func (c *Client) do(ctx context.Context, r request) error {
	if !r.noDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, callTimeout)
		defer cancel()
	}

	u := *c.base
	u.RawPath = r.path
	p, err := url.PathUnescape(r.path)
	if err != nil {
		return errors.New("apiclient: invalid request path")
	}
	u.Path = p
	var body io.Reader
	if r.body != nil {
		body = bytes.NewReader(r.body)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, u.String(), body)
	if err != nil {
		return errors.New("apiclient: build request")
	}
	req.Header.Set("Authorization", "Bearer "+r.token.Reveal())
	req.Header.Set("Accept", "application/json")
	if r.idemKey != "" {
		req.Header.Set("Idempotency-Key", r.idemKey)
	}
	if r.contentType != "" {
		req.Header.Set("Content-Type", r.contentType)
	}
	if r.generation != "" {
		req.Header.Set("X-LensDock-Lease-Epoch", strconv.FormatUint(r.epoch, 10))
		req.Header.Set("X-LensDock-Runtime-Generation", r.generation)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		// url.Error carries only method and URL, never headers or bodies.
		return fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeError(resp)
	}
	if r.out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	// Responses are read in full: the Gateway is the authenticated peer and
	// a large file listing must not be refused by an implicit size quota.
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%w: read response: %w", ErrUnreachable, err)
	}
	if err := json.Unmarshal(data, r.out); err != nil {
		return errors.New("apiclient: malformed response body")
	}
	return nil
}

func decodeError(resp *http.Response) error {
	e := &APIError{Status: resp.StatusCode, RequestID: resp.Header.Get("X-Request-Id")}
	if w, ok := apiwire.ReadError(resp.Body, maxErrorBody); ok {
		e.Code, e.Message = w.Code, w.Message
		if w.RequestID != "" {
			e.RequestID = w.RequestID
		}
	}
	return e
}

func workspacePath(ws string, rest ...string) string {
	p := "/api/v1/workspaces/" + url.PathEscape(ws)
	for _, s := range rest {
		p += "/" + url.PathEscape(s)
	}
	return p
}

// AcquireLease requests a lease for the authenticated identity. A zero ttl selects
// the server default. feedbackID binds an agent grant to acknowledged feedback.
func (c *Client) AcquireLease(ctx context.Context, ws string, op auth.Secret, mode lease.Mode, ttl time.Duration, idemKey, feedbackID string, feedbackRevision int) (apiwire.LeaseGrant, error) {
	req := apiwire.AcquireLease{Mode: string(mode), FeedbackID: feedbackID, FeedbackRevision: feedbackRevision}
	if ttl > 0 {
		secs := int(ttl / time.Second)
		req.TTLSeconds = &secs
	}
	body, err := json.Marshal(req)
	if err != nil {
		return apiwire.LeaseGrant{}, errors.New("apiclient: encode request")
	}
	var grant apiwire.LeaseGrant
	err = c.do(ctx, request{
		method: http.MethodPost, path: workspacePath(ws, "leases"), token: op,
		idemKey: idemKey, contentType: "application/json", body: body, out: &grant,
	})
	return grant, err
}

// RenewLease extends the lease identified by leaseID with its own token.
func (c *Client) RenewLease(ctx context.Context, ws, leaseID string, tok auth.Secret, epoch uint64, gen string) (apiwire.Lease, error) {
	body, err := json.Marshal(apiwire.RenewLease{Epoch: epoch, RuntimeGeneration: gen})
	if err != nil {
		return apiwire.Lease{}, errors.New("apiclient: encode request")
	}
	var l apiwire.Lease
	err = c.do(ctx, request{
		method: http.MethodPost, path: workspacePath(ws, "leases", leaseID, "renew"), token: tok,
		contentType: "application/json", body: body, out: &l,
	})
	return l, err
}

// ReleaseLease ends a lease. tok is the lease token or the operator token.
func (c *Client) ReleaseLease(ctx context.Context, ws, leaseID string, tok auth.Secret) error {
	return c.do(ctx, request{method: http.MethodDelete, path: workspacePath(ws, "leases", leaseID), token: tok})
}

// CurrentLease reads the grant currently bound to this capability. It does not
// renew it or resume a disconnected controller.
func (c *Client) CurrentLease(ctx context.Context, ws string, tok auth.Secret) (apiwire.Lease, error) {
	var current apiwire.Lease
	err := c.do(ctx, request{method: http.MethodGet, path: workspacePath(ws, "leases", "current"), token: tok, out: &current})
	return current, err
}

// Workspace reads the workspace state with the operator token.
func (c *Client) Workspace(ctx context.Context, ws string, op auth.Secret) (apiwire.Workspace, error) {
	var w apiwire.Workspace
	err := c.do(ctx, request{method: http.MethodGet, path: workspacePath(ws), token: op, out: &w})
	return w, err
}

// SetClipboard publishes text with a HUMAN or AGENT lease token. text is sent
// verbatim.
func (c *Client) SetClipboard(ctx context.Context, ws string, tok auth.Secret, text []byte, epoch uint64, generation string) error {
	return c.do(ctx, request{
		method: http.MethodPut, path: workspacePath(ws, "clipboard"), token: tok,
		contentType: clipboardContent, body: text,
		epoch: epoch, generation: generation, noDeadline: true,
	})
}
