//go:build e2e

package e2e

import (
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/tests/e2e/internal/cdpc"
	"github.com/JiaWeiXie/lensdock/tests/e2e/internal/harness"
)

const (
	closeLeaseEnded        = 4001
	closeGenerationChanged = 4002
)

// waitClosed waits for the connection to end and returns the elapsed time since
// start together with the close status.
func waitClosed(t *testing.T, c *cdpc.Client, start time.Time, limit time.Duration) (time.Duration, int) {
	t.Helper()
	select {
	case <-c.Done():
		return time.Since(start), int(c.CloseStatus())
	case <-time.After(limit):
		t.Fatalf("connection still open %v after the lease ended", limit)
		return 0, 0
	}
}

// TestCT03 (CT-03): a released, expired, stale-epoch or stale-generation lease can
// no longer drive the browser, over HTTP and over an already open WebSocket.
// The generation subtest kills Chromium and therefore runs last.
func TestCT03(t *testing.T) {
	api := harness.NewAPI(t)
	ev := harness.NewEvidence(t)
	results := map[string]any{}
	t.Cleanup(func() { ev.SaveJSON(t, "TestCT03/results.json", results) })

	t.Run("a_release_closes_open_websocket_with_4001_within_1s", func(t *testing.T) {
		g := acquire(t, api, modeAgent, 60)
		c := browserClient(t, api, g.Token)
		start := time.Now()
		release(t, api, g)
		elapsed, status := waitClosed(t, c, start, 10*time.Second)
		results["releaseCloseMillis"], results["releaseCloseStatus"] = elapsed.Milliseconds(), status
		if status != closeLeaseEnded {
			t.Errorf("close status = %d, want %d", status, closeLeaseEnded)
		}
		if elapsed > time.Second {
			t.Errorf("WebSocket closed %v after release, want <= 1s", elapsed)
		}
	})

	t.Run("b_ttl_expiry_closes_open_websocket_with_4001_at_10s", func(t *testing.T) {
		g := acquire(t, api, modeAgent, 10)
		start := time.Now()
		c := browserClient(t, api, g.Token)
		elapsed, status := waitClosed(t, c, start, 20*time.Second)
		results["ttlCloseMillis"], results["ttlCloseStatus"] = elapsed.Milliseconds(), status
		if status != closeLeaseEnded {
			t.Errorf("close status = %d, want %d", status, closeLeaseEnded)
		}
		if d := elapsed - 10*time.Second; d < -time.Second || d > time.Second {
			t.Errorf("WebSocket closed after %v, want 10s +/- 1s", elapsed)
		}
		// The expired token is dead over HTTP and for new WebSockets as well.
		ctx := timeout(t, 15*time.Second)
		resp, err := api.Do(ctx, http.MethodGet, cdpPath(api, "/json/version"), g.Token, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		assertErrorBody(t, "expired token on /json/version", resp, 409, apiwire.CodeLeaseEnded)
	})

	t.Run("d_ended_tokens_stale_epochs_and_stale_generations_are_refused", func(t *testing.T) {
		ctx := timeout(t, 60*time.Second)
		g1 := acquire(t, api, modeAgent, 30)
		release(t, api, g1)

		resp, err := api.Do(ctx, http.MethodGet, cdpPath(api, "/json/version"), g1.Token, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		assertErrorBody(t, "reconnect over HTTP with the old token", resp, 409, apiwire.CodeLeaseEnded)

		_, err = api.CDP(g1.Token).Dial(ctx, "ws://x"+cdpPath(api, "/devtools/browser/00000000-0000-4000-8000-000000000000"))
		wantHTTPError(t, "reconnect over WebSocket with the old token", err, 409)

		_, err = api.Renew(ctx, g1.Lease.LeaseID, g1.Token, g1.Lease.Epoch, g1.Lease.RuntimeGeneration)
		wantAPIError(t, "renew of an ended lease", err, 409, apiwire.CodeLeaseEnded)

		g2 := acquire(t, api, modeAgent, 30)
		if g2.Lease.Epoch <= g1.Lease.Epoch {
			t.Errorf("epoch did not increase: %d -> %d", g1.Lease.Epoch, g2.Lease.Epoch)
		}
		_, err = api.Renew(ctx, g2.Lease.LeaseID, g2.Token, g1.Lease.Epoch, g2.Lease.RuntimeGeneration)
		wantAPIError(t, "renew with the old epoch", err, 412, apiwire.CodeEpochMismatch)
		_, err = api.Renew(ctx, g2.Lease.LeaseID, g2.Token, g2.Lease.Epoch, zeroGeneration)
		wantAPIError(t, "renew with a foreign generation", err, 412, apiwire.CodeGenerationMismatch)
		if _, err := api.Renew(ctx, g2.Lease.LeaseID, g2.Token, g2.Lease.Epoch, g2.Lease.RuntimeGeneration); err != nil {
			t.Errorf("renew with the current epoch and generation failed: %v", err)
		}
		release(t, api, g2)
	})

	t.Run("e_real_bridge_exits_3_and_closes_clients_after_revoke", func(t *testing.T) {
		cli := harness.NewCLI(t, api)
		leaseFile := filepath.Join(t.TempDir(), "lease.json")
		if err := cli.AcquireToFile(timeout(t, 30*time.Second), modeAgent, 30, leaseFile); err != nil {
			t.Fatalf("lensdock lease acquire: %v", err)
		}
		t.Cleanup(func() { _, _ = cli.ReleaseFile(timeout(t, 15*time.Second), leaseFile) })
		bridge, err := cli.StartBridge(timeout(t, 5*time.Minute), leaseFile, 20*time.Second, false)
		if err != nil {
			t.Fatalf("lensdock cdp bridge: %v", err)
		}
		t.Cleanup(bridge.Kill)

		ctx := timeout(t, 60*time.Second)
		disc := &cdpc.Discovery{Base: strings.TrimSuffix(bridge.Endpoints.DiscoveryURL, "/")}
		// Default mode serves at the root: no URL prefix, so `--cdp PORT` clients work directly.
		if du, err := url.Parse(bridge.Endpoints.DiscoveryURL); err != nil || du.Hostname() != "127.0.0.1" || strings.Trim(du.Path, "/") != "" {
			t.Errorf("default-mode discoveryUrl %q is not a bare loopback root URL (%v)", bridge.Endpoints.DiscoveryURL, err)
		}
		if bu, err := url.Parse(bridge.Endpoints.BrowserWebSocketURL); err != nil || !strings.HasPrefix(bu.Path, "/devtools/browser/") {
			t.Errorf("default-mode browserWebSocketUrl %q is not rooted at /devtools/browser/ (%v)", bridge.Endpoints.BrowserWebSocketURL, err)
		}
		ver, err := disc.Version(ctx)
		if err != nil {
			t.Fatalf("GET /json/version through the bridge: %v", err)
		}
		if !strings.HasPrefix(ver.WebSocketDebuggerURL, "ws://127.0.0.1:") || strings.Contains(ver.WebSocketDebuggerURL, ":9223") {
			t.Errorf("bridge rewrote webSocketDebuggerUrl to %q", ver.WebSocketDebuggerURL)
		}
		if ver.WebSocketDebuggerURL != bridge.Endpoints.BrowserWebSocketURL {
			t.Errorf("discovery webSocketDebuggerUrl %q differs from the printed browserWebSocketUrl %q",
				ver.WebSocketDebuggerURL, bridge.Endpoints.BrowserWebSocketURL)
		}
		c, err := cdpc.Dial(ctx, bridge.Endpoints.BrowserWebSocketURL, cdpc.DialOptions{})
		if err != nil {
			t.Fatalf("dial the bridge: %v", err)
		}
		defer c.Close()
		if err := c.Call(ctx, "Browser.getVersion", nil, nil); err != nil {
			t.Fatalf("Browser.getVersion through the bridge: %v", err)
		}

		start := time.Now()
		if err := api.Release(ctx, bridge.Endpoints.LeaseID, ""); err != nil {
			t.Fatalf("operator release of the bridged lease: %v", err)
		}
		elapsed, status := waitClosed(t, c, start, 10*time.Second)
		code, err := bridge.WaitExit(15 * time.Second)
		results["bridgeClientCloseStatus"], results["bridgeClientCloseMillis"], results["bridgeExitCode"] = status, elapsed.Milliseconds(), code
		if status != closeLeaseEnded {
			t.Errorf("bridge client close status = %d, want %d", status, closeLeaseEnded)
		}
		if err != nil {
			t.Fatalf("bridge did not exit after the lease was revoked: %v\nstderr: %s", err, bridge.Stderr())
		}
		if code != 3 {
			t.Errorf("bridge exit code = %d, want 3 (lease ended)\nstderr: %s", code, bridge.Stderr())
		}
	})

	t.Run("f_path_secret_bridge_requires_the_prefix", func(t *testing.T) {
		cli := harness.NewCLI(t, api)
		leaseFile := filepath.Join(t.TempDir(), "lease.json")
		if err := cli.AcquireToFile(timeout(t, 30*time.Second), modeAgent, 30, leaseFile); err != nil {
			t.Fatalf("lensdock lease acquire: %v", err)
		}
		t.Cleanup(func() { _, _ = cli.ReleaseFile(timeout(t, 15*time.Second), leaseFile) })
		bridge, err := cli.StartBridge(timeout(t, 5*time.Minute), leaseFile, 20*time.Second, true)
		if err != nil {
			t.Fatalf("lensdock cdp bridge --path-secret: %v", err)
		}
		t.Cleanup(bridge.Kill)

		du, err := url.Parse(bridge.Endpoints.DiscoveryURL)
		if err != nil {
			t.Fatalf("discoveryUrl %q: %v", bridge.Endpoints.DiscoveryURL, err)
		}
		secret := strings.Trim(du.Path, "/")
		if len(secret) != 64 || strings.Trim(secret, "0123456789abcdef") != "" {
			t.Fatalf("discoveryUrl %q: path %q is not a 32-byte hex prefix", bridge.Endpoints.DiscoveryURL, du.Path)
		}
		bu, err := url.Parse(bridge.Endpoints.BrowserWebSocketURL)
		if err != nil || !strings.HasPrefix(bu.Path, "/"+secret+"/devtools/browser/") {
			t.Fatalf("browserWebSocketUrl %q does not carry the prefix %q", bridge.Endpoints.BrowserWebSocketURL, secret)
		}

		ctx := timeout(t, 60*time.Second)
		disc := &cdpc.Discovery{Base: strings.TrimSuffix(bridge.Endpoints.DiscoveryURL, "/")}
		ver, err := disc.Version(ctx)
		if err != nil {
			t.Fatalf("GET /json/version under the secret prefix: %v", err)
		}
		if ver.WebSocketDebuggerURL != bridge.Endpoints.BrowserWebSocketURL {
			t.Errorf("discovery webSocketDebuggerUrl %q differs from the printed browserWebSocketUrl %q",
				ver.WebSocketDebuggerURL, bridge.Endpoints.BrowserWebSocketURL)
		}
		c, err := cdpc.Dial(ctx, bridge.Endpoints.BrowserWebSocketURL, cdpc.DialOptions{})
		if err != nil {
			t.Fatalf("dial the bridge under the secret prefix: %v", err)
		}
		defer c.Close()
		if err := c.Call(ctx, "Browser.getVersion", nil, nil); err != nil {
			t.Fatalf("Browser.getVersion under the secret prefix: %v", err)
		}

		// Without the prefix (or with a wrong one) every route is a plain 404.
		for name, p := range map[string]string{
			"no prefix":    "/json/version",
			"wrong prefix": "/" + strings.Repeat("0", 64) + "/json/version",
		} {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+du.Host+p, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET %s (%s): %v", p, name, err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("GET %s (%s) = %d, want 404", p, name, resp.StatusCode)
			}
		}
		browserID := bu.Path[strings.LastIndex(bu.Path, "/")+1:]
		_, err = cdpc.Dial(ctx, "ws://"+du.Host+"/devtools/browser/"+browserID, cdpc.DialOptions{})
		wantHTTPError(t, "WebSocket upgrade without the secret prefix", err, 404)
	})

	t.Run("c_browser_restart_closes_open_websocket_with_4002_and_new_generation", func(t *testing.T) {
		dk := harness.NewDocker(t)
		ctx := timeout(t, 4*time.Minute)
		before := mustWorkspace(t, api)
		g := acquire(t, api, modeAgent, 60)
		c := browserClient(t, api, g.Token)

		procs, _, err := dk.ProcSnapshot(ctx)
		if err != nil {
			t.Fatalf("process snapshot: %v", err)
		}
		main, ok := harness.ChromiumMain(procs)
		if !ok {
			t.Fatal("no Chromium browser process to kill")
		}
		start := time.Now()
		if err := dk.KillPID(ctx, main.PID); err != nil {
			t.Fatalf("kill Chromium (pid %d): %v", main.PID, err)
		}
		elapsed, status := waitClosed(t, c, start, 30*time.Second)
		results["generationCloseMillis"], results["generationCloseStatus"] = elapsed.Milliseconds(), status
		if status != closeGenerationChanged {
			t.Errorf("close status = %d, want %d", status, closeGenerationChanged)
		}

		after, err := api.WaitReady(ctx, 2*time.Minute)
		if err != nil {
			t.Fatalf("runtime did not become ready again: %v", err)
		}
		results["generationBefore"], results["generationAfter"] = before.RuntimeGeneration, after.RuntimeGeneration
		if after.RuntimeGeneration == before.RuntimeGeneration {
			t.Errorf("runtime generation did not change (%s)", after.RuntimeGeneration)
		}
		if g.Lease.RuntimeGeneration != before.RuntimeGeneration {
			t.Errorf("lease generation %s differs from workspace generation %s before the kill",
				g.Lease.RuntimeGeneration, before.RuntimeGeneration)
		}

		resp, err := api.Do(ctx, http.MethodGet, cdpPath(api, "/json/version"), g.Token, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		assertErrorBody(t, "pre-restart token after the restart", resp, 409, apiwire.CodeLeaseEnded)

		g2 := acquire(t, api, modeAgent, 30)
		if g2.Lease.RuntimeGeneration != after.RuntimeGeneration {
			t.Errorf("new lease generation %s, want %s", g2.Lease.RuntimeGeneration, after.RuntimeGeneration)
		}
		_, err = api.Renew(ctx, g2.Lease.LeaseID, g2.Token, g2.Lease.Epoch, before.RuntimeGeneration)
		wantAPIError(t, "renew with the pre-restart generation", err, 412, apiwire.CodeGenerationMismatch)
		release(t, api, g2)
	})
}

// assertErrorBody checks status and apiwire error code of a raw response.
func assertErrorBody(t *testing.T, what string, resp harness.Response, status int, code string) {
	t.Helper()
	body, ok := resp.ErrorBody()
	if resp.Status != status || !ok || body.Code != code {
		t.Errorf("%s: got HTTP %d code %q, want HTTP %d %s", what, resp.Status, body.Code, status, code)
	}
}
