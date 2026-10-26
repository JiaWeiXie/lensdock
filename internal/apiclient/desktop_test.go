package apiclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/auth"
)

func desktopServer(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDialDesktopBindsTheSocketToTheLeaseEpochAndGeneration(t *testing.T) {
	var gotAuth, gotPath, gotEpoch, gotGen string
	c := desktopServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		gotEpoch, gotGen = r.URL.Query().Get("epoch"), r.URL.Query().Get("generation")
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"binary"}})
		if err != nil {
			return
		}
		defer ws.CloseNow()
		_, data, err := ws.Read(r.Context())
		if err == nil {
			_ = ws.Write(r.Context(), websocket.MessageBinary, append([]byte("echo:"), data...))
		}
		_ = ws.Close(websocket.StatusNormalClosure, "")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := c.DialDesktop(ctx, "default", auth.Secret("ldl_tok"), 9, "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	if err != nil || string(buf[:n]) != "echo:hi" {
		t.Fatalf("read = %q, %v", buf[:n], err)
	}
	if gotAuth != "Bearer ldl_tok" || gotPath != "/api/v1/workspaces/default/vnc/control" || gotEpoch != "9" || gotGen != "gen-1" {
		t.Errorf("request auth %q path %q epoch %q generation %q", gotAuth, gotPath, gotEpoch, gotGen)
	}
}

func TestDialDesktopReportsAnEndedLeaseAsTerminal(t *testing.T) {
	c := desktopServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(apiwire.Error{Code: apiwire.CodeLeaseEnded, Message: "ended"})
	})
	_, err := c.DialDesktop(context.Background(), "default", auth.Secret("ldl_tok"), 1, "g")
	apiErr, ok := err.(*APIError)
	if !ok || !apiErr.Terminal() || apiErr.Code != apiwire.CodeLeaseEnded {
		t.Fatalf("err = %v, want a terminal LEASE_ENDED APIError", err)
	}
}

func TestDialDesktopWaitsOutTheRuntimeCountingThePreviousSocket(t *testing.T) {
	var attempts atomic.Int32
	c := desktopServer(t, func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) <= 2 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(apiwire.Error{Code: apiwire.CodeTooManyConnections, Message: "busy"})
			return
		}
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"binary"}})
		if err != nil {
			return
		}
		_, _, _ = ws.Read(r.Context())
		_ = ws.CloseNow()
	})
	conn, err := c.DialDesktop(context.Background(), "default", auth.Secret("ldl_tok"), 1, "g")
	if err != nil {
		t.Fatalf("dial after two busy answers: %v", err)
	}
	_ = conn.Close()
	if attempts.Load() != 3 {
		t.Errorf("attempts = %d, want 3", attempts.Load())
	}
}

func TestDesktopLeaseEndedRecognizesTheRuntimeCloseCodes(t *testing.T) {
	c := desktopServer(t, func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"binary"}})
		if err != nil {
			return
		}
		_ = ws.Close(4002, "browser generation changed")
	})
	conn, err := c.DialDesktop(context.Background(), "default", auth.Secret("ldl_tok"), 1, "g")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = io.ReadAll(conn)
	if !DesktopLeaseEnded(err) {
		t.Fatalf("read error %v is not recognized as a lease end", err)
	}
	if DesktopLeaseEnded(io.EOF) {
		t.Error("a plain EOF was reported as a lease end")
	}
}
