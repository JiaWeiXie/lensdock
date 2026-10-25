package vnc

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeVNC is a TCP server standing in for x11vnc: it records every byte it
// receives and echoes it back prefixed with "echo:".
type fakeVNC struct {
	ln       net.Listener
	mu       sync.Mutex
	received []byte
	conns    []net.Conn
	closedBy chan struct{} // closed when the first connection sees EOF
	once     sync.Once
}

func newFakeVNC(t *testing.T) *fakeVNC {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeVNC{ln: ln, closedBy: make(chan struct{})}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.conns = append(f.conns, c)
			f.mu.Unlock()
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeVNC) serve(c net.Conn) {
	defer c.Close()
	buf := make([]byte, 1024)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			f.mu.Lock()
			f.received = append(f.received, buf[:n]...)
			f.mu.Unlock()
			_, _ = c.Write(append([]byte("echo:"), buf[:n]...))
		}
		if err != nil {
			f.once.Do(func() { close(f.closedBy) })
			return
		}
	}
}

func (f *fakeVNC) got() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.received)
}

func (f *fakeVNC) addr() string { return f.ln.Addr().String() }

type proxyEnv struct {
	srv     *httptest.Server
	handled chan struct{} // receives one value per finished Serve
	bind    atomic.Pointer[Binding]
}

func newProxyEnv(t *testing.T, p *Proxy, b Binding) *proxyEnv {
	t.Helper()
	e := &proxyEnv{handled: make(chan struct{}, 16)}
	e.bind.Store(&b)
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { e.handled <- struct{}{} }()
		p.Serve(w, r, *e.bind.Load())
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *proxyEnv) dial(t *testing.T) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.srv.URL, "http"), &websocket.DialOptions{
		Subprotocols:    []string{"binary"},
		CompressionMode: websocket.CompressionDisabled,
	})
}

func (e *proxyEnv) waitHandled(t *testing.T) {
	t.Helper()
	select {
	case <-e.handled:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
}

func readBinary(t *testing.T, c *websocket.Conn) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != websocket.MessageBinary {
		t.Fatalf("message type = %v, want binary", typ)
	}
	return string(data)
}

func TestProxyRelaysBinaryBothWaysAndNegotiatesBinarySubprotocol(t *testing.T) {
	up := newFakeVNC(t)
	p := NewProxy(ProxyConfig{Instance: View, Addr: up.addr()})
	e := newProxyEnv(t, p, Binding{Key: "view"})

	c, _, err := e.dial(t)
	if err != nil {
		t.Fatal(err)
	}
	if c.Subprotocol() != "binary" {
		t.Fatalf("subprotocol = %q, want binary", c.Subprotocol())
	}
	ctx := context.Background()
	if err := c.Write(ctx, websocket.MessageBinary, []byte("RFB 003.008\n")); err != nil {
		t.Fatal(err)
	}
	if got := readBinary(t, c); got != "echo:RFB 003.008\n" {
		t.Fatalf("reply = %q", got)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
	e.waitHandled(t)
	select {
	case <-up.closedBy:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream connection stayed open after the client closed")
	}
}

func TestProxyClosesBothSidesWhenLeaseEndsAndForwardsNothingAfter(t *testing.T) {
	up := newFakeVNC(t)
	done := make(chan struct{})
	p := NewProxy(ProxyConfig{Instance: Control, Addr: up.addr()})
	e := newProxyEnv(t, p, Binding{Key: "lse_1", Done: done})

	c, _, err := e.dial(t)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Write(ctx, websocket.MessageBinary, []byte("before")); err != nil {
		t.Fatal(err)
	}
	if got := readBinary(t, c); got != "echo:before" {
		t.Fatalf("reply = %q", got)
	}

	close(done)
	e.waitHandled(t) // Serve returns only after both directions stopped

	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, _, err = c.Read(rctx)
	if got := websocket.CloseStatus(err); got != CloseLeaseEnded {
		t.Fatalf("close status = %d (%v), want %d", got, err, CloseLeaseEnded)
	}
	select {
	case <-up.closedBy:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream connection stayed open after the lease ended")
	}
	_ = c.Write(ctx, websocket.MessageBinary, []byte("after")) // may fail; must never arrive
	time.Sleep(50 * time.Millisecond)
	if got := up.got(); got != "before" {
		t.Fatalf("upstream received %q, want only the pre-revocation bytes", got)
	}
}

func TestProxyReportsGenerationChange(t *testing.T) {
	up := newFakeVNC(t)
	done := make(chan struct{})
	p := NewProxy(ProxyConfig{Instance: Control, Addr: up.addr()})
	e := newProxyEnv(t, p, Binding{Key: "k", Done: done, Reason: func() (websocket.StatusCode, string) {
		return CloseGenerationChanged, "browser generation changed"
	}})
	c, _, err := e.dial(t)
	if err != nil {
		t.Fatal(err)
	}
	close(done)
	rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err = c.Read(rctx)
	if got := websocket.CloseStatus(err); got != CloseGenerationChanged {
		t.Fatalf("close status = %d, want %d", got, CloseGenerationChanged)
	}
	e.waitHandled(t)
}

func TestProxyRejectsTextFrames(t *testing.T) {
	up := newFakeVNC(t)
	p := NewProxy(ProxyConfig{Instance: View, Addr: up.addr()})
	e := newProxyEnv(t, p, Binding{Key: "view"})
	c, _, err := e.dial(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(context.Background(), websocket.MessageText, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err = c.Read(rctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusUnsupportedData {
		t.Fatalf("close status = %d, want %d", got, websocket.StatusUnsupportedData)
	}
	e.waitHandled(t)
	if got := up.got(); got != "" {
		t.Fatalf("upstream received %q from a text frame", got)
	}
}

func TestProxyRefusesBeforeUpgrade(t *testing.T) {
	up := newFakeVNC(t)

	t.Run("plain GET", func(t *testing.T) {
		p := NewProxy(ProxyConfig{Instance: View, Addr: up.addr()})
		e := newProxyEnv(t, p, Binding{Key: "view"})
		resp, err := http.Get(e.srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("lease already ended", func(t *testing.T) {
		done := make(chan struct{})
		close(done)
		p := NewProxy(ProxyConfig{Instance: Control, Addr: up.addr()})
		e := newProxyEnv(t, p, Binding{Key: "k", Done: done})
		_, resp, err := e.dial(t)
		if err == nil || resp == nil || resp.StatusCode != http.StatusConflict {
			t.Fatalf("dial = %v resp=%v, want 409", err, resp)
		}
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), "LEASE_ENDED") {
			t.Fatalf("body = %q", body)
		}
	})

	t.Run("upstream down", func(t *testing.T) {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		addr := ln.Addr().String()
		_ = ln.Close()
		p := NewProxy(ProxyConfig{Instance: View, Addr: addr})
		e := newProxyEnv(t, p, Binding{Key: "view"})
		_, resp, err := e.dial(t)
		if err == nil || resp == nil || resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("dial = %v resp=%v, want 502", err, resp)
		}
	})

	t.Run("per key limit", func(t *testing.T) {
		p := NewProxy(ProxyConfig{Instance: Control, Addr: up.addr(), MaxConns: 1})
		e := newProxyEnv(t, p, Binding{Key: "lse_1", Done: make(chan struct{})})
		c, _, err := e.dial(t)
		if err != nil {
			t.Fatal(err)
		}
		defer c.CloseNow()
		_, resp, err := e.dial(t)
		if err == nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("second dial = %v resp=%v, want 429", err, resp)
		}
		e.bind.Store(&Binding{Key: "lse_2", Done: make(chan struct{})})
		c2, _, err := e.dial(t)
		if err != nil {
			t.Fatalf("another key must not share the limit: %v", err)
		}
		_ = c2.CloseNow()
	})
}
