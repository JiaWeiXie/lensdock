package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/JiaWeiXie/lensdock/internal/auth"
	"github.com/JiaWeiXie/lensdock/internal/lease"
)

var errTerminalRenew = errors.New("renew: lease is gone")

// bridgeEnv is a Bridge in front of the runtime test environment.
type bridgeEnv struct {
	*runtimeEnv
	bridge  *Bridge
	ln      net.Listener
	token   auth.Secret
	leaseID string
	renews  atomic.Int32
	renewFn atomic.Pointer[func(context.Context) (time.Time, error)]

	served chan error
	cancel context.CancelFunc
}

func newBridgeEnv(t *testing.T) *bridgeEnv { return newBridgeEnvMode(t, false) }

// forEachBridgeMode runs fn once per path mode, each with a fresh bridge.
func forEachBridgeMode(t *testing.T, fn func(t *testing.T, env *bridgeEnv)) {
	t.Helper()
	for _, m := range []struct {
		name       string
		pathSecret bool
	}{{"root", false}, {"path secret", true}} {
		t.Run(m.name, func(t *testing.T) { fn(t, newBridgeEnvMode(t, m.pathSecret)) })
	}
}

func newBridgeEnvMode(t *testing.T, pathSecret bool) *bridgeEnv {
	t.Helper()
	env := &bridgeEnv{runtimeEnv: newRuntimeEnv(t, 0), served: make(chan error, 1)}
	l, tok := env.acquire(t, lease.ModeAgent, "bridge")
	env.token, env.leaseID = tok, l.ID

	up, err := url.Parse(env.srv.URL + "/cdp")
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewBridge(BridgeConfig{
		Upstream: up,
		Token:    tok,
		Renew: func(ctx context.Context) (time.Time, error) {
			env.renews.Add(1)
			if f := env.renewFn.Load(); f != nil {
				return (*f)(ctx)
			}
			return time.Now().Add(time.Hour), nil
		},
		RenewEvery: 10 * time.Millisecond,
		IsTerminal: func(err error) bool { return errors.Is(err, errTerminalRenew) },
		PathSecret: pathSecret,
	})
	if err != nil {
		t.Fatal(err)
	}
	env.bridge = b
	env.ln, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.ln.Close() })
	return env
}

func (e *bridgeEnv) serve(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	t.Cleanup(cancel)
	go func() { e.served <- e.bridge.Serve(ctx, e.ln) }()
}

func (e *bridgeEnv) waitServe(t *testing.T) error {
	t.Helper()
	select {
	case err := <-e.served:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
		return nil
	}
}

func (e *bridgeEnv) port() string { return strings.TrimPrefix(e.ln.Addr().String(), "127.0.0.1:") }

func (e *bridgeEnv) base() string { return "127.0.0.1:" + e.port() + e.bridge.mount() }

// path returns p under the bridge's mount: unchanged at the root, behind the
// secret prefix with PathSecret.
func (e *bridgeEnv) path(p string) string { return e.bridge.mount() + p }

func (e *bridgeEnv) get(t *testing.T, method, path, host string, hdr map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, "http://127.0.0.1:"+e.port()+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, sb.String()
}

func TestNewBridgeValidatesConfig(t *testing.T) {
	u, _ := url.Parse("http://127.0.0.1:8080/api/v1/workspaces/default/cdp")
	good := BridgeConfig{
		Upstream:   u,
		Token:      "ldl_x",
		Renew:      func(context.Context) (time.Time, error) { return time.Time{}, nil },
		RenewEvery: time.Second,
		IsTerminal: func(error) bool { return false },
	}
	if _, err := NewBridge(good); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	withQuery, _ := url.Parse("http://127.0.0.1:8080/x?a=b")
	withUser, _ := url.Parse("http://u:p@127.0.0.1:8080/x")
	ftp, _ := url.Parse("ftp://127.0.0.1/x")
	for name, mut := range map[string]func(*BridgeConfig){
		"nil upstream":    func(c *BridgeConfig) { c.Upstream = nil },
		"query":           func(c *BridgeConfig) { c.Upstream = withQuery },
		"userinfo":        func(c *BridgeConfig) { c.Upstream = withUser },
		"scheme":          func(c *BridgeConfig) { c.Upstream = ftp },
		"empty token":     func(c *BridgeConfig) { c.Token = "" },
		"nil renew":       func(c *BridgeConfig) { c.Renew = nil },
		"nil IsTerminal":  func(c *BridgeConfig) { c.IsTerminal = nil },
		"zero RenewEvery": func(c *BridgeConfig) { c.RenewEvery = 0 },
	} {
		cfg := good
		mut(&cfg)
		if _, err := NewBridge(cfg); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestBridgePathSecretPrefixIsRandomHex(t *testing.T) {
	a := newBridgeEnvMode(t, true).bridge.prefix
	b := newBridgeEnvMode(t, true).bridge.prefix
	if a == b {
		t.Fatal("two bridges share a prefix")
	}
	if len(a) != 64 || strings.Trim(a, "0123456789abcdef") != "" {
		t.Fatalf("prefix %q is not 32 bytes of lowercase hex", a)
	}
}

func TestBridgeEndpoints(t *testing.T) {
	for _, tt := range []struct {
		name       string
		pathSecret bool
	}{{"root", false}, {"path secret", true}} {
		t.Run(tt.name, func(t *testing.T) {
			env := newBridgeEnvMode(t, tt.pathSecret)
			eps, err := env.bridge.Endpoints(context.Background(), env.ln.Addr())
			if err != nil {
				t.Fatal(err)
			}
			wantDiscovery := "http://127.0.0.1:" + env.port()
			wantWS := "ws://127.0.0.1:" + env.port() + "/devtools/browser/" + browserID
			if tt.pathSecret {
				wantDiscovery = "http://127.0.0.1:" + env.port() + "/" + env.bridge.prefix
				wantWS = "ws://127.0.0.1:" + env.port() + "/" + env.bridge.prefix + "/devtools/browser/" + browserID
			}
			if eps.DiscoveryURL != wantDiscovery {
				t.Errorf("DiscoveryURL = %q, want %q", eps.DiscoveryURL, wantDiscovery)
			}
			if eps.BrowserWebSocketURL != wantWS {
				t.Errorf("BrowserWebSocketURL = %q, want %q", eps.BrowserWebSocketURL, wantWS)
			}

			if _, err := env.bridge.Endpoints(context.Background(), &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 9000}); err == nil {
				t.Error("non-loopback address accepted")
			}
			if _, err := env.bridge.Endpoints(context.Background(), &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}); err == nil {
				t.Error("unbound port accepted")
			}

			env.mgr.StartGeneration() // ends the lease: the runtime now answers 409
			if _, err := env.bridge.Endpoints(context.Background(), env.ln.Addr()); err == nil {
				t.Error("Endpoints succeeded with an ended lease")
			}
		})
	}
}

func TestBridgeRefusesNonLoopbackListener(t *testing.T) {
	env := newBridgeEnv(t)
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Skipf("cannot listen on 0.0.0.0: %v", err)
	}
	defer ln.Close()
	if err := env.bridge.Serve(context.Background(), ln); err == nil {
		t.Fatal("Serve accepted a non-loopback listener")
	}
}

type httpCase struct {
	name, path, host string
	hdr              map[string]string
	want             int
}

func runHTTPCases(t *testing.T, env *bridgeEnv, tests []httpCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, body := env.get(t, "GET", tt.path, tt.host, tt.hdr)
			if got != tt.want {
				t.Fatalf("status = %d, want %d: %s", got, tt.want, body)
			}
		})
	}
}

// TestBridgeRejectsBadHostAndOrigin holds in both modes: the Host, Origin and
// route allowlist checks do not depend on the path prefix.
func TestBridgeRejectsBadHostAndOrigin(t *testing.T) {
	forEachBridgeMode(t, func(t *testing.T, env *bridgeEnv) {
		env.serve(t)
		good := env.path("/json/version")
		port := env.port()
		runHTTPCases(t, env, []httpCase{
			{"ok with 127.0.0.1", good, "", nil, 200},
			{"ok with localhost", good, "localhost:" + port, nil, 200},
			{"foreign host", good, "evil.example:" + port, nil, 403},
			{"rebinding host without port", good, "localhost", nil, 403},
			{"host with wrong port", good, "127.0.0.1:1", nil, 403},
			{"origin", good, "", map[string]string{"Origin": "https://evil.example"}, 403},
			{"empty origin", good, "", map[string]string{"Origin": ""}, 403},
			{"route outside allowlist", env.path("/json/foo"), "", nil, 404},
			{"wrong method", env.path("/json/new"), "", nil, 405},
		})
	})
}

func TestBridgeRootModeRejectsPrefixShapedPaths(t *testing.T) {
	env := newBridgeEnv(t)
	env.serve(t)
	runHTTPCases(t, env, []httpCase{
		{"bare root", "/", "", nil, 404},
		{"prefix-shaped path", "/" + strings.Repeat("0", 64) + "/json/version", "", nil, 404},
	})
}

func TestBridgePathSecretRejectsWrongPrefix(t *testing.T) {
	env := newBridgeEnvMode(t, true)
	env.serve(t)
	prefix := env.bridge.prefix
	runHTTPCases(t, env, []httpCase{
		{"correct prefix", "/" + prefix + "/json/version", "", nil, 200},
		{"wrong prefix", "/" + strings.Repeat("0", 64) + "/json/version", "", nil, 404},
		{"no prefix", "/json/version", "", nil, 404},
		{"prefix is a prefix of a longer segment", "/" + prefix + "x/json/version", "", nil, 404},
		{"truncated prefix", "/" + prefix[:63] + "/json/version", "", nil, 404},
	})
}

func TestBridgeRewritesDiscoveryToBridgeURLs(t *testing.T) {
	forEachBridgeMode(t, func(t *testing.T, env *bridgeEnv) {
		env.serve(t)

		for _, path := range []string{"/json/version", "/json/list"} {
			status, body := env.get(t, "GET", env.path(path), "", nil)
			if status != 200 {
				t.Fatalf("%s: status %d: %s", path, status, body)
			}
			for _, bad := range []string{"9223", "devtoolsFrontendUrl", env.srv.Listener.Addr().String(), "/cdp/", env.token.Reveal()} {
				if strings.Contains(body, bad) {
					t.Errorf("%s leaks %q: %s", path, bad, body)
				}
			}
			if want := "ws://" + env.base() + "/devtools/"; !strings.Contains(body, want) {
				t.Errorf("%s lacks %q: %s", path, want, body)
			}
		}
		if up := env.chromium.lastRequest(t); up.Header.Get("Authorization") != "" {
			t.Errorf("token reached Chromium: %q", up.Header.Get("Authorization"))
		}
	})
}

func TestBridgeRelaysRuntimeErrorsAsAPIErrors(t *testing.T) {
	env := newBridgeEnv(t)
	env.serve(t)
	status, body := env.get(t, "GET", env.path("/json/activate/missing"), "", nil)
	if status != 404 || decodeError(t, []byte(body)).Code != "NOT_FOUND" {
		t.Fatalf("status %d body %s", status, body)
	}
}

func TestBridgeWebSocketInjectsTokenAndAppliesRuntimePolicy(t *testing.T) {
	forEachBridgeMode(t, func(t *testing.T, env *bridgeEnv) {
		env.serve(t)
		url := "ws://" + env.base() + "/devtools/page/" + pageID

		c := mustDial(t, url, "") // the client holds no token
		expectEcho(t, c, `{"id":1,"method":"Page.enable"}`)

		writeText(t, c, `{"id":2,"method":"Browser.crash"}`)
		_, data, err := readWithin(t, c, 2*time.Second)
		if err != nil || !strings.Contains(string(data), "Method denied by LensDock policy") {
			t.Fatalf("policy reply = %s, %v", data, err)
		}
		expectEcho(t, c, `{"id":3,"method":"Page.reload"}`)

		big := `{"id":4,"v":"` + strings.Repeat("y", 2<<20) + `"}`
		expectEcho(t, c, big)
	})
}

// TestBridgeRootModeForwardsAtTheBarePort is what agent-browser --cdp PORT
// does: discovery at the root, then the WebSocket URL from that document.
func TestBridgeRootModeForwardsAtTheBarePort(t *testing.T) {
	env := newBridgeEnv(t)
	env.serve(t)
	status, body := env.get(t, "GET", "/json/version", "", nil)
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	var doc struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	want := "ws://127.0.0.1:" + env.port() + "/devtools/browser/" + browserID
	if doc.WebSocketDebuggerURL != want {
		t.Fatalf("webSocketDebuggerUrl = %q, want %q", doc.WebSocketDebuggerURL, want)
	}
	c := mustDial(t, doc.WebSocketDebuggerURL, "")
	expectEcho(t, c, `{"id":1,"method":"Browser.getVersion"}`)
}

func TestBridgeWebSocketRejections(t *testing.T) {
	forEachBridgeMode(t, func(t *testing.T, env *bridgeEnv) {
		env.serve(t)
		base := "ws://" + env.base()
		rootURL := "ws://127.0.0.1:" + env.port()

		type wsCase struct {
			name string
			url  string
			hdr  http.Header
			want int
		}
		tests := []wsCase{
			{"origin", base + "/devtools/page/" + pageID, http.Header{"Origin": {"https://evil.example"}}, 403},
			{"foreign host", base + "/devtools/page/" + pageID, http.Header{"Host": {"evil.example"}}, 403},
			{"route outside allowlist", base + "/devtools/inspector.html", nil, 404},
			{"unknown prefix", rootURL + "/" + strings.Repeat("a", 64) + "/devtools/page/" + pageID, nil, 404},
		}
		if env.bridge.prefix != "" {
			tests = append(tests, wsCase{"missing prefix", rootURL + "/devtools/page/" + pageID, nil, 404})
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				opts := &websocket.DialOptions{HTTPHeader: tt.hdr}
				if h := tt.hdr.Get("Host"); h != "" {
					opts.Host = h
					opts.HTTPHeader = nil
				}
				_, resp, err := websocket.Dial(ctx, tt.url, opts)
				if err == nil || resp == nil || resp.StatusCode != tt.want {
					t.Fatalf("resp %v err %v, want status %d", resp, err, tt.want)
				}
			})
		}
		if n := env.chromium.requestCount(); n != 0 {
			t.Errorf("chromium saw %d requests", n)
		}
	})
}

func TestBridgeLeaseLostClosesClientsAndStopsServing(t *testing.T) {
	tests := []struct {
		name string
		end  func(env *bridgeEnv)
		want websocket.StatusCode
	}{
		{"released at runtime", func(e *bridgeEnv) { _ = e.mgr.Release(e.leaseID) }, CloseLeaseEnded},
		{"workspace locked at runtime", func(e *bridgeEnv) { e.mgr.SetLocked(true, "SANDBOX_PREFLIGHT_FAILED") }, CloseLeaseEnded},
		{"new generation", func(e *bridgeEnv) { e.mgr.StartGeneration() }, CloseGenerationChanged},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newBridgeEnv(t)
			env.serve(t)
			base := "ws://" + env.base()
			c1 := mustDial(t, base+"/devtools/page/"+pageID, "")
			c2 := mustDial(t, base+"/devtools/browser/"+browserID, "")
			expectEcho(t, c1, `{"id":1,"method":"A.b"}`)
			expectEcho(t, c2, `{"id":2,"method":"A.b"}`)

			tt.end(env)
			expectClose(t, c1, tt.want, 2*time.Second)
			expectClose(t, c2, tt.want, 2*time.Second)
			if err := env.waitServe(t); !errors.Is(err, ErrLeaseLost) {
				t.Fatalf("Serve = %v, want ErrLeaseLost", err)
			}
			if _, _, err := dialWS(t, base+"/devtools/page/"+pageID, "", nil); err == nil {
				t.Error("bridge still accepts connections after losing the lease")
			}
		})
	}
}

func TestBridgeRenewLoop(t *testing.T) {
	t.Run("terminal error loses the lease", func(t *testing.T) {
		env := newBridgeEnv(t)
		var calls atomic.Int32
		fn := func(context.Context) (time.Time, error) {
			if calls.Add(1) >= 3 {
				return time.Time{}, errTerminalRenew
			}
			return time.Now().Add(time.Hour), nil
		}
		env.renewFn.Store(&fn)
		env.serve(t)
		c := mustDial(t, "ws://"+env.base()+"/devtools/page/"+pageID, "")

		expectClose(t, c, CloseLeaseEnded, 3*time.Second)
		err := env.waitServe(t)
		if !errors.Is(err, ErrLeaseLost) || !errors.Is(err, errTerminalRenew) {
			t.Fatalf("Serve = %v, want ErrLeaseLost wrapping the renew error", err)
		}
		if n := calls.Load(); n != 3 {
			t.Errorf("renew called %d times, want 3 (loop must stop after the terminal error)", n)
		}
	})
	t.Run("transient errors are retried", func(t *testing.T) {
		env := newBridgeEnv(t)
		var calls atomic.Int32
		fn := func(context.Context) (time.Time, error) {
			if n := calls.Add(1); n <= 3 {
				return time.Time{}, errors.New("connection refused")
			}
			return time.Now().Add(time.Hour), nil
		}
		env.renewFn.Store(&fn)
		env.serve(t)
		deadline := time.Now().Add(3 * time.Second)
		for calls.Load() < 6 {
			if time.Now().After(deadline) {
				t.Fatalf("renew called only %d times", calls.Load())
			}
			select {
			case err := <-env.served:
				t.Fatalf("Serve returned on transient errors: %v", err)
			case <-time.After(10 * time.Millisecond):
			}
		}
		c := mustDial(t, "ws://"+env.base()+"/devtools/page/"+pageID, "")
		expectEcho(t, c, `{"id":1,"method":"A.b"}`)
	})
	t.Run("transient errors past the last expiry lose the lease", func(t *testing.T) {
		env := newBridgeEnv(t)
		var calls atomic.Int32
		fn := func(context.Context) (time.Time, error) {
			if calls.Add(1) == 1 {
				return time.Now().Add(30 * time.Millisecond), nil
			}
			return time.Time{}, errors.New("connection refused")
		}
		env.renewFn.Store(&fn)
		env.serve(t)
		if err := env.waitServe(t); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("Serve = %v, want ErrLeaseLost", err)
		}
	})
}

func TestBridgeContextCancelClosesClientsGoingAway(t *testing.T) {
	env := newBridgeEnv(t)
	env.serve(t)
	c := mustDial(t, "ws://"+env.base()+"/devtools/page/"+pageID, "")
	expectEcho(t, c, `{"id":1,"method":"A.b"}`)

	env.cancel()
	expectClose(t, c, websocket.StatusGoingAway, 2*time.Second)
	if err := env.waitServe(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve = %v, want context.Canceled", err)
	}
}

func TestBridgeClientCloseClosesRuntimeConnection(t *testing.T) {
	env := newBridgeEnv(t)
	env.serve(t)
	c := mustDial(t, "ws://"+env.base()+"/devtools/page/"+pageID, "")
	expectEcho(t, c, `{"id":1,"method":"A.b"}`)
	if err := c.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatal(err)
	}
	env.chromium.waitWSFinished(t, 1)
	select {
	case err := <-env.served:
		t.Fatalf("Serve returned after a client disconnect: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
}
