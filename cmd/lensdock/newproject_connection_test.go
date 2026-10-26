package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiclient"
	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/hostmanager"
)

type seenRequest struct{ method, path, query, auth string }

// fakeGateway is the Gateway of one project. It records what reached it so
// tests can assert which credential was used and, more often, that nothing was.
type fakeGateway struct {
	srv  *httptest.Server
	port int
	mu   sync.Mutex
	seen []seenRequest
}

func newFakeGateway(t *testing.T) *fakeGateway {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := &fakeGateway{port: ln.Addr().(*net.TCPAddr).Port}
	g.srv = httptest.NewUnstartedServer(http.HandlerFunc(g.serve))
	g.srv.Listener.Close()
	g.srv.Listener = ln
	g.srv.Start()
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGateway) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.seen = append(g.seen, seenRequest{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")})
	g.mu.Unlock()
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // api v1 workspaces WS ...
	switch {
	case r.Method == http.MethodPost && len(parts) == 5 && parts[2] == "workspaces" && parts[4] == "leases":
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(apiwire.LeaseGrant{
			Lease:           apiwire.Lease{LeaseID: "lse_1", WorkspaceID: parts[3], HolderID: "agent", Mode: "AGENT", Epoch: 1, RuntimeGeneration: "g", ExpiresAt: time.Now().Add(30 * time.Second)},
			CapabilityToken: "ldl_lease_secret",
		})
	case r.Method == http.MethodDelete && len(parts) == 6 && parts[4] == "leases":
		w.WriteHeader(204)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/feedback":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"nextCursor":null}`))
	default:
		apiJSONError(w, 404, apiwire.CodeNotFound)
	}
}

func (g *fakeGateway) requests() []seenRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]seenRequest(nil), g.seen...)
}

// connectBackend answers the manager's connect verification. Nothing else
// runs Docker: these tests never start a project.
type connectBackend struct {
	mu      sync.Mutex
	connect func(hostmanager.Project) (hostmanager.Connection, error)
}

func (b *connectBackend) Inspect(_ context.Context, cfg hostmanager.Config, projects []hostmanager.Project) (hostmanager.Inventory, error) {
	inv := hostmanager.Inventory{EngineID: cfg.EngineID, Capacity: hostmanager.Resources{MemoryBytes: 64 << 30, NanoCPUs: 64e9}, Projects: map[string]hostmanager.Observation{}}
	for _, p := range projects {
		inv.Projects[p.ID] = hostmanager.Observation{State: "running", PortOwned: true}
	}
	return inv, nil
}

func (b *connectBackend) Execute(context.Context, hostmanager.Config, hostmanager.Project, hostmanager.Operation, []*os.File) error {
	return nil
}

func (b *connectBackend) VerifyConnection(_ context.Context, _ hostmanager.Config, p hostmanager.Project) (hostmanager.Connection, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.connect(p)
}

func (b *connectBackend) set(f func(hostmanager.Project) (hostmanager.Connection, error)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.connect = f
}

// projectEnv is a real host manager on a private Unix socket whose only
// project is served by fakeGateway.
type projectEnv struct {
	t       *testing.T
	root    string
	env     map[string]string
	gateway *fakeGateway
	backend *connectBackend
}

func newProjectEnv(t *testing.T) *projectEnv {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.MkdirTemp("/tmp", "ldpc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	docker := filepath.Join(base, "docker")
	writeFile(t, docker, "#!/bin/sh\nexit 0\n", 0o755)
	gw := &fakeGateway{}
	gw.srv = httptest.NewUnstartedServer(http.HandlerFunc(gw.serve))
	gw.port = gw.srv.Listener.Addr().(*net.TCPAddr).Port
	gw.srv.Listener.Close()
	t.Cleanup(gw.srv.Close)

	root := filepath.Join(base, "manager")
	cfg, err := hostmanager.Initialize(hostmanager.Config{
		Root: root, Executable: exe, DockerBinary: docker, EngineID: "test-engine",
		BundleDir: writeBundle(t, "1.0.0"), RuntimeImage: "runtime:test", GatewayImage: "gateway:test", PullPolicy: "missing",
		PortFirst: gw.port, PortLast: gw.port, Policy: hostmanager.DefaultPolicy(),
	})
	if err != nil {
		t.Fatal(err)
	}
	backend := &connectBackend{}
	e := &projectEnv{t: t, root: cfg.Root, env: map[string]string{"LENSDOCK_MANAGER_DIR": cfg.Root}, gateway: gw, backend: backend}
	backend.set(func(p hostmanager.Project) (hostmanager.Connection, error) {
		return hostmanager.Connection{ProjectID: p.ID, API: gw.srv.URL, Workspace: p.Workspace, TokenFile: filepath.Join(p.StateDir, "product", "agent-token")}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = hostmanager.Serve(ctx, cfg.Root, backend)
	}()
	t.Cleanup(func() { cancel(); <-done })
	return e
}

// authorize registers dir as a project, waiting for the daemon's socket.
func (e *projectEnv) authorize(dir string) hostmanager.Project {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := hostmanager.Call(context.Background(), e.root, hostmanager.AdminTokenFile(e.root), hostmanager.Request{Action: "authorize", Path: dir})
		if err == nil && resp.Project != nil {
			if e.gateway.srv.URL == "" {
				ln, err := net.Listen("tcp", strings.TrimPrefix(resp.Project.Origin, "http://"))
				if err != nil {
					e.t.Fatal(err)
				}
				e.gateway.srv.Listener = ln
				e.gateway.srv.Start()
			}
			if err := os.MkdirAll(filepath.Join(resp.Project.StateDir, "product"), 0700); err != nil {
				e.t.Fatal(err)
			}
			writeFile(e.t, filepath.Join(resp.Project.StateDir, "product", "agent-token"), "ldl_agent_credential\n", 0o600)
			return *resp.Project
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("authorize %s: %v", dir, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *projectEnv) run(stdin string, args ...string) result {
	e.t.Helper()
	return runCLI(e.t, context.Background(), stdin, e.env, args...)
}

func (e *projectEnv) gatewayCredentialsSeen() map[string]bool {
	seen := map[string]bool{}
	for _, r := range e.gateway.requests() {
		seen[r.auth] = true
	}
	return seen
}

func TestProjectCommandsUseTheProjectsOwnTupleAndAgentCredential(t *testing.T) {
	e := newProjectEnv(t)
	dir := t.TempDir()
	p := e.authorize(dir)
	// Explicit project selection must win over stale standalone defaults.
	e.env[envAPI] = "http://127.0.0.1:1"
	e.env[envTokenFile] = filepath.Join(t.TempDir(), "missing-standalone-token")
	leasePath := filepath.Join(t.TempDir(), "lease.json")

	r := e.run("", "lease", "acquire", "--project", dir, "--mode", "agent", "--output", leasePath)
	if r.code != 0 {
		t.Fatalf("acquire exit %d: %s", r.code, r.stderr)
	}
	lf, err := apiclient.ReadLeaseFile(leasePath)
	if err != nil {
		t.Fatal(err)
	}
	wantAPI, _ := normalizeAPI(p.Origin)
	if lf.APIBaseURL != wantAPI || lf.WorkspaceID != p.Workspace {
		t.Errorf("lease file is for %s %s, want the project's %s %s", lf.APIBaseURL, lf.WorkspaceID, wantAPI, p.Workspace)
	}

	if r := e.run("", "lease", "release", "--project", dir, "--lease-file", leasePath); r.code != 0 {
		t.Fatalf("release exit %d: %s", r.code, r.stderr)
	}
	if r := e.run("", "feedback", "list", "--project", dir); r.code != 0 || strings.TrimSpace(r.stdout) == "" {
		t.Fatalf("feedback list exit %d stdout %q: %s", r.code, r.stdout, r.stderr)
	}
	if r := e.run("", "mcp", "serve", "--project", dir); r.code != 0 || r.stdout != "" {
		t.Fatalf("mcp serve exit %d stdout %q: %s", r.code, r.stdout, r.stderr)
	}

	reqs := e.gateway.requests()
	if len(reqs) != 3 {
		t.Fatalf("gateway saw %d requests, want 3: %+v", len(reqs), reqs)
	}
	if reqs[0].method != "POST" || reqs[0].path != "/api/v1/workspaces/"+p.Workspace+"/leases" || reqs[0].auth != "Bearer ldl_agent_credential" {
		t.Errorf("acquire request = %+v", reqs[0])
	}
	if reqs[1].method != "DELETE" || reqs[1].auth != "Bearer ldl_lease_secret" {
		t.Errorf("release request = %+v", reqs[1])
	}
	if reqs[2].path != "/api/v1/feedback" || !strings.Contains(reqs[2].query, p.Workspace) || reqs[2].auth != "Bearer ldl_agent_credential" {
		t.Errorf("feedback request = %+v", reqs[2])
	}
	// Neither manager capability may ever reach the Gateway.
	for _, file := range []string{p.TokenFile, hostmanager.AdminTokenFile(e.root)} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if secret := strings.TrimSpace(string(data)); e.gatewayCredentialsSeen()["Bearer "+secret] || strings.Contains(r.stdout+r.stderr, secret) {
			t.Errorf("the manager credential %s leaked", file)
		}
	}
}

func TestProjectLeaseFilesOfAnotherEndpointAreRefusedBeforeAnyRequest(t *testing.T) {
	e := newProjectEnv(t)
	dir := t.TempDir()
	p := e.authorize(dir)
	other := newFakeGateway(t)
	files := t.TempDir()
	write := func(name, api, workspace, mode string) string {
		path := filepath.Join(files, name)
		err := apiclient.WriteLeaseFile(path, apiclient.LeaseFile{
			APIBaseURL: api, WorkspaceID: workspace,
			Lease:           apiwire.Lease{LeaseID: "lse_1", WorkspaceID: workspace, HolderID: "agent", Mode: mode, Epoch: 1, RuntimeGeneration: "g", ExpiresAt: time.Now().Add(30 * time.Second)},
			CapabilityToken: "ldl_foreign_lease",
		})
		if err != nil {
			t.Fatal(err)
		}
		return path
	}
	foreign := map[string]string{
		"another project's gateway": write("api.json", other.srv.URL, p.Workspace, "AGENT"),
		"another workspace":         write("ws.json", e.gateway.srv.URL, "someone-else", "AGENT"),
	}
	humanForeign := write("human.json", other.srv.URL, p.Workspace, "HUMAN")
	for name, path := range foreign {
		for _, args := range [][]string{
			{"lease", "release", "--project", dir, "--lease-file", path},
			{"lease", "refresh", "--project", dir, "--lease-file", path, "--output", filepath.Join(files, "new-"+filepath.Base(path))},
			{"cdp", "bridge", "--project", dir, "--lease-file", path},
		} {
			if r := e.run("", args...); r.code != exitLease {
				t.Errorf("%s %v: exit %d, want %d (stderr %q)", name, args[:2], r.code, exitLease, r.stderr)
			}
		}
	}
	if r := e.run("text", "clipboard", "set", "--project", dir, "--lease-file", humanForeign); r.code != exitLease {
		t.Errorf("clipboard with a foreign lease: exit %d, want %d", r.code, exitLease)
	}
	if got := append(e.gateway.requests(), other.requests()...); len(got) != 0 {
		t.Errorf("a foreign lease file reached a gateway: %+v", got)
	}
}

func TestExplicitStartPolicyRefusesUnreachableProjectsWithoutProtocolOutput(t *testing.T) {
	e := newProjectEnv(t)
	dir := t.TempDir()
	e.authorize(dir)
	policy := hostmanager.DefaultPolicy()
	policy.RequireExplicitStart = true
	policy.RequireProjectAuthorization = true
	if _, err := hostmanager.Call(context.Background(), e.root, hostmanager.AdminTokenFile(e.root), hostmanager.Request{Action: hostmanager.ActionPolicy, Policy: &policy}); err != nil {
		t.Fatal(err)
	}
	e.backend.set(func(hostmanager.Project) (hostmanager.Connection, error) {
		return hostmanager.Connection{}, os.ErrNotExist
	})
	unauthorized := t.TempDir()
	for name, args := range map[string][]string{
		"mcp":      {"mcp", "serve", "--project", dir},
		"feedback": {"feedback", "list", "--project", dir},
		"watch":    {"workspace", "watch", "--project", dir},
		"status":   {"workspace", "status", "--project", dir},
		"unlisted": {"mcp", "serve", "--project", unauthorized},
		"lease":    {"lease", "acquire", "--project", dir, "--mode", "agent", "--output", filepath.Join(t.TempDir(), "l.json")},
		"clip":     {"clipboard", "set", "--project", dir, "--lease-file", filepath.Join(t.TempDir(), "missing.json")},
	} {
		start := time.Now()
		r := e.run("", args...)
		if r.code == 0 {
			t.Errorf("%s: succeeded although the project cannot be reached", name)
		}
		if r.stdout != "" {
			t.Errorf("%s: wrote %q to stdout", name, r.stdout)
		}
		if time.Since(start) > 15*time.Second {
			t.Errorf("%s: took %v; a stopped project must not be waited for", name, time.Since(start))
		}
	}
	if got := e.gateway.requests(); len(got) != 0 {
		t.Errorf("gateway was contacted: %+v", got)
	}
}

// The Gateway credential is only opened once the manager has described the
// project that was asked for. The unusable file below would fail loudly if it
// were read first.
func TestProjectCredentialIsNotReadBeforeTheIdentityIsVerified(t *testing.T) {
	e := newProjectEnv(t)
	dir := t.TempDir()
	e.authorize(dir)
	open := filepath.Join(t.TempDir(), "open-token")
	writeFile(t, open, "ldl_open\n", 0o644)
	for name, answer := range map[string]func(hostmanager.Project) hostmanager.Connection{
		"another project": func(p hostmanager.Project) hostmanager.Connection {
			return hostmanager.Connection{ProjectID: p.ID + "x", API: e.gateway.srv.URL, Workspace: p.Workspace, TokenFile: open}
		},
		"another workspace": func(p hostmanager.Project) hostmanager.Connection {
			return hostmanager.Connection{ProjectID: p.ID, API: e.gateway.srv.URL, Workspace: p.Workspace + "x", TokenFile: open}
		},
		"another gateway": func(p hostmanager.Project) hostmanager.Connection {
			return hostmanager.Connection{ProjectID: p.ID, API: "http://127.0.0.1:1", Workspace: p.Workspace, TokenFile: open}
		},
	} {
		e.backend.set(func(p hostmanager.Project) (hostmanager.Connection, error) { return answer(p), nil })
		r := e.run("", "lease", "acquire", "--project", dir, "--mode", "agent", "--output", filepath.Join(t.TempDir(), "l.json"))
		if r.code == 0 {
			t.Errorf("%s: accepted", name)
		}
		if strings.Contains(r.stderr, "identity token file") {
			t.Errorf("%s: the credential was read before the identity was verified: %q", name, r.stderr)
		}
	}
	if got := e.gateway.requests(); len(got) != 0 {
		t.Errorf("gateway was contacted: %+v", got)
	}
}

func TestProjectNeverAcceptsAManagerCredentialAsTheGatewayCredential(t *testing.T) {
	e := newProjectEnv(t)
	dir := t.TempDir()
	p := e.authorize(dir)
	for name, file := range map[string]string{"management": p.TokenFile, "admin": hostmanager.AdminTokenFile(e.root), "relative": "agent-token", "another project": filepath.Join(e.root, "projects", "another-project", "product", "agent-token")} {
		e.backend.set(func(p hostmanager.Project) (hostmanager.Connection, error) {
			return hostmanager.Connection{ProjectID: p.ID, API: e.gateway.srv.URL, Workspace: p.Workspace, TokenFile: file}, nil
		})
		if r := e.run("", "workspace", "status", "--project", dir); r.code == 0 {
			t.Errorf("%s credential accepted for the Gateway", name)
		}
	}
	if got := e.gateway.requests(); len(got) != 0 {
		t.Errorf("gateway was contacted: %+v", got)
	}
}

// The manual connection options and --project select the same thing twice;
// whichever won would be a surprise, so none is allowed to. These refusals
// precede any manager or Gateway contact.
func TestProjectRefusesManualConnectionOverrides(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer srv.Close()
	dir := t.TempDir()
	tok := filepath.Join(dir, "tok")
	writeFile(t, tok, "ldo_x\n", 0o600)
	leasePath := writeLease(t, dir, srv.URL, "AGENT", "ldl_agent")
	out := filepath.Join(dir, "out.json")
	for name, tc := range map[string]struct {
		env  map[string]string
		args []string
	}{
		"status --api":          {nil, []string{"workspace", "status", "--project", dir, "--api", srv.URL}},
		"status operator token": {nil, []string{"workspace", "status", "--project", dir, "--operator-token-file", tok}},
		"status positional":     {nil, []string{"workspace", "status", "default", "--project", dir}},
		"watch --token-file":    {nil, []string{"workspace", "watch", "--project", dir, "--token-file", tok}},
		"watch positional":      {nil, []string{"workspace", "watch", "default", "--project", dir}},
		"acquire --api":         {nil, []string{"lease", "acquire", "--project", dir, "--mode", "agent", "--output", out, "--api", srv.URL}},
		"acquire positional":    {nil, []string{"lease", "acquire", "default", "--project", dir, "--mode", "agent", "--output", out}},
		"acquire human":         {nil, []string{"lease", "acquire", "--project", dir, "--mode", "human", "--output", out}},
		"release positional":    {nil, []string{"lease", "release", "default", "--project", dir, "--lease-id", "l"}},
		"release --api":         {nil, []string{"lease", "release", "--project", dir, "--lease-file", leasePath, "--api", srv.URL}},
		"feedback --workspace":  {nil, []string{"feedback", "list", "--project", dir, "--workspace", "default"}},
		"feedback --api":        {nil, []string{"feedback", "get", "fb1", "--project", dir, "--revision", "1", "--api", srv.URL}},
		"mcp --workspace":       {nil, []string{"mcp", "serve", "--project", dir, "--workspace", "default"}},
		"mcp --token-file":      {nil, []string{"mcp", "serve", "--project", dir, "--token-file", tok}},
		"empty project":         {nil, []string{"mcp", "serve", "--project="}},
	} {
		env := map[string]string{"LENSDOCK_MANAGER_DIR": filepath.Join(dir, "no-manager")}
		for k, v := range tc.env {
			env[k] = v
		}
		if r := runCLI(t, context.Background(), "", env, tc.args...); r.code != exitUsage {
			t.Errorf("%s: exit %d, want %d (stderr %q)", name, r.code, exitUsage, r.stderr)
		}
	}
	if calls != 0 {
		t.Errorf("runtime was contacted %d times", calls)
	}
}

func TestWorkspaceIsStillRequiredWithoutProject(t *testing.T) {
	tok := filepath.Join(t.TempDir(), "tok")
	writeFile(t, tok, "ldo_x\n", 0o600)
	for name, args := range map[string][]string{
		"status":  {"workspace", "status", "--operator-token-file", tok},
		"watch":   {"workspace", "watch", "--token-file", tok},
		"release": {"lease", "release", "--lease-id", "l", "--operator-token-file", tok},
		"mcp":     {"mcp", "serve", "--token-file", tok},
	} {
		if r := runCLI(t, context.Background(), "", nil, args...); r.code != exitUsage {
			t.Errorf("%s: exit %d, want %d", name, r.code, exitUsage)
		}
	}
}
