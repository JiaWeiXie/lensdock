package cdp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/auth"
	"github.com/JiaWeiXie/lensdock/internal/lease"
)

// Close codes sent to proxied clients when the lease ends.
const (
	// CloseLeaseEnded is sent when the lease was released, expired or the
	// runtime shut down.
	CloseLeaseEnded websocket.StatusCode = 4001
	// CloseGenerationChanged is sent when the browser generation changed.
	CloseGenerationChanged websocket.StatusCode = 4002
)

const (
	// UpstreamHost is the Host header sent to Chromium, which only accepts
	// IP-literal and localhost hosts.
	UpstreamHost = "127.0.0.1:9223"

	defaultMaxConnsPerLease = 16
	maxClientMessage        = 16 << 20
	maxUpstreamMessage      = 256 << 20
	httpTimeout             = 15 * time.Second
	dialTimeout             = 10 * time.Second
	// leaseGrace bounds how long an upstream failure waits for the lease-end
	// signal that usually follows a browser restart, so the client sees 4002
	// rather than a generic close.
	leaseGrace = 500 * time.Millisecond

	deniedMessage    = "Method denied by LensDock policy"
	malformedMessage = "Invalid message"
)

// Authorizer admits a bearer token for a lease mode. lease.Manager satisfies it.
type Authorizer interface {
	Authorize(token string, want lease.Mode) (lease.Ticket, error)
}

// ProxyConfig configures NewProxy.
type ProxyConfig struct {
	// Upstream is Chromium's DevTools endpoint, http://127.0.0.1:9223.
	Upstream string
	Auth     Authorizer
	// Policy is the denied method set. The zero value selects DefaultPolicy.
	Policy MethodPolicy
	Logger *slog.Logger
	// MaxConnsPerLease caps concurrent WebSockets per lease (default 16).
	MaxConnsPerLease int
	// PublicPrefix returns the externally visible path prefix of the CDP
	// mount, e.g. /api/v1/workspaces/default/cdp. Nil means no prefix.
	PublicPrefix func(r *http.Request) string
}

// Proxy exposes an allowlisted view of Chromium's DevTools endpoints to the
// holder of an AGENT lease. Mount it with http.StripPrefix.
type Proxy struct {
	auth     Authorizer
	policy   MethodPolicy
	log      *slog.Logger
	maxConns int
	prefix   func(*http.Request) string
	upstream *url.URL
	client   *http.Client

	mu    sync.Mutex
	conns map[string]int
}

// NewProxy validates cfg and returns the proxy handler.
func NewProxy(cfg ProxyConfig) (*Proxy, error) {
	if cfg.Auth == nil {
		return nil, errors.New("cdp: proxy requires an Authorizer")
	}
	u, err := url.Parse(cfg.Upstream)
	if err != nil || u.Scheme != "http" || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return nil, errors.New("cdp: proxy upstream must be an http://host:port URL")
	}
	p := &Proxy{
		auth:     cfg.Auth,
		policy:   cfg.Policy,
		log:      cfg.Logger,
		maxConns: cfg.MaxConnsPerLease,
		prefix:   cfg.PublicPrefix,
		upstream: &url.URL{Scheme: "http", Host: u.Host},
		client:   newUpstreamClient(),
		conns:    map[string]int{},
	}
	if p.policy.deny == nil {
		p.policy = DefaultPolicy()
	}
	if p.log == nil {
		p.log = slog.New(slog.DiscardHandler)
	}
	if p.maxConns <= 0 {
		p.maxConns = defaultMaxConnsPerLease
	}
	return p, nil
}

func newUpstreamClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			DisableCompression:    true,
			MaxIdleConns:          4,
			IdleConnTimeout:       30 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// ServeHTTP authenticates and dispatches one request.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, ok := r.Header["Origin"]; ok {
		writeError(w, http.StatusForbidden, apiwire.CodeOriginNotAllowed, "requests with an Origin header are not allowed")
		return
	}
	token, ok := auth.BearerToken(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, apiwire.CodeUnauthenticated, "a bearer lease token is required")
		return
	}
	ticket, err := p.auth.Authorize(token, lease.ModeAgent)
	if err != nil {
		writeAuthorizeError(w, err)
		return
	}
	route, err := ParseRoute(r.Method, r.URL.Path, r.URL.RawQuery)
	if writeRouteError(w, err) {
		return
	}
	if route.IsWebSocket() {
		p.serveWebSocket(w, r, ticket, route)
		return
	}
	p.serveDiscovery(w, r, ticket, route)
}

// authorizeMessages are the fixed client-visible texts per wire code.
var authorizeMessages = map[string]string{
	apiwire.CodeWrongLeaseMode:         "an AGENT lease is required",
	apiwire.CodeLeaseEnded:             "the lease has ended",
	apiwire.CodeRuntimeNotReady:        "the runtime is not ready",
	apiwire.CodeSandboxPreflightFailed: "the runtime is not ready",
	apiwire.CodeUnauthenticated:        "invalid lease token",
}

func writeAuthorizeError(w http.ResponseWriter, err error) {
	status, code, ok := lease.HTTPError(err)
	if !ok {
		writeError(w, http.StatusInternalServerError, apiwire.CodeInternal, "authorization failed")
		return
	}
	msg, known := authorizeMessages[code]
	if !known {
		msg = "authorization failed"
	}
	writeError(w, status, code, msg)
}

func (p *Proxy) publicURLs(r *http.Request) (func(string) string, error) {
	host := r.Host
	if !validHostHeader(host) {
		return nil, errors.New("invalid Host header")
	}
	prefix := ""
	if p.prefix != nil {
		prefix = strings.TrimRight(p.prefix(r), "/")
	}
	return func(devtoolsPath string) string { return "ws://" + host + prefix + devtoolsPath }, nil
}

// validHostHeader accepts host[:port] made of hostname/IP characters only, so
// the value is safe to reflect into discovery URLs.
func validHostHeader(h string) bool {
	if h == "" || len(h) > 255 {
		return false
	}
	for _, c := range []byte(h) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '-', c == ':', c == '[', c == ']':
		default:
			return false
		}
	}
	return true
}

// cancelOnEnd cancels ctx when the ticket's lease ends.
func cancelOnEnd(ctx context.Context, cancel context.CancelFunc, t lease.Ticket) {
	go func() {
		select {
		case <-t.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
}

func (p *Proxy) serveDiscovery(w http.ResponseWriter, r *http.Request, ticket lease.Ticket, route Route) {
	wsURL, err := p.publicURLs(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, apiwire.CodeInvalidRequest, "invalid Host header")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), httpTimeout)
	defer cancel()
	cancelOnEnd(ctx, cancel, ticket)

	u := *p.upstream
	u.Path = route.Path()
	u.RawQuery = route.RawQuery
	req, err := http.NewRequestWithContext(ctx, route.Method(), u.String(), nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, apiwire.CodeUpstreamUnavailable, "upstream request failed")
		return
	}
	req.Host = UpstreamHost
	if accept := r.Header.Get("Accept"); accept != "" {
		req.Header.Set("Accept", accept)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		p.writeUpstreamFailure(w, ticket)
		return
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		writeError(w, http.StatusNotFound, apiwire.CodeNotFound, "target not found")
		return
	case resp.StatusCode != http.StatusOK:
		p.log.Warn("cdp upstream returned unexpected status", "status", resp.StatusCode)
		writeError(w, http.StatusBadGateway, apiwire.CodeUpstreamUnavailable, "upstream request failed")
		return
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDiscoveryBody+1))
	if err != nil {
		p.writeUpstreamFailure(w, ticket)
		return
	}
	if len(body) > maxDiscoveryBody {
		writeError(w, http.StatusBadGateway, apiwire.CodeUpstreamUnavailable, "upstream response too large")
		return
	}
	body, err = RewriteDiscovery(route.Kind, body, wsURL)
	if err != nil {
		p.log.Warn("cdp discovery rewrite failed", "kind", int(route.Kind))
		writeError(w, http.StatusBadGateway, apiwire.CodeUpstreamUnavailable, "upstream response not understood")
		return
	}

	writeDiscovery(w, route.Kind, body)
}

func (p *Proxy) writeUpstreamFailure(w http.ResponseWriter, t lease.Ticket) {
	select {
	case <-t.Done():
		writeError(w, http.StatusConflict, apiwire.CodeLeaseEnded, "the lease has ended")
	default:
		writeError(w, http.StatusBadGateway, apiwire.CodeUpstreamUnavailable, "upstream unavailable")
	}
}

func (p *Proxy) acquireSlot(leaseID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conns[leaseID] >= p.maxConns {
		return false
	}
	p.conns[leaseID]++
	return true
}

func (p *Proxy) releaseSlot(leaseID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conns[leaseID]--; p.conns[leaseID] <= 0 {
		delete(p.conns, leaseID)
	}
}

func (p *Proxy) serveWebSocket(w http.ResponseWriter, r *http.Request, ticket lease.Ticket, route Route) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		writeError(w, http.StatusBadRequest, apiwire.CodeInvalidRequest, "a WebSocket upgrade is required")
		return
	}
	leaseID := ticket.Lease().ID
	if !p.acquireSlot(leaseID) {
		writeError(w, http.StatusTooManyRequests, apiwire.CodeTooManyConnections, "too many connections for this lease")
		return
	}
	defer p.releaseSlot(leaseID)

	dialCtx, cancelDial := context.WithTimeout(r.Context(), dialTimeout)
	cancelOnEnd(dialCtx, cancelDial, ticket)
	// Dial sends no Origin (net/http never adds one) and no client headers.
	up, _, err := websocket.Dial(dialCtx, "ws://"+p.upstream.Host+route.Path(), &websocket.DialOptions{
		HTTPClient:      p.client,
		HTTPHeader:      http.Header{},
		Host:            UpstreamHost,
		CompressionMode: websocket.CompressionDisabled,
	})
	cancelDial()
	if err != nil {
		p.writeUpstreamFailure(w, ticket)
		return
	}
	up.SetReadLimit(maxUpstreamMessage)

	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		up.CloseNow()
		return
	}
	client.SetReadLimit(maxClientMessage)

	p.log.Debug("cdp websocket opened", "lease", leaseID)
	p.relay(r.Context(), client, up, ticket)
	p.log.Debug("cdp websocket closed", "lease", leaseID)
}

// closeReq says how to finish a relay. A zero code means the side is closed
// without a handshake (or already closed by the peer).
type closeReq struct {
	clientCode websocket.StatusCode
	reason     string
	upCode     websocket.StatusCode
}

func leaseCloseReq(t lease.Ticket) closeReq {
	if t.Reason() == lease.EndGeneration {
		return closeReq{clientCode: CloseGenerationChanged, reason: "browser generation changed"}
	}
	return closeReq{clientCode: CloseLeaseEnded, reason: "lease ended"}
}

// relay pumps frames until the first side fails, the lease ends or ctx is
// cancelled, then closes both connections.
func (p *Proxy) relay(parent context.Context, client, up *websocket.Conn, ticket lease.Ticket) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	events := make(chan closeReq, 3) // one send per goroutine; never blocks
	var wg sync.WaitGroup

	wg.Go(func() { events <- p.clientToUpstream(ctx, client, up) })
	wg.Go(func() { events <- upstreamToClient(ctx, client, up, ticket) })
	wg.Go(func() {
		select {
		case <-ticket.Done():
			events <- leaseCloseReq(ticket)
		case <-ctx.Done():
			events <- closeReq{}
		}
	})

	ev := <-events

	var closing sync.WaitGroup
	closing.Go(func() {
		if ev.clientCode != 0 {
			_ = client.Close(ev.clientCode, ev.reason)
		} else {
			_ = client.CloseNow()
		}
	})
	closing.Go(func() {
		if ev.upCode != 0 {
			_ = up.Close(ev.upCode, "")
		} else {
			_ = up.CloseNow()
		}
	})
	closing.Wait()
	cancel()
	wg.Wait()
}

func (p *Proxy) clientToUpstream(ctx context.Context, client, up *websocket.Conn) closeReq {
	for {
		typ, data, err := client.Read(ctx)
		if err != nil {
			switch websocket.CloseStatus(err) {
			case websocket.StatusNormalClosure, websocket.StatusGoingAway:
				return closeReq{upCode: websocket.StatusNormalClosure}
			}
			return closeReq{}
		}
		if typ != websocket.MessageText {
			return closeReq{clientCode: websocket.StatusUnsupportedData, reason: "binary frames are not supported"}
		}
		var out []byte
		switch v, req := p.policy.inspect(data); v {
		case verdictAllow:
			if err := up.Write(ctx, websocket.MessageText, data); err != nil {
				return closeReq{}
			}
			continue
		case verdictDeny:
			out = req.reply(-32000, deniedMessage)
		default:
			out = req.reply(-32700, malformedMessage)
		}
		if err := client.Write(ctx, websocket.MessageText, out); err != nil {
			return closeReq{}
		}
	}
}

func upstreamToClient(ctx context.Context, client, up *websocket.Conn, ticket lease.Ticket) closeReq {
	if srcErr := pump(ctx, up, client); srcErr != nil {
		return upstreamFailure(srcErr, ticket)
	}
	return closeReq{}
}

func upstreamFailure(err error, t lease.Ticket) closeReq {
	switch s := websocket.CloseStatus(err); s {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway:
		return closeReq{clientCode: s}
	}
	timer := time.NewTimer(leaseGrace)
	defer timer.Stop()
	select {
	case <-t.Done():
		return leaseCloseReq(t)
	case <-timer.C:
	}
	return closeReq{clientCode: websocket.StatusInternalError, reason: "upstream connection closed"}
}
