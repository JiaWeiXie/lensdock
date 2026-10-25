package hostmanager

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitializeRefusesOverwriteAndKeepsCredentials(t *testing.T) {
	cfg := initTestManager(t, DefaultPolicy())
	before, err := os.ReadFile(AdminTokenFile(cfg.Root))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Initialize(Config{
		Root: cfg.Root, Executable: cfg.Executable, DockerBinary: "/bin/sh", EngineID: "other",
		BundleDir: t.TempDir(), RuntimeImage: "r", GatewayImage: "g",
	})
	if err == nil || !strings.Contains(err.Error(), "already initialized") {
		t.Fatalf("second initialization must be refused, got %v", err)
	}
	after, err := os.ReadFile(AdminTokenFile(cfg.Root))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("the admin credential changed")
	}
	loaded, err := LoadConfig(cfg.Root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != cfg.ID || loaded.EngineID != "engine-1" {
		t.Fatalf("configuration was overwritten: %+v", loaded)
	}
	if cfg.BundleDir != filepath.Join(cfg.Root, "bundle") {
		t.Fatalf("bundle must be snapshotted into the manager root, got %s", cfg.BundleDir)
	}
	if _, err := os.Stat(filepath.Join(cfg.BundleDir, "VERSION")); err != nil {
		t.Fatalf("bundle snapshot is missing: %v", err)
	}
}

func TestInitializedManagerSurvivesRemovalOfSelectedExecutable(t *testing.T) {
	source := filepath.Join(t.TempDir(), "lensdock")
	if err := os.WriteFile(source, []byte("#!/bin/sh\nprintf 'selected-manager\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg, err := Initialize(Config{Root: shortRoot(t), Executable: source, DockerBinary: "/bin/sh", EngineID: "engine-1", BundleDir: t.TempDir(), RuntimeImage: "runtime:test", GatewayImage: "gateway:test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(cfg.Root)
	if err != nil {
		t.Fatalf("removing the installation invalidated the manager: %v", err)
	}
	out, err := exec.Command(loaded.Executable).Output()
	if err != nil || string(out) != "selected-manager\n" {
		t.Fatalf("selected manager no longer runs: %q %v", out, err)
	}
}

func TestInitializeValidation(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	base := func() Config {
		return Config{
			Root: shortRoot(t), Executable: exe, DockerBinary: "/bin/sh", EngineID: "e",
			BundleDir: t.TempDir(), RuntimeImage: "r", GatewayImage: "g",
		}
	}
	cases := map[string]func(*Config){
		"port range containing the standalone port": func(c *Config) { c.PortFirst, c.PortLast = 18000, 19000 },
		"relative executable":                       func(c *Config) { c.Executable = "lensdock" },
		"missing engine identity":                   func(c *Config) { c.EngineID = "" },
		"unknown pull policy":                       func(c *Config) { c.PullPolicy = "sometimes" },
		"budget below one pair": func(c *Config) {
			c.Policy = DefaultPolicy()
			c.Policy.Budget = Resources{MemoryBytes: gib, NanoCPUs: 1_000_000_000}
		},
		"allowance without explicit flag": func(c *Config) {
			c.Policy = DefaultPolicy()
			c.Policy.UnboundedExternalAllowance = Resources{MemoryBytes: gib, NanoCPUs: 1_000_000_000}
		},
		"root too long for a socket": func(c *Config) { c.Root += strings.Repeat("a", 100) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := base()
			mutate(&cfg)
			if _, err := Initialize(cfg); err == nil {
				t.Fatal("expected an error")
			}
			if _, err := os.Stat(filepath.Join(cfg.Root, configName)); err == nil {
				t.Fatal("a failed initialization must not write the configuration")
			}
		})
	}
}

func TestGrantIdentityAliasesOverlapAndWorktrees(t *testing.T) {
	env := newEnv(t, testPolicy(2))
	dir := t.TempDir()
	a := env.authorizeDir(dir)
	if !projectPattern.MatchString(a.ID) || a.ComposeProject != "ld-"+a.ID || a.Workspace != a.ID {
		t.Fatalf("unexpected identity %+v", a)
	}
	if a.StateDir != filepath.Join(env.cfg.Root, "projects", a.ID) || !strings.HasPrefix(a.Origin, "http://127.0.0.1:") {
		t.Fatalf("unexpected paths %+v", a)
	}
	if a.Port < env.cfg.PortFirst || a.Port > env.cfg.PortLast {
		t.Fatalf("port %d is outside the managed range", a.Port)
	}
	if fi, err := os.Stat(a.TokenFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("project credential must be a private file: %v", err)
	}

	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if b := env.authorizeDir(link); b.ID != a.ID {
		t.Fatalf("a symlink alias must converge on project %s, got %s", a.ID, b.ID)
	}

	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := env.call(admin, Request{Action: ActionAuthorize, Path: sub})
	wantStatus(t, err, http.StatusConflict)
	_, err = env.call(admin, Request{Action: ActionAuthorize, Path: filepath.Dir(dir)})
	wantStatus(t, err, http.StatusConflict)

	sibling := env.authorize()
	if sibling.ID == a.ID || sibling.Port == a.Port || sibling.TokenFile == a.TokenFile {
		t.Fatalf("worktrees must be independent projects: %+v %+v", a, sibling)
	}
	if string(mustRead(t, a.TokenFile)) == string(mustRead(t, sibling.TokenFile)) {
		t.Fatal("project credentials must differ")
	}

	got, err := ResolveProject(env.cfg.Root, sub)
	if err != nil || got.ID != a.ID {
		t.Fatalf("a subdirectory must resolve to its project: %v %+v", err, got)
	}
	if got, err := ResolveProject(env.cfg.Root, link); err != nil || got.ID != a.ID {
		t.Fatalf("an alias must resolve to its project: %v %+v", err, got)
	}
	if _, err := ResolveProject(env.cfg.Root, t.TempDir()); err == nil {
		t.Fatal("an unauthorized directory must not resolve")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestReplacedAndMissingDirectoriesRefuseNewWorkUntilRelocated(t *testing.T) {
	env := newEnv(t, testPolicy(2))
	dir := filepath.Join(t.TempDir(), "proj")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := env.authorizeDir(dir)
	agent := principal{projectID: p.ID}

	if err := os.Rename(dir, dir+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := env.call(agent, Request{Action: ActionEnsure})
	wantStatus(t, err, http.StatusConflict)
	if !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("unexpected error %v", err)
	}
	if _, err := env.call(agent, Request{Action: ActionConnect}); statusCode(err) != http.StatusConflict {
		t.Fatalf("connect must refuse a replaced directory, got %v", err)
	}
	if _, err := ResolveProject(env.cfg.Root, dir); err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("resolve must refuse a replaced directory, got %v", err)
	}
	_, err = env.call(admin, Request{Action: ActionAuthorize, Path: dir})
	wantStatus(t, err, http.StatusConflict)

	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	_, err = env.call(agent, Request{Action: ActionEnsure})
	wantStatus(t, err, http.StatusConflict)
	if _, err := ResolveProject(env.cfg.Root, filepath.Join(dir, "nested")); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("a path below a missing directory must report it missing, got %v", err)
	}
	// The human can still inspect and stop by ID.
	if got := env.status(p); got.ID != p.ID || got.StateDir != p.StateDir {
		t.Fatalf("status by ID must keep working: %+v", got)
	}
	env.mustCall(admin, Request{Action: ActionStop, ProjectID: p.ID, DiscardPageState: true})

	moved := t.TempDir()
	resp := env.mustCall(admin, Request{Action: ActionRelocate, ProjectID: p.ID, Path: moved})
	if resp.Project.ID != p.ID || resp.Project.Root == p.Root {
		t.Fatalf("relocation keeps the ID and changes the directory: %+v", resp.Project)
	}
	if r := env.ensure(*resp.Project); r.Operation == nil {
		t.Fatal("ensure must work again after an explicit relocation")
	}
}

func TestRevokeInvalidatesCredentialAndKeepsStopByID(t *testing.T) {
	env := newEnv(t, testPolicy(2))
	dir := t.TempDir()
	p := env.authorizeDir(dir)
	token := readTokenFile(t, p.TokenFile)
	if code, _ := httpDo(t, env.srv, token, Request{Action: ActionStatus}); code != http.StatusOK {
		t.Fatalf("status before revoke: %d", code)
	}
	r := env.ensure(p)
	env.mustCall(admin, Request{Action: ActionRevoke, ProjectID: p.ID})
	if code, _ := httpDo(t, env.srv, token, Request{Action: ActionStatus}); code != http.StatusUnauthorized {
		t.Fatalf("a revoked credential must be refused, got %d", code)
	}
	if op := env.op(r.Operation.ID); op.Phase != PhaseCanceled {
		t.Fatalf("queued work of a revoked grant must be canceled, got %s", op.Phase)
	}
	_, err := env.call(admin, Request{Action: ActionEnsure, ProjectID: p.ID})
	wantStatus(t, err, http.StatusForbidden)
	if _, err := ResolveProject(env.cfg.Root, dir); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("resolve must report the revocation, got %v", err)
	}
	env.mustCall(admin, Request{Action: ActionStop, ProjectID: p.ID, DiscardPageState: true})

	again := env.authorizeDir(dir)
	if again.ID != p.ID || again.Port != p.Port || again.Revoked {
		t.Fatalf("re-authorizing the same directory reinstates the project: %+v", again)
	}
	if code, _ := httpDo(t, env.srv, readTokenFile(t, again.TokenFile), Request{Action: ActionStatus}); code != http.StatusOK {
		t.Fatalf("the reissued credential must work, got %d", code)
	}
	if code, _ := httpDo(t, env.srv, token, Request{Action: ActionStatus}); code != http.StatusUnauthorized {
		t.Fatalf("the old credential must stay invalid, got %d", code)
	}
}

func TestReinstatingRevokedGrantRefusesOverlapWithAnotherActiveGrant(t *testing.T) {
	for _, tc := range []struct {
		name                string
		revokedFirstIsChild bool
	}{{"revoked parent", false}, {"revoked child", true}} {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv(t, testPolicy(2))
			parent := t.TempDir()
			child := filepath.Join(parent, "child")
			if err := os.Mkdir(child, 0o755); err != nil {
				t.Fatal(err)
			}
			first, second := parent, child
			if tc.revokedFirstIsChild {
				first, second = child, parent
			}
			revoked := env.authorizeDir(first)
			env.mustCall(admin, Request{Action: ActionRevoke, ProjectID: revoked.ID})
			active := env.authorizeDir(second) // overlaps only a revoked grant
			activeToken := readTokenFile(t, active.TokenFile)

			_, err := env.call(admin, Request{Action: ActionAuthorize, Path: first})
			wantStatus(t, err, http.StatusConflict)

			if got := env.status(revoked); !got.Revoked {
				t.Fatalf("a refused reinstatement must leave the grant revoked: %+v", got)
			}
			if _, err := os.Stat(revoked.TokenFile); !os.IsNotExist(err) {
				t.Fatalf("a refused reinstatement must not issue a credential: %v", err)
			}
			if readTokenFile(t, active.TokenFile) != activeToken {
				t.Fatal("the active grant's credential must be stable")
			}
			if code, _ := httpDo(t, env.srv, activeToken, Request{Action: ActionStatus}); code != http.StatusOK {
				t.Fatalf("the active grant's credential must keep working, got %d", code)
			}
			if got, err := ResolveProject(env.cfg.Root, child); err != nil || got.ID != active.ID {
				t.Fatalf("routing must stay with the active grant %s: %v %+v", active.ID, err, got)
			}

			// Once the conflicting grant is gone the directory can be reinstated.
			env.mustCall(admin, Request{Action: ActionRevoke, ProjectID: active.ID})
			if again := env.authorizeDir(first); again.ID != revoked.ID || again.Revoked {
				t.Fatalf("reinstatement must work after the overlap is revoked: %+v", again)
			}
		})
	}
}

func TestCapabilitiesAreScopedToOneProject(t *testing.T) {
	env := newEnv(t, testPolicy(2))
	a, b := env.authorize(), env.authorize()
	tokenA := readTokenFile(t, a.TokenFile)
	adminToken := readTokenFile(t, AdminTokenFile(env.cfg.Root))

	if code, _ := httpDo(t, env.srv, "", Request{Action: ActionList}); code != http.StatusUnauthorized {
		t.Fatalf("no credential: %d", code)
	}
	if code, _ := httpDo(t, env.srv, "not-a-token", Request{Action: ActionList}); code != http.StatusUnauthorized {
		t.Fatalf("garbage credential: %d", code)
	}
	code, resp := httpDo(t, env.srv, tokenA, Request{Action: ActionStatus})
	if code != http.StatusOK || resp.Project == nil || resp.Project.ID != a.ID {
		t.Fatalf("a project credential defaults to its own project: %d %+v", code, resp)
	}
	if code, _ := httpDo(t, env.srv, tokenA, Request{Action: ActionStatus, ProjectID: b.ID}); code != http.StatusForbidden {
		t.Fatalf("cross-project status must be forbidden, got %d", code)
	}
	forbidden := []Request{
		{Action: ActionList},
		{Action: ActionAuthorize, Path: t.TempDir()},
		{Action: ActionRelocate, ProjectID: a.ID, Path: t.TempDir()},
		{Action: ActionRevoke, ProjectID: a.ID},
		{Action: ActionPolicy},
		{Action: ActionRecover, ProjectID: a.ID, OperationID: "op-0000000000000000"},
		{Action: ActionUpdate, ProjectID: a.ID, Update: &UpdateAction{Kind: UpdateAbort, UpdateID: "u"}},
		{Action: ActionStop, ProjectID: a.ID, DiscardPageState: true, Force: true, ExpectedGeneration: "1"},
	}
	for _, req := range forbidden {
		if code, _ := httpDo(t, env.srv, tokenA, req); code != http.StatusForbidden {
			t.Fatalf("%s with a project credential must be forbidden, got %d", req.Action, code)
		}
	}
	// Another project's waiter is invisible to this credential.
	rb := env.ensure(b)
	if code, _ := httpDo(t, env.srv, tokenA, Request{Action: ActionHeartbeat, WaiterID: rb.Waiter.ID}); code != http.StatusNotFound {
		t.Fatalf("a foreign waiter must not be found, got %d", code)
	}
	if code, _ := httpDo(t, env.srv, tokenA, Request{Action: ActionCancel, WaiterID: rb.Waiter.ID}); code != http.StatusOK {
		t.Fatalf("canceling a foreign waiter is a no-op, got %d", code)
	}
	if env.op(rb.Operation.ID).Phase != PhaseQueued {
		t.Fatal("a foreign credential must not cancel another project's start")
	}

	code, resp = httpDo(t, env.srv, adminToken, Request{Action: ActionList})
	if code != http.StatusOK || len(resp.Projects) != 2 || resp.Policy == nil {
		t.Fatalf("the admin lists every project: %d %+v", code, resp)
	}
	if code, _ := httpDo(t, env.srv, adminToken, Request{Action: "shell"}); code != http.StatusBadRequest {
		t.Fatalf("unknown actions are refused, got %d", code)
	}
}

func TestRefusalCarriesTheConflictingOperation(t *testing.T) {
	env := newEnv(t, testPolicy(2))
	env.runInline = false
	p := env.authorize()
	r := env.ensure(p)
	env.pass() // admitted, worker not run
	code, out := httpDo(t, env.srv, readTokenFile(t, p.TokenFile), Request{Action: ActionStop, DiscardPageState: true})
	if code != http.StatusConflict || out.Error == "" || out.Operation == nil || out.Operation.ID != r.Operation.ID {
		t.Fatalf("a refused stop must name the operation in progress: %d %+v", code, out)
	}
}

func TestRegistryWriteIsAtomicAndNeverRecreatedWhenCorrupt(t *testing.T) {
	env := newEnv(t, testPolicy(2))
	env.authorize()
	root := env.cfg.Root
	// A crashed writer leaves a temporary file; readers never see it.
	if err := os.WriteFile(filepath.Join(root, ".registry.json.tmp-1"), []byte("{garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := loadRegistry(root)
	if err != nil || len(reg.Projects) != 1 {
		t.Fatalf("stray temporary files must not matter: %v", err)
	}
	if fi, err := os.Stat(registryPath(root)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the registry must be private: %v", err)
	}

	if err := os.WriteFile(registryPath(root), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := defaultOptions()
	opts.interval = 0
	if _, err := newServer(root, env.backend, opts, nil); err == nil {
		t.Fatal("a corrupt registry must stop the manager from starting")
	}
	if string(mustRead(t, registryPath(root))) != "{" {
		t.Fatal("a corrupt registry must never be recreated or overwritten")
	}
}

func TestFailedPersistNeverBecomesVisible(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not bind root")
	}
	env := newEnv(t, testPolicy(2))
	if err := os.Chmod(env.cfg.Root, 0o500); err != nil {
		t.Fatal(err)
	}
	_, err := env.call(admin, Request{Action: ActionAuthorize, Path: t.TempDir()})
	if err := os.Chmod(env.cfg.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err == nil {
		t.Fatal("authorization must fail when the registry cannot be persisted")
	}
	if resp := env.mustCall(admin, Request{Action: ActionList}); len(resp.Projects) != 0 {
		t.Fatalf("an unpersisted grant must not be visible: %+v", resp.Projects)
	}
}
