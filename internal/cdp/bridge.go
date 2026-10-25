package cdp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/auth"
)

// ErrLeaseLost is returned by Bridge.Serve when the lease ended: the renew
// loop hit a terminal error or the runtime closed a proxied connection with
// CloseLeaseEnded or CloseGenerationChanged. The cause is wrapped alongside.
var ErrLeaseLost = errors.New("cdp: lease lost")

const (
	bridgeRenewTimeout = 10 * time.Second
	maxErrorBody       = 64 << 10
)

// BridgeConfig configures NewBridge.
type BridgeConfig struct {
	// Upstream is the runtime CDP mount: {api}/api/v1/workspaces/{ws}/cdp.
	Upstream *url.URL
	// Token is the AGENT lease capability token injected into every upstream request.
	Token auth.Secret
	// Epoch and RuntimeGeneration bind requests to the grant loaded by the CLI.
	Epoch             uint64
	RuntimeGeneration string
	// Renew extends the lease and returns its new expiry.
	Renew func(context.Context) (time.Time, error)
	// RenewEvery is the base renew interval (TTL/3); each wait is jittered by ±10%.
	RenewEvery time.Duration
	// IsTerminal classifies a Renew error as the end of the lease (for
	// example apiclient.APIError.Terminal). Other errors are retried on the
	// next tick until the last known expiry passes.
	IsTerminal func(error) bool
	Logger     *slog.Logger
	// HTTPClient is used for discovery and the upstream WebSocket handshake.
	// Redirects are never followed. Nil selects a client with sane timeouts.
	HTTPClient *http.Client
	// PathSecret serves every route under a random 32-byte hex path prefix
	// (/<hex>/json/version, ...), compared in constant time. By default the
	// bridge serves at the root, so clients that take a bare port work
	// (agent-browser --cdp PORT); then any local process that can reach the
	// loopback port can use the bridge and thereby the lease. With the
	// prefix a local process must also know it, which is only printed to the
	// bridge's stdout and --url-file. The prefix is not a defence against
	// processes that can read those or this process's memory. Both modes
	// keep the loopback-only listener and the Host, Origin and route checks.
	PathSecret bool
}

// Bridge serves a loopback CDP endpoint for local clients (Playwright,
// Puppeteer) and forwards it to the runtime proxy with the lease token
// injected, so the token never reaches the client.
type Bridge struct {
	renew      func(context.Context) (time.Time, error)
	renewEvery time.Duration
	isTerminal func(error) bool
	token      auth.Secret
	epoch      string
	generation string
	upstream   *url.URL
	log        *slog.Logger
	client     *http.Client
	prefix     string // random hex with PathSecret, else empty (served at the root)

	mu      sync.Mutex
	clients map[*websocket.Conn]struct{}
	served  bool
	closing bool
	port    string

	lostStart chan struct{} // closed when the lease is lost
	lostDone  chan struct{} // closed once all clients were closed
	lostOnce  sync.Once
	lostErr   error
	relays    sync.WaitGroup
}

// NewBridge validates cfg and, with PathSecret, draws the random URL prefix.
func NewBridge(cfg BridgeConfig) (*Bridge, error) {
	u := cfg.Upstream
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, errors.New("cdp: bridge upstream must be an http(s) URL without credentials or query")
	}
	if cfg.Token == "" {
		return nil, errors.New("cdp: bridge requires a lease token")
	}
	if cfg.Renew == nil || cfg.IsTerminal == nil {
		return nil, errors.New("cdp: bridge requires Renew and IsTerminal")
	}
	if cfg.RenewEvery <= 0 {
		return nil, errors.New("cdp: bridge RenewEvery must be positive")
	}
	prefix := ""
	if cfg.PathSecret {
		var err error
		if prefix, err = auth.RandomHex(32); err != nil {
			return nil, fmt.Errorf("cdp: generate bridge prefix: %w", err)
		}
	}

	up := *u
	up.Path = strings.TrimRight(u.Path, "/")
	up.RawPath = ""

	client := cfg.HTTPClient
	if client == nil {
		client = newUpstreamClient()
	} else {
		c := *client
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client = &c
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Bridge{
		renew:      cfg.Renew,
		renewEvery: cfg.RenewEvery,
		isTerminal: cfg.IsTerminal,
		token:      cfg.Token,
		epoch:      strconv.FormatUint(cfg.Epoch, 10),
		generation: cfg.RuntimeGeneration,
		upstream:   &up,
		log:        log,
		client:     client,
		prefix:     prefix,
		clients:    map[*websocket.Conn]struct{}{},
		lostStart:  make(chan struct{}),
		lostDone:   make(chan struct{}),
	}, nil
}

// Endpoints is what a local CDP client should connect to.
type Endpoints struct{ DiscoveryURL, BrowserWebSocketURL string }

// Endpoints asks the runtime for the browser WebSocket path and returns the
// bridge URLs for the listener at addr, which must be loopback. It may be
// called before Serve.
func (b *Bridge) Endpoints(ctx context.Context, addr net.Addr) (Endpoints, error) {
	port, err := loopbackPort(addr)
	if err != nil {
		return Endpoints{}, err
	}
	base := "127.0.0.1:" + port + b.mount()
	status, body, apiErr, err := b.fetch(ctx, Route{Kind: RouteVersion}, "ws://"+base)
	if err != nil {
		return Endpoints{}, err
	}
	if apiErr != nil {
		return Endpoints{}, fmt.Errorf("cdp: runtime rejected discovery (HTTP %d %s)", status, apiErr.Code)
	}
	var doc struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.WebSocketDebuggerURL == "" {
		return Endpoints{}, errors.New("cdp: runtime discovery has no browser WebSocket URL")
	}
	return Endpoints{DiscoveryURL: "http://" + base, BrowserWebSocketURL: doc.WebSocketDebuggerURL}, nil
}

func loopbackPort(addr net.Addr) (string, error) {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok || tcp.IP == nil || !tcp.IP.IsLoopback() || tcp.Port == 0 {
		return "", errors.New("cdp: bridge listener must be a bound loopback TCP address")
	}
	return fmt.Sprint(tcp.Port), nil
}

// Serve accepts local CDP clients on ln, which must be loopback, and runs the
// renew loop. It returns ErrLeaseLost (wrapped) when the lease is lost, after
// closing every client with the matching close code, or ctx.Err() when ctx is
// cancelled. Serve may be called once.
func (b *Bridge) Serve(ctx context.Context, ln net.Listener) error {
	port, err := loopbackPort(ln.Addr())
	if err != nil {
		return err
	}
	b.mu.Lock()
	if b.served {
		b.mu.Unlock()
		return errors.New("cdp: bridge already served")
	}
	b.served = true
	b.port = port
	b.mu.Unlock()

	srv := &http.Server{
		Handler:           http.HandlerFunc(b.handle),
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(slog.DiscardHandler, slog.LevelError),
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	loopCtx, stopLoop := context.WithCancel(ctx)
	var loop sync.WaitGroup
	loop.Go(func() { b.renewLoop(loopCtx) })

	var result error
	select {
	case <-ctx.Done():
		b.markClosing()
		b.closeClients(websocket.StatusGoingAway, "bridge shutting down")
		result = ctx.Err()
	case <-b.lostDone:
		result = b.lostErr
	case err := <-serveErr:
		b.markClosing()
		b.closeClients(websocket.StatusInternalError, "bridge failed")
		result = fmt.Errorf("cdp: bridge listener failed: %w", err)
	}
	stopLoop()
	_ = srv.Close()
	loop.Wait()
	b.relays.Wait()
	return result
}

func (b *Bridge) renewLoop(ctx context.Context) {
	var expiry time.Time
	for {
		timer := time.NewTimer(jitter(b.renewEvery))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		rctx, cancel := context.WithTimeout(ctx, bridgeRenewTimeout)
		exp, err := b.renew(rctx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		switch {
		case err == nil:
			expiry = exp
		case b.isTerminal(err):
			b.lose(CloseLeaseEnded, fmt.Errorf("%w: renew failed: %w", ErrLeaseLost, err))
			return
		default:
			b.log.Warn("cdp bridge lease renew failed; will retry")
			if !expiry.IsZero() && time.Now().After(expiry) {
				b.lose(CloseLeaseEnded, fmt.Errorf("%w: lease expired while renew kept failing: %w", ErrLeaseLost, err))
				return
			}
		}
	}
}

// jitter returns d scaled by a uniform factor in [0.9, 1.1].
func jitter(d time.Duration) time.Duration {
	n, err := rand.Int(rand.Reader, big.NewInt(201))
	if err != nil {
		return d
	}
	return d * time.Duration(900+n.Int64()) / 1000
}

// lose records the first loss, rejects new clients, closes all current
// clients with code and then releases Serve.
func (b *Bridge) lose(code websocket.StatusCode, cause error) {
	b.lostOnce.Do(func() {
		b.lostErr = cause
		close(b.lostStart)
		b.closeClients(code, "lease lost")
		close(b.lostDone)
	})
}

func (b *Bridge) closeClients(code websocket.StatusCode, reason string) {
	b.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(b.clients))
	for c := range b.clients {
		conns = append(conns, c)
	}
	b.mu.Unlock()
	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Go(func() { _ = c.Close(code, reason) })
	}
	wg.Wait()
}

// admit registers a client connection and counts its relay, unless the
// bridge is closing or the lease is lost.
func (b *Bridge) admit(c *websocket.Conn) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	select {
	case <-b.lostStart:
		return false
	default:
	}
	if b.closing {
		return false
	}
	b.clients[c] = struct{}{}
	b.relays.Add(1)
	return true
}

func (b *Bridge) markClosing() {
	b.mu.Lock()
	b.closing = true
	b.mu.Unlock()
}

func (b *Bridge) unregister(c *websocket.Conn) {
	b.mu.Lock()
	delete(b.clients, c)
	b.mu.Unlock()
}

// handle admits a local client request: Host and Origin checks, the secret
// prefix check when PathSecret is set, then route allowlisting, then
// forwarding.
func (b *Bridge) handle(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	port := b.port
	b.mu.Unlock()
	if r.Host != "127.0.0.1:"+port && r.Host != "localhost:"+port {
		writeError(w, http.StatusForbidden, apiwire.CodeOriginNotAllowed, "host not allowed")
		return
	}
	if _, ok := r.Header["Origin"]; ok {
		writeError(w, http.StatusForbidden, apiwire.CodeOriginNotAllowed, "requests with an Origin header are not allowed")
		return
	}
	rest, ok := b.stripPrefix(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, apiwire.CodeNotFound, "not found")
		return
	}
	select {
	case <-b.lostStart:
		writeError(w, http.StatusConflict, apiwire.CodeLeaseEnded, "the lease has ended")
		return
	default:
	}
	route, err := ParseRoute(r.Method, rest, r.URL.RawQuery)
	if writeRouteError(w, err) {
		return
	}
	if route.IsWebSocket() {
		b.serveWebSocket(w, r, route)
		return
	}
	b.serveDiscovery(w, r, route)
}

// mount is the path prefix every request must carry: empty in the default
// mode, "/<secret>" with PathSecret.
func (b *Bridge) mount() string {
	if b.prefix == "" {
		return ""
	}
	return "/" + b.prefix
}

// stripPrefix returns the route path of a request path. Without a secret
// prefix that is the path itself; otherwise the path must start with
// "/<prefix>" (compared in constant time) followed by "/" or the end.
func (b *Bridge) stripPrefix(path string) (string, bool) {
	if b.prefix == "" {
		return path, true
	}
	trimmed, ok := strings.CutPrefix(path, "/")
	if !ok || len(trimmed) < len(b.prefix) {
		return "", false
	}
	if subtle.ConstantTimeCompare([]byte(trimmed[:len(b.prefix)]), []byte(b.prefix)) != 1 {
		return "", false
	}
	rest := trimmed[len(b.prefix):]
	if rest != "" && rest[0] != '/' {
		return "", false
	}
	return rest, true
}

func (b *Bridge) upstreamURL(route Route) string {
	u := *b.upstream
	u.Path += route.Path()
	u.RawQuery = route.RawQuery
	return u.String()
}

// fetch performs one discovery request against the runtime. On a non-200
// answer it returns the runtime's apiwire.Error (never raw upstream text).
func (b *Bridge) fetch(ctx context.Context, route Route, wsBase string) (int, []byte, *apiwire.Error, error) {
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, route.Method(), b.upstreamURL(route), nil)
	if err != nil {
		return 0, nil, nil, errors.New("cdp: build upstream request")
	}
	req.Header.Set("Authorization", "Bearer "+b.token.Reveal())
	if b.generation != "" {
		req.Header.Set("X-LensDock-Lease-Epoch", b.epoch)
		req.Header.Set("X-LensDock-Runtime-Generation", b.generation)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return 0, nil, nil, errors.New("cdp: runtime unreachable")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil, decodeAPIError(resp.Body), nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDiscoveryBody+1))
	if err != nil || len(body) > maxDiscoveryBody {
		return 0, nil, nil, errors.New("cdp: runtime response unreadable")
	}
	body, err = rewriteDiscovery(route.Kind, body, b.upstream.Path, func(p string) string { return wsBase + p })
	if err != nil {
		return 0, nil, nil, errors.New("cdp: runtime response not understood")
	}
	return http.StatusOK, body, nil, nil
}

// decodeAPIError returns the runtime's error body, or a generic upstream
// error when it is not a well-formed apiwire.Error.
func decodeAPIError(body io.Reader) *apiwire.Error {
	e, ok := apiwire.ReadError(body, maxErrorBody)
	if !ok {
		return &apiwire.Error{Code: apiwire.CodeUpstreamUnavailable, Message: "runtime request failed"}
	}
	return &e
}

func (b *Bridge) serveDiscovery(w http.ResponseWriter, r *http.Request, route Route) {
	b.mu.Lock()
	port := b.port
	b.mu.Unlock()
	status, body, apiErr, err := b.fetch(r.Context(), route, "ws://127.0.0.1:"+port+b.mount())
	switch {
	case err != nil:
		writeError(w, http.StatusBadGateway, apiwire.CodeUpstreamUnavailable, "runtime unavailable")
		return
	case apiErr != nil:
		writeError(w, relayStatus(status), apiErr.Code, apiErr.Message)
		return
	}
	writeDiscovery(w, route.Kind, body)
}

func relayStatus(s int) int {
	if s >= 400 && s < 600 {
		return s
	}
	return http.StatusBadGateway
}

func (b *Bridge) serveWebSocket(w http.ResponseWriter, r *http.Request, route Route) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		writeError(w, http.StatusBadRequest, apiwire.CodeInvalidRequest, "a WebSocket upgrade is required")
		return
	}
	dialURL := b.upstreamURL(route)
	header := http.Header{}
	header.Set("Authorization", "Bearer "+b.token.Reveal())
	if b.generation != "" {
		header.Set("X-LensDock-Lease-Epoch", b.epoch)
		header.Set("X-LensDock-Runtime-Generation", b.generation)
	}

	dialCtx, cancel := context.WithTimeout(r.Context(), dialTimeout)
	up, resp, err := websocket.Dial(dialCtx, dialURL, &websocket.DialOptions{
		HTTPClient:      b.client,
		HTTPHeader:      header,
		CompressionMode: websocket.CompressionDisabled,
	})
	cancel()
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			e := decodeAPIError(resp.Body)
			writeError(w, relayStatus(resp.StatusCode), e.Code, e.Message)
			return
		}
		writeError(w, http.StatusBadGateway, apiwire.CodeUpstreamUnavailable, "runtime unavailable")
		return
	}
	up.SetReadLimit(maxUpstreamMessage)

	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		_ = up.CloseNow()
		return
	}
	client.SetReadLimit(maxClientMessage)

	if !b.admit(client) {
		_ = client.Close(CloseLeaseEnded, "bridge closed")
		_ = up.CloseNow()
		return
	}
	defer b.relays.Done()
	defer b.unregister(client)
	b.relay(r.Context(), client, up)
}

// relay forwards messages transparently in both directions; policy is the
// runtime's job. A runtime close with CloseLeaseEnded or CloseGenerationChanged
// loses the lease for the whole bridge.
func (b *Bridge) relay(parent context.Context, client, up *websocket.Conn) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	type result struct {
		fromUpstream bool
		srcErr       error
	}
	events := make(chan result, 2)
	var wg sync.WaitGroup
	wg.Go(func() {
		events <- result{fromUpstream: false, srcErr: pump(ctx, client, up)}
	})
	wg.Go(func() {
		events <- result{fromUpstream: true, srcErr: pump(ctx, up, client)}
	})

	ev := <-events
	status := websocket.CloseStatus(ev.srcErr)
	switch {
	case ev.fromUpstream && (status == CloseLeaseEnded || status == CloseGenerationChanged):
		_ = up.CloseNow()
		b.lose(status, fmt.Errorf("%w: runtime closed the connection with code %d", ErrLeaseLost, status))
	case ev.fromUpstream && (status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway):
		_ = client.Close(status, "")
		_ = up.CloseNow()
	case ev.fromUpstream && ev.srcErr != nil:
		_ = client.Close(websocket.StatusInternalError, "runtime connection closed")
		_ = up.CloseNow()
	case !ev.fromUpstream && (status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway):
		_ = up.Close(websocket.StatusNormalClosure, "")
		_ = client.CloseNow()
	default:
		_ = client.CloseNow()
		_ = up.CloseNow()
	}
	cancel()
	wg.Wait()
}
