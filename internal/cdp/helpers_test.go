package cdp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/auth"
	"github.com/JiaWeiXie/lensdock/internal/lease"
)

const (
	browserID = "BROWSER-GUID-1"
	pageID    = "AB12CD34"
)

// handshake records what the fake Chromium received.
type handshake struct {
	Method   string
	Path     string
	RawQuery string
	Host     string
	Header   http.Header
}

// fakeChromium imitates Chromium's DevTools HTTP endpoint on an httptest
// server: discovery documents that reference 127.0.0.1:9223 and a WebSocket
// echo. A message calling Test.bye makes it close normally, Test.drop makes
// it drop the TCP connection.
type fakeChromium struct {
	srv *httptest.Server

	mu         sync.Mutex
	requests   []handshake
	received   []string
	wsFinished chan struct{}
}

func newFakeChromium(t *testing.T) *fakeChromium {
	t.Helper()
	f := &fakeChromium{wsFinished: make(chan struct{}, 64)}
	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		writeJSON(w, `{"Browser":"Chrome/200.0.1","Protocol-Version":"1.3","webSocketDebuggerUrl":"ws://127.0.0.1:9223/devtools/browser/`+browserID+`"}`)
	})
	list := func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		writeJSON(w, `[{"id":"`+pageID+`","type":"page","title":"A <b>&</b>","url":"https://example.com/","webSocketDebuggerUrl":"ws://127.0.0.1:9223/devtools/page/`+pageID+`","devtoolsFrontendUrl":"/devtools/inspector.html?ws=127.0.0.1:9223/devtools/page/`+pageID+`","devtoolsFrontendUrlCompat":"/devtools/inspector.html?experiments=true&v8only=true&ws=127.0.0.1:9223/devtools/page/`+pageID+`"}]`)
	}
	mux.HandleFunc("/json", list)
	mux.HandleFunc("/json/list", list)
	mux.HandleFunc("/json/protocol", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		writeJSON(w, `{"version":{"major":"1","minor":"3"},"domains":[]}`)
	})
	mux.HandleFunc("/json/new", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		if r.Method != http.MethodPut {
			http.Error(w, "UPSTREAM SECRET method", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, `{"id":"NEW1","type":"page","url":"`+r.URL.RawQuery+`","webSocketDebuggerUrl":"ws://127.0.0.1:9223/devtools/page/NEW1","devtoolsFrontendUrl":"/x"}`)
	})
	mux.HandleFunc("/json/activate/", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		if strings.HasSuffix(r.URL.Path, "/missing") {
			http.Error(w, "UPSTREAM SECRET no such target", http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, "Target activated")
	})
	mux.HandleFunc("/json/close/", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		if strings.HasSuffix(r.URL.Path, "/broken") {
			http.Error(w, "UPSTREAM SECRET internal", http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, "Target is closing")
	})
	mux.HandleFunc("/devtools/", f.serveWS)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_, _ = io.WriteString(w, body)
}

func (f *fakeChromium) record(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, handshake{
		Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery, Host: r.Host, Header: r.Header.Clone(),
	})
}

func (f *fakeChromium) lastRequest(t *testing.T) handshake {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("fake chromium saw no request")
	}
	return f.requests[len(f.requests)-1]
}

func (f *fakeChromium) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeChromium) receivedMessages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.received...)
}

func (f *fakeChromium) serveWS(w http.ResponseWriter, r *http.Request) {
	f.record(r)
	defer func() { f.wsFinished <- struct{}{} }()
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(-1)
	ctx := r.Context()
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.received = append(f.received, string(data))
		f.mu.Unlock()
		switch {
		case strings.Contains(string(data), "Test.bye"):
			_ = c.Close(websocket.StatusNormalClosure, "")
			return
		case strings.Contains(string(data), "Test.drop"):
			return
		}
		if err := c.Write(ctx, typ, data); err != nil {
			return
		}
	}
}

// waitWSFinished waits for n fake upstream WebSocket handlers to return.
func (f *fakeChromium) waitWSFinished(t *testing.T, n int) {
	t.Helper()
	for range n {
		select {
		case <-f.wsFinished:
		case <-time.After(3 * time.Second):
			t.Fatal("upstream WebSocket was not closed")
		}
	}
}

// runtimeEnv is a Proxy over a real lease.Manager in front of a fake Chromium.
type runtimeEnv struct {
	chromium *fakeChromium
	mgr      *lease.Manager
	proxy    *Proxy
	srv      *httptest.Server // serves the proxy under /cdp
}

func newRuntimeEnv(t *testing.T, maxConns int) *runtimeEnv {
	t.Helper()
	mgr, err := lease.NewManager(lease.Config{WorkspaceID: "default"})
	if err != nil {
		t.Fatal(err)
	}
	mgr.SetLocked(false, "")
	chromium := newFakeChromium(t)
	proxy, err := NewProxy(ProxyConfig{
		Upstream:         chromium.srv.URL,
		Auth:             mgr,
		MaxConnsPerLease: maxConns,
		PublicPrefix:     func(*http.Request) string { return "/cdp" },
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/cdp/", http.StripPrefix("/cdp", proxy))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Cleanup(mgr.Shutdown)
	return &runtimeEnv{chromium: chromium, mgr: mgr, proxy: proxy, srv: srv}
}

func (e *runtimeEnv) acquire(t *testing.T, mode lease.Mode, idem string) (lease.Lease, auth.Secret) {
	t.Helper()
	l, tok, err := e.mgr.Acquire("holder", mode, 30*time.Second, idem)
	if err != nil {
		t.Fatal(err)
	}
	return l, tok
}

func (e *runtimeEnv) host() string { return e.srv.Listener.Addr().String() }

func (e *runtimeEnv) wsURL(path string) string { return "ws://" + e.host() + "/cdp" + path }

func (e *runtimeEnv) do(t *testing.T, method, path, token string, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+"/cdp"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

func decodeError(t *testing.T, body []byte) apiwire.Error {
	t.Helper()
	var e apiwire.Error
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("response is not an apiwire.Error: %v: %q", err, body)
	}
	return e
}

func dialWS(t *testing.T, url, token string, hdr http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	h := http.Header{}
	for k, v := range hdr {
		h[k] = v
	}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: h})
	if c != nil {
		c.SetReadLimit(-1)
		t.Cleanup(func() { _ = c.CloseNow() })
	}
	return c, resp, err
}

func mustDial(t *testing.T, url, token string) *websocket.Conn {
	t.Helper()
	c, _, err := dialWS(t, url, token, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	return c
}

func readWithin(t *testing.T, c *websocket.Conn, d time.Duration) (websocket.MessageType, []byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return c.Read(ctx)
}

func writeText(t *testing.T, c *websocket.Conn, s string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, []byte(s)); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func expectEcho(t *testing.T, c *websocket.Conn, s string) {
	t.Helper()
	writeText(t, c, s)
	typ, data, err := readWithin(t, c, 3*time.Second)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if typ != websocket.MessageText || string(data) != s {
		t.Fatalf("echo = %v %q, want text %q", typ, data, s)
	}
}

func expectClose(t *testing.T, c *websocket.Conn, want websocket.StatusCode, within time.Duration) {
	t.Helper()
	_, _, err := readWithin(t, c, within)
	if got := websocket.CloseStatus(err); got != want {
		t.Fatalf("close status = %d (err %v), want %d", got, err, want)
	}
}
