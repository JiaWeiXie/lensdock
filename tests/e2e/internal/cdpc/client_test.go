package cdpc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeBrowser serves a CDP WebSocket at /devtools/page/p1. handle receives every
// request and writes frames through send; it runs until the client goes away.
func fakeBrowser(t *testing.T, handle func(ctx context.Context, conn *websocket.Conn, id int64, method string, params json.RawMessage)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var req struct {
				ID     int64           `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal(data, &req); err != nil {
				return
			}
			handle(ctx, conn, req.ID, req.Method, req.Params)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/devtools/page/p1"
}

func send(ctx context.Context, conn *websocket.Conn, v any) {
	b, _ := json.Marshal(v)
	_ = conn.Write(ctx, websocket.MessageText, b)
}

func dial(t *testing.T, url string) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, url, DialOptions{})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestCallMatchesOutOfOrderRepliesAndCDPErrors(t *testing.T) {
	var held []int64
	srv := fakeBrowser(t, func(ctx context.Context, conn *websocket.Conn, id int64, method string, _ json.RawMessage) {
		switch method {
		case "Slow.first": // answered only after the second call arrives
			held = append(held, id)
		case "Fast.second":
			send(ctx, conn, map[string]any{"id": id, "result": map[string]string{"v": "second"}})
			for _, h := range held {
				send(ctx, conn, map[string]any{"id": h, "result": map[string]string{"v": "first"}})
			}
		case "Denied.call":
			send(ctx, conn, map[string]any{"id": id, "error": map[string]any{"code": -32000, "message": "Method denied by LensDock policy"}})
		}
	})
	c := dial(t, wsURL(srv))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	firstDone := make(chan string, 1)
	go func() {
		var r struct{ V string }
		if err := c.Call(ctx, "Slow.first", nil, &r); err != nil {
			firstDone <- "error: " + err.Error()
			return
		}
		firstDone <- r.V
	}()
	time.Sleep(100 * time.Millisecond) // let the first request reach the server
	var second struct{ V string }
	if err := c.Call(ctx, "Fast.second", nil, &second); err != nil || second.V != "second" {
		t.Fatalf("second call = %+v, %v", second, err)
	}
	if got := <-firstDone; got != "first" {
		t.Fatalf("first call = %q, want first", got)
	}

	err := c.Call(ctx, "Denied.call", nil, nil)
	var cdpErr *Error
	if !errors.As(err, &cdpErr) || cdpErr.Code != -32000 || !strings.Contains(cdpErr.Message, "denied") {
		t.Fatalf("error = %v, want *Error -32000", err)
	}
}

func TestCloseStatusReportsServerCloseCodeAndFailsPendingCalls(t *testing.T) {
	srv := fakeBrowser(t, func(ctx context.Context, conn *websocket.Conn, _ int64, _ string, _ json.RawMessage) {
		_ = conn.Close(4001, "lease ended")
	})
	c := dial(t, wsURL(srv))
	if got := c.CloseStatus(); got != -1 {
		t.Fatalf("CloseStatus before close = %d, want -1", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Call(ctx, "Any.method", nil, nil); err == nil {
		t.Fatal("Call succeeded although the server closed the connection")
	}
	select {
	case <-c.Done():
	case <-ctx.Done():
		t.Fatal("Done was not closed")
	}
	if got := c.CloseStatus(); got != 4001 {
		t.Fatalf("CloseStatus = %d, want 4001", got)
	}
	if err := c.Call(ctx, "After.close", nil, nil); err == nil {
		t.Fatal("Call after close succeeded")
	}
}

func TestEvaluateDecodesValuesAndSurfacesExceptions(t *testing.T) {
	srv := fakeBrowser(t, func(ctx context.Context, conn *websocket.Conn, id int64, method string, params json.RawMessage) {
		var p struct {
			Expression    string `json:"expression"`
			ReturnByValue bool   `json:"returnByValue"`
			AwaitPromise  bool   `json:"awaitPromise"`
		}
		_ = json.Unmarshal(params, &p)
		if method != "Runtime.evaluate" || !p.ReturnByValue || !p.AwaitPromise {
			send(ctx, conn, map[string]any{"id": id, "error": map[string]any{"code": -32602, "message": "bad params"}})
			return
		}
		if p.Expression == "throw" {
			send(ctx, conn, map[string]any{"id": id, "result": map[string]any{
				"result":           map[string]any{"type": "object"},
				"exceptionDetails": map[string]any{"text": "Uncaught", "exception": map[string]any{"description": "Error: boom"}},
			}})
			return
		}
		send(ctx, conn, map[string]any{"id": id, "result": map[string]any{"result": map[string]any{"type": "string", "value": "繁體 😀"}}})
	})
	c := dial(t, wsURL(srv))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var s string
	if err := c.Evaluate(ctx, "ok", &s); err != nil || s != "繁體 😀" {
		t.Fatalf("Evaluate = %q, %v", s, err)
	}
	err := c.Evaluate(ctx, "throw", &s)
	if !errors.Is(err, ErrException) || !strings.Contains(err.Error(), "Error: boom") {
		t.Fatalf("exception error = %v", err)
	}
}

func TestNavigateWaitsForLoadEventAndReportsErrorText(t *testing.T) {
	srv := fakeBrowser(t, func(ctx context.Context, conn *websocket.Conn, id int64, method string, params json.RawMessage) {
		switch method {
		case "Page.navigate":
			var p struct{ URL string }
			_ = json.Unmarshal(params, &p)
			if p.URL == "http://bad.invalid/" {
				send(ctx, conn, map[string]any{"id": id, "result": map[string]any{"errorText": "net::ERR_NAME_NOT_RESOLVED"}})
				return
			}
			send(ctx, conn, map[string]any{"id": id, "result": map[string]any{"frameId": "f"}})
			time.AfterFunc(100*time.Millisecond, func() {
				send(context.Background(), conn, map[string]any{"method": "Page.loadEventFired", "params": map[string]any{"timestamp": 1}})
			})
		default:
			send(ctx, conn, map[string]any{"id": id, "result": map[string]any{}})
		}
	})
	c := dial(t, wsURL(srv))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := c.Navigate(ctx, "http://ok.invalid/"); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	if time.Since(start) < 90*time.Millisecond {
		t.Fatal("Navigate returned before the load event")
	}
	if err := c.Navigate(ctx, "http://bad.invalid/"); !errors.Is(err, ErrNavigation) {
		t.Fatalf("Navigate error = %v, want ErrNavigation", err)
	}
}

func TestDominantFontPicksHighestGlyphCount(t *testing.T) {
	fonts := []PlatformFont{{FamilyName: "A", GlyphCount: 2}, {FamilyName: "B", GlyphCount: 40}, {FamilyName: "C", GlyphCount: 3}}
	if f, ok := DominantFont(fonts); !ok || f.FamilyName != "B" {
		t.Fatalf("DominantFont = %+v, %v", f, ok)
	}
	if _, ok := DominantFont(nil); ok {
		t.Fatal("DominantFont of an empty list reported a font")
	}
}
