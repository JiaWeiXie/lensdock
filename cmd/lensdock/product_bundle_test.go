package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JiaWeiXie/lensdock/internal/product"
	"github.com/JiaWeiXie/lensdock/internal/update"
)

// writeBundle creates a release bundle fixture and returns its root.
func writeBundle(t *testing.T, version string) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range []struct {
		rel, body string
		perm      os.FileMode
	}{
		{"lensdock", "host binary " + version, 0o755},
		{"VERSION", version + "\n", 0o644},
		{"docker/compose.yaml", "name: lensdock-product\nservices: {}\n", 0o644},
		{"docker/browser/seccomp/chromium.json", "{\"defaultAction\":\"SCMP_ACT_ERRNO\"}\n", 0o644},
		{"bin/linux-amd64/lensdock", "linux amd64 helper " + version, 0o755},
		{"bin/linux-arm64/lensdock", "linux arm64 helper " + version, 0o755},
	} {
		p := filepath.Join(root, filepath.FromSlash(f.rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, p, f.body, f.perm)
	}
	return root
}

// runCLIWith is runCLI with an injected executable path and Docker runner.
func runCLIWith(t *testing.T, exe string, runner update.Runner, env map[string]string, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	c := &cli{
		stdin: strings.NewReader(""), stdout: &out, stderr: &errb,
		getenv:     func(k string) string { return env[k] },
		executable: func() (string, error) { return exe, nil },
		runner:     runner,
	}
	code := c.run(context.Background(), args)
	return result{code, out.String(), errb.String()}
}

func TestProductInitUsesTheReleaseDefaultsOfABundle(t *testing.T) {
	bundle := writeBundle(t, "1.2.3")
	state := filepath.Join(t.TempDir(), "state")
	r := runCLI(t, context.Background(), "", nil, "admin", "product-init", "--state-dir", state, "--bundle-dir", bundle, "--origin", "http://127.0.0.1:18581")
	if r.code != 0 {
		t.Fatalf("init failed: %s", r.stderr)
	}
	// The deployment must not depend on the bundle any more.
	if err := os.RemoveAll(bundle); err != nil {
		t.Fatal(err)
	}
	dep, err := update.LoadDeployment(filepath.Join(state, "deployment.json"))
	if err != nil {
		t.Fatal(err)
	}
	if dep.Compose.Project != product.DefaultProject {
		t.Fatalf("project = %q", dep.Compose.Project)
	}
	if want := filepath.Join(state, "product", "release", "docker", "compose.yaml"); dep.Compose.Files[0] != want {
		t.Fatalf("compose file = %s, want %s", dep.Compose.Files[0], want)
	}
	env, err := os.ReadFile(filepath.Join(state, "product", "compose.env"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"LENSDOCK_RUNTIME_IMAGE='" + product.ReleaseRuntimeImage + "'\n", "LENSDOCK_GATEWAY_IMAGE='" + product.ReleaseGatewayImage + "'\n", "LENSDOCK_HOST_PORT=18581\n"} {
		if !strings.Contains(string(env), want) {
			t.Errorf("compose.env lacks %q:\n%s", want, env)
		}
	}
	for _, want := range []string{"Release 1.2.3", filepath.Join(state, "product", "release"), "admin product-start --state-dir " + state} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output lacks %q: %q", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout+r.stderr, "ld_admin_") {
		t.Error("a credential was printed")
	}
}

func TestProductInitResolvesTheBundleOfASymlinkedExecutable(t *testing.T) {
	bundle := writeBundle(t, "2.0.0")
	link := filepath.Join(t.TempDir(), "lensdock")
	if err := os.Symlink(filepath.Join(bundle, "lensdock"), link); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	// No --state-dir and no --bundle-dir: both come from the executable.
	r := runCLIWith(t, link, nil, map[string]string{"HOME": home}, "admin", "product-init")
	if r.code != 0 {
		t.Fatalf("init failed: %s", r.stderr)
	}
	state := filepath.Join(home, ".local", "share", "lensdock")
	dep, err := update.LoadDeployment(filepath.Join(state, "deployment.json"))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(state, "product", "release", "docker", "compose.yaml"); dep.Compose.Files[0] != want {
		t.Fatalf("compose file = %s, want %s", dep.Compose.Files[0], want)
	}
	if !strings.Contains(r.stdout, "Release 2.0.0") {
		t.Errorf("output does not name the release: %q", r.stdout)
	}
	if r := runCLIWith(t, link, nil, map[string]string{"HOME": home}, "admin", "product-init"); r.code == 0 {
		t.Fatal("a second init must refuse to touch the existing deployment")
	}
}

func TestProductInitValidatesBeforeCreatingState(t *testing.T) {
	bundle := writeBundle(t, "1.0.0")
	broken := writeBundle(t, "1.0.1")
	if err := os.Remove(filepath.Join(broken, "docker", "browser", "seccomp", "chromium.json")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		code int
	}{
		{"project", []string{"--bundle-dir", bundle, "--project", "Bad_Name"}, exitUsage},
		{"image", []string{"--bundle-dir", bundle, "--runtime-image", "bad image"}, exitUsage},
		{"incomplete bundle", []string{"--bundle-dir", broken}, exitUsage},
		{"missing bundle", []string{"--bundle-dir", filepath.Join(t.TempDir(), "absent")}, 0},
		{"not a bundle", []string{"--bundle-dir", t.TempDir()}, exitUsage},
		{"origin port", []string{"--bundle-dir", bundle, "--origin", "http://127.0.0.1"}, exitUsage},
		{"no bundle and no source checkout", nil, exitUsage},
	} {
		state := filepath.Join(t.TempDir(), "state")
		wd := t.TempDir()
		t.Chdir(wd)
		r := runCLIWith(t, filepath.Join(wd, "not-a-bundle", "lensdock"), nil, nil, append([]string{"admin", "product-init", "--state-dir", state}, tc.args...)...)
		if r.code == 0 || tc.code != 0 && r.code != tc.code {
			t.Errorf("%s: exit %d: %s", tc.name, r.code, r.stderr)
		}
		if _, err := os.Stat(state); !os.IsNotExist(err) {
			t.Errorf("%s: invalid input created private state: %v", tc.name, err)
		}
	}
}

func TestProductInitKeepsTheSourceCheckoutFlow(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "docker"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(src, "docker", "compose.yaml"), "services: {}\n", 0o644)
	t.Chdir(src)
	state := filepath.Join(t.TempDir(), "state")
	// An executable outside any bundle: `go run` or bin/lensdock of a checkout.
	r := runCLIWith(t, filepath.Join(src, "bin", "lensdock"), nil, nil, "admin", "product-init", "--state-dir", state)
	if r.code != 0 {
		t.Fatalf("init failed: %s", r.stderr)
	}
	dep, err := update.LoadDeployment(filepath.Join(state, "deployment.json"))
	if err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	if len(dep.Compose.Files) != 1 || dep.Compose.Files[0] != filepath.Join(wd, "docker", "compose.yaml") {
		t.Fatalf("compose files = %v", dep.Compose.Files)
	}
	env, _ := os.ReadFile(filepath.Join(state, "product", "compose.env"))
	for _, want := range []string{"LENSDOCK_RUNTIME_IMAGE='" + product.SourceRuntimeImage + "'\n", "LENSDOCK_GATEWAY_IMAGE='" + product.SourceGatewayImage + "'\n"} {
		if !strings.Contains(string(env), want) {
			t.Errorf("compose.env lacks %q:\n%s", want, env)
		}
	}
}

// fakeDocker answers the docker commands of a first product-start with
// images that carry registry digests.
type fakeDocker struct {
	pulled map[string]bool
	log    []string
}

func (f *fakeDocker) Run(_ context.Context, c update.Cmd) error {
	a := c.Args
	f.log = append(f.log, strings.Join(a, " "))
	out := func(s string) {
		if c.Stdout != nil {
			fmt.Fprint(c.Stdout, s)
		}
	}
	switch {
	case a[0] == "version":
		out(`{"Version":"27.0.0","Os":"linux","Arch":"arm64"}`)
	case a[0] == "compose" && a[1] == "version":
		out("v2.30.0\n")
	case a[0] == "pull":
		f.pulled[a[1]] = true
	case a[0] == "image" && a[1] == "inspect":
		ref := a[len(a)-1]
		repo, _, pinned := strings.Cut(ref, "@")
		if !pinned {
			repo = strings.TrimSuffix(ref, ":latest")
			if !f.pulled[ref] {
				return &update.ExitError{Argv: a, Code: 1, Stderr: "Error: No such image: " + ref}
			}
		}
		out(fmt.Sprintf(`{"Id":"sha256:%064d","RepoDigests":[%q]}`, 1, repo+"@sha256:"+strings.Repeat("c", 64)))
	}
	return nil
}

func TestProductStartThroughTheCLIPinsAndReportsTheOrigin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(t.TempDir(), "state")
	bundle := writeBundle(t, "1.2.3")
	if r := runCLI(t, context.Background(), "", nil, "admin", "product-init", "--state-dir", state, "--bundle-dir", bundle, "--origin", "http://127.0.0.1:"+port); r.code != 0 {
		t.Fatalf("init failed: %s", r.stderr)
	}
	f := &fakeDocker{pulled: map[string]bool{}}
	r := runCLIWith(t, "/nonexistent/lensdock", f, nil, "admin", "product-start", "--state-dir", state, "--pull", "missing")
	if r.code != 0 {
		t.Fatalf("start failed: %s", r.stderr)
	}
	if !strings.Contains(r.stdout, "http://127.0.0.1:"+port+"/") || !strings.Contains(r.stdout, filepath.Join(state, "product", "admin-token")) || strings.Contains(r.stdout+r.stderr, "ld_admin_") {
		t.Fatalf("unexpected output (or a token leaked): %q", r.stdout)
	}
	pin, err := os.ReadFile(filepath.Join(state, "product", product.PinFile))
	if err != nil || !strings.Contains(string(pin), "@sha256:"+strings.Repeat("c", 64)) {
		t.Fatalf("the first start did not pin: %v %q", err, pin)
	}
	if _, err := os.Stat(filepath.Join(state, "product", product.StartedFile)); err != nil {
		t.Errorf("the first start was not recorded: %v", err)
	}
	if r := runCLIWith(t, "/nonexistent/lensdock", f, nil, "admin", "product-start", "--state-dir", state, "--pull", "sometimes"); r.code != exitUsage {
		t.Fatalf("invalid --pull exit %d", r.code)
	}
}

func TestVersionCommand(t *testing.T) {
	r := runCLI(t, context.Background(), "", nil, "version")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "lensdock "+version+" (") {
		t.Fatalf("version: %d %q", r.code, r.stdout)
	}
}
