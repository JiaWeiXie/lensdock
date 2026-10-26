package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/hostmanager"
)

// limitsBackend reports every project stopped unless a test marks it running.
type limitsBackend struct {
	mu      sync.Mutex
	running map[string]bool
}

func (b *limitsBackend) Inspect(_ context.Context, cfg hostmanager.Config, projects []hostmanager.Project) (hostmanager.Inventory, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	inv := hostmanager.Inventory{EngineID: cfg.EngineID, Capacity: hostmanager.Resources{MemoryBytes: 64 << 30, NanoCPUs: 64e9}, Projects: map[string]hostmanager.Observation{}}
	for _, p := range projects {
		if b.running[p.ID] {
			inv.Projects[p.ID] = hostmanager.Observation{State: "running", PortOwned: true}
		} else {
			inv.Projects[p.ID] = hostmanager.Observation{State: "stopped"}
		}
	}
	return inv, nil
}

func (b *limitsBackend) Execute(context.Context, hostmanager.Config, hostmanager.Project, hostmanager.Operation, []*os.File) error {
	return nil
}

func (b *limitsBackend) VerifyConnection(context.Context, hostmanager.Config, hostmanager.Project) (hostmanager.Connection, error) {
	return hostmanager.Connection{}, errors.New("not used")
}

func (b *limitsBackend) setRunning(id string, on bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.running[id] = on
}

type limitsEnv struct {
	t       *testing.T
	root    string
	project hostmanager.Project
	backend *limitsBackend
}

// newLimitsEnv serves a real manager with one stopped project under policy.
func newLimitsEnv(t *testing.T, policy hostmanager.Policy) *limitsEnv {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.MkdirTemp("/tmp", "ldpl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	docker := filepath.Join(base, "docker")
	writeFile(t, docker, "#!/bin/sh\nexit 0\n", 0o755)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	cfg, err := hostmanager.Initialize(hostmanager.Config{
		Root: filepath.Join(base, "manager"), Executable: exe, DockerBinary: docker, EngineID: "test-engine",
		BundleDir: writeBundle(t, "1.0.0"), RuntimeImage: "runtime:test", GatewayImage: "gateway:test", PullPolicy: "missing",
		PortFirst: port, PortLast: port, Policy: policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	backend := &limitsBackend{running: map[string]bool{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = hostmanager.Serve(ctx, cfg.Root, backend)
	}()
	t.Cleanup(func() { cancel(); <-done })

	e := &limitsEnv{t: t, root: cfg.Root, backend: backend}
	dir := t.TempDir()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := hostmanager.Call(context.Background(), e.root, hostmanager.AdminTokenFile(e.root), hostmanager.Request{Action: "authorize", Path: dir})
		if err == nil && resp.Project != nil {
			e.project = *resp.Project
			return e
		}
		if time.Now().After(deadline) {
			t.Fatalf("authorize: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *limitsEnv) limits() hostmanager.Limits {
	e.t.Helper()
	resp, err := hostmanager.Call(context.Background(), e.root, hostmanager.AdminTokenFile(e.root), hostmanager.Request{Action: "status", ProjectID: e.project.ID})
	if err != nil || resp.Project == nil {
		e.t.Fatalf("status: %v", err)
	}
	return resp.Project.Limits
}

func (e *limitsEnv) run(args ...string) (hostmanager.Response, error) {
	e.t.Helper()
	var out bytes.Buffer
	c := &cli{
		stdin: strings.NewReader(""), stdout: &out, stderr: &bytes.Buffer{},
		getenv: func(k string) string {
			if k == "LENSDOCK_MANAGER_DIR" {
				return e.root
			}
			return ""
		},
	}
	err := c.projectLimits(context.Background(), append([]string{"--id", e.project.ID}, args...))
	var resp hostmanager.Response
	if err == nil {
		if jerr := json.Unmarshal(out.Bytes(), &resp); jerr != nil {
			e.t.Fatalf("project limits printed invalid JSON %q: %v", out.String(), jerr)
		}
	}
	return resp, err
}

func TestProjectLimitsOptsAnExistingProjectIntoCapsAndPreservesOmittedDimensions(t *testing.T) {
	e := newLimitsEnv(t, hostmanager.DefaultPolicy())
	if got := e.limits(); got != (hostmanager.Limits{}) {
		t.Fatalf("a default project has no caps: %+v", got)
	}

	resp, err := e.run("--runtime-memory-mib", "1024")
	if err != nil {
		t.Fatal(err)
	}
	want := hostmanager.Limits{Runtime: hostmanager.Resources{MemoryBytes: 1 << 30}}
	if resp.Project == nil || resp.Project.Limits != want || e.limits() != want {
		t.Fatalf("only the runtime memory cap may change: %+v", e.limits())
	}

	if _, err := e.run("--gateway-cpus", "0.5", "--gateway-memory-mib", "256"); err != nil {
		t.Fatal(err)
	}
	want.Gateway = hostmanager.Resources{MemoryBytes: 256 << 20, NanoCPUs: 500_000_000}
	if got := e.limits(); got != want {
		t.Fatalf("omitted runtime dimensions must be preserved: %+v want %+v", got, want)
	}

	if _, err := e.run("--runtime-cpus", "1.5"); err != nil {
		t.Fatal(err)
	}
	want.Runtime.NanoCPUs = 1_500_000_000
	if got := e.limits(); got != want {
		t.Fatalf("limits = %+v want %+v", got, want)
	}

	// An explicit zero removes exactly that cap.
	if _, err := e.run("--runtime-memory-mib", "0"); err != nil {
		t.Fatal(err)
	}
	want.Runtime.MemoryBytes = 0
	if got := e.limits(); got != want {
		t.Fatalf("zero must remove only the named cap: %+v want %+v", got, want)
	}
}

func TestPositiveCPUCapCannotBecomeUnlimited(t *testing.T) {
	e := newLimitsEnv(t, hostmanager.DefaultPolicy())
	if _, err := e.run("--runtime-cpus", "1", "--gateway-cpus", "0.5"); err != nil {
		t.Fatal(err)
	}
	want := e.limits()
	for _, name := range []string{"--runtime-cpus", "--gateway-cpus"} {
		for _, value := range []string{"0.0000000001", "0.0000000009"} {
			_, err := e.run(name, value)
			var exit *exitError
			if !errors.As(err, &exit) || exit.code != exitUsage {
				t.Errorf("%s %s must be rejected as unrepresentable: %v", name, value, err)
			}
			if got := e.limits(); got != want {
				t.Fatalf("invalid CPU input changed existing caps: %+v, want %+v", got, want)
			}
		}
	}
	if _, err := e.run("--runtime-cpus", "0"); err != nil {
		t.Fatal(err)
	}
	want.Runtime.NanoCPUs = 0
	if got := e.limits(); got != want {
		t.Fatalf("explicit zero must remove only the selected cap: %+v, want %+v", got, want)
	}
}

func TestProjectLimitsValidatesArgumentsBeforeContactingTheManager(t *testing.T) {
	e := newLimitsEnv(t, hostmanager.DefaultPolicy())
	for name, args := range map[string][]string{
		"no limit flag":       nil,
		"negative memory":     {"--runtime-memory-mib", "-1"},
		"memory too large":    {"--gateway-memory-mib", "1073741825"},
		"negative CPU":        {"--runtime-cpus", "-0.5"},
		"CPU not finite":      {"--gateway-cpus", "NaN"},
		"unexpected argument": {"--runtime-cpus", "1", "extra"},
	} {
		_, err := e.run(args...)
		var exit *exitError
		if !errors.As(err, &exit) || exit.code != exitUsage {
			t.Errorf("%s must be a usage error, got %v", name, err)
		}
	}
	var out bytes.Buffer
	c := &cli{stdin: strings.NewReader(""), stdout: &out, stderr: &bytes.Buffer{}, getenv: func(string) string { return "" }}
	var exit *exitError
	if err := c.projectLimits(context.Background(), []string{"--runtime-cpus", "1"}); !errors.As(err, &exit) || exit.code != exitUsage {
		t.Errorf("--id is required, got %v", err)
	}
	if got := e.limits(); got != (hostmanager.Limits{}) {
		t.Fatalf("refused input must change nothing: %+v", got)
	}
}

func TestProjectLimitsRefusesARunningProject(t *testing.T) {
	e := newLimitsEnv(t, hostmanager.DefaultPolicy())
	e.backend.setRunning(e.project.ID, true)
	_, err := e.run("--runtime-memory-mib", "1024")
	if err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("a running browser must never be resized: %v", err)
	}
	if got := e.limits(); got != (hostmanager.Limits{}) {
		t.Fatalf("limits changed under a running project: %+v", got)
	}

	e.backend.setRunning(e.project.ID, false)
	if _, err := e.run("--runtime-memory-mib", "1024"); err != nil {
		t.Fatalf("a stopped project may be reconfigured: %v", err)
	}
}

func TestProjectLimitsHonorsAnExplicitBudget(t *testing.T) {
	policy := hostmanager.Policy{
		Budget: hostmanager.Resources{MemoryBytes: 4 << 30},
		ProjectLimits: hostmanager.Limits{
			Runtime: hostmanager.Resources{MemoryBytes: 2 << 30},
			Gateway: hostmanager.Resources{MemoryBytes: 256 << 20},
		},
	}
	e := newLimitsEnv(t, policy)
	if _, err := e.run("--runtime-memory-mib", "0"); err == nil {
		t.Fatal("removing a budgeted cap must be refused")
	}
	if _, err := e.run("--runtime-memory-mib", "8192"); err == nil {
		t.Fatal("a cap above the budget must be refused")
	}
	if got := e.limits(); got != policy.ProjectLimits {
		t.Fatalf("refused changes must keep the authorized limits: %+v", got)
	}
	if _, err := e.run("--gateway-memory-mib", "512"); err != nil {
		t.Fatal(err)
	}
	if got := e.limits(); got.Runtime != policy.ProjectLimits.Runtime || got.Gateway.MemoryBytes != 512<<20 {
		t.Fatalf("limits = %+v", got)
	}
}
