//go:build e2e

package e2e

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/tests/e2e/internal/cdpc"
	"github.com/JiaWeiXie/lensdock/tests/e2e/internal/harness"
)

const (
	modeAgent = "AGENT"
	modeHuman = "HUMAN"

	// zeroGeneration is a syntactically valid UUID that is never a real generation.
	zeroGeneration = "00000000-0000-4000-8000-000000000000"
)

// timeout returns a context bounded by d and cancelled at test end.
func timeout(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// bridgeRenewInterval is how often `lensdock cdp bridge` renews a lease of the
// given TTL: the TTL is clamped to [10 s, 120 s] and divided by three
// (cmd/lensdock/bridge.go). A bridge notices an ended lease at most one
// interval after it ended.
func bridgeRenewInterval(ttl time.Duration) time.Duration {
	return min(max(ttl, 10*time.Second), 120*time.Second) / 3
}

// acquire takes a lease (waiting while another is held) and registers a
// best-effort operator release so a failed test never strands the lease.
func acquire(t *testing.T, api *harness.API, mode string, ttlSeconds int) harness.Grant {
	t.Helper()
	g, err := api.AcquireWhenFree(timeout(t, 3*time.Minute), t, mode, ttlSeconds, 2*time.Minute)
	if err != nil {
		t.Fatalf("acquire %s lease: %v", mode, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = api.Release(ctx, g.Lease.LeaseID, "") // already-ended leases answer with an error; irrelevant here
	})
	return g
}

// release ends a lease as the operator and fails the test when the runtime refuses.
func release(t *testing.T, api *harness.API, g harness.Grant) {
	t.Helper()
	if err := api.Release(timeout(t, 15*time.Second), g.Lease.LeaseID, ""); err != nil {
		t.Fatalf("release lease: %v", err)
	}
}

// wantAPIError asserts that err is a runtime API error with the given status and code.
func wantAPIError(t *testing.T, what string, err error, status int, code string) {
	t.Helper()
	var ae *harness.APIError
	if !errors.As(err, &ae) {
		t.Errorf("%s: error = %v, want HTTP %d %s", what, err, status, code)
		return
	}
	if ae.Status != status || ae.Code != code {
		t.Errorf("%s: got HTTP %d %s, want HTTP %d %s", what, ae.Status, ae.Code, status, code)
	}
}

// wantHTTPError asserts that err wraps a *cdpc.HTTPError with the given status.
func wantHTTPError(t *testing.T, what string, err error, status int) {
	t.Helper()
	var he *cdpc.HTTPError
	if !errors.As(err, &he) {
		t.Errorf("%s: error = %v, want HTTP %d", what, err, status)
		return
	}
	if he.Status != status {
		t.Errorf("%s: got HTTP %d, want HTTP %d", what, he.Status, status)
	}
}

// browserClient dials the browser-level CDP WebSocket through the runtime proxy.
func browserClient(t *testing.T, api *harness.API, token string) *cdpc.Client {
	t.Helper()
	ctx := timeout(t, 30*time.Second)
	disc := api.CDP(token)
	ver, err := disc.Version(ctx)
	if err != nil {
		t.Fatalf("GET /json/version through the proxy: %v", err)
	}
	c, err := disc.Dial(ctx, ver.WebSocketDebuggerURL)
	if err != nil {
		t.Fatalf("dial browser WebSocket: %v", err)
	}
	t.Cleanup(c.Close)
	var out struct {
		Product string `json:"product"`
	}
	if err := c.Call(ctx, "Browser.getVersion", nil, &out); err != nil || out.Product == "" {
		t.Fatalf("Browser.getVersion over the fresh connection = %+v, %v", out, err)
	}
	return c
}

// firstPage returns the first real page target (the tab the user sees); it
// opens about:blank when the browser has none.
func firstPage(ctx context.Context, disc *cdpc.Discovery) (cdpc.Target, error) {
	ts, err := disc.List(ctx)
	if err != nil {
		return cdpc.Target{}, err
	}
	for _, tg := range ts {
		if tg.Type == "page" {
			return tg, nil
		}
	}
	return disc.NewTarget(ctx, "about:blank")
}

// pageClient dials the first page tab with an AGENT token.
func pageClient(t *testing.T, api *harness.API, token string) (*cdpc.Client, *cdpc.Discovery) {
	t.Helper()
	ctx := timeout(t, 30*time.Second)
	disc := api.CDP(token)
	tg, err := firstPage(ctx, disc)
	if err != nil {
		t.Fatalf("find page target: %v", err)
	}
	c, err := disc.Dial(ctx, tg.WebSocketDebuggerURL)
	if err != nil {
		t.Fatalf("dial page WebSocket: %v", err)
	}
	t.Cleanup(c.Close)
	return c, disc
}

// withAgent runs fn with a CDP client for the first page under a fresh AGENT
// lease, then closes the client and releases the lease.
func withAgent(t *testing.T, api *harness.API, fn func(ctx context.Context, c *cdpc.Client, disc *cdpc.Discovery)) {
	t.Helper()
	g := acquire(t, api, modeAgent, 120)
	c, disc := pageClient(t, api, g.Token)
	fn(timeout(t, 60*time.Second), c, disc)
	c.Close()
	release(t, api, g)
}

// withHuman runs fn while a HUMAN lease is held, then releases it.
func withHuman(t *testing.T, api *harness.API, fn func(ctx context.Context, g harness.Grant)) {
	t.Helper()
	g := acquire(t, api, modeHuman, 120)
	fn(timeout(t, 60*time.Second), g)
	release(t, api, g)
}

// cdpPath is the workspace-relative CDP route, for raw requests.
func cdpPath(api *harness.API, suffix string) string { return api.WorkspacePath("/cdp" + suffix) }

// mustWorkspace reads the workspace state.
func mustWorkspace(t *testing.T, api *harness.API) apiwire.Workspace {
	t.Helper()
	w, err := api.WorkspaceState(timeout(t, 15*time.Second))
	if err != nil {
		t.Fatalf("workspace state: %v", err)
	}
	return w
}
