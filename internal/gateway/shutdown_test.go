package gateway

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/JiaWeiXie/lensdock/internal/auth"
	"github.com/JiaWeiXie/lensdock/internal/storage"
)

const shutdownWithin = 5 * time.Second

var shutdownHuman = Identity{ID: "human", Role: "human", WorkspaceID: "default"}

// fakeRuntime stands in for the runtime API: one VNC WebSocket route that
// stays open until its peer disconnects, and one ordinary route that answers
// only when the test lets it.
type fakeRuntime struct {
	server       *httptest.Server
	socketUp     chan struct{} // an upstream VNC socket was accepted
	socketGone   chan struct{} // an upstream VNC socket ended
	filesEntered chan struct{} // an ordinary request reached the runtime
	filesRelease chan struct{}
	releaseOnce  sync.Once
}

func (f *fakeRuntime) release() { f.releaseOnce.Do(func() { close(f.filesRelease) }) }

func newFakeRuntime(t *testing.T) *fakeRuntime {
	t.Helper()
	f := &fakeRuntime{
		socketUp:     make(chan struct{}, 4),
		socketGone:   make(chan struct{}, 4),
		filesEntered: make(chan struct{}, 4),
		filesRelease: make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/default/vnc/view", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer runtime-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		f.socketUp <- struct{}{}
		for {
			if _, _, err := c.Read(context.Background()); err != nil {
				break
			}
		}
		f.socketGone <- struct{}{}
	})
	mux.HandleFunc("/api/v1/workspaces/default/files", func(w http.ResponseWriter, r *http.Request) {
		f.filesEntered <- struct{}{}
		<-f.filesRelease
		_, _ = io.WriteString(w, "listed")
	})
	mux.HandleFunc("/api/v1/workspaces/default/agent", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"mode":"AGENT","paused":false,"revision":1,"updatedAt":"2026-01-01T00:00:00Z"}`)
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	t.Cleanup(f.release) // runs first: a blocked handler would stall server.Close
	return f
}

// shutdownEnv is a real Gateway serving a real listener, with feedback storage
// open, in front of fakeRuntime.
type shutdownEnv struct {
	g         *Gateway
	srv       *http.Server
	runtime   *fakeRuntime
	origin    string
	host      string
	storage   storage.Config
	served    chan error
	closeOnce sync.Once
	closeErr  error
	closer    func() error
}

// closeStorage is what main's deferred closeFeedback does after Shutdown.
func (e *shutdownEnv) closeStorage() error {
	e.closeOnce.Do(func() { e.closeErr = e.closer() })
	return e.closeErr
}

func newShutdownEnv(t *testing.T) *shutdownEnv {
	t.Helper()
	rt := newFakeRuntime(t)
	dir := t.TempDir()
	web := filepath.Join(dir, "web")
	if err := os.MkdirAll(web, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "index.html"), []byte("<title>test</title>"), 0o600); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(dir, "token")
	if err := auth.WritePrivateFile(tokenFile, []byte("runtime-token")); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data")
	for _, name := range []string{"db", "private", "artifacts"} {
		if err := os.MkdirAll(filepath.Join(data, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host := ln.Addr().String()
	origin := "http://" + host
	cfg := Config{
		Origin:     origin,
		DataDir:    data,
		WebDir:     web,
		Identities: []Identity{{ID: "human", Role: "human", WorkspaceID: "default", TokenSHA256: digest("human-token")}},
		Runtimes:   []RuntimeConfig{{WorkspaceID: "default", URL: rt.server.URL, OperatorTokenFile: tokenFile}},
	}
	g, err := New(cfg, Integrations{})
	if err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	closer, err := g.OpenFeedback(ctx)
	if err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	e := &shutdownEnv{
		g:       g,
		srv:     Server(cfg, g),
		runtime: rt,
		origin:  origin,
		host:    host,
		storage: storage.Config{DBPath: filepath.Join(data, "db", "lensdock.db"), PrivateDir: filepath.Join(data, "private"), ArtifactDir: filepath.Join(data, "artifacts")},
		served:  make(chan error, 1),
		closer:  closer,
	}
	go func() { e.served <- e.srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = e.srv.Close()
		_ = e.closeStorage()
		g.Close()
	})
	return e
}

func (e *shutdownEnv) header() http.Header {
	return http.Header{"Authorization": {"Bearer human-token"}, "Origin": {e.origin}}
}

func (e *shutdownEnv) get(t *testing.T, path string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.origin+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = e.header()
	return req
}

// readLine reads one line from an open response body, failing instead of
// hanging when nothing arrives.
func readLine(t *testing.T, br *bufio.Reader, body io.Closer) string {
	t.Helper()
	timer := time.AfterFunc(shutdownWithin, func() { _ = body.Close() })
	defer timer.Stop()
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("reading stream: %v", err)
	}
	return line
}

func awaitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(shutdownWithin):
		t.Fatalf("timed out waiting for %s", what)
	}
}

type reply struct {
	status int
	body   string
	err    error
}

// startOrdinaryRequest sends a request that the runtime holds until released:
// the kind of in-flight work Shutdown must let finish.
func (e *shutdownEnv) startOrdinaryRequest(t *testing.T, client *http.Client) <-chan reply {
	t.Helper()
	out := make(chan reply, 1)
	req := e.get(t, "/api/v1/workspaces/default/files")
	go func() {
		resp, err := client.Do(req)
		if err != nil {
			out <- reply{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		out <- reply{status: resp.StatusCode, body: string(b), err: err}
	}()
	awaitSignal(t, e.runtime.filesEntered, "the ordinary request to reach the runtime")
	return out
}

func TestShutdownEndsStreamsAndFinishesOrdinaryRequests(t *testing.T) {
	e := newShutdownEnv(t)
	ctx := context.Background()
	client := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(client.CloseIdleConnections)

	// A live UI subscriber: it must be connected and receiving from storage.
	sse, err := client.Do(e.get(t, "/api/v1/events?workspaceId=default"))
	if err != nil {
		t.Fatal(err)
	}
	defer sse.Body.Close()
	if sse.StatusCode != 200 || sse.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("events: %d %q", sse.StatusCode, sse.Header.Get("Content-Type"))
	}
	br := bufio.NewReader(sse.Body)
	if line := readLine(t, br, sse.Body); !strings.HasPrefix(line, ": connected") {
		t.Fatalf("first SSE line = %q", line)
	}
	e.g.record(ctx, shutdownHuman, "lease.acquired") // old data, committed before the stop
	for {
		if line := readLine(t, br, sse.Body); strings.HasPrefix(line, "data:") {
			break
		}
	}
	sseEnded := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, br)
		sseEnded <- err
	}()

	// A live VNC viewer proxied to the runtime.
	dialCtx, cancelDial := context.WithTimeout(ctx, shutdownWithin)
	defer cancelDial()
	viewer, _, err := websocket.Dial(dialCtx, "ws://"+e.host+"/api/v1/workspaces/default/vnc/view", &websocket.DialOptions{HTTPHeader: e.header()})
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.CloseNow()
	awaitSignal(t, e.runtime.socketUp, "the runtime to accept the VNC socket")

	// An ordinary request still being served by the runtime.
	ordinary := e.startOrdinaryRequest(t, client)

	shutdownCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.g.Shutdown(shutdownCtx, e.srv) }()

	// Both streams end while the ordinary request is still in flight.
	select {
	case err := <-sseEnded:
		if err != nil {
			t.Fatalf("SSE stream ended uncleanly: %v", err)
		}
	case <-time.After(shutdownWithin):
		t.Fatal("SSE stream still open after shutdown began")
	}
	readCtx, cancelRead := context.WithTimeout(ctx, shutdownWithin)
	defer cancelRead()
	_, _, err = viewer.Read(readCtx)
	if err == nil || readCtx.Err() != nil {
		t.Fatalf("VNC viewer was not closed: err=%v ctx=%v", err, readCtx.Err())
	}
	if code := websocket.CloseStatus(err); code == 4001 || code == 4002 {
		t.Fatalf("Gateway forged the runtime fence close code %d", code)
	}
	awaitSignal(t, e.runtime.socketGone, "the upstream VNC socket to close")

	// Shutdown is graceful: it still waits for the ordinary request, and no new
	// subscription or socket can start meanwhile.
	select {
	case err := <-done:
		t.Fatalf("Shutdown returned with a request in flight: %v", err)
	default:
	}
	events := httptest.NewRequest(http.MethodGet, "/api/v1/events?workspaceId=default", nil)
	events.Host, events.Header = e.host, e.header()
	rec := httptest.NewRecorder()
	e.g.ServeHTTP(rec, events)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("new subscription during shutdown = %d, want 503", rec.Code)
	}
	vnc := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/default/vnc/view", nil)
	vnc.Host, vnc.Header = e.host, e.header()
	vnc.Header.Set("Connection", "Upgrade")
	vnc.Header.Set("Upgrade", "websocket")
	rec = httptest.NewRecorder()
	e.g.ServeHTTP(rec, vnc)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("new socket during shutdown = %d, want 503", rec.Code)
	}

	e.runtime.release()
	select {
	case r := <-ordinary:
		if r.err != nil || r.status != 200 || r.body != "listed" {
			t.Fatalf("in-flight request = %+v, want 200 \"listed\"", r)
		}
	case <-time.After(shutdownWithin):
		t.Fatal("in-flight request never finished")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown = %v, want clean exit", err)
		}
	case <-time.After(shutdownWithin):
		t.Fatal("Shutdown did not return after the last request finished")
	}
	if n := e.g.streams.open(); n != 0 {
		t.Fatalf("%d stream handler(s) outlived Shutdown, so storage could close under them", n)
	}
	if err := <-e.served; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve = %v, want http.ErrServerClosed", err)
	}

	// Storage closes cleanly afterwards and the old data is intact.
	if err := e.closeStorage(); err != nil {
		t.Fatalf("closing storage: %v", err)
	}
	st, err := storage.Open(ctx, e.storage)
	if err != nil {
		t.Fatalf("reopen storage: %v", err)
	}
	defer st.Close()
	if n, err := st.Events.MaxSeq(ctx); err != nil || n < 1 {
		t.Fatalf("event committed before shutdown: MaxSeq=%d err=%v", n, err)
	}
}

func TestShutdownReportsOrdinaryRequestThatOutlivesDeadline(t *testing.T) {
	e := newShutdownEnv(t)
	client := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(client.CloseIdleConnections)
	ordinary := e.startOrdinaryRequest(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := e.g.Shutdown(ctx, e.srv); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v, want context.DeadlineExceeded for the request still in flight", err)
	}

	e.runtime.release()
	select {
	case r := <-ordinary:
		if r.err != nil || r.status != 200 {
			t.Fatalf("in-flight request = %+v", r)
		}
	case <-time.After(shutdownWithin):
		t.Fatal("in-flight request never finished")
	}
}

func TestStreamGroupDrainCancelsRefusesAndWaits(t *testing.T) {
	var s streamGroup
	parent := context.Background()
	first, releaseFirst, ok := s.begin(parent)
	if !ok {
		t.Fatal("stream refused before draining")
	}
	second, releaseSecond, ok := s.begin(parent)
	if !ok {
		t.Fatal("second stream refused before draining")
	}
	if first.Err() != nil || second.Err() != nil {
		t.Fatal("streams cancelled before drain")
	}

	idle := s.drain()
	for _, c := range []context.Context{first, second} {
		if !errors.Is(context.Cause(c), errDraining) {
			t.Fatalf("stream cause = %v, want errDraining", context.Cause(c))
		}
	}
	if _, _, ok := s.begin(parent); ok {
		t.Fatal("stream admitted while draining")
	}
	select {
	case <-idle:
		t.Fatal("idle before the handlers returned")
	default:
	}

	expired, cancel := context.WithTimeout(parent, 20*time.Millisecond)
	defer cancel()
	err := s.wait(expired)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "2 stream(s)") {
		t.Fatalf("wait with open streams = %v, want a deadline error naming 2 streams", err)
	}

	releaseFirst()
	releaseFirst() // release is idempotent
	releaseSecond()
	select {
	case <-idle:
	default:
		t.Fatal("idle not closed after the last release")
	}
	if err := s.wait(expired); err != nil {
		t.Fatalf("wait after the last release = %v, want nil even with an expired context", err)
	}

	var empty streamGroup
	select {
	case <-empty.drain():
	default:
		t.Fatal("draining a group without streams must be idle at once")
	}
}
