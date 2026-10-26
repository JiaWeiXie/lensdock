package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/hostmanager"
	"github.com/JiaWeiXie/lensdock/internal/update"
)

type recordingRunner struct {
	mu   sync.Mutex
	cmds []update.Cmd
}

func (r *recordingRunner) Run(_ context.Context, c update.Cmd) error {
	r.mu.Lock()
	r.cmds = append(r.cmds, c)
	r.mu.Unlock()
	if c.Stdout != nil {
		_, _ = c.Stdout.Write([]byte("fixed-engine-output\n"))
	}
	return nil
}

func (r *recordingRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.cmds)
}

// fakeManager replaces the host manager for the update commands.
type fakeManager struct {
	mu        sync.Mutex
	cfg       hostmanager.Config
	cfgErr    error
	ensureErr error
	updateErr error
	project   hostmanager.Project
	noProject bool
	// statuses are the operation snapshots returned by successive polls; the
	// last one repeats.
	statuses []hostmanager.Operation
	onPoll   func()
	polls    int
	ensures  int
	requests []hostmanager.Request
	runner   *recordingRunner
}

func (f *fakeManager) LoadConfig(string) (hostmanager.Config, error) { return f.cfg, f.cfgErr }

func (f *fakeManager) EnsureRunning(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensures++
	return f.ensureErr
}

func (f *fakeManager) AdminTokenFile(root string) string { return filepath.Join(root, "admin.token") }

func (f *fakeManager) Docker(hostmanager.Config, *update.Deployment) update.Docker {
	return update.Docker{Run: f.runner, Bin: "/fixed/docker"}
}

func (f *fakeManager) Call(_ context.Context, _, _ string, req hostmanager.Request) (hostmanager.Response, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	onPoll := f.onPoll
	switch req.Action {
	case "status":
		if req.OperationID == "" {
			defer f.mu.Unlock()
			if f.noProject {
				return hostmanager.Response{}, nil
			}
			p := f.project
			return hostmanager.Response{Project: &p}, nil
		}
		f.polls++
		op := f.statuses[min(f.polls, len(f.statuses))-1]
		f.mu.Unlock()
		if onPoll != nil {
			onPoll()
		}
		return hostmanager.Response{Operation: &op}, nil
	case "update":
		defer f.mu.Unlock()
		if f.updateErr != nil {
			return hostmanager.Response{Operation: &hostmanager.Operation{ID: "op-1"}}, f.updateErr
		}
		return hostmanager.Response{Operation: &hostmanager.Operation{ID: "op-1", Kind: "update", Phase: "queued"}}, nil
	}
	f.mu.Unlock()
	return hostmanager.Response{}, errors.New("unexpected action " + req.Action)
}

func (f *fakeManager) updates() []hostmanager.UpdateAction {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []hostmanager.UpdateAction
	for _, r := range f.requests {
		if r.Action == "update" {
			out = append(out, *r.Update)
		}
	}
	return out
}

func (f *fakeManager) actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.requests {
		out = append(out, r.Action)
	}
	return out
}

type managedFixture struct {
	root, projectDir, depFile string
	fake                      *fakeManager
}

func canonicalDir(t *testing.T, dir string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// newManagedFixture writes a managed deployment at its manager-owned location
// and installs a fake manager that registers it.
func newManagedFixture(t *testing.T) *managedFixture {
	t.Helper()
	return newManagedFixtureIn(t, canonicalDir(t, t.TempDir()))
}

// newManagedFixtureIn is newManagedFixture under the manager root root. Like a
// real managed deployment its only Workspace is named after the project ID.
func newManagedFixtureIn(t *testing.T, root string) *managedFixture {
	t.Helper()
	projectDir := filepath.Join(root, "projects", "proj-1")
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	depFile := filepath.Join(projectDir, "deployment.json")
	productDir := filepath.Join(projectDir, "product")
	if err := os.Mkdir(productDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dep := update.ScaffoldM0(depFile, productDir, filepath.Join(projectDir, "compose.yaml"), filepath.Join(projectDir, "bin"))
	dep.Workspaces = map[string]update.WorkspaceConfig{"proj-1": dep.Workspaces["default"]}
	dep.ManagedBy = &update.ManagedOwner{ManagerID: "mgr-1", ProjectID: "proj-1", ManagerRoot: root}
	if err := dep.Save(); err != nil {
		t.Fatal(err)
	}
	f := &fakeManager{
		cfg:      hostmanager.Config{ID: "mgr-1", Root: root},
		project:  hostmanager.Project{ID: "proj-1", StateDir: projectDir},
		statuses: []hostmanager.Operation{{ID: "op-1", Kind: "update", Phase: "succeeded"}},
		runner:   &recordingRunner{},
	}
	old, oldInterval := manager, managedPollInterval
	manager, managedPollInterval = f, time.Millisecond
	t.Cleanup(func() { manager, managedPollInterval = old, oldInterval })
	return &managedFixture{root: root, projectDir: projectDir, depFile: depFile, fake: f}
}

func newUpdateCLI(t *testing.T, stdin string) (*cli, *bytes.Buffer, *bytes.Buffer, *recordingRunner) {
	t.Helper()
	var out, errb bytes.Buffer
	direct := &recordingRunner{}
	exe := filepath.Join(t.TempDir(), "lensdock")
	c := &cli{
		stdin: strings.NewReader(stdin), stdout: &out, stderr: &errb,
		getenv:     func(string) string { return "" },
		executable: func() (string, error) { return exe, nil },
		runner:     direct,
	}
	return c, &out, &errb, direct
}

func TestManagedConfirmationsStayInTheCLI(t *testing.T) {
	fx := newManagedFixture(t)
	c, _, _, _ := newUpdateCLI(t, "n\n")
	err := c.dispatch(context.Background(), []string{"update", "approve", "--update", "upd-1", "--deployment", fx.depFile})
	if !errors.Is(err, update.ErrNotConfirmed) {
		t.Fatalf("err = %v, want ErrNotConfirmed", err)
	}
	if len(fx.fake.requests) != 0 {
		t.Fatalf("declined confirmation reached the manager: %+v", fx.fake.requests)
	}

	c, _, _, _ = newUpdateCLI(t, "y\n")
	if err := c.dispatch(context.Background(), []string{"update", "branches", "delete", "vol", "--deployment", fx.depFile}); err != nil {
		t.Fatal(err)
	}
	if got := fx.fake.updates(); len(got) != 1 || !got[0].Confirmed {
		t.Fatalf("updates = %+v, want one confirmed deletion", got)
	}
}

func TestManagedApplyAwaitingApprovalIsReported(t *testing.T) {
	for _, op := range []hostmanager.Operation{
		{ID: "op-1", Phase: "awaiting-approval", Error: update.ErrAwaitingApproval.Error()},
		{ID: "op-1", Phase: "failed", Error: update.ErrAwaitingApproval.Error()},
	} {
		fx := newManagedFixture(t)
		fx.fake.statuses = []hostmanager.Operation{op}
		c, _, _, _ := newUpdateCLI(t, "")
		err := c.dispatch(context.Background(), []string{"update", "apply", "--plan", "p.json", "--yes", "--deployment", fx.depFile})
		if !errors.Is(err, update.ErrAwaitingApproval) {
			t.Fatalf("phase %s: err = %v, want ErrAwaitingApproval", op.Phase, err)
		}
		if got := fx.fake.updates(); len(got) != 1 || got[0].ApproveSwitch {
			t.Fatalf("updates = %+v: the switch must not be approved implicitly", got)
		}
	}
}

func TestManagedFailedOperationsAreErrors(t *testing.T) {
	for _, phase := range []string{"failed", "canceled", "blocked", "abandoned"} {
		fx := newManagedFixture(t)
		fx.fake.statuses = []hostmanager.Operation{{ID: "op-1", Phase: phase, Error: "disk full"}}
		c, out, _, _ := newUpdateCLI(t, "")
		err := c.dispatch(context.Background(), []string{"update", "checkpoints", "prune", "--deployment", fx.depFile})
		if err == nil || !strings.Contains(err.Error(), phase) || !strings.Contains(err.Error(), "disk full") {
			t.Fatalf("phase %s: err = %v", phase, err)
		}
		if out.Len() != 0 {
			t.Fatalf("phase %s reported success output %q", phase, out.String())
		}
	}
}

func TestManagedRefusedUpdateReportsOperationID(t *testing.T) {
	fx := newManagedFixture(t)
	fx.fake.updateErr = errors.New("project is blocked")
	c, _, errb, _ := newUpdateCLI(t, "")
	err := c.dispatch(context.Background(), []string{"update", "checkpoints", "prune", "--deployment", fx.depFile})
	if err == nil || !strings.Contains(err.Error(), "project is blocked") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(errb.String(), "op-1") {
		t.Fatalf("operation ID not printed: %q", errb.String())
	}
	if fx.fake.polls != 0 {
		t.Fatal("a refused update must not be polled")
	}
}

func TestManagedInterruptDetachesWithoutStopping(t *testing.T) {
	fx := newManagedFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fx.fake.statuses = []hostmanager.Operation{{ID: "op-1", Phase: "running"}}
	fx.fake.onPoll = cancel
	c, _, errb, _ := newUpdateCLI(t, "")
	err := c.dispatch(ctx, []string{"update", "checkpoints", "prune", "--deployment", fx.depFile})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if !strings.Contains(errb.String(), "op-1") {
		t.Fatalf("stderr = %q does not identify the operation that keeps running", errb.String())
	}
	for _, a := range fx.fake.actions() {
		if a != "status" && a != "update" {
			t.Fatalf("interrupt sent %q; it may only detach", a)
		}
	}
}

func TestManagedPollFailuresDetach(t *testing.T) {
	fx := newManagedFixture(t)
	fx.fake.statuses = []hostmanager.Operation{{ID: "other", Phase: "running"}}
	old := managedPollFailures
	managedPollFailures = 2
	t.Cleanup(func() { managedPollFailures = old })
	c, _, _, _ := newUpdateCLI(t, "")
	err := c.dispatch(context.Background(), []string{"update", "checkpoints", "prune", "--deployment", fx.depFile})
	if err == nil || !strings.Contains(err.Error(), "op-1") {
		t.Fatalf("err = %v", err)
	}
}

func TestManagedOwnerMetadataFailsClosed(t *testing.T) {
	type mutate func(t *testing.T, fx *managedFixture) string // returns the deployment file to use
	rewrite := func(t *testing.T, path string, edit func(*update.Deployment)) {
		t.Helper()
		dep, err := update.LoadDeployment(path)
		if err != nil {
			t.Fatal(err)
		}
		edit(dep)
		if err := dep.Save(); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]mutate{
		"manager ID differs": func(t *testing.T, fx *managedFixture) string {
			fx.fake.cfg.ID = "mgr-other"
			return fx.depFile
		},
		"manager config unreadable": func(t *testing.T, fx *managedFixture) string {
			fx.fake.cfgErr = errors.New("no such manager")
			return fx.depFile
		},
		"config belongs to another root": func(t *testing.T, fx *managedFixture) string {
			fx.fake.cfg.Root = canonicalDir(t, t.TempDir())
			return fx.depFile
		},
		"deployment lives outside the project directory": func(t *testing.T, fx *managedFixture) string {
			dir := canonicalDir(t, t.TempDir())
			file := filepath.Join(dir, "deployment.json")
			dep, err := update.LoadDeployment(fx.depFile)
			if err != nil {
				t.Fatal(err)
			}
			dep.File, dep.StateDir = file, dir
			if err := dep.Save(); err != nil {
				t.Fatal(err)
			}
			return file
		},
		"deployment for another project ID": func(t *testing.T, fx *managedFixture) string {
			rewrite(t, fx.depFile, func(d *update.Deployment) { d.ManagedBy.ProjectID = "proj-2" })
			return fx.depFile
		},
		"state directory is not the project directory": func(t *testing.T, fx *managedFixture) string {
			other := canonicalDir(t, t.TempDir())
			rewrite(t, fx.depFile, func(d *update.Deployment) { d.StateDir = other })
			return fx.depFile
		},
		"project ID escapes the projects directory": func(t *testing.T, fx *managedFixture) string {
			rewrite(t, fx.depFile, func(d *update.Deployment) { d.ManagedBy.ProjectID = ".." })
			return fx.depFile
		},
		"manager root is not canonical": func(t *testing.T, fx *managedFixture) string {
			rewrite(t, fx.depFile, func(d *update.Deployment) { d.ManagedBy.ManagerRoot = fx.root + "/../" + filepath.Base(fx.root) })
			return fx.depFile
		},
		"registry reports no project": func(t *testing.T, fx *managedFixture) string {
			fx.fake.noProject = true
			return fx.depFile
		},
		"registry project has another ID": func(t *testing.T, fx *managedFixture) string {
			fx.fake.project.ID = "proj-2"
			return fx.depFile
		},
		"registry project is revoked": func(t *testing.T, fx *managedFixture) string {
			fx.fake.project.Revoked = true
			return fx.depFile
		},
		"registry project has another state directory": func(t *testing.T, fx *managedFixture) string {
			fx.fake.project.StateDir = canonicalDir(t, t.TempDir())
			return fx.depFile
		},
		"manager cannot start": func(t *testing.T, fx *managedFixture) string {
			fx.fake.ensureErr = errors.New("executable changed")
			return fx.depFile
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			fx := newManagedFixture(t)
			depFile := mutate(t, fx)
			c, _, _, direct := newUpdateCLI(t, "")
			err := c.dispatch(context.Background(), []string{"update", "approve", "--update", "upd-1", "--yes", "--deployment", depFile})
			if err == nil {
				t.Fatal("an invalid or unavailable owner must fail the command")
			}
			if len(fx.fake.updates()) != 0 {
				t.Fatal("the update reached the manager")
			}
			if direct.count() != 0 || fx.fake.runner.count() != 0 {
				t.Fatal("Docker ran although ownership was not established")
			}
		})
	}
}

func TestManagedReadOnlyCommandsNeedOwnerIdentity(t *testing.T) {
	fx := newManagedFixture(t)
	fx.fake.cfg.ID = "mgr-other"
	c, _, _, direct := newUpdateCLI(t, "")
	for _, args := range [][]string{
		{"update", "status", "--deployment", fx.depFile},
		{"update", "check", "--deployment", fx.depFile},
		{"update", "plan", "--release", "r1", "--deployment", fx.depFile},
		{"update", "checkpoints", "list", "--deployment", fx.depFile},
		{"update", "branches", "list", "--deployment", fx.depFile},
		{"update", "compose", "--deployment", fx.depFile, "--", "ps"},
	} {
		if err := c.dispatch(context.Background(), args); err == nil {
			t.Fatalf("%v succeeded with a wrong owner identity", args)
		}
	}
	if direct.count() != 0 || fx.fake.runner.count() != 0 {
		t.Fatal("Docker ran although the owner identity did not match")
	}
}

func TestManagedComposeRefusesMutatingCommands(t *testing.T) {
	for cmd, hint := range map[string]string{
		"up": "project up", "start": "project up", "restart": "project up",
		"down": "project stop", "stop": "project stop",
	} {
		fx := newManagedFixture(t)
		c, _, _, direct := newUpdateCLI(t, "")
		err := c.dispatch(context.Background(), []string{"update", "compose", "--deployment", fx.depFile, "--", cmd, "--force-recreate"})
		if err == nil || !strings.Contains(err.Error(), hint) {
			t.Fatalf("compose %s: err = %v, want guidance containing %q", cmd, err, hint)
		}
		if direct.count() != 0 || fx.fake.runner.count() != 0 {
			t.Fatalf("compose %s executed a command", cmd)
		}
		if fx.fake.ensures != 0 || len(fx.fake.requests) != 0 {
			t.Fatalf("compose %s contacted the manager", cmd)
		}
	}
	fx := newManagedFixture(t)
	c, _, _, direct := newUpdateCLI(t, "")
	if err := c.dispatch(context.Background(), []string{"update", "compose", "--deployment", fx.depFile, "--", "exec", "runtime", "sh"}); err == nil {
		t.Fatal("exec must be refused")
	}
	if direct.count() != 0 || fx.fake.runner.count() != 0 {
		t.Fatal("a refused compose command executed")
	}
}

func TestManagedComposeReadOnlyRunsOnTheFixedEngine(t *testing.T) {
	for _, cmd := range []string{"ps", "logs", "config"} {
		fx := newManagedFixture(t)
		c, out, _, direct := newUpdateCLI(t, "stdin-data")
		if err := c.dispatch(context.Background(), []string{"update", "compose", "--deployment", fx.depFile, "--", cmd}); err != nil {
			t.Fatalf("compose %s: %v", cmd, err)
		}
		if direct.count() != 0 {
			t.Fatalf("compose %s used the caller's runner instead of the fixed engine", cmd)
		}
		if fx.fake.runner.count() != 1 {
			t.Fatalf("compose %s ran %d commands, want 1", cmd, fx.fake.runner.count())
		}
		got := fx.fake.runner.cmds[0]
		if got.Name != "/fixed/docker" || got.Stdin == nil || got.Stdout == nil || got.Stderr == nil {
			t.Fatalf("compose %s command = %+v, want the fixed binary with stdio passed through", cmd, got)
		}
		if !strings.Contains(strings.Join(got.Args, " "), cmd) {
			t.Fatalf("args = %q", got.Args)
		}
		if !strings.Contains(out.String(), "fixed-engine-output") {
			t.Fatalf("stdout = %q", out.String())
		}
	}
}

func TestManagedInitIsRefused(t *testing.T) {
	fx := newManagedFixture(t)
	c, _, _, direct := newUpdateCLI(t, "")
	err := c.dispatch(context.Background(), []string{"update", "init", "--deployment", fx.depFile, "--state-dir", fx.projectDir})
	if err == nil || !strings.Contains(err.Error(), "host manager") {
		t.Fatalf("err = %v", err)
	}
	dep, lerr := update.LoadDeployment(fx.depFile)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if _, err := os.Stat(dep.BackupKeyFile); !os.IsNotExist(err) {
		t.Fatalf("update init created a backup key of a managed deployment: %v", err)
	}
	if direct.count() != 0 || len(fx.fake.requests) != 0 {
		t.Fatal("update init must not run Docker or call the manager")
	}
}

func TestManagedStatusPublishIsRefused(t *testing.T) {
	fx := newManagedFixture(t)
	c, _, _, _ := newUpdateCLI(t, "")
	err := c.dispatch(context.Background(), []string{"update", "status", "--publish", "--deployment", fx.depFile})
	if err == nil || !strings.Contains(err.Error(), "manager") {
		t.Fatalf("err = %v", err)
	}
}

func TestStandaloneUpdateFlowsIgnoreTheManager(t *testing.T) {
	fake := &fakeManager{runner: &recordingRunner{}}
	old := manager
	manager = fake
	t.Cleanup(func() { manager = old })

	dir := canonicalDir(t, t.TempDir())
	depFile := filepath.Join(dir, "deployment.json")
	c, out, _, direct := newUpdateCLI(t, "")
	if err := c.dispatch(context.Background(), []string{"update", "init", "--deployment", depFile, "--state-dir", dir}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if !strings.Contains(out.String(), "created backup key") {
		t.Fatalf("init output = %q", out.String())
	}
	dep, err := update.LoadDeployment(depFile)
	if err != nil {
		t.Fatal(err)
	}
	if dep.ManagedBy != nil {
		t.Fatal("a standalone init must not record an owner")
	}

	out.Reset()
	if err := c.dispatch(context.Background(), []string{"update", "checkpoints", "prune", "--deployment", depFile}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if !strings.Contains(out.String(), `"removed"`) {
		t.Fatalf("prune output = %q", out.String())
	}

	c, _, _, _ = newUpdateCLI(t, "n\n")
	err = c.dispatch(context.Background(), []string{"update", "branches", "delete", "vol", "--deployment", depFile})
	if !errors.Is(err, update.ErrNotConfirmed) {
		t.Fatalf("delete err = %v, want ErrNotConfirmed", err)
	}
	if len(fake.requests) != 0 || fake.ensures != 0 {
		t.Fatalf("a standalone deployment contacted the manager: %+v", fake.requests)
	}
	if fake.runner.count() != 0 || direct.count() != 0 {
		t.Fatal("unexpected Docker command")
	}
}

// standaloneDeployment writes a valid standalone deployment with the M0
// "default" workspace and returns its file.
func standaloneDeployment(t *testing.T) string {
	t.Helper()
	dir := canonicalDir(t, t.TempDir())
	file := filepath.Join(dir, "deployment.json")
	dep := update.ScaffoldM0(file, dir, filepath.Join(dir, "compose.yaml"), filepath.Join(dir, "bin"))
	if err := dep.Save(); err != nil {
		t.Fatal(err)
	}
	return file
}

// A managed deployment has one workspace, named after its project. check and
// plan use it unless the caller names a workspace; a standalone deployment
// keeps the "default" workspace. A workspace the deployment does not define is
// refused before Docker is contacted; one it defines proceeds to Docker.
func TestManagedPlanningDefaultsToTheProjectWorkspace(t *testing.T) {
	cases := []struct {
		name          string
		managed       bool
		args          []string
		reachesDocker bool
	}{
		{"managed without --workspace", true, nil, true},
		{"managed explicit project workspace", true, []string{"--workspace", "proj-1"}, true},
		{"managed explicit default workspace", true, []string{"--workspace", "default"}, false},
		{"managed explicit empty workspace", true, []string{"--workspace", ""}, false},
		{"standalone without --workspace", false, nil, true},
		{"standalone explicit unknown workspace", false, []string{"--workspace", "proj-1"}, false},
	}
	for _, sub := range [][]string{{"check"}, {"plan", "--release", "r1"}} {
		for _, tc := range cases {
			t.Run(sub[0]+"/"+tc.name, func(t *testing.T) {
				c, _, _, direct := newUpdateCLI(t, "")
				var depFile string
				docker := direct
				var fake *fakeManager
				if tc.managed {
					fx := newManagedFixture(t)
					depFile, docker, fake = fx.depFile, fx.fake.runner, fx.fake
				} else {
					fake = &fakeManager{runner: &recordingRunner{}}
					old := manager
					manager = fake
					t.Cleanup(func() { manager = old })
					depFile = standaloneDeployment(t)
				}
				args := append([]string{"update"}, sub...)
				args = append(args, tc.args...)
				args = append(args, "--deployment", depFile)
				err := c.dispatch(context.Background(), args)
				if reached := docker.count() > 0; reached != tc.reachesDocker {
					t.Fatalf("Docker reached = %v, want %v (err %v)", reached, tc.reachesDocker, err)
				}
				if !tc.reachesDocker && err == nil {
					t.Fatal("an undefined workspace must be refused")
				}
				if !tc.managed && len(fake.requests) != 0 {
					t.Fatalf("a standalone deployment contacted the manager: %+v", fake.requests)
				}
			})
		}
	}
}

// hostileDirName holds a quote, expansions, command substitutions, command
// separators, glob characters and a backslash: everything a shell reads
// specially. Each substitution or separator would create "pwned" in the
// directory the guidance runs in.
const hostileDirName = "it's $(touch pwned) `touch pwned` ; touch pwned & \"q\" $HOME *?[x] #~ \\ !! (d)"

// guidanceCommands returns the commands the output tells the user to run. A
// command runs to the end of its line.
func guidanceCommands(output string) []string {
	var cmds []string
	for _, line := range strings.Split(output, "\n") {
		if i := strings.Index(line, "lensdock update "); i >= 0 {
			cmds = append(cmds, line[i:])
		}
	}
	return cmds
}

// shellArgv runs command in a POSIX shell in workDir, where "lensdock" is a
// stub that records its arguments, and returns those arguments.
func shellArgv(t *testing.T, workDir, command string) []string {
	t.Helper()
	bin, out := t.TempDir(), filepath.Join(t.TempDir(), "argv")
	stub := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$STUB_ARGV\"\n"
	if err := os.WriteFile(filepath.Join(bin, "lensdock"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Dir = workDir
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "STUB_ARGV=" + out}
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("guidance %q is not a runnable command: %v: %s", command, err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// followGuidance runs the CLI with argv, as the shell passed it, in an
// environment whose default deployment is the standalone decoy, and requires
// that it operated on the managed fixture and not on the decoy.
func followGuidance(t *testing.T, fx *managedFixture, decoy string, argv []string) {
	t.Helper()
	fx.fake.mu.Lock()
	fx.fake.requests, fx.fake.polls, fx.fake.onPoll = nil, 0, nil
	fx.fake.statuses = []hostmanager.Operation{{ID: "op-1", Kind: "update", Phase: "succeeded"}}
	fx.fake.mu.Unlock()
	managedBefore := fx.fake.runner.count()
	c, _, _, direct := newUpdateCLI(t, "y\n")
	c.getenv = func(k string) string {
		if k == envDeployment {
			return decoy
		}
		return ""
	}
	err := c.dispatch(context.Background(), argv)
	if direct.count() != 0 {
		t.Fatalf("%v operated on the standalone deployment named by the environment", argv)
	}
	switch argv[1] {
	case "approve", "abort":
		got := fx.fake.updates()
		if err != nil || len(got) != 1 || got[0].Kind != argv[1] {
			t.Fatalf("%v: err %v, manager updates %+v; want one %s of the managed project", argv, err, got, argv[1])
		}
	case "status":
		if fx.fake.runner.count() == managedBefore {
			t.Fatalf("%v did not reach the managed deployment (err %v)", argv, err)
		}
	}
}

// Whatever a managed command tells the user to run next must act on the same
// deployment file, however the path is spelled, and must never execute part of
// the path.
func TestManagedRecoveryGuidanceKeepsTheDeployment(t *testing.T) {
	awaiting := func(t *testing.T, fx *managedFixture, c *cli) {
		fx.fake.statuses = []hostmanager.Operation{{ID: "op-1", Phase: "awaiting-approval", Error: update.ErrAwaitingApproval.Error()}}
		err := c.dispatch(context.Background(), []string{"update", "apply", "--plan", "p.json", "--yes", "--deployment", fx.depFile})
		if !errors.Is(err, update.ErrAwaitingApproval) {
			t.Fatalf("err = %v, want ErrAwaitingApproval", err)
		}
	}
	detached := func(t *testing.T, fx *managedFixture, c *cli) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		fx.fake.statuses = []hostmanager.Operation{{ID: "op-1", Phase: "running"}}
		fx.fake.onPoll = cancel
		err := c.dispatch(ctx, []string{"update", "checkpoints", "prune", "--deployment", fx.depFile})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	}
	scenarios := []struct {
		name    string
		trigger func(*testing.T, *managedFixture, *cli)
		verbs   []string
	}{
		{"awaiting approval", awaiting, []string{"abort", "approve"}},
		{"detached wait", detached, []string{"status"}},
	}
	for _, sc := range scenarios {
		for _, dir := range []struct{ name, elem string }{{"plain", "managed"}, {"hostile", hostileDirName}} {
			t.Run(sc.name+"/"+dir.name, func(t *testing.T) {
				root := filepath.Join(t.TempDir(), dir.elem)
				if err := os.MkdirAll(root, 0o700); err != nil {
					t.Fatal(err)
				}
				fx := newManagedFixtureIn(t, canonicalDir(t, root))
				decoy := standaloneDeployment(t)
				c, _, errb, _ := newUpdateCLI(t, "")
				sc.trigger(t, fx, c)

				work := t.TempDir()
				var verbs []string
				for _, command := range guidanceCommands(errb.String()) {
					argv := shellArgv(t, work, command)
					if len(argv) < 2 || argv[0] != "update" {
						t.Fatalf("guidance %q runs lensdock with %q", command, argv)
					}
					verbs = append(verbs, argv[1])
					followGuidance(t, fx, decoy, argv)
				}
				slices.Sort(verbs)
				if !slices.Equal(verbs, sc.verbs) {
					t.Fatalf("guided operations = %v, want %v (stderr %q)", verbs, sc.verbs, errb.String())
				}
				if entries, _ := os.ReadDir(work); len(entries) != 0 {
					t.Fatalf("running the guidance executed part of the deployment path: %v", entries)
				}
			})
		}
	}
}
