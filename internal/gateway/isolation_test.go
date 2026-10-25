package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/auth"
)

// isolatedGateway is a real Gateway served on its own loopback port. Every
// instance shares the hostname 127.0.0.1 with its siblings, which is exactly
// when browsers share cookies between unrelated ports.
type isolatedGateway struct {
	origin    string
	workspace string
	dir       string
	tokenFile string
}

func startIsolatedGateway(t *testing.T, deploymentID, workspace string, demo bool) *isolatedGateway {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<title>test</title>"), 0600); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(dir, "runtime-token")
	if err := auth.WritePrivateFile(tokenFile, []byte("runtime-secret-"+workspace)); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(upstream.Close)
	edge := httptest.NewUnstartedServer(nil)
	t.Cleanup(edge.Close)
	origin := "http://" + edge.Listener.Addr().String()
	g, err := New(Config{
		Origin:       origin,
		DeploymentID: deploymentID,
		WebDir:       dir,
		DemoNoAuth:   demo,
		Identities:   []Identity{{ID: "human-" + workspace, Role: "human", WorkspaceID: workspace, TokenSHA256: digest("pair-" + workspace)}},
		Runtimes:     []RuntimeConfig{{WorkspaceID: workspace, URL: upstream.URL, OperatorTokenFile: tokenFile}},
	}, Integrations{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	edge.Config.Handler = g
	edge.Start()
	return &isolatedGateway{origin: origin, workspace: workspace, dir: dir, tokenFile: tokenFile}
}

// browserHeaders are the headers a same-origin fetch() from the served UI sends.
func browserHeaders(origin string) http.Header {
	h := http.Header{}
	h.Set("Sec-Fetch-Site", "same-origin")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Dest", "empty")
	if origin != "" {
		h.Set("Origin", origin)
	}
	return h
}

func (g *isolatedGateway) call(t *testing.T, c *http.Client, method, path string, h http.Header, body string) (int, []byte) {
	t.Helper()
	r, err := http.NewRequest(method, g.origin+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range h {
		r.Header[k] = v
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}

type sessionProbe struct {
	Actor Identity `json:"actor"`
	CSRF  string   `json:"csrfToken"`
	Demo  bool     `json:"demo"`
}

func (g *isolatedGateway) pair(t *testing.T, c *http.Client) string {
	t.Helper()
	h := http.Header{}
	h.Set("Origin", g.origin)
	status, b := g.call(t, c, "POST", "/api/v1/session", h, `{"token":"pair-`+g.workspace+`"}`)
	if status != 200 {
		t.Fatalf("%s pairing: %d %s", g.workspace, status, b)
	}
	var s sessionProbe
	if err := json.Unmarshal(b, &s); err != nil || s.CSRF == "" {
		t.Fatalf("%s pairing reply: %v %s", g.workspace, err, b)
	}
	return s.CSRF
}

// probe reads the session through the shared cookie jar.
func (g *isolatedGateway) probe(t *testing.T, c *http.Client, h http.Header) (int, sessionProbe, string) {
	t.Helper()
	status, b := g.call(t, c, "GET", "/api/v1/session", h, "")
	var s sessionProbe
	if status == 200 {
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatal(err)
		}
	}
	return status, s, string(b)
}

func (g *isolatedGateway) requireActor(t *testing.T, c *http.Client, h http.Header, wantID, wantCSRF string) {
	t.Helper()
	status, s, body := g.probe(t, c, h)
	if status != 200 || s.Actor.ID != wantID || s.Actor.WorkspaceID != g.workspace || (wantCSRF != "" && s.CSRF != wantCSRF) {
		t.Fatalf("gateway %s lost its identity: %d %s", g.workspace, status, body)
	}
}

func (g *isolatedGateway) logout(t *testing.T, c *http.Client, csrf, origin string) int {
	t.Helper()
	h := http.Header{}
	h.Set("Origin", origin)
	h.Set("X-CSRF-Token", csrf)
	status, _ := g.call(t, c, "DELETE", "/api/v1/session", h, "")
	return status
}

func newJarClient(t *testing.T) (*http.Client, http.CookieJar) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar}, jar
}

func TestSameHostGatewaysKeepIndependentSessionsInOneCookieJar(t *testing.T) {
	a := startIsolatedGateway(t, "proj-a", "alpha", false)
	b := startIsolatedGateway(t, "proj-b", "beta", false)
	client, jar := newJarClient(t)

	csrfA := a.pair(t, client)
	csrfB := b.pair(t, client)

	// Pairing B must not replace A's cookie: both identities stay usable at once.
	a.requireActor(t, client, nil, "human-alpha", csrfA)
	b.requireActor(t, client, nil, "human-beta", csrfB)
	u, _ := url.Parse(a.origin)
	names := map[string]bool{}
	for _, c := range jar.Cookies(u) {
		names[c.Name] = true
	}
	if len(names) != 2 {
		t.Fatalf("expected one distinct session cookie per Gateway, got %d", len(names))
	}

	// CSRF and Origin protection is unchanged and scoped per Gateway.
	if got := b.logout(t, client, csrfA, b.origin); got != 403 {
		t.Fatalf("another Gateway's CSRF token accepted: %d", got)
	}
	if got := b.logout(t, client, "wrong", b.origin); got != 403 {
		t.Fatalf("wrong CSRF accepted: %d", got)
	}
	if got := b.logout(t, client, csrfB, "https://attacker.example"); got != 403 {
		t.Fatalf("foreign Origin accepted: %d", got)
	}
	if got := b.logout(t, client, csrfB, a.origin); got != 403 {
		t.Fatalf("sibling Gateway origin accepted: %d", got)
	}
	a.requireActor(t, client, nil, "human-alpha", csrfA)
	b.requireActor(t, client, nil, "human-beta", csrfB)

	// Logging out of A leaves B valid.
	if got := a.logout(t, client, csrfA, a.origin); got != 204 {
		t.Fatalf("logout A: %d", got)
	}
	if status, _, _ := a.probe(t, client, nil); status != 401 {
		t.Fatalf("A still authenticated after logout: %d", status)
	}
	b.requireActor(t, client, nil, "human-beta", csrfB)
	if got := b.logout(t, client, csrfB, b.origin); got != 204 {
		t.Fatalf("logout B: %d", got)
	}
	if status, _, _ := b.probe(t, client, nil); status != 401 {
		t.Fatalf("B still authenticated after logout: %d", status)
	}
}

func TestSameHostGatewaysKeepDemoSignOutIsolated(t *testing.T) {
	a := startIsolatedGateway(t, "proj-a", "alpha", true)
	b := startIsolatedGateway(t, "proj-b", "beta", true)
	client, _ := newJarClient(t)

	_, sa, body := a.probe(t, client, browserHeaders(""))
	_, sb, body2 := b.probe(t, client, browserHeaders(""))
	if !sa.Demo || sa.Actor.WorkspaceID != "alpha" || !sb.Demo || sb.Actor.WorkspaceID != "beta" || sa.CSRF == "" || sb.CSRF == "" {
		t.Fatalf("demo sessions not established: %s / %s", body, body2)
	}
	// Both demo sessions coexist in one jar.
	a.requireActor(t, client, browserHeaders(""), demoActorID, sa.CSRF)
	b.requireActor(t, client, browserHeaders(""), demoActorID, sb.CSRF)

	if got := a.logout(t, client, sa.CSRF, a.origin); got != 204 {
		t.Fatalf("demo logout A: %d", got)
	}
	// A is signed out and stays out until explicit re-entry.
	status, _, reply := a.probe(t, client, browserHeaders(""))
	if status != 401 || !strings.Contains(reply, "DEMO_SIGNED_OUT") {
		t.Fatalf("A reload after demo logout: %d %s", status, reply)
	}
	// B keeps the very same session; A's sign-out marker does not leak to it.
	b.requireActor(t, client, browserHeaders(""), demoActorID, sb.CSRF)

	// A new sibling Gateway on the same host still auto-enters despite A's marker.
	c := startIsolatedGateway(t, "proj-c", "gamma", true)
	if status, s, reply := c.probe(t, client, browserHeaders("")); status != 200 || !s.Demo || s.Actor.WorkspaceID != "gamma" {
		t.Fatalf("fresh Gateway inherited A's demo sign-out: %d %s", status, reply)
	}

	// Explicit re-entry on A clears only A's marker.
	status, entry := a.call(t, client, "POST", "/api/v1/session", browserHeaders(a.origin), `{"demo":true}`)
	if status != 200 {
		t.Fatalf("explicit demo re-entry: %d %s", status, entry)
	}
	a.requireActor(t, client, browserHeaders(""), demoActorID, "")
	b.requireActor(t, client, browserHeaders(""), demoActorID, sb.CSRF)
}

func TestCookieNamesAreStableAndNamespacedByOriginAndDeployment(t *testing.T) {
	users := []Identity{{ID: "operator", Role: "human", WorkspaceID: "alpha", TokenSHA256: digest("pair")}}
	mk := func(origin, deploymentID string) *identities {
		t.Helper()
		a, err := newIdentities(users, origin, deploymentID)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	const origin = "http://127.0.0.1:18101"
	base := mk(origin, "proj-a")
	again := mk(origin, "proj-a")
	if base.sessionCookie != again.sessionCookie || base.demoOffCookie != again.demoOffCookie {
		t.Fatal("cookie names differ between identically configured Gateways")
	}
	if mk(origin, "").sessionCookie != mk(origin, "").sessionCookie {
		t.Fatal("origin-only cookie name is not stable")
	}

	seen := map[string]string{}
	for label, a := range map[string]*identities{
		"base":                    base,
		"same deployment, port":   mk("http://127.0.0.1:18102", "proj-a"),
		"same origin, deployment": mk(origin, "proj-b"),
		"origin only":             mk(origin, ""),
		"origin only, port":       mk("http://127.0.0.1:18102", ""),
		"same port, hostname":     mk("http://localhost:18101", "proj-a"),
	} {
		for _, name := range []string{a.sessionCookie, a.demoOffCookie} {
			if prev, dup := seen[name]; dup {
				t.Fatalf("%q and %q share cookie name %q", prev, label, name)
			}
			seen[name] = label
			for _, ch := range name {
				if !(ch == '-' || ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'z') {
					t.Fatalf("cookie name %q is not a plain token", name)
				}
			}
		}
	}

	// Issuance uses the derived name and keeps the existing cookie protection.
	login := func(a *identities, origin string) *http.Cookie {
		t.Helper()
		r := httptest.NewRequest("POST", "/api/v1/session", strings.NewReader(`{"token":"pair"}`))
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		a.serveSession(w, r)
		cs := w.Result().Cookies()
		if w.Code != 200 || len(cs) != 1 {
			t.Fatalf("login: %d, %d cookies", w.Code, len(cs))
		}
		return cs[0]
	}
	c := login(base, origin)
	if c.Name != base.sessionCookie || c.Path != "/" || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.MaxAge != int(sessionTTL/time.Second) || c.Secure {
		t.Fatalf("session cookie attributes changed: %+v", c)
	}
	if sc := login(mk("https://lensdock.example.com", "proj-a"), "https://lensdock.example.com"); !sc.Secure {
		t.Fatal("HTTPS session cookie is not Secure")
	}

	// The cookie is only honored under the derived name; neither the previous
	// fixed name nor another Gateway's name authenticates.
	for _, name := range []string{"lensdock-session", mk("http://127.0.0.1:18102", "proj-a").sessionCookie} {
		r := httptest.NewRequest("GET", "/api/v1/workspaces/alpha", nil)
		r.AddCookie(&http.Cookie{Name: name, Value: c.Value})
		if _, ok := base.authenticate(r); ok {
			t.Fatalf("session honored under cookie name %q", name)
		}
	}
	r := httptest.NewRequest("GET", "/api/v1/workspaces/alpha", nil)
	r.AddCookie(c)
	if _, ok := base.authenticate(r); !ok {
		t.Fatal("session not honored under its derived name")
	}
	// A restarted Gateway with the same configuration derives the same name but
	// holds no session, so the old cookie stays rejected rather than re-adopted.
	if _, ok := again.authenticate(r); ok {
		t.Fatal("session survived into a different Gateway instance")
	}
	if c2 := login(again, origin); c2.Name != c.Name {
		t.Fatalf("cookie name changed across constructed configs: %q vs %q", c2.Name, c.Name)
	}
}

func TestDeploymentEndpointExposesOnlyTheIdentity(t *testing.T) {
	g := startIsolatedGateway(t, "proj-a", "alpha", false)
	client := &http.Client{}

	// No credentials, cookies or Origin are needed.
	req, err := http.NewRequest("GET", g.origin+"/api/v1/deployment", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") || resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("deployment identity: %d %v", resp.StatusCode, resp.Header)
	}
	if len(resp.Cookies()) != 0 {
		t.Fatal("identity endpoint set cookies")
	}
	var fields map[string]any
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 1 || fields["deploymentId"] != "proj-a" {
		t.Fatalf("identity endpoint exposes more than the ID: %s", b)
	}
	for _, secret := range []string{g.dir, g.tokenFile, "pair-alpha", digest("pair-alpha"), "runtime-secret"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("identity endpoint leaks %q", secret)
		}
	}

	// Only exact GET is served; every other method is refused.
	for _, method := range []string{"HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
		status, _ := g.call(t, client, method, "/api/v1/deployment", nil, "")
		if status != 405 {
			t.Fatalf("%s /api/v1/deployment: %d", method, status)
		}
	}
	if status, _ := g.call(t, client, "GET", "/api/v1/deployment/", nil, ""); status == 200 {
		t.Fatal("identity served under a non-exact path")
	}

	// The configured Host is still enforced before the endpoint.
	req, _ = http.NewRequest("GET", g.origin+"/api/v1/deployment", nil)
	req.Host = "evil.example"
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 421 {
		t.Fatalf("identity served to a foreign Host: %d", resp.StatusCode)
	}

	// Standalone Gateways answer with an empty ID.
	standalone := startIsolatedGateway(t, "", "beta", false)
	status, body := standalone.call(t, client, "GET", "/api/v1/deployment", nil, "")
	fields = nil
	if err := json.Unmarshal(body, &fields); err != nil || status != 200 || len(fields) != 1 || fields["deploymentId"] != "" {
		t.Fatalf("standalone identity: %d %s", status, body)
	}
}

func TestDeploymentIDValidation(t *testing.T) {
	for _, id := range []string{"", "a", "A1", "0abc", "proj_1-x", strings.Repeat("a", 64)} {
		if err := ValidateDeploymentID(id); err != nil {
			t.Fatalf("valid ID %q rejected: %v", id, err)
		}
	}
	for _, id := range []string{strings.Repeat("a", 65), "-a", "_a", "a b", "a/b", "a.b", "a:b", "é", "a\n", "a\x00"} {
		if err := ValidateDeploymentID(id); err == nil {
			t.Fatalf("invalid ID %q accepted", id)
		}
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(dir, "token")
	if err := auth.WritePrivateFile(tokenFile, []byte("secret")); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Listen:     ":0",
		Origin:     "http://127.0.0.1:18101",
		DataDir:    dir,
		WebDir:     dir,
		Identities: []Identity{{ID: "operator", Role: "human", WorkspaceID: "alpha", TokenSHA256: digest("pair")}},
		Runtimes:   []RuntimeConfig{{WorkspaceID: "alpha", URL: "http://127.0.0.1:1", OperatorTokenFile: tokenFile}},
	}
	cfg.DeploymentID = "bad id"
	if _, err := New(cfg, Integrations{}); err == nil {
		t.Fatal("New accepted an invalid deployment ID")
	}
	load := func(c Config) (Config, error) {
		t.Helper()
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "gateway.json")
		if err := auth.WritePrivateFile(path, b); err != nil {
			t.Fatal(err)
		}
		return LoadConfig(path)
	}
	if _, err := load(cfg); err == nil {
		t.Fatal("LoadConfig accepted an invalid deployment ID")
	}
	cfg.DeploymentID = "proj-a"
	if c, err := load(cfg); err != nil || c.DeploymentID != "proj-a" {
		t.Fatalf("valid deployment ID not loaded: %q %v", c.DeploymentID, err)
	}
	managed, err := New(cfg, Integrations{})
	if err != nil {
		t.Fatal(err)
	}
	managed.Close()
	// Existing standalone configs without an ID keep working.
	cfg.DeploymentID = ""
	if c, err := load(cfg); err != nil || c.DeploymentID != "" {
		t.Fatalf("standalone config rejected: %v", err)
	}
	standalone, err := New(cfg, Integrations{})
	if err != nil {
		t.Fatal(err)
	}
	standalone.Close()
}
