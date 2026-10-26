package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/auth"
)

type runtimeEdge struct {
	config       RuntimeConfig
	url          *url.URL
	publicOrigin *url.URL
	operator     string
	client       *http.Client
	proxy        *httputil.ReverseProxy
	streams      *streamGroup
	control      sync.Mutex
	// collab orders usage-mode transitions against collaboration mutations: a
	// mutation holds the read side from its policy check to its end, and the
	// switch to NOVNC takes the write side before it asks the runtime.
	collab        sync.RWMutex
	collabStreams streamCancels
}
type ownedLease struct {
	Actor      Identity
	Grant      apiwire.LeaseGrant
	FeedbackID string
}
type leaseRegistry struct {
	mu      sync.Mutex
	byToken map[string]ownedLease
}

func (s *leaseRegistry) put(l ownedLease) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, old := range s.byToken {
		if old.Actor.WorkspaceID == l.Actor.WorkspaceID && old.Grant.Lease.RuntimeGeneration == l.Grant.Lease.RuntimeGeneration && old.Grant.Lease.Epoch > l.Grant.Lease.Epoch {
			return
		}
	}
	s.byToken[digest(l.Grant.CapabilityToken)] = l
}
func (s *leaseRegistry) get(token string) (ownedLease, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.byToken[digest(token)]
	if ok && !time.Now().Before(l.Grant.Lease.ExpiresAt) {
		delete(s.byToken, digest(token))
		return ownedLease{}, false
	}
	return l, ok
}
func (s *leaseRegistry) removeWorkspace(ws string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, l := range s.byToken {
		if l.Actor.WorkspaceID == ws {
			delete(s.byToken, k)
		}
	}
}

// removeAgents drops every Agent grant of a workspace. A HUMAN grant stays.
func (s *leaseRegistry) removeAgents(ws string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, l := range s.byToken {
		if l.Actor.WorkspaceID == ws && (l.Actor.Role == "agent" || l.Grant.Lease.Mode == "AGENT") {
			delete(s.byToken, k)
		}
	}
}
func (s *leaseRegistry) forActor(p Identity) (ownedLease, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.byToken {
		if l.Actor.ID == p.ID && l.Actor.WorkspaceID == p.WorkspaceID && time.Now().Before(l.Grant.Lease.ExpiresAt) {
			return l, true
		}
	}
	return ownedLease{}, false
}

func newRuntimeEdge(c RuntimeConfig, streams *streamGroup) (*runtimeEdge, error) {
	u, t, err := runtimeTransport(c)
	if err != nil {
		return nil, err
	}
	b, err := auth.ReadPrivateFile(c.OperatorTokenFile, 4096)
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return nil, errors.New("gateway: empty runtime credential")
	}
	e := &runtimeEdge{config: c, url: u, operator: token, streams: streams, client: &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	e.proxy = &httputil.ReverseProxy{Transport: t, Rewrite: func(p *httputil.ProxyRequest) {
		p.SetURL(u)
		p.Out.Host = u.Host
		p.Out.Header.Del("Cookie")
		p.Out.Header.Del("Origin")
		p.Out.Header.Del("X-CSRF-Token")
		p.Out.Header.Del("X-LensDock-Lease")
		p.Out.Header.Del("Forwarded")
		p.Out.Header.Del("X-Forwarded-For")
		p.Out.Header.Del("X-Forwarded-Host")
		p.Out.Header.Del("X-Forwarded-Proto")
	}, ModifyResponse: e.rewriteDiscovery, ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
		fail(w, 502, "UPSTREAM_UNAVAILABLE", "runtime connection failed")
	}}
	return e, nil
}

// call is a short control request to the runtime; it has a fixed deadline.
func (e *runtimeEdge) call(ctx context.Context, method, path, credential, holder string, body []byte, headers http.Header) (int, http.Header, []byte, error) {
	return e.roundTrip(ctx, 20*time.Second, method, path, credential, holder, body, headers)
}

// callTransfer fetches user data (a viewport screenshot) whose size is not
// limited. It has no deadline unless RuntimeConfig.TransferTimeoutSeconds sets one.
func (e *runtimeEdge) callTransfer(ctx context.Context, method, path, credential, holder string, body []byte, headers http.Header) (int, http.Header, []byte, error) {
	return e.roundTrip(ctx, time.Duration(e.config.TransferTimeoutSeconds)*time.Second, method, path, credential, holder, body, headers)
}

func (e *runtimeEdge) roundTrip(ctx context.Context, timeout time.Duration, method, path, credential, holder string, body []byte, headers http.Header) (int, http.Header, []byte, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	u := *e.url
	u.Path, u.RawQuery, _ = strings.Cut(path, "?")
	r, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	r.Header = headers.Clone()
	if r.Header == nil {
		r.Header = make(http.Header)
	}
	r.Header.Set("Authorization", "Bearer "+credential)
	if holder != "" {
		r.Header.Set("X-LensDock-Holder-ID", holder)
	}
	r.Header.Del("Cookie")
	r.Header.Del("Origin")
	resp, err := e.client.Do(r)
	if err != nil {
		return 0, nil, nil, errors.New("runtime unavailable")
	}
	defer resp.Body.Close()
	// The runtime is a fixed trusted peer and every call has a deadline, so
	// the response is read in full: a large viewport screenshot must not be
	// refused by an implicit size quota (limits are opt-in at the source).
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, errors.New("runtime response failed")
	}
	return resp.StatusCode, resp.Header, b, nil
}
func forwardReply(w http.ResponseWriter, status int, h http.Header, b []byte) {
	for _, k := range []string{"Content-Type", "ETag", "Allow", "X-Request-Id"} {
		if v := h.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
func (e *runtimeEdge) stream(w http.ResponseWriter, r *http.Request, credential, holder string) {
	if r.Header.Get("Upgrade") != "" {
		// An upgraded VNC/CDP socket is hijacked: http.Server.Shutdown neither
		// closes nor waits for it. Binding it to the stream group lets the
		// Gateway cancel it (the reverse proxy then closes both halves) and wait
		// for this handler before storage closes. Ordinary proxied requests stay
		// outside the group and finish gracefully.
		var release func()
		var ok bool
		if r, release, ok = e.streams.admit(w, r); !ok {
			return
		}
		defer release()
	}
	r = r.Clone(r.Context())
	r.Header = r.Header.Clone()
	r.Header.Set("Authorization", "Bearer "+credential)
	r.Header.Del("X-LensDock-Holder-ID")
	if holder != "" {
		r.Header.Set("X-LensDock-Holder-ID", holder)
	}
	e.proxy.ServeHTTP(w, r)
}
func decodeGrant(b []byte) (apiwire.LeaseGrant, error) {
	var g apiwire.LeaseGrant
	err := json.Unmarshal(b, &g)
	if err == nil && (g.CapabilityToken == "" || g.Lease.LeaseID == "") {
		err = errors.New("runtime returned an invalid lease grant")
	}
	return g, err
}

func (e *runtimeEdge) rewriteDiscovery(resp *http.Response) error {
	if resp.StatusCode != 200 || !strings.Contains(resp.Request.URL.Path, "/cdp/json") || e.publicOrigin == nil {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	resp.Body.Close()
	if err != nil || len(b) >= 16<<20 {
		return errors.New("gateway: discovery exceeds limit")
	}
	var value any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return err
	}
	prefix := "/api/v1/workspaces/" + e.config.WorkspaceID + "/cdp/devtools/"
	if err := e.rewriteDiscoveryValue(value, prefix); err != nil {
		return err
	}
	out, err := json.Marshal(value)
	if err != nil {
		return err
	}
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
	return nil
}

func (e *runtimeEdge) rewriteDiscoveryValue(v any, prefix string) error {
	switch v := v.(type) {
	case []any:
		for _, item := range v {
			if err := e.rewriteDiscoveryValue(item, prefix); err != nil {
				return err
			}
		}
	case map[string]any:
		delete(v, "devtoolsFrontendUrl")
		delete(v, "devtoolsFrontendUrlCompat")
		raw, ok := v["webSocketDebuggerUrl"]
		if !ok {
			return nil
		}
		s, ok := raw.(string)
		if !ok {
			return errors.New("gateway: invalid discovery URL")
		}
		u, err := url.Parse(s)
		if err != nil || !strings.HasPrefix(u.Path, prefix) || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("gateway: unexpected discovery path")
		}
		u.Host = e.publicOrigin.Host
		u.Scheme = "ws"
		if e.publicOrigin.Scheme == "https" {
			u.Scheme = "wss"
		}
		u.User = nil
		v["webSocketDebuggerUrl"] = u.String()
	}
	return nil
}
