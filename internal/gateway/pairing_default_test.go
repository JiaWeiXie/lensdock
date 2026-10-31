package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/JiaWeiXie/lensdock/internal/auth"
)

// pairingGateway serves a real Gateway on a loopback port. tweak adjusts the
// default configuration, which sets none of the pairing options.
func pairingGateway(t *testing.T, tweak func(*Config), integrations Integrations) (*isolatedGateway, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<title>test</title>"), 0600); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(dir, "runtime-token")
	if err := auth.WritePrivateFile(tokenFile, []byte("runtime-secret")); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(upstream.Close)
	edge := httptest.NewUnstartedServer(nil)
	t.Cleanup(edge.Close)
	origin := "http://" + edge.Listener.Addr().String()
	c := Config{
		Origin:     origin,
		WebDir:     dir,
		Identities: []Identity{{ID: "human-alpha", Role: "human", WorkspaceID: "alpha", TokenSHA256: digest("pair-alpha")}},
		Runtimes:   []RuntimeConfig{{WorkspaceID: "alpha", URL: upstream.URL, OperatorTokenFile: tokenFile}},
	}
	if tweak != nil {
		tweak(&c)
	}
	g, err := New(c, integrations)
	if err != nil {
		return nil, err
	}
	t.Cleanup(g.Close)
	edge.Config.Handler = g
	edge.Start()
	return &isolatedGateway{origin: origin, workspace: "alpha", dir: dir, tokenFile: tokenFile}, nil
}

// A loopback, single-workspace Gateway with no pairing option set requires a
// token. A client that sends the headers of a same-origin browser fetch gets no
// session; this is the request a local process (or a local proxy's client) can forge.
func TestDefaultGatewayRejectsForgedLocalEntry(t *testing.T) {
	g, err := pairingGateway(t, nil, Integrations{})
	if err != nil {
		t.Fatal(err)
	}
	client, _ := newJarClient(t)
	if status, _, body := g.probe(t, client, browserHeaders("")); status != 401 {
		t.Fatalf("forged local entry accepted by default: %d %s", status, body)
	}
	h := browserHeaders(g.origin)
	if status, b := g.call(t, client, "POST", "/api/v1/session", h, `{"localAccess":true}`); status != 401 {
		t.Fatalf("explicit local entry accepted by default: %d %s", status, b)
	}
	if status, b := g.call(t, client, "GET", "/api/v1/workspaces", browserHeaders(""), ""); status != 401 {
		t.Fatalf("API reachable without a session: %d %s", status, b)
	}
	// Token pairing still works.
	g.pair(t, client)
	if status, s, body := g.probe(t, client, browserHeaders("")); status != 200 || s.Actor.ID != "human-alpha" || s.LocalAccess {
		t.Fatalf("paired session: %d %s", status, body)
	}
}

func TestLocalAutoEntryIsAnExplicitOptIn(t *testing.T) {
	g, err := pairingGateway(t, func(c *Config) { c.LocalAutoEntry = true }, Integrations{})
	if err != nil {
		t.Fatal(err)
	}
	client, _ := newJarClient(t)
	status, s, body := g.probe(t, client, browserHeaders(""))
	if status != 200 || s.Actor.ID != localActorID || !s.LocalAccess {
		t.Fatalf("opted-in local entry failed: %d %s", status, body)
	}
}

func TestRequirePairingWinsOverLocalAutoEntry(t *testing.T) {
	g, err := pairingGateway(t, func(c *Config) { c.LocalAutoEntry, c.RequirePairing = true, true }, Integrations{})
	if err != nil {
		t.Fatal(err)
	}
	client, _ := newJarClient(t)
	if status, _, body := g.probe(t, client, browserHeaders("")); status != 401 {
		t.Fatalf("pairing requirement bypassed by auto-entry: %d %s", status, body)
	}
}

// Pairing is required for every Gateway that is not loopback and single-workspace:
// opting into local auto-entry there is a startup error, never silently ignored.
func TestLocalAutoEntryRefusedOutsideLoopbackSingleWorkspace(t *testing.T) {
	if _, err := pairingGateway(t, func(c *Config) {
		c.LocalAutoEntry = true
		c.Origin = "https://lensdock.example.com"
		c.TLSCertFile, c.TLSKeyFile = "cert.pem", "key.pem"
	}, Integrations{}); err == nil {
		t.Fatal("local auto-entry accepted for a remote origin")
	}
	if _, err := pairingGateway(t, func(c *Config) {
		c.LocalAutoEntry = true
		c.Runtimes = append(c.Runtimes, RuntimeConfig{WorkspaceID: "beta", URL: c.Runtimes[0].URL, OperatorTokenFile: c.Runtimes[0].OperatorTokenFile})
	}, Integrations{}); err == nil {
		t.Fatal("local auto-entry accepted for two workspaces")
	}
	// Without the opt-in the same remote or multi-workspace Gateways start and require identities.
	if _, err := pairingGateway(t, func(c *Config) {
		c.Runtimes = append(c.Runtimes, RuntimeConfig{WorkspaceID: "beta", URL: c.Runtimes[0].URL, OperatorTokenFile: c.Runtimes[0].OperatorTokenFile})
	}, Integrations{}); err != nil {
		t.Fatalf("two-workspace Gateway without auto-entry: %v", err)
	}
}

func TestLocalEntryIsAudited(t *testing.T) {
	var mu sync.Mutex
	var actions []string
	audit := func(_ context.Context, p Identity, action, _ string) error {
		mu.Lock()
		defer mu.Unlock()
		actions = append(actions, p.ID+" "+action)
		return nil
	}
	g, err := pairingGateway(t, func(c *Config) { c.LocalAutoEntry = true }, Integrations{Audit: audit})
	if err != nil {
		t.Fatal(err)
	}
	client, _ := newJarClient(t)
	g.pair(t, client)
	mu.Lock()
	paired := len(actions)
	mu.Unlock()
	if paired != 0 {
		t.Fatalf("token pairing recorded as local entry: %v", actions)
	}
	other, _ := newJarClient(t)
	if status, _, body := g.probe(t, other, browserHeaders("")); status != 200 {
		t.Fatalf("local entry: %d %s", status, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(actions) != 1 || actions[0] != localActorID+" session.local" {
		t.Fatalf("audit trail: %v", actions)
	}
}
