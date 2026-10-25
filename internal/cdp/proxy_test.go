package cdp

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/lease"
)

func TestProxyRejectsBeforeReachingChromium(t *testing.T) {
	env := newRuntimeEnv(t, 0)
	endedLease, endedTok := env.acquire(t, lease.ModeAgent, "k1")
	if err := env.mgr.Release(endedLease.ID); err != nil {
		t.Fatal(err)
	}
	_, humanTok := env.acquire(t, lease.ModeHuman, "k2")

	tests := []struct {
		name       string
		method     string
		path       string
		token      string
		hdr        map[string]string
		wantStatus int
		wantCode   string
	}{
		{"no token", "GET", "/json/version", "", nil, 401, apiwire.CodeUnauthenticated},
		{"unknown token", "GET", "/json/version", "ldl_nope", nil, 401, apiwire.CodeUnauthenticated},
		{"basic scheme", "GET", "/json/version", "", map[string]string{"Authorization": "Basic Zm9vOmJhcg=="}, 401, apiwire.CodeUnauthenticated},
		{"origin without token", "GET", "/json/version", "", map[string]string{"Origin": "https://evil.example"}, 403, apiwire.CodeOriginNotAllowed},
		{"origin with human token", "GET", "/json/version", humanTok.Reveal(), map[string]string{"Origin": "https://evil.example"}, 403, apiwire.CodeOriginNotAllowed},
		{"empty origin", "GET", "/json/version", humanTok.Reveal(), map[string]string{"Origin": ""}, 403, apiwire.CodeOriginNotAllowed},
		{"wrong mode", "GET", "/json/version", humanTok.Reveal(), nil, 403, apiwire.CodeWrongLeaseMode},
		{"ended lease", "GET", "/json/version", endedTok.Reveal(), nil, 409, apiwire.CodeLeaseEnded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := env.do(t, tt.method, tt.path, tt.token, tt.hdr)
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tt.wantStatus, body)
			}
			if e := decodeError(t, body); e.Code != tt.wantCode {
				t.Fatalf("code = %q, want %q", e.Code, tt.wantCode)
			}
			if tt.wantStatus == 401 && resp.Header.Get("WWW-Authenticate") != "Bearer" {
				t.Errorf("missing WWW-Authenticate challenge")
			}
		})
	}
	if n := env.chromium.requestCount(); n != 0 {
		t.Errorf("chromium saw %d requests from rejected callers", n)
	}
}

func TestProxyLockedWorkspaceRefusesTheOldToken(t *testing.T) {
	env := newRuntimeEnv(t, 0)
	_, tok := env.acquire(t, lease.ModeAgent, "k1")
	env.mgr.SetLocked(true, "SANDBOX_PREFLIGHT_FAILED")
	resp, body := env.do(t, "GET", "/json/version", tok.Reveal(), nil)
	if resp.StatusCode != 409 || decodeError(t, body).Code != apiwire.CodeLeaseEnded {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	env.mgr.SetLocked(false, "")
	resp, body = env.do(t, "GET", "/json/version", tok.Reveal(), nil)
	if resp.StatusCode != 409 || decodeError(t, body).Code != apiwire.CodeLeaseEnded {
		t.Fatalf("after unlock: status %d body %s", resp.StatusCode, body)
	}
}

func TestProxyAllowlist(t *testing.T) {
	env := newRuntimeEnv(t, 0)
	_, tok := env.acquire(t, lease.ModeAgent, "k1")
	tests := []struct {
		method, path string
		want         int
		wantCode     string
	}{
		{"GET", "/json/version", 200, ""},
		{"GET", "/json/version/", 200, ""},
		{"GET", "/json", 200, ""},
		{"GET", "/json/list", 200, ""},
		{"GET", "/json/protocol", 200, ""},
		{"PUT", "/json/new?https://example.com/", 200, ""},
		{"GET", "/json/activate/" + pageID, 200, ""},
		{"GET", "/json/close/" + pageID, 200, ""},

		{"GET", "/json/new?https://example.com/", 405, apiwire.CodeInvalidRequest},
		{"POST", "/json/new", 405, apiwire.CodeInvalidRequest},
		{"DELETE", "/json/list", 405, apiwire.CodeInvalidRequest},
		{"GET", "/json/foo", 404, apiwire.CodeNotFound},
		{"GET", "/devtools/inspector.html", 404, apiwire.CodeNotFound},
		{"GET", "/json/activate/bad_id", 404, apiwire.CodeNotFound},
		{"GET", "/devtools/page/" + pageID, 400, apiwire.CodeInvalidRequest}, // not an upgrade
		{"GET", "/", 404, apiwire.CodeNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			resp, body := env.do(t, tt.method, tt.path, tok.Reveal(), nil)
			if resp.StatusCode != tt.want {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tt.want, body)
			}
			if tt.wantCode != "" && decodeError(t, body).Code != tt.wantCode {
				t.Fatalf("code = %q, want %q", decodeError(t, body).Code, tt.wantCode)
			}
		})
	}
}

func TestProxyDiscoveryRewriteAndUpstreamRequest(t *testing.T) {
	env := newRuntimeEnv(t, 0)
	_, tok := env.acquire(t, lease.ModeAgent, "k1")

	hdr := map[string]string{
		"Accept":          "application/json",
		"Cookie":          "session=secret",
		"X-Forwarded-For": "203.0.113.9",
		"Referer":         "https://evil.example/",
		"User-Agent":      "attacker/1.0",
	}
	resp, body := env.do(t, "GET", "/json/list", tok.Reveal(), hdr)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	s := string(body)
	for _, bad := range []string{"9223", "devtoolsFrontendUrl", "inspector.html"} {
		if strings.Contains(s, bad) {
			t.Errorf("response contains %q: %s", bad, s)
		}
	}
	if want := "ws://" + env.host() + "/cdp/devtools/page/" + pageID; !strings.Contains(s, want) {
		t.Errorf("response lacks public URL %q: %s", want, s)
	}
	if !strings.Contains(s, `"title":"A <b>&</b>"`) {
		t.Errorf("other fields not preserved: %s", s)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content type = %q", ct)
	}

	up := env.chromium.lastRequest(t)
	if up.Host != UpstreamHost {
		t.Errorf("upstream Host = %q, want %q", up.Host, UpstreamHost)
	}
	for _, h := range []string{"Origin", "Authorization", "Cookie", "X-Forwarded-For", "Referer"} {
		if v := up.Header.Get(h); v != "" {
			t.Errorf("upstream received %s: %q", h, v)
		}
	}
	if up.Header.Get("Accept") != "application/json" {
		t.Errorf("Accept was not forwarded: %q", up.Header.Get("Accept"))
	}
	if ua := up.Header.Get("User-Agent"); strings.Contains(ua, "attacker") {
		t.Errorf("client User-Agent leaked upstream: %q", ua)
	}
}

func TestProxyVersionAndNew(t *testing.T) {
	env := newRuntimeEnv(t, 0)
	_, tok := env.acquire(t, lease.ModeAgent, "k1")

	_, body := env.do(t, "GET", "/json/version", tok.Reveal(), nil)
	if want := "ws://" + env.host() + "/cdp/devtools/browser/" + browserID; !strings.Contains(string(body), want) {
		t.Errorf("version lacks %q: %s", want, body)
	}

	resp, body := env.do(t, "PUT", "/json/new?https://example.com/a?b=c", tok.Reveal(), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if up := env.chromium.lastRequest(t); up.Method != "PUT" || up.RawQuery != "https://example.com/a?b=c" {
		t.Errorf("upstream saw %s ?%s", up.Method, up.RawQuery)
	}
	if s := string(body); strings.Contains(s, "9223") || strings.Contains(s, "devtoolsFrontendUrl") || !strings.Contains(s, "/cdp/devtools/page/NEW1") {
		t.Errorf("new target not rewritten: %s", s)
	}
}

func TestProxyNeverRelaysUpstreamText(t *testing.T) {
	env := newRuntimeEnv(t, 0)
	_, tok := env.acquire(t, lease.ModeAgent, "k1")

	resp, body := env.do(t, "GET", "/json/activate/missing", tok.Reveal(), nil)
	if resp.StatusCode != 404 || decodeError(t, body).Code != apiwire.CodeNotFound {
		t.Fatalf("missing target: %d %s", resp.StatusCode, body)
	}
	resp, body = env.do(t, "GET", "/json/close/broken", tok.Reveal(), nil)
	if resp.StatusCode != 502 || decodeError(t, body).Code != apiwire.CodeUpstreamUnavailable {
		t.Fatalf("broken upstream: %d %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "SECRET") {
		t.Errorf("upstream text leaked: %s", body)
	}
}

func TestProxyUpstreamDownIsBadGateway(t *testing.T) {
	env := newRuntimeEnv(t, 0)
	_, tok := env.acquire(t, lease.ModeAgent, "k1")
	env.chromium.srv.Close()
	resp, body := env.do(t, "GET", "/json/version", tok.Reveal(), nil)
	if resp.StatusCode != 502 || decodeError(t, body).Code != apiwire.CodeUpstreamUnavailable {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
}

func TestProxyWebSocketRelayAndUpstreamHandshake(t *testing.T) {
	env := newRuntimeEnv(t, 0)
	_, tok := env.acquire(t, lease.ModeAgent, "k1")

	c := mustDial(t, env.wsURL("/devtools/page/"+pageID), tok.Reveal())
	expectEcho(t, c, `{"id":1,"method":"Page.enable"}`)

	up := env.chromium.lastRequest(t)
	if up.Path != "/devtools/page/"+pageID {
		t.Errorf("upstream path = %q", up.Path)
	}
	if up.Host != UpstreamHost {
		t.Errorf("upstream Host = %q, want %q", up.Host, UpstreamHost)
	}
	for _, h := range []string{"Origin", "Authorization", "Cookie"} {
		if v := up.Header.Get(h); v != "" {
			t.Errorf("upstream handshake carried %s: %q", h, v)
		}
	}
	if ext := up.Header.Get("Sec-WebSocket-Extensions"); ext != "" {
		t.Errorf("upstream negotiated compression: %q", ext)
	}

	// Large messages cross intact and in both directions.
	big := `{"id":2,"result":"` + strings.Repeat("x", 3<<20) + `"}`
	expectEcho(t, c, big)
}

func TestProxyWebSocketRejectsBeforeUpgrade(t *testing.T) {
	env := newRuntimeEnv(t, 0)
	_, tok := env.acquire(t, lease.ModeAgent, "k1")
	url := env.wsURL("/devtools/page/" + pageID)

	t.Run("origin present", func(t *testing.T) {
		_, resp, err := dialWS(t, url, tok.Reveal(), http.Header{"Origin": {"https://evil.example"}})
		if err == nil || resp == nil || resp.StatusCode != 403 {
			t.Fatalf("resp %v err %v", resp, err)
		}
	})
	t.Run("same-origin origin is still rejected", func(t *testing.T) {
		_, resp, err := dialWS(t, url, tok.Reveal(), http.Header{"Origin": {"http://" + env.host()}})
		if err == nil || resp == nil || resp.StatusCode != 403 {
			t.Fatalf("resp %v err %v", resp, err)
		}
	})
	t.Run("no token", func(t *testing.T) {
		_, resp, err := dialWS(t, url, "", nil)
		if err == nil || resp == nil || resp.StatusCode != 401 {
			t.Fatalf("resp %v err %v", resp, err)
		}
	})
	t.Run("route outside allowlist", func(t *testing.T) {
		_, resp, err := dialWS(t, env.wsURL("/devtools/inspector.html"), tok.Reveal(), nil)
		if err == nil || resp == nil || resp.StatusCode != 404 {
			t.Fatalf("resp %v err %v", resp, err)
		}
	})
	if n := env.chromium.requestCount(); n != 0 {
		t.Errorf("chromium saw %d requests from rejected callers", n)
	}
}

func TestProxyWebSocketWrongModeAndEnded(t *testing.T) {
	env := newRuntimeEnv(t, 0)
	url := env.wsURL("/devtools/browser/" + browserID)

	l, tok := env.acquire(t, lease.ModeHuman, "k1")
	_, resp, err := dialWS(t, url, tok.Reveal(), nil)
	if err == nil || resp == nil || resp.StatusCode != 403 {
		t.Fatalf("wrong mode: resp %v err %v", resp, err)
	}
	if err := env.mgr.Release(l.ID); err != nil {
		t.Fatal(err)
	}
	_, resp, err = dialWS(t, url, tok.Reveal(), nil)
	if err == nil || resp == nil || resp.StatusCode != 409 {
		t.Fatalf("ended: resp %v err %v", resp, err)
	}
}

func TestProxyDeniedMethodGetsSyntheticErrorAndConnectionStaysUsable(t *testing.T) {
	env := newRuntimeEnv(t, 0)
	_, tok := env.acquire(t, lease.ModeAgent, "k1")
	c := mustDial(t, env.wsURL("/devtools/browser/"+browserID), tok.Reveal())

	writeText(t, c, `{"id":41,"method":"Browser.close"}`)
	typ, data, err := readWithin(t, c, 2*time.Second)
	if err != nil || typ != websocket.MessageText {
		t.Fatalf("read: %v %v", typ, err)
	}
	if got, want := string(data), `{"id":41,"error":{"code":-32000,"message":"Method denied by LensDock policy"}}`; got != want {
		t.Fatalf("reply = %s, want %s", got, want)
	}

	writeText(t, c, `{"id":42,"method":"Page.enable","method":"Browser.close"}`)
	_, data, err = readWithin(t, c, 2*time.Second)
	if err != nil || !strings.Contains(string(data), `"code":-32700`) {
		t.Fatalf("malformed reply = %s, %v", data, err)
	}

	expectEcho(t, c, `{"id":43,"method":"Browser.getVersion"}`)
	for _, m := range env.chromium.receivedMessages() {
		if strings.Contains(m, "Browser.close") {
			t.Errorf("denied message reached Chromium: %s", m)
		}
	}
}

func TestProxyClientBinaryFrameClosesWithUnsupportedData(t *testing.T) {
	env := newRuntimeEnv(t, 0)
	_, tok := env.acquire(t, lease.ModeAgent, "k1")
	c := mustDial(t, env.wsURL("/devtools/page/"+pageID), tok.Reveal())

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageBinary, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	expectClose(t, c, websocket.StatusUnsupportedData, 2*time.Second)
	env.chromium.waitWSFinished(t, 1)
	if got := env.chromium.receivedMessages(); len(got) != 0 {
		t.Errorf("binary frame reached Chromium: %q", got)
	}
}

func TestProxyLeaseEndClosesConnections(t *testing.T) {
	tests := []struct {
		name string
		end  func(env *runtimeEnv, l lease.Lease)
		want websocket.StatusCode
	}{
		{"released", func(e *runtimeEnv, l lease.Lease) { _ = e.mgr.Release(l.ID) }, CloseLeaseEnded},
		{"shutdown", func(e *runtimeEnv, _ lease.Lease) { e.mgr.Shutdown() }, CloseLeaseEnded},
		{"workspace locked", func(e *runtimeEnv, _ lease.Lease) { e.mgr.SetLocked(true, "SANDBOX_PREFLIGHT_FAILED") }, CloseLeaseEnded},
		{"new generation", func(e *runtimeEnv, _ lease.Lease) { e.mgr.StartGeneration() }, CloseGenerationChanged},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newRuntimeEnv(t, 0)
			l, tok := env.acquire(t, lease.ModeAgent, "k1")
			c1 := mustDial(t, env.wsURL("/devtools/page/"+pageID), tok.Reveal())
			c2 := mustDial(t, env.wsURL("/devtools/browser/"+browserID), tok.Reveal())
			expectEcho(t, c1, `{"id":1,"method":"A.one"}`)
			expectEcho(t, c2, `{"id":2,"method":"A.two"}`)

			tt.end(env, l)
			expectClose(t, c1, tt.want, time.Second)
			expectClose(t, c2, tt.want, time.Second)
			env.chromium.waitWSFinished(t, 2)
		})
	}
}

func TestProxyUpstreamCloseReachesClient(t *testing.T) {
	t.Run("normal close is mirrored", func(t *testing.T) {
		env := newRuntimeEnv(t, 0)
		_, tok := env.acquire(t, lease.ModeAgent, "k1")
		c := mustDial(t, env.wsURL("/devtools/page/"+pageID), tok.Reveal())
		writeText(t, c, `{"id":1,"method":"Test.bye"}`)
		expectClose(t, c, websocket.StatusNormalClosure, 2*time.Second)
	})
	t.Run("dropped upstream becomes internal error", func(t *testing.T) {
		env := newRuntimeEnv(t, 0)
		_, tok := env.acquire(t, lease.ModeAgent, "k1")
		c := mustDial(t, env.wsURL("/devtools/page/"+pageID), tok.Reveal())
		writeText(t, c, `{"id":1,"method":"Test.drop"}`)
		expectClose(t, c, websocket.StatusInternalError, 3*time.Second)
	})
	t.Run("drop followed by generation change reports 4002", func(t *testing.T) {
		env := newRuntimeEnv(t, 0)
		_, tok := env.acquire(t, lease.ModeAgent, "k1")
		c := mustDial(t, env.wsURL("/devtools/page/"+pageID), tok.Reveal())
		writeText(t, c, `{"id":1,"method":"Test.drop"}`)
		env.mgr.StartGeneration()
		expectClose(t, c, CloseGenerationChanged, 3*time.Second)
	})
}

func TestProxyCapsConnectionsPerLease(t *testing.T) {
	env := newRuntimeEnv(t, 2)
	l, tok := env.acquire(t, lease.ModeAgent, "k1")
	url := env.wsURL("/devtools/page/" + pageID)

	c1 := mustDial(t, url, tok.Reveal())
	_ = mustDial(t, url, tok.Reveal())
	_, resp, err := dialWS(t, url, tok.Reveal(), nil)
	if err == nil || resp == nil || resp.StatusCode != 429 {
		t.Fatalf("third connection: resp %v err %v", resp, err)
	}

	if err := c1.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("close: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		c, _, err := dialWS(t, url, tok.Reveal(), nil)
		if err == nil {
			expectEcho(t, c, `{"id":3,"method":"A.again"}`)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot was not released: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The cap is per lease: another lease starts with a clean count.
	if err := env.mgr.Release(l.ID); err != nil {
		t.Fatal(err)
	}
	_, tok2 := env.acquire(t, lease.ModeAgent, "k2")
	_ = mustDial(t, url, tok2.Reveal())
}
