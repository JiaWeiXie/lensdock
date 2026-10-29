package capture

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeWindow is a page target and the browser window that holds it.
type fakeWindow struct {
	target   string
	windowID int
	state    string
	// stuck keeps the window minimized when it is asked to normalize.
	stuck bool
}

// fakeRestoreBrowser serves the DevTools HTTP and browser-level WebSocket
// surface RestoreWindow uses.
type fakeRestoreBrowser struct {
	srv *httptest.Server

	mu      sync.Mutex
	windows []*fakeWindow
	// versionFailures is how many /json/version requests fail before it answers.
	versionFailures int
	windowError     string
	calls           []string
	created         []map[string]any
}

func newFakeRestoreBrowser(t *testing.T, windows ...*fakeWindow) *fakeRestoreBrowser {
	t.Helper()
	f := &fakeRestoreBrowser{windows: windows}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRestoreBrowser) endpoint() string { return strings.TrimPrefix(f.srv.URL, "http://") }

func (f *fakeRestoreBrowser) called(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == method {
			n++
		}
	}
	return n
}

func (f *fakeRestoreBrowser) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/json/version":
		f.mu.Lock()
		fail := f.versionFailures > 0
		if fail {
			f.versionFailures--
		}
		f.mu.Unlock()
		if fail {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"Browser":              "Chrome/139.0.0.0",
			"webSocketDebuggerUrl": "ws://" + f.endpoint() + "/devtools/browser/abc",
		})
	case "/json/list":
		f.mu.Lock()
		out := make([]map[string]string, 0, len(f.windows))
		for _, win := range f.windows {
			out = append(out, map[string]string{"id": win.target, "type": "page", "url": "https://example.com/"})
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(out)
	case "/devtools/browser/abc":
		f.serveWS(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeRestoreBrowser) serveWS(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer c.CloseNow()
	for {
		_, data, err := c.Read(r.Context())
		if err != nil {
			return
		}
		var req struct {
			ID     int64          `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(data, &req) != nil {
			return
		}
		result, rpcErr := f.handle(req.Method, req.Params)
		resp := map[string]any{"id": req.ID}
		if rpcErr != "" {
			resp["error"] = map[string]any{"code": -32000, "message": rpcErr}
		} else {
			resp["result"] = result
		}
		b, _ := json.Marshal(resp)
		if err := c.Write(r.Context(), websocket.MessageText, b); err != nil {
			return
		}
	}
}

func (f *fakeRestoreBrowser) window(target string) *fakeWindow {
	for _, w := range f.windows {
		if w.target == target {
			return w
		}
	}
	return nil
}

func (f *fakeRestoreBrowser) handle(method string, params map[string]any) (any, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, method)
	switch method {
	case "Browser.getWindowForTarget":
		if f.windowError != "" {
			return nil, f.windowError
		}
		target, _ := params["targetId"].(string)
		w := f.window(target)
		if w == nil {
			return nil, "No window for target"
		}
		return map[string]any{"windowId": w.windowID, "bounds": map[string]any{"windowState": w.state}}, ""
	case "Browser.setWindowBounds":
		id, _ := params["windowId"].(float64)
		bounds, _ := params["bounds"].(map[string]any)
		for _, w := range f.windows {
			if w.windowID == int(id) && !w.stuck {
				w.state, _ = bounds["windowState"].(string)
			}
		}
		return map[string]any{}, ""
	case "Target.activateTarget":
		return map[string]any{}, ""
	case "Target.createTarget":
		f.created = append(f.created, params)
		f.windows = append(f.windows, &fakeWindow{target: "NEWTARGET1", windowID: 1, state: "normal"})
		return map[string]any{"targetId": "NEWTARGET1"}, ""
	}
	return nil, "unknown method " + method
}

func newRestoreCapturer(t *testing.T, f *fakeRestoreBrowser) *Capturer {
	t.Helper()
	c, err := New(Config{Endpoint: f.endpoint(), CallTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRestoreWindowUnminimizesExistingWindowWithoutNewTab(t *testing.T) {
	// Two tabs share window 7; window 9 is already visible.
	f := newFakeRestoreBrowser(t,
		&fakeWindow{target: "TAB1", windowID: 7, state: "minimized"},
		&fakeWindow{target: "TAB2", windowID: 7, state: "minimized"},
		&fakeWindow{target: "TAB3", windowID: 9, state: "normal"},
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := newRestoreCapturer(t, f).RestoreWindow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != RestoredExisting {
		t.Fatalf("outcome = %q, want %q", got, RestoredExisting)
	}
	if n := f.called("Browser.setWindowBounds"); n != 1 {
		t.Errorf("setWindowBounds calls = %d, want 1 (one minimized window, shared by two tabs)", n)
	}
	if f.called("Target.createTarget") != 0 {
		t.Error("an existing window must be restored, not replaced by a new target")
	}
	if f.called("Target.activateTarget") != 1 {
		t.Error("the restored window was not activated")
	}
	if f.windows[0].state != "normal" {
		t.Errorf("window state = %q, want normal", f.windows[0].state)
	}
}

func TestRestoreWindowLeavesVisibleWindowAlone(t *testing.T) {
	f := newFakeRestoreBrowser(t, &fakeWindow{target: "TAB1", windowID: 7, state: "maximized"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := newRestoreCapturer(t, f).RestoreWindow(ctx); err != nil {
		t.Fatal(err)
	}
	if f.called("Browser.setWindowBounds") != 0 {
		t.Error("a visible window's state must not be changed")
	}
	if f.windows[0].state != "maximized" {
		t.Errorf("window state = %q, want it kept maximized", f.windows[0].state)
	}
}

func TestRestoreWindowOpensBlankWindowWhenNoneRemains(t *testing.T) {
	f := newFakeRestoreBrowser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := newRestoreCapturer(t, f).RestoreWindow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != OpenedNewWindow {
		t.Fatalf("outcome = %q, want %q", got, OpenedNewWindow)
	}
	if len(f.created) != 1 || f.created[0]["url"] != "about:blank" || f.created[0]["newWindow"] != true {
		t.Fatalf("createTarget params = %v, want one about:blank newWindow target", f.created)
	}
}

func TestRestoreWindowWaitsForBrowserTheSupervisorIsRestarting(t *testing.T) {
	f := newFakeRestoreBrowser(t)
	f.versionFailures = 2
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	outcome, err := newRestoreCapturer(t, f).RestoreWindow(ctx)
	if err != nil || outcome != OpenedNewWindow {
		t.Fatalf("RestoreWindow after a short outage: outcome=%q, err=%v", outcome, err)
	}
	if len(f.windows) != 1 || f.windows[0].state != "normal" {
		t.Fatal("browser recovery must leave one visible window")
	}
}

func TestRestoreWindowReportsUnreachableBrowserWithoutActing(t *testing.T) {
	f := newFakeRestoreBrowser(t)
	f.versionFailures = 1 << 20
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	_, err := newRestoreCapturer(t, f).RestoreWindow(ctx)
	if !errors.Is(err, ErrBrowserUnavailable) {
		t.Fatalf("err = %v, want ErrBrowserUnavailable", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("browser commands were sent to an endpoint that never became ready: %v", f.calls)
	}
}

func TestRestoreWindowReportsWindowThatStaysMinimized(t *testing.T) {
	f := newFakeRestoreBrowser(t, &fakeWindow{target: "TAB1", windowID: 7, state: "minimized", stuck: true})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := newRestoreCapturer(t, f).RestoreWindow(ctx)
	if !errors.Is(err, ErrRestoreFailed) {
		t.Fatalf("err = %v, want ErrRestoreFailed", err)
	}
}

func TestRestoreWindowDoesNotReplaceWindowOnProtocolFailure(t *testing.T) {
	f := newFakeRestoreBrowser(t, &fakeWindow{target: "TAB1", windowID: 7, state: "minimized"})
	f.windowError = "Internal error while finding browser window"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := newRestoreCapturer(t, f).RestoreWindow(ctx); err == nil || !strings.Contains(err.Error(), f.windowError) {
		t.Fatalf("err=%v, want the original protocol failure", err)
	}
	if len(f.created) != 0 || f.windows[0].state != "minimized" {
		t.Fatal("a failed window lookup must not create or change a browser window")
	}
}
