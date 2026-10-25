package vnc

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
)

// Close codes sent to proxied clients when the lease behind a control socket
// ends. They match the CDP proxy's codes.
const (
	// CloseLeaseEnded: released, expired, fenced by a takeover, capture or
	// maintenance, or the runtime shut down.
	CloseLeaseEnded websocket.StatusCode = 4001
	// CloseGenerationChanged: the browser generation changed.
	CloseGenerationChanged websocket.StatusCode = 4002
)

const (
	defaultMaxConns   = 16
	maxClientMessage  = 4 << 20
	tcpReadChunk      = 32 << 10
	upstreamDialLimit = 5 * time.Second
	// closeHandshake is how long the proxy waits for a peer to acknowledge the
	// close frame. The frame is written first, so the peer always learns the
	// code; a peer that never reads (or is gone) is force-closed after this.
	closeHandshake = 250 * time.Millisecond
)

// Binding ties one proxied connection to the lease that authorizes it. A nil
// Done (the view-only path) means the connection is not lease-bound.
type Binding struct {
	// Key groups connections for the per-key limit: the lease ID on the
	// control path, a fixed string on the view path.
	Key string
	// Done is closed when the authorizing lease ends.
	Done <-chan struct{}
	// Reason is called after Done closed and returns the WebSocket close
	// code and text to send; nil selects CloseLeaseEnded.
	Reason func() (websocket.StatusCode, string)
}

// ProxyConfig configures NewProxy.
type ProxyConfig struct {
	// Instance is the x11vnc server this proxy fronts.
	Instance Instance
	// Addr overrides the upstream address (default 127.0.0.1:<port>).
	Addr string
	// MaxConns caps concurrent sockets per Binding.Key (default 16).
	MaxConns int
	Logger   *slog.Logger
}

// Proxy relays RFB between a WebSocket (noVNC's transport) and one x11vnc
// instance. The proxy itself never interprets RFB: server-side enforcement
// of the viewer/controller split is the x11vnc `-viewonly` instance behind
// the view proxy, and the lease Binding behind the control proxy.
type Proxy struct {
	inst     Instance
	addr     string
	maxConns int
	log      *slog.Logger

	mu    sync.Mutex
	conns map[string]int
}

// NewProxy returns a Proxy for cfg.Instance.
func NewProxy(cfg ProxyConfig) *Proxy {
	p := &Proxy{
		inst:     cfg.Instance,
		addr:     cfg.Addr,
		maxConns: cfg.MaxConns,
		log:      cfg.Logger,
		conns:    map[string]int{},
	}
	if p.addr == "" {
		p.addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Instance.Port))
	}
	if p.maxConns <= 0 {
		p.maxConns = defaultMaxConns
	}
	if p.log == nil {
		p.log = slog.New(slog.DiscardHandler)
	}
	return p
}

func (p *Proxy) acquire(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conns[key] >= p.maxConns {
		return false
	}
	p.conns[key]++
	return true
}

func (p *Proxy) release(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conns[key]--; p.conns[key] <= 0 {
		delete(p.conns, key)
	}
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	body, err := json.Marshal(apiwire.Error{Code: code, Message: msg, RequestID: w.Header().Get("X-Request-Id")})
	if err != nil {
		body = []byte(`{"code":"INTERNAL","message":"internal error","requestId":""}`)
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

func ended(done <-chan struct{}) bool {
	if done == nil {
		return false
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// Serve upgrades the request to a WebSocket and relays it to x11vnc until the
// binding's lease ends, either side closes or the request context ends. The
// caller authenticated the request. Serve returns only after both relay
// directions have stopped and both connections are closed, so a caller that
// registered the connection with the lease manager can release it here and
// trust that nothing of this connection can still write.
func (p *Proxy) Serve(w http.ResponseWriter, r *http.Request, b Binding) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		writeErr(w, http.StatusBadRequest, apiwire.CodeInvalidRequest, "a WebSocket upgrade is required")
		return
	}
	if ended(b.Done) {
		writeErr(w, http.StatusConflict, apiwire.CodeLeaseEnded, "the lease has ended")
		return
	}
	if !p.acquire(b.Key) {
		writeErr(w, http.StatusTooManyRequests, apiwire.CodeTooManyConnections, "too many connections")
		return
	}
	defer p.release(b.Key)

	dialCtx, cancelDial := context.WithTimeout(r.Context(), upstreamDialLimit)
	if b.Done != nil {
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			select {
			case <-b.Done:
				cancelDial()
			case <-stop:
			}
		}()
	}
	var d net.Dialer
	up, err := d.DialContext(dialCtx, "tcp", p.addr)
	cancelDial()
	if err != nil {
		if ended(b.Done) {
			writeErr(w, http.StatusConflict, apiwire.CodeLeaseEnded, "the lease has ended")
			return
		}
		writeErr(w, http.StatusBadGateway, apiwire.CodeUpstreamUnavailable, "VNC server unavailable")
		return
	}
	if tc, ok := up.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}

	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:    []string{"binary"},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		_ = up.Close()
		return
	}
	client.SetReadLimit(maxClientMessage)

	p.log.Debug("vnc websocket opened", "instance", p.inst.Name)
	p.relay(r.Context(), client, up, b)
	p.log.Debug("vnc websocket closed", "instance", p.inst.Name)
}

type closeReq struct {
	code   websocket.StatusCode // 0: no close handshake
	reason string
}

func (p *Proxy) relay(parent context.Context, client *websocket.Conn, up net.Conn, b Binding) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	events := make(chan closeReq, 3)
	var wg sync.WaitGroup

	// Client -> x11vnc. The lease is re-checked before every write.
	wg.Go(func() {
		for {
			typ, data, err := client.Read(ctx)
			if err != nil {
				switch websocket.CloseStatus(err) {
				case websocket.StatusNormalClosure, websocket.StatusGoingAway:
					events <- closeReq{code: websocket.StatusNormalClosure}
				default:
					events <- closeReq{}
				}
				return
			}
			if typ != websocket.MessageBinary {
				events <- closeReq{code: websocket.StatusUnsupportedData, reason: "binary frames only"}
				return
			}
			if ended(b.Done) {
				events <- p.leaseClose(b)
				return
			}
			if _, err := up.Write(data); err != nil {
				events <- closeReq{code: websocket.StatusBadGateway, reason: "VNC server closed the connection"}
				return
			}
		}
	})

	// x11vnc -> client.
	wg.Go(func() {
		buf := make([]byte, tcpReadChunk)
		for {
			n, err := up.Read(buf)
			if n > 0 {
				if werr := client.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					events <- closeReq{}
					return
				}
			}
			if err != nil {
				events <- closeReq{code: websocket.StatusBadGateway, reason: "VNC server closed the connection"}
				return
			}
		}
	})

	// Lease end.
	wg.Go(func() {
		select {
		case <-b.Done: // a nil Done blocks until ctx ends
			events <- p.leaseClose(b)
		case <-ctx.Done():
			events <- closeReq{}
		}
	})

	ev := <-events

	// The upstream connection goes first: from here on nothing can reach
	// x11vnc, whichever side ended the session.
	_ = up.Close()
	// A close frame must be written before the relay context is canceled:
	// coder/websocket closes the underlying TCP connection as soon as a read or
	// write context ends, which would leave the peer with a bare EOF instead of
	// the lease's close code. The cancel also acts as the unblocker when the
	// peer does not answer the handshake in time.
	//
	// CloseNow is deliberately NOT used while a handshake is in flight: the
	// first Close sets the conn's closing flag, and any later Close/CloseNow
	// only waits (up to 15s, coder/websocket waitGoroutines) for it to finish,
	// which would stall the takeover/capture drain that waits for this socket.
	if ev.code != 0 {
		done := make(chan struct{})
		go func() {
			_ = client.Close(ev.code, ev.reason)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(closeHandshake):
			// No handshake answer: canceling closes the connection through the
			// library's read watcher, which also lets the pending Close return.
			cancel()
			select {
			case <-done:
			case <-time.After(closeHandshake):
			}
		}
	} else {
		// The peer closed the connection itself or the request context ended;
		// no close frame is owed.
		_ = client.CloseNow()
	}
	cancel()
	wg.Wait()
}

func (p *Proxy) leaseClose(b Binding) closeReq {
	if b.Reason != nil {
		code, reason := b.Reason()
		return closeReq{code: code, reason: reason}
	}
	return closeReq{code: CloseLeaseEnded, reason: "lease ended"}
}
