package product

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JiaWeiXie/lensdock/internal/update"
)

func start(t *testing.T, dep *update.Deployment, f *fakeDocker, policy string) error {
	t.Helper()
	return Start(context.Background(), dep, DeploymentDocker(dep, f), StartOptions{PullPolicy: policy}, &bytes.Buffer{})
}

func TestStartPinsTheImagesOnceAndNeverPullsAgain(t *testing.T) {
	dep, f := startFixture(t)
	if err := start(t, dep, f, ""); err != nil {
		t.Fatalf("first start failed: %v", err)
	}
	for _, want := range []string{"pull ghcr.io/jiaweixie/lensdock/runtime:latest", "pull ghcr.io/jiaweixie/lensdock/gateway:latest", "volume ls", "ps -a"} {
		if !f.did(want) {
			t.Errorf("first start did not run %q; log:\n%s", want, f.text())
		}
	}
	pinPath := filepath.Join(dep.StateDir, PinFile)
	pin := readFile(t, pinPath)
	for _, want := range []string{
		"  runtime:\n    image: \"ghcr.io/jiaweixie/lensdock/runtime@" + testRuntimeDigest + "\"\n",
		"  gateway:\n    image: \"ghcr.io/jiaweixie/lensdock/gateway@" + testGatewayDigest + "\"\n",
		"  config-init:\n    image: \"ghcr.io/jiaweixie/lensdock/gateway@" + testGatewayDigest + "\"\n",
	} {
		if !strings.Contains(pin, want) {
			t.Errorf("pin file lacks %q:\n%s", want, pin)
		}
	}
	if fi, _ := os.Stat(pinPath); fi.Mode().Perm() != 0o600 {
		t.Errorf("pin file mode %v", fi.Mode())
	}
	if _, err := os.Stat(filepath.Join(dep.StateDir, StartedFile)); err != nil {
		t.Errorf("the first start was not recorded: %v", err)
	}
	// config-init runs before up, both through the deployment's own arguments.
	configInit, up := -1, -1
	for i, l := range f.lines() {
		switch {
		case strings.Contains(l, "run --rm -T config-init"):
			configInit = i
			if !strings.Contains(l, "-f "+pinPath) {
				t.Errorf("config-init ignores the pin file: %s", l)
			}
		case strings.Contains(l, " up -d --wait") && strings.Contains(l, "--pull=never"):
			up = i
		}
	}
	if configInit < 0 || up < 0 || configInit > up {
		t.Fatalf("config-init %d, up %d; log:\n%s", configInit, up, f.text())
	}

	// A repeated start neither pulls, nor re-resolves, nor looks at the project again.
	f.reset()
	if err := start(t, dep, f, ""); err != nil {
		t.Fatalf("second start failed: %v", err)
	}
	if f.did("pull") || f.did("image inspect") || f.did("volume ls") || f.did("ps -a") {
		t.Fatalf("second start pulled, re-resolved or re-checked the project:\n%s", f.text())
	}
	if readFile(t, pinPath) != pin {
		t.Fatal("the pin changed on a repeated start")
	}
}

func TestStartRunsEveryCommandThroughThePassedDocker(t *testing.T) {
	dep, f := startFixture(t)
	d := DeploymentDocker(dep, f)
	d.Bin = "pinned-docker"
	if err := Start(context.Background(), dep, d, StartOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	// A first start exercises the ownership queries, the pulls, config-init and up.
	if len(f.cmds) < 8 {
		t.Fatalf("too few commands to be a first start:\n%s", f.text())
	}
	for _, c := range f.cmds {
		if c.Name != "pinned-docker" {
			t.Errorf("%q ran with binary %q", strings.Join(c.Args, " "), c.Name)
		}
	}
	// A restart is `up` alone and also uses the passed binary.
	f.reset()
	if err := Start(context.Background(), dep, d, StartOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.cmds {
		if c.Name != "pinned-docker" {
			t.Errorf("%q ran with binary %q", strings.Join(c.Args, " "), c.Name)
		}
	}
}

func TestStartPutsTheDeploymentEnvironmentBeforeTheCallers(t *testing.T) {
	dep, f := startFixture(t)
	if err := start(t, dep, f, ""); err != nil {
		t.Fatal(err)
	}
	want := "LENSDOCK_PUBLIC_ORIGIN=" + strings.TrimSuffix(dep.Gateway.ReadyURL, "/readyz")
	for _, c := range f.cmds {
		if len(c.Env) == 0 || !containsString(c.Env, want) || !containsString(c.Env, "LENSDOCK_STATE_DIR="+filepath.Dir(dep.File)) {
			t.Fatalf("%q did not carry the deployment environment: %q", strings.Join(c.Args, " "), c.Env)
		}
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestStartRefusesAnExistingComposeProject(t *testing.T) {
	dep, f := startFixture(t)
	f.project = true
	err := start(t, dep, f, "")
	if err == nil || !strings.Contains(err.Error(), `Compose project "lensdock-smoke"`) {
		t.Fatalf("start must refuse an existing project: %v", err)
	}
	if f.did("pull") || strings.Contains(f.text(), "config-init") || strings.Contains(f.text(), " up ") {
		t.Fatalf("a refused start changed Docker state:\n%s", f.text())
	}
	if readFile(t, filepath.Join(dep.StateDir, PinFile)) != PinPlaceholder {
		t.Fatal("a refused start wrote the pin file")
	}
}

func TestStartRetriesItsOwnFailedFirstStartButNotAForeignProject(t *testing.T) {
	dep, f := startFixture(t)
	f.failUp = true
	if err := start(t, dep, f, ""); err == nil {
		t.Fatal("the first start was expected to fail")
	}
	if _, err := os.Stat(filepath.Join(dep.StateDir, StartedFile)); !os.IsNotExist(err) {
		t.Fatalf("a failed start was recorded: %v", err)
	}
	if !strings.Contains(readFile(t, filepath.Join(dep.StateDir, PinFile)), "@"+testRuntimeDigest) {
		t.Fatal("the failed start did not pin")
	}

	// The failed attempt left the credential volume behind; it carries our CA.
	ca := readFile(t, filepath.Join(dep.StateDir, "ca.crt"))
	f.reset()
	f.failUp, f.project, f.proof = false, true, "someone else's certificate\n"
	if err := start(t, dep, f, ""); err == nil || f.count("config-init") != 0 {
		t.Fatalf("a project this state did not create was reused: %v\n%s", err, f.text())
	}
	if f.did("pull") {
		t.Fatalf("a retry pulled:\n%s", f.text())
	}

	f.reset()
	f.proof = ca
	if err := start(t, dep, f, ""); err != nil {
		t.Fatalf("retry of an own first start failed: %v\n%s", err, f.text())
	}
	if f.did("pull") || f.count("run --rm --pull never") != 1 {
		t.Fatalf("retry must prove ownership once and never pull:\n%s", f.text())
	}
	if _, err := os.Stat(filepath.Join(dep.StateDir, StartedFile)); err != nil {
		t.Fatalf("the successful retry was not recorded: %v", err)
	}
}

func TestStartFailsInsteadOfKeepingAMutableImage(t *testing.T) {
	dep, f := startFixture(t)
	f.noDigest = true
	err := start(t, dep, f, "")
	if err == nil || !strings.Contains(err.Error(), "pin ghcr.io/jiaweixie/lensdock/runtime:latest") {
		t.Fatalf("an image that cannot be pinned must fail the start: %v", err)
	}
	if strings.Contains(f.text(), "config-init") || strings.Contains(f.text(), " up ") {
		t.Fatalf("a failed pin continued:\n%s", f.text())
	}
	if readFile(t, filepath.Join(dep.StateDir, PinFile)) != PinPlaceholder {
		t.Fatal("a failed pin wrote the pin file")
	}
}

func TestStartPullPolicy(t *testing.T) {
	dep, f := startFixture(t)
	err := start(t, dep, f, PullNever)
	if err == nil || !strings.Contains(err.Error(), "not present locally") || f.did("pull") {
		t.Fatalf("PullNever must not pull and must fail clearly: %v", err)
	}
	var oe *OptionError
	if err := start(t, dep, f, "sometimes"); !errors.As(err, &oe) {
		t.Fatalf("an invalid pull policy: %v", err)
	}
}

func TestStartIsSerializedPerDeployment(t *testing.T) {
	dep, f := startFixture(t)
	unlock, err := lockFile(filepath.Join(dep.StateDir, startLockFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := start(t, dep, f, ""); !errors.Is(err, ErrBusy) {
		t.Fatalf("a concurrent start: %v", err)
	}
	if len(f.cmds) != 0 {
		t.Fatalf("a refused concurrent start ran docker:\n%s", f.text())
	}
	unlock()
	if err := start(t, dep, f, ""); err != nil {
		t.Fatalf("start after the lock was released: %v", err)
	}
}
