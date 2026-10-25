package cdpc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestDiscoverySendsBearerAndDecodesDocuments(t *testing.T) {
	var gotAuth, gotMethod, gotQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("/p/json/version", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"Browser":"Chrome/1","Protocol-Version":"1.3","webSocketDebuggerUrl":"ws://h/p/devtools/browser/b"}`))
	})
	mux.HandleFunc("/p/json/new", func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotQuery = r.Method, r.URL.RawQuery
		_, _ = w.Write([]byte(`{"id":"t1","type":"page","url":"chrome://sandbox","webSocketDebuggerUrl":"ws://h/p/devtools/page/t1"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	d := &Discovery{Base: srv.URL + "/p", Token: "tok-123"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	v, err := d.Version(ctx)
	if err != nil || v.Browser != "Chrome/1" || gotAuth != "Bearer tok-123" {
		t.Fatalf("Version = %+v, %v, auth %q", v, err, gotAuth)
	}
	tg, err := d.NewTarget(ctx, "chrome://sandbox")
	if err != nil || tg.ID != "t1" || gotMethod != http.MethodPut || gotQuery != "chrome://sandbox" {
		t.Fatalf("NewTarget = %+v, %v (method %q, query %q)", tg, err, gotMethod, gotQuery)
	}
}

func TestDiscoveryReturnsHTTPErrorWithBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"LEASE_ENDED"}`))
	}))
	t.Cleanup(srv.Close)
	d := &Discovery{Base: srv.URL}
	_, err := d.Version(context.Background())
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 409 || !strings.Contains(he.Body, "LEASE_ENDED") {
		t.Fatalf("error = %v, want *HTTPError 409 with body", err)
	}
}

func TestWebSocketURLRerootsHostButRejectsForeignPaths(t *testing.T) {
	d := &Discovery{Base: "http://127.0.0.1:8080/api/v1/workspaces/default/cdp"}
	got, err := d.WebSocketURL("ws://elsewhere:9/api/v1/workspaces/default/cdp/devtools/page/abc")
	if err != nil || got != "ws://127.0.0.1:8080/api/v1/workspaces/default/cdp/devtools/page/abc" {
		t.Fatalf("WebSocketURL = %q, %v", got, err)
	}
	if _, err := d.WebSocketURL("ws://127.0.0.1:9223/devtools/page/abc"); err == nil {
		t.Fatal("a WebSocket URL that bypasses the proxy prefix was accepted")
	}
}

func TestDialReportsRefusedUpgradeStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"code":"WRONG_LEASE_MODE"}`, http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	_, err := Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), DialOptions{})
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 403 || !strings.Contains(he.Body, "WRONG_LEASE_MODE") {
		t.Fatalf("error = %v, want *HTTPError 403", err)
	}
}

func TestDiscoveryDialSendsAuthorizationOnUpgrade(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("Authorization")
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}))
	t.Cleanup(srv.Close)
	d := &Discovery{Base: srv.URL + "/p", Token: "tok"}
	c, err := d.Dial(context.Background(), "ws://ignored/p/devtools/browser/b")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if a := <-got; a != "Bearer tok" {
		t.Fatalf("Authorization on upgrade = %q", a)
	}
}
