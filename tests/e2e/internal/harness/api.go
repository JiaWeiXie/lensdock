package harness

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/tests/e2e/internal/cdpc"
)

const maxBody = 8 << 20

// API is a black-box client of the runtime API. It deliberately does not use
// internal/apiclient so the tests observe the wire contract itself.
type API struct {
	Base      string // http://host:port without trailing slash
	Workspace string
	Operator  string // operator token; never logged
	HC        *http.Client
}

// NewAPI builds an API from the LENSDOCK_E2E_* environment.
func NewAPI(t TB) *API {
	t.Helper()
	tok, err := os.ReadFile(Require(t, EnvOperatorToken))
	if err != nil {
		t.Fatalf("read operator token file: %v", err)
	}
	return &API{
		Base:      strings.TrimSuffix(Require(t, EnvAPI), "/"),
		Workspace: Default(EnvWorkspace, "default"),
		Operator:  strings.TrimSpace(string(tok)),
		HC:        &http.Client{Timeout: 30 * time.Second},
	}
}

// Response is a fully read HTTP answer.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// ErrorBody decodes the body as an apiwire.Error. The second result is false
// when the body is not a well-formed error document.
func (r Response) ErrorBody() (apiwire.Error, bool) {
	var e apiwire.Error
	if err := json.Unmarshal(r.Body, &e); err != nil || e.Code == "" {
		return apiwire.Error{}, false
	}
	return e, true
}

// APIError is a non-2xx answer decoded as the runtime error document.
type APIError struct {
	Status    int
	Code      string
	Message   string
	RequestID string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("runtime API error: HTTP %d %s: %s", e.Status, e.Code, e.Message)
}

// Do performs one request. token is sent as a Bearer credential when not
// empty; headers are added verbatim. It never follows redirects.
func (a *API) Do(ctx context.Context, method, path, token string, headers map[string]string, body []byte) (Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.Base+path, rd)
	if err != nil {
		return Response{}, fmt.Errorf("build request: %w", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	hc := *a.HC
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return Response{}, fmt.Errorf("read %s %s: %w", method, path, err)
	}
	return Response{Status: resp.StatusCode, Header: resp.Header, Body: b}, nil
}

func (a *API) wsPath(suffix string) string {
	return "/api/v1/workspaces/" + url.PathEscape(a.Workspace) + suffix
}

// WorkspacePath returns the API path below the workspace, for tests that need
// raw requests.
func (a *API) WorkspacePath(suffix string) string { return a.wsPath(suffix) }

func asError(r Response) error {
	if r.Status >= 200 && r.Status <= 299 {
		return nil
	}
	e, _ := r.ErrorBody()
	return &APIError{Status: r.Status, Code: e.Code, Message: e.Message, RequestID: e.RequestID}
}

// Grant is a lease plus its capability token.
type Grant struct {
	Lease apiwire.Lease
	Token string
}

func randomKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("random idempotency key: %w", err)
	}
	return "e2e-" + hex.EncodeToString(b[:]), nil
}

// Acquire requests a lease as the operator. ttlSeconds 0 selects the server default.
func (a *API) Acquire(ctx context.Context, mode string, ttlSeconds int) (Grant, error) {
	key, err := randomKey()
	if err != nil {
		return Grant{}, err
	}
	req := apiwire.AcquireLease{Mode: mode}
	if ttlSeconds > 0 {
		req.TTLSeconds = &ttlSeconds
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Grant{}, fmt.Errorf("marshal acquire: %w", err)
	}
	resp, err := a.Do(ctx, http.MethodPost, a.wsPath("/leases"), a.Operator,
		map[string]string{"Idempotency-Key": key, "Content-Type": "application/json"}, body)
	if err != nil {
		return Grant{}, err
	}
	if err := asError(resp); err != nil {
		return Grant{}, err
	}
	var g apiwire.LeaseGrant
	if err := json.Unmarshal(resp.Body, &g); err != nil {
		return Grant{}, fmt.Errorf("decode lease grant: %w", err)
	}
	return Grant{Lease: g.Lease, Token: g.CapabilityToken}, nil
}

// AcquireWhenFree is Acquire that waits (up to wait) while another lease is
// held or the runtime is restarting, logging each distinct reason once. It
// exists so one failed test does not cascade into every later test.
func (a *API) AcquireWhenFree(ctx context.Context, t TB, mode string, ttlSeconds int, wait time.Duration) (Grant, error) {
	t.Helper()
	deadline := time.Now().Add(wait)
	lastCode := ""
	for {
		g, err := a.Acquire(ctx, mode, ttlSeconds)
		var ae *APIError
		if err == nil || !errors.As(err, &ae) ||
			(ae.Code != apiwire.CodeLeaseConflict && ae.Code != apiwire.CodeRuntimeNotReady) || time.Now().After(deadline) {
			return g, err
		}
		if ae.Code != lastCode {
			t.Logf("acquire %s lease: waiting while %s", mode, ae.Code)
			lastCode = ae.Code
		}
		select {
		case <-ctx.Done():
			return Grant{}, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// Renew renews a lease with its own token.
func (a *API) Renew(ctx context.Context, leaseID, token string, epoch uint64, generation string) (apiwire.Lease, error) {
	body, err := json.Marshal(apiwire.RenewLease{Epoch: epoch, RuntimeGeneration: generation})
	if err != nil {
		return apiwire.Lease{}, fmt.Errorf("marshal renew: %w", err)
	}
	resp, err := a.Do(ctx, http.MethodPost, a.wsPath("/leases/"+url.PathEscape(leaseID)+"/renew"), token,
		map[string]string{"Content-Type": "application/json"}, body)
	if err != nil {
		return apiwire.Lease{}, err
	}
	if err := asError(resp); err != nil {
		return apiwire.Lease{}, err
	}
	var l apiwire.Lease
	if err := json.Unmarshal(resp.Body, &l); err != nil {
		return apiwire.Lease{}, fmt.Errorf("decode lease: %w", err)
	}
	return l, nil
}

// Release ends a lease. An empty token releases as the operator.
func (a *API) Release(ctx context.Context, leaseID, token string) error {
	if token == "" {
		token = a.Operator
	}
	resp, err := a.Do(ctx, http.MethodDelete, a.wsPath("/leases/"+url.PathEscape(leaseID)), token, nil, nil)
	if err != nil {
		return err
	}
	return asError(resp)
}

// WorkspaceState reads the workspace as the operator.
func (a *API) WorkspaceState(ctx context.Context) (apiwire.Workspace, error) {
	resp, err := a.Do(ctx, http.MethodGet, a.wsPath(""), a.Operator, nil, nil)
	if err != nil {
		return apiwire.Workspace{}, err
	}
	if err := asError(resp); err != nil {
		return apiwire.Workspace{}, err
	}
	var w apiwire.Workspace
	if err := json.Unmarshal(resp.Body, &w); err != nil {
		return apiwire.Workspace{}, fmt.Errorf("decode workspace: %w", err)
	}
	return w, nil
}

// WaitReady polls until the workspace reports ready.
func (a *API) WaitReady(ctx context.Context, wait time.Duration) (apiwire.Workspace, error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		w, err := a.WorkspaceState(ctx)
		if err == nil && w.Ready {
			return w, nil
		}
		select {
		case <-ctx.Done():
			return w, fmt.Errorf("workspace not ready within %v (last error: %v): %w", wait, err, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// PutClipboard writes the HUMAN clipboard.
func (a *API) PutClipboard(ctx context.Context, token string, text []byte) error {
	resp, err := a.Do(ctx, http.MethodPut, a.wsPath("/clipboard"), token,
		map[string]string{"Content-Type": "text/plain; charset=utf-8"}, text)
	if err != nil {
		return err
	}
	return asError(resp)
}

// Readyz fetches /readyz; with detail it asks for ?detail=1 as the operator.
func (a *API) Readyz(ctx context.Context, detail bool) (Response, error) {
	if detail {
		return a.Do(ctx, http.MethodGet, "/readyz?detail=1", a.Operator, nil, nil)
	}
	return a.Do(ctx, http.MethodGet, "/readyz", "", nil, nil)
}

// CDPBase is the public URL prefix of the CDP proxy.
func (a *API) CDPBase() string { return a.Base + a.wsPath("/cdp") }

// CDP returns a discovery client for the CDP proxy authenticated with token.
func (a *API) CDP(token string) *cdpc.Discovery {
	return &cdpc.Discovery{Base: a.CDPBase(), Token: token, HC: &http.Client{Timeout: 30 * time.Second}}
}
