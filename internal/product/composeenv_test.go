package product

import (
	"context"
	"path/filepath"
	"reflect"
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
