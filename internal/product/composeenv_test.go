package product

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/JiaWeiXie/lensdock/internal/update"
)

func TestComposeEnvUsesOnlyWhatItCanInterpretExactly(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "compose.env")
	writeFile(t, envFile, "# a comment\nLENSDOCK_STATE_DIR='/state/it\\'s'\nLENSDOCK_HOST_PORT=18081\nQUOTED=\"two words\"\nexport EXPORTED=1\nINLINE=AGENT # comment\nno equals sign\nLENSDOCK_RUNTIME_IMAGE='ghcr.io/o/runtime@sha256:abc'\n", 0o600)
	dep := &update.Deployment{Compose: update.ComposeConfig{EnvFiles: []string{envFile, filepath.Join(t.TempDir(), "absent.env")}}}
	want := []string{"LENSDOCK_STATE_DIR=/state/it's", "LENSDOCK_HOST_PORT=18081", "LENSDOCK_RUNTIME_IMAGE=ghcr.io/o/runtime@sha256:abc"}
	if got := ComposeEnv(dep); !reflect.DeepEqual(got, want) {
		t.Fatalf("ComposeEnv = %q, want %q", got, want)
	}
}

func TestComposeEnvValueRoundTrips(t *testing.T) {
	for _, v := range []string{"/state/dir", "/state/it's", "a$b", "ghcr.io/o/r:latest"} {
		k, got, ok := parseEnvLine("K=" + composeEnvValue(v))
		if !ok || k != "K" || got != v {
			t.Errorf("%q round-tripped to %q (%v)", v, got, ok)
		}
	}
}

type recordingRunner struct{ cmd update.Cmd }

func (r *recordingRunner) Run(_ context.Context, c update.Cmd) error {
	r.cmd = c
	return nil
}

func TestDeploymentDockerPutsTheDeploymentFirstAndTheCallerLast(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "compose.env")
	writeFile(t, envFile, "LENSDOCK_STATE_DIR=/deployment\n", 0o600)
	dep := &update.Deployment{Compose: update.ComposeConfig{EnvFiles: []string{envFile}}}
	base := &recordingRunner{}
	d := DeploymentDocker(dep, base)
	if err := d.Run.Run(context.Background(), update.Cmd{Name: "docker", Env: []string{"EXTRA=1"}}); err != nil {
		t.Fatal(err)
	}
	// Later entries win in os/exec, so the caller's explicit variables still beat the env files
	// and both beat the process environment the runner appends them to.
	if want := []string{"LENSDOCK_STATE_DIR=/deployment", "EXTRA=1"}; !reflect.DeepEqual(base.cmd.Env, want) {
		t.Fatalf("env = %q, want %q", base.cmd.Env, want)
	}
	if d.Bin != "" {
		t.Fatalf("the wrapper must leave the binary to its caller, got %q", d.Bin)
	}

	empty := &recordingRunner{}
	none := &update.Deployment{}
	if err := DeploymentDocker(none, empty).Run.Run(context.Background(), update.Cmd{Name: "docker"}); err != nil || empty.cmd.Env != nil {
		t.Fatalf("an empty env changed the command: %v %q", err, empty.cmd.Env)
	}
}

func TestHelperContainersHaveNoPidCapUnlessTheDeploymentOptsIn(t *testing.T) {
	t.Setenv(HelperPidsEnv, "")
	envFile := filepath.Join(t.TempDir(), "compose.env")
	writeFile(t, envFile, "LENSDOCK_STATE_DIR=/deployment\n", 0o600)
	dep := &update.Deployment{Compose: update.ComposeConfig{EnvFiles: []string{envFile}}}
	base := &recordingRunner{}
	run := func(dep *update.Deployment, args ...string) error {
		return DeploymentDocker(dep, base).Run.Run(context.Background(), update.Cmd{Name: "docker", Args: args})
	}
	if err := run(dep, "run", "--rm", "image"); err != nil || !reflect.DeepEqual(base.cmd.Args, []string{"run", "--rm", "image"}) {
		t.Fatalf("default helpers must run without a PID cap: %v %q", err, base.cmd.Args)
	}

	capped := filepath.Join(t.TempDir(), "capped.env")
	writeFile(t, capped, "LENSDOCK_HELPER_PIDS_LIMIT=128\n", 0o600)
	cappedDep := &update.Deployment{Compose: update.ComposeConfig{EnvFiles: []string{capped}}}
	if err := run(cappedDep, "run", "--rm", "image"); err != nil || !reflect.DeepEqual(base.cmd.Args, []string{"run", "--pids-limit", "128", "--rm", "image"}) {
		t.Fatalf("an opted-in cap must reach docker run: %v %q", err, base.cmd.Args)
	}
	if err := run(cappedDep, "compose", "up"); err != nil || !reflect.DeepEqual(base.cmd.Args, []string{"compose", "up"}) {
		t.Fatalf("only docker run is capped: %v %q", err, base.cmd.Args)
	}

	t.Setenv(HelperPidsEnv, "64")
	if err := run(dep, "run", "image"); err != nil || !reflect.DeepEqual(base.cmd.Args, []string{"run", "--pids-limit", "64", "image"}) {
		t.Fatalf("the process environment is the fallback: %v %q", err, base.cmd.Args)
	}
	if err := run(cappedDep, "run", "image"); err != nil || base.cmd.Args[2] != "128" {
		t.Fatalf("a deployment definition must win over the process environment: %v %q", err, base.cmd.Args)
	}
	t.Setenv(HelperPidsEnv, "0")
	if err := run(dep, "run", "image"); err != nil || !reflect.DeepEqual(base.cmd.Args, []string{"run", "image"}) {
		t.Fatalf("0 means unlimited: %v %q", err, base.cmd.Args)
	}
	t.Setenv(HelperPidsEnv, "many")
	if err := run(dep, "run", "image"); err == nil {
		t.Fatal("an invalid cap must be refused, not ignored")
	}
}

func TestInvalidHelperPidCapIsRefusedBeforeCompose(t *testing.T) {
	t.Setenv(HelperPidsEnv, "many")
	base := &recordingRunner{}
	err := DeploymentDocker(&update.Deployment{}, base).Run.Run(context.Background(), update.Cmd{Name: "docker", Args: []string{"compose", "up"}})
	if err == nil || base.cmd.Name != "" {
		t.Fatalf("an invalid cap must be refused instead of reaching Compose: %v %+v", err, base.cmd)
	}
}

// resolvedConfigInitPids resolves the deployment's own Compose arguments
// through real Docker Compose, with the environment DeploymentDocker hands to
// Docker, and returns config-init's pids_limit (0 when the cap is absent).
func resolvedConfigInitPids(t *testing.T, dep *update.Deployment, process ...string) int64 {
	t.Helper()
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("docker compose is not available")
	}
	base := &recordingRunner{}
	cc := update.ComposeCLI{Project: dep.Compose.Project, Files: dep.Compose.Files, EnvFiles: dep.Compose.EnvFiles, Dir: dep.Compose.ProjectDir}
	args := cc.Args("--profile", "setup", "config", "--format", "json")
	if err := DeploymentDocker(dep, base).Run.Run(context.Background(), update.Cmd{Name: "docker", Args: args}); err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "LENSDOCK_") && !strings.HasPrefix(e, "COMPOSE_") {
			env = append(env, e)
		}
	}
	cmd := exec.Command("docker", base.cmd.Args...)
	cmd.Env = append(append(env, process...), base.cmd.Env...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("docker compose config: %v", err)
	}
	var cfg struct {
		Services map[string]struct {
			Pids int64 `json:"pids_limit"`
		} `json:"services"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		t.Fatal(err)
	}
	svc, ok := cfg.Services["config-init"]
	if !ok {
		t.Fatal("the setup profile must resolve config-init")
	}
	return svc.Pids
}

// setComposeEnv replaces the definition of key in the deployment's compose.env.
func setComposeEnv(t *testing.T, dep *update.Deployment, key, value string) {
	t.Helper()
	file := filepath.Join(dep.StateDir, "compose.env")
	var kept []string
	for _, l := range strings.Split(readFile(t, file), "\n") {
		if l != "" && !strings.HasPrefix(l, key+"=") {
			kept = append(kept, l)
		}
	}
	if value != "" {
		kept = append(kept, key+"="+value)
	}
	writeFile(t, file, strings.Join(kept, "\n")+"\n", 0o600)
}

// assertHelperPidCapFollowsTheSetting checks every way a standalone deployment
// can set the cap against what Compose resolves for config-init.
func assertHelperPidCapFollowsTheSetting(t *testing.T, dep *update.Deployment) {
	t.Helper()
	for _, c := range []struct {
		file, process string
		want          int64
	}{
		{"", "", 0},
		{"64", "", 64},
		{"0", "32", 0},
		{"", "32", 32},
		{"64", "32", 64},
	} {
		setComposeEnv(t, dep, HelperPidsEnv, c.file)
		var process []string
		if c.process != "" {
			process = []string{HelperPidsEnv + "=" + c.process}
		}
		if got := resolvedConfigInitPids(t, dep, process...); got != c.want {
			t.Fatalf("compose.env %q, process %q: config-init pids_limit = %d, want %d", c.file, c.process, got, c.want)
		}
	}
}

// legacyReleaseCompose stands in for the copied docker/compose.yaml of an
// installed release that predates any helper PID setting.
const legacyReleaseCompose = `services:
  config-init:
    image: example/gateway:test
    profiles: [setup]
  runtime:
    image: example/runtime:test
  gateway:
    image: example/gateway:test
`

func TestStartAppliesTheHelperPidCapToAnInstalledReleaseCopyWithoutEditingIt(t *testing.T) {
	t.Setenv(HelperPidsEnv, "")
	dep, f := startFixture(t)
	base := dep.Compose.Files[0]
	if err := os.Chmod(base, 0o600); err != nil {
		t.Fatal(err)
	}
	writeFile(t, base, legacyReleaseCompose, 0o600)
	if err := start(t, dep, f, ""); err != nil {
		t.Fatal(err)
	}

	overlay := filepath.Join(dep.StateDir, HelperPolicyFile)
	if readFile(t, base) != legacyReleaseCompose {
		t.Fatal("the pinned release copy of the Compose file must stay byte for byte")
	}
	loaded, err := update.LoadDeployment(dep.File)
	if err != nil || loaded.Compose.Files[len(loaded.Compose.Files)-1] != overlay || len(loaded.Compose.Files) != 3 {
		t.Fatalf("the persisted deployment must list the helper policy after the release copy and pin file: %v %v", err, loaded)
	}
	assertHelperPidCapFollowsTheSetting(t, loaded)

	// A repeated start keeps the overlay as it is, and a changed one is not trusted.
	before := readFile(t, overlay)
	if err := start(t, loaded, f, ""); err != nil || readFile(t, overlay) != before || len(loaded.Compose.Files) != 3 {
		t.Fatalf("a repeated start must keep the overlay: %v %v", err, loaded.Compose.Files)
	}
	writeFile(t, overlay, "services:\n  config-init:\n    pids_limit: 100000\n", 0o600)
	configInits := f.count("config-init")
	if err := start(t, loaded, f, ""); err == nil || f.count("config-init") != configInits {
		t.Fatalf("a modified helper policy must be refused before config-init runs: %v", err)
	}
}

func TestHelperPolicyOverlayOfASourceCheckoutResolvesThroughTheRepositoryCompose(t *testing.T) {
	t.Setenv(HelperPidsEnv, "")
	compose, err := filepath.Abs(filepath.Join("..", "..", "docker", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	writeFile(t, filepath.Join(state, "runtime.env"), "", 0o600)
	dep := update.ScaffoldM0(filepath.Join(state, "deployment.json"), state, compose, filepath.Join(state, "bin"))
	envFile := filepath.Join(state, "compose.env")
	writeFile(t, envFile, "LENSDOCK_STATE_DIR="+state+"\n", 0o600)
	dep.Compose.EnvFiles = []string{envFile}
	if err := ensureHelperPolicy(dep); err != nil {
		t.Fatal(err)
	}
	assertHelperPidCapFollowsTheSetting(t, dep)
}
