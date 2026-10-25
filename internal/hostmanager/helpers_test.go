package hostmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestMain doubles as the manager executable: the daemon and workers the code
// under test launches are this very test binary.
func TestMain(m *testing.M) {
	snapshotBundle = func(src, dest string) error {
		if err := os.MkdirAll(dest, 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dest, "VERSION"), []byte("test\n"), 0o600)
	}
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "__manager":
			os.Exit(managerMain(os.Args[2:]))
		case "__child":
			time.Sleep(time.Hour)
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

func managerMain(args []string) int {
	if len(args) == 0 {
		return 2
	}
	var root, op string
	for i := 1; i+1 < len(args); i += 2 {
		switch args[i] {
		case "--root":
			root = args[i+1]
		case "--operation":
			op = args[i+1]
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	var err error
	switch args[0] {
	case "serve":
		err = Serve(ctx, root, subprocessBackend{})
	case "worker":
		err = RunWorker(ctx, root, op, subprocessBackend{})
	default:
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// subprocessBackend serves daemon and worker subprocesses of tests. Its
// Execute mode comes from LDTEST_MODE: "child" starts a long-lived child that
// inherits the lock descriptors and then blocks forever.
type subprocessBackend struct{}

func (subprocessBackend) Inspect(_ context.Context, cfg Config, projects []Project) (Inventory, error) {
	inv := Inventory{
		EngineID: cfg.EngineID,
		Capacity: Resources{MemoryBytes: 64 * gib, NanoCPUs: 64_000_000_000},
		Projects: map[string]Observation{},
	}
	for _, p := range projects {
		inv.Projects[p.ID] = Observation{State: StateStopped}
	}
	return inv, nil
}

func (subprocessBackend) Execute(_ context.Context, cfg Config, p Project, op Operation, locks []*os.File) error {
	if os.Getenv("LDTEST_MODE") != "child" {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "__child")
	cmd.ExtraFiles = locks
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(cfg.Root, "child.pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		return err
	}
	select {} // the test kills this worker
}

func (subprocessBackend) VerifyConnection(context.Context, Config, Project) (Connection, error) {
	return Connection{}, errors.New("not used")
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeBackend is a deterministic Docker stand-in.
type fakeBackend struct {
	mu         sync.Mutex
	engine     string
	capacity   Resources
	external   Resources
	unbounded  []string
	states     map[string]Observation
	inspectErr error
	executed   []string // "<kind>:<operationID>"
	execFn     func(ctx context.Context, cfg Config, p Project, op Operation, locks []*os.File) error
}

func (b *fakeBackend) Inspect(_ context.Context, _ Config, projects []Project) (Inventory, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inspectErr != nil {
		return Inventory{}, b.inspectErr
	}
	inv := Inventory{
		EngineID: b.engine, Capacity: b.capacity, External: b.external,
		Unbounded: append([]string(nil), b.unbounded...), Projects: map[string]Observation{},
	}
	for _, p := range projects {
		obs, ok := b.states[p.ID]
		if !ok {
			obs = Observation{State: StateStopped}
		}
		inv.Projects[p.ID] = obs
	}
	return inv, nil
}

func (b *fakeBackend) Execute(ctx context.Context, cfg Config, p Project, op Operation, locks []*os.File) error {
	b.mu.Lock()
	b.executed = append(b.executed, op.Kind+":"+op.ID)
	fn := b.execFn
	b.mu.Unlock()
	if fn != nil {
		return fn(ctx, cfg, p, op, locks)
	}
	b.applyDefault(p, op)
	return nil
}

// applyDefault models a successful start or stop.
func (b *fakeBackend) applyDefault(p Project, op Operation) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch op.Kind {
	case KindStart:
		b.states[p.ID] = Observation{State: StateRunning, PortOwned: true, Enforced: pairOf(p.Limits)}
	case KindStop:
		b.states[p.ID] = Observation{State: StateStopped}
	}
}

func (b *fakeBackend) VerifyConnection(_ context.Context, _ Config, p Project) (Connection, error) {
	return Connection{ProjectID: p.ID, API: p.Origin, Workspace: p.Workspace, TokenFile: filepath.Join(p.StateDir, "gateway.token")}, nil
}

func (b *fakeBackend) set(id string, obs Observation) {
	b.mu.Lock()
	b.states[id] = obs
	b.mu.Unlock()
}

func (b *fakeBackend) executions() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.executed...)
}

// testPair is one project pair under the test policy.
var testPair = pairOf(DefaultPolicy().ProjectLimits)

// testPolicy admits exactly `pairs` project pairs.
func testPolicy(pairs int64) Policy {
	p := DefaultPolicy()
	p.Budget = Resources{MemoryBytes: testPair.MemoryBytes * pairs, NanoCPUs: testPair.NanoCPUs * pairs}
	return p
}

type testEnv struct {
	t       *testing.T
	cfg     Config
	backend *fakeBackend
	clock   *fakeClock
	srv     *server

	mu       sync.Mutex
	launched []string
	// runInline executes workers synchronously inside launch.
	runInline bool
}

// shortRoot returns a canonical short directory: Unix socket paths are limited.
func shortRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ldm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "m")
}

func initTestManager(t *testing.T, policy Policy) Config {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Initialize(Config{
		Root:       shortRoot(t),
		Executable: exe, DockerBinary: "/bin/sh", EngineID: "engine-1",
		BundleDir:    t.TempDir(),
		RuntimeImage: "runtime:test", GatewayImage: "gateway:test",
		Policy: policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func newEnv(t *testing.T, policy Policy) *testEnv {
	t.Helper()
	cfg := initTestManager(t, policy)
	env := &testEnv{
		t: t, cfg: cfg,
		clock: &fakeClock{t: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)},
		backend: &fakeBackend{
			engine:   cfg.EngineID,
			capacity: Resources{MemoryBytes: 64 * gib, NanoCPUs: 64_000_000_000},
			states:   map[string]Observation{},
		},
		runInline: true,
	}
	env.srv = env.startServer()
	return env
}

// startServer builds a daemon over the root, as a restart would.
func (e *testEnv) startServer() *server {
	opts := defaultOptions()
	opts.interval = 0
	opts.inventoryMaxAge = 24 * time.Hour
	opts.now = e.clock.Now
	opts.launch = func(ctx context.Context, cfg Config, id string) error {
		e.mu.Lock()
		e.launched = append(e.launched, id)
		e.mu.Unlock()
		if e.runInline {
			return RunWorker(ctx, cfg.Root, id, e.backend)
		}
		return nil
	}
	srv, err := newServer(e.cfg.Root, e.backend, opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		e.t.Fatal(err)
	}
	return srv
}

func (e *testEnv) launchedOps() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.launched...)
}

var admin = principal{admin: true}

func (e *testEnv) call(p principal, req Request) (Response, error) {
	return e.srv.dispatch(context.Background(), p, req)
}

func (e *testEnv) mustCall(p principal, req Request) Response {
	e.t.Helper()
	resp, err := e.call(p, req)
	if err != nil {
		e.t.Fatalf("%s: %v", req.Action, err)
	}
	return resp
}

func (e *testEnv) authorize() Project {
	e.t.Helper()
	dir := e.t.TempDir()
	return e.authorizeDir(dir)
}

func (e *testEnv) authorizeDir(dir string) Project {
	e.t.Helper()
	resp := e.mustCall(admin, Request{Action: ActionAuthorize, Path: dir})
	return *resp.Project
}

func (e *testEnv) ensure(p Project) Response {
	e.t.Helper()
	return e.mustCall(principal{projectID: p.ID}, Request{Action: ActionEnsure, Deadline: e.clock.Now().Add(10 * time.Minute)})
}

func (e *testEnv) status(p Project) Project {
	e.t.Helper()
	return *e.mustCall(admin, Request{Action: ActionStatus, ProjectID: p.ID}).Project
}

func (e *testEnv) pass() { e.srv.pass(context.Background()) }

func (e *testEnv) op(id string) Operation {
	e.t.Helper()
	e.srv.mu.Lock()
	defer e.srv.mu.Unlock()
	op := e.srv.reg.Ops[id]
	if op == nil {
		e.t.Fatalf("operation %s is not registered", id)
	}
	return op.Operation
}

func (e *testEnv) stopProject(p Project) Response {
	e.t.Helper()
	return e.mustCall(principal{projectID: p.ID}, Request{Action: ActionStop, DiscardPageState: true})
}

// statusCode extracts the HTTP status of a refusal.
func statusCode(err error) int {
	var re *rpcError
	if errors.As(err, &re) {
		return re.status
	}
	return 0
}

func wantStatus(t *testing.T, err error, status int) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected status %d, got success", status)
	}
	if got := statusCode(err); got != status {
		t.Fatalf("expected status %d, got %d: %v", status, got, err)
	}
}

// httpDo sends a request through the real handler with a bearer credential.
func httpDo(t *testing.T, s *server, token string, req Request) (int, Response) {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/request", strings.NewReader(string(body)))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	var out Response
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func readTokenFile(t *testing.T, path string) string {
	t.Helper()
	token, err := readToken(path)
	if err != nil {
		t.Fatal(err)
	}
	return token
}
