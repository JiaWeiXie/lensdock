package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/JiaWeiXie/lensdock/internal/update"
)

const (
	testRuntimeDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testGatewayDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
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

func TestProductInitSnapshotsTheBundleIntoPrivateState(t *testing.T) {
	bundle := writeBundle(t, "1.2.3")
	state := filepath.Join(t.TempDir(), "state")
	r := runCLI(t, context.Background(), "", nil, "admin", "product-init", "--state-dir", state, "--bundle-dir", bundle, "--project", "lensdock-smoke", "--origin", "http://127.0.0.1:18581")
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
	product := filepath.Join(state, "product")
	rel := filepath.Join(product, "release")
	if got, want := dep.Compose.Files, []string{filepath.Join(rel, "docker", "compose.yaml"), filepath.Join(product, "images.pin.yaml")}; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("compose files = %v, want %v", got, want)
	}
	if dep.Compose.Project != "lensdock-smoke" {
		t.Fatalf("project = %q", dep.Compose.Project)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		p := dep.LinuxCLI[arch]
		if p != filepath.Join(rel, "bin", "linux-"+arch, "lensdock") {
			t.Fatalf("linuxCli[%s] = %s", arch, p)
		}
		b, err := os.ReadFile(p)
		fi, serr := os.Stat(p)
		if err != nil || serr != nil || string(b) != "linux "+arch+" helper 1.2.3" || fi.Mode().Perm() != 0o755 {
			t.Fatalf("helper %s: %v %v %q", arch, err, serr, b)
		}
	}
	for _, p := range []string{rel, filepath.Join(rel, "docker"), product} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v %v", p, err, fi)
		}
	}
	for _, name := range []string{"docker/compose.yaml", "docker/browser/seccomp/chromium.json", "VERSION"} {
		if fi, err := os.Stat(filepath.Join(rel, filepath.FromSlash(name))); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", name, err, fi)
		}
	}
	if pin, err := os.ReadFile(filepath.Join(product, "images.pin.yaml")); err != nil || string(pin) != pinPlaceholder {
		t.Fatalf("pin file: %v %q", err, pin)
	}
	env, err := os.ReadFile(filepath.Join(product, "compose.env"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"LENSDOCK_RUNTIME_IMAGE='ghcr.io/jiaweixie/lensdock/runtime:latest'\n", "LENSDOCK_GATEWAY_IMAGE='ghcr.io/jiaweixie/lensdock/gateway:latest'\n", "LENSDOCK_HOST_PORT=18581\n"} {
		if !strings.Contains(string(env), want) {
			t.Errorf("compose.env lacks %q:\n%s", want, env)
		}
	}
	if _, err := os.Stat(filepath.Join(product, "gateway-host.json")); !os.IsNotExist(err) {
		t.Errorf("a release deployment must not carry the source-checkout host Gateway config: %v", err)
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
	}{
		{"project", []string{"--bundle-dir", bundle, "--project", "Bad_Name"}},
		{"image", []string{"--bundle-dir", bundle, "--runtime-image", "bad image"}},
		{"image option", []string{"--bundle-dir", bundle, "--gateway-image", "-oops"}},
		{"incomplete bundle", []string{"--bundle-dir", broken}},
		{"missing bundle", []string{"--bundle-dir", filepath.Join(t.TempDir(), "absent")}},
		{"not a bundle", []string{"--bundle-dir", t.TempDir()}},
		{"origin port", []string{"--bundle-dir", bundle, "--origin", "http://127.0.0.1"}},
		{"no bundle and no source checkout", nil},
	} {
		state := filepath.Join(t.TempDir(), "state")
		wd := t.TempDir()
		t.Chdir(wd)
		r := runCLIWith(t, filepath.Join(wd, "not-a-bundle", "lensdock"), nil, nil, append([]string{"admin", "product-init", "--state-dir", state}, tc.args...)...)
		if r.code == 0 {
			t.Errorf("%s: init succeeded", tc.name)
		}
		if _, err := os.Stat(state); !os.IsNotExist(err) {
			t.Errorf("%s: invalid input created private state: %v", tc.name, err)
		}
	}
}

func TestProductInitNeverOverwritesAnExistingDeployment(t *testing.T) {
	bundle := writeBundle(t, "1.0.0")
	state := filepath.Join(t.TempDir(), "state")
	args := []string{"admin", "product-init", "--state-dir", state, "--bundle-dir", bundle}
	if r := runCLI(t, context.Background(), "", nil, args...); r.code != 0 {
		t.Fatalf("init failed: %s", r.stderr)
	}
	before, err := os.ReadFile(filepath.Join(state, "deployment.json"))
	if err != nil {
		t.Fatal(err)
	}
	token, _ := os.ReadFile(filepath.Join(state, "product", "admin-token"))
	if r := runCLI(t, context.Background(), "", nil, args...); r.code == 0 {
		t.Fatal("second init succeeded")
	}
	after, _ := os.ReadFile(filepath.Join(state, "deployment.json"))
	token2, _ := os.ReadFile(filepath.Join(state, "product", "admin-token"))
	if !bytes.Equal(before, after) || !bytes.Equal(token, token2) || len(token) == 0 {
		t.Fatal("the existing deployment changed")
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
	if dep.LinuxCLI["amd64"] != filepath.Join(wd, "bin", "linux-amd64", "lensdock") {
		t.Fatalf("linuxCli = %v", dep.LinuxCLI)
	}
	env, _ := os.ReadFile(filepath.Join(state, "product", "compose.env"))
	for _, want := range []string{"LENSDOCK_RUNTIME_IMAGE='lensdock/runtime:dev'\n", "LENSDOCK_GATEWAY_IMAGE='lensdock/gateway:dev'\n"} {
		if !strings.Contains(string(env), want) {
			t.Errorf("compose.env lacks %q:\n%s", want, env)
		}
	}
	if _, err := os.Stat(filepath.Join(state, "product", "gateway-host.json")); err != nil {
		t.Errorf("source checkout lost its host Gateway config: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, "product", "images.pin.yaml")); !os.IsNotExist(err) {
		t.Errorf("a source checkout must not get a pin file: %v", err)
	}
}

// fakeDocker answers the docker commands product-start issues.
type fakeDocker struct {
	mu       sync.Mutex
	log      []string
	pulled   map[string]bool
	project  bool   // the Compose project already has volumes
	proof    string // what the credential volume's ca.crt holds
	failUp   bool
	noDigest bool // images carry no registry digest
}

func (f *fakeDocker) did(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range f.log {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeDocker) count(substr string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, l := range f.log {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

func (f *fakeDocker) text() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.log, "\n")
}

func (f *fakeDocker) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = nil
	f.pulled = map[string]bool{}
}

func digestOf(ref string) string {
	if strings.Contains(ref, "/gateway") {
		return testGatewayDigest
	}
	return testRuntimeDigest
}

func (f *fakeDocker) Run(_ context.Context, c update.Cmd) error {
	f.mu.Lock()
	defer f.mu.Unlock()
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
	case a[0] == "volume" && a[1] == "ls":
		if f.project {
			out("lensdock-product_gateway-credentials\nlensdock-product_runtime-credentials\n")
		}
	case a[0] == "run" && a[len(a)-1] == "/proof/ca.crt":
		out(f.proof)
	case a[0] == "compose" && f.failUp && strings.Contains(strings.Join(a, " "), " up -d "):
		return errors.New("up failed")
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
		digests := fmt.Sprintf("%q", repo+"@"+digestOf(ref))
		if f.noDigest {
			digests = ""
		}
		out(fmt.Sprintf(`{"Id":"sha256:%064d","RepoDigests":[%s]}`, 1, digests))
	}
	return nil
}

func startFixture(t *testing.T, f *fakeDocker) (state, port string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	state = filepath.Join(t.TempDir(), "state")
	bundle := writeBundle(t, "1.2.3")
	if r := runCLI(t, context.Background(), "", nil, "admin", "product-init", "--state-dir", state, "--bundle-dir", bundle, "--origin", "http://127.0.0.1:"+port); r.code != 0 {
		t.Fatalf("init failed: %s", r.stderr)
	}
	f.pulled = map[string]bool{}
	return state, port
}

func productStartCLI(t *testing.T, f *fakeDocker, state string, extra ...string) result {
	t.Helper()
	return runCLIWith(t, "/nonexistent/lensdock", f, nil, append([]string{"admin", "product-start", "--state-dir", state}, extra...)...)
}

func TestProductStartPinsTheImagesOnceAndNeverPullsAgain(t *testing.T) {
	f := &fakeDocker{}
	state, port := startFixture(t, f)
	r := productStartCLI(t, f, state)
	if r.code != 0 {
		t.Fatalf("first start failed: %s", r.stderr)
	}
	if !strings.Contains(r.stdout, "http://127.0.0.1:"+port+"/") || strings.Contains(r.stdout+r.stderr, "ld_admin_") {
		t.Fatalf("unexpected output (or a token leaked): %q", r.stdout)
	}
	for _, want := range []string{"pull ghcr.io/jiaweixie/lensdock/runtime:latest", "pull ghcr.io/jiaweixie/lensdock/gateway:latest", "volume ls", "ps -a"} {
		if !f.did(want) {
			t.Errorf("first start did not run %q; log:\n%s", want, f.text())
		}
	}
	pinPath := filepath.Join(state, "product", "images.pin.yaml")
	pin, err := os.ReadFile(pinPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"  runtime:\n    image: \"ghcr.io/jiaweixie/lensdock/runtime@" + testRuntimeDigest + "\"\n",
		"  gateway:\n    image: \"ghcr.io/jiaweixie/lensdock/gateway@" + testGatewayDigest + "\"\n",
		"  config-init:\n    image: \"ghcr.io/jiaweixie/lensdock/gateway@" + testGatewayDigest + "\"\n",
	} {
		if !strings.Contains(string(pin), want) {
			t.Errorf("pin file lacks %q:\n%s", want, pin)
		}
	}
	if fi, _ := os.Stat(pinPath); fi.Mode().Perm() != 0o600 {
		t.Errorf("pin file mode %v", fi.Mode())
	}
	if _, err := os.Stat(filepath.Join(state, "product", startedFile)); err != nil {
		t.Errorf("the first start was not recorded: %v", err)
	}
	// config-init runs before up, both through the deployment's own arguments.
	configInit, up := -1, -1
	for i, l := range f.log {
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
	if r := productStartCLI(t, f, state); r.code != 0 {
		t.Fatalf("second start failed: %s", r.stderr)
	}
	if f.did("pull") || f.did("image inspect") || f.did("volume ls") || f.did("ps -a") {
		t.Fatalf("second start pulled, re-resolved or re-checked the project:\n%s", f.text())
	}
	if again, _ := os.ReadFile(pinPath); !bytes.Equal(pin, again) {
		t.Fatal("the pin changed on a repeated start")
	}
}

func TestProductStartRefusesAnExistingComposeProject(t *testing.T) {
	f := &fakeDocker{project: true}
	state, _ := startFixture(t, f)
	r := productStartCLI(t, f, state)
	if r.code == 0 || !strings.Contains(r.stderr, `Compose project "lensdock-product"`) {
		t.Fatalf("start must refuse an existing project: code %d, %q", r.code, r.stderr)
	}
	if f.did("pull") || strings.Contains(f.text(), "config-init") || strings.Contains(f.text(), " up ") {
		t.Fatalf("a refused start changed Docker state:\n%s", f.text())
	}
	if pin, _ := os.ReadFile(filepath.Join(state, "product", "images.pin.yaml")); string(pin) != pinPlaceholder {
		t.Fatal("a refused start wrote the pin file")
	}
}

func TestProductStartRetriesItsOwnFailedFirstStartButNotAForeignProject(t *testing.T) {
	f := &fakeDocker{failUp: true}
	state, _ := startFixture(t, f)
	if r := productStartCLI(t, f, state); r.code == 0 {
		t.Fatal("the first start was expected to fail")
	}
	if _, err := os.Stat(filepath.Join(state, "product", startedFile)); !os.IsNotExist(err) {
		t.Fatalf("a failed start was recorded: %v", err)
	}
	pin, _ := os.ReadFile(filepath.Join(state, "product", "images.pin.yaml"))
	if !strings.Contains(string(pin), "@"+testRuntimeDigest) {
		t.Fatalf("the failed start did not pin: %s", pin)
	}

	// The failed attempt left the credential volume behind; it carries our CA.
	ca, err := os.ReadFile(filepath.Join(state, "product", "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	f.reset()
	f.failUp, f.project, f.proof = false, true, "someone else's certificate\n"
	if r := productStartCLI(t, f, state); r.code == 0 || f.count("config-init") != 0 {
		t.Fatalf("a project this state did not create was reused: code %d\n%s", r.code, f.text())
	}
	if f.did("pull") {
		t.Fatalf("a retry pulled:\n%s", f.text())
	}

	f.reset()
	f.proof = string(ca)
	if r := productStartCLI(t, f, state); r.code != 0 {
		t.Fatalf("retry of an own first start failed: %s\n%s", r.stderr, f.text())
	}
	if f.did("pull") || f.count("run --rm --pull never") != 1 {
		t.Fatalf("retry must prove ownership once and never pull:\n%s", f.text())
	}
	if _, err := os.Stat(filepath.Join(state, "product", startedFile)); err != nil {
		t.Fatalf("the successful retry was not recorded: %v", err)
	}
}

func TestProductStartFailsInsteadOfKeepingAMutableImage(t *testing.T) {
	f := &fakeDocker{noDigest: true}
	state, _ := startFixture(t, f)
	r := productStartCLI(t, f, state)
	if r.code == 0 || !strings.Contains(r.stderr, "pin ghcr.io/jiaweixie/lensdock/runtime:latest") {
		t.Fatalf("an image that cannot be pinned must fail the start: code %d, %q", r.code, r.stderr)
	}
	if strings.Contains(f.text(), "config-init") || strings.Contains(f.text(), " up ") {
		t.Fatalf("a failed pin continued:\n%s", f.text())
	}
	if pin, _ := os.ReadFile(filepath.Join(state, "product", "images.pin.yaml")); string(pin) != pinPlaceholder {
		t.Fatal("a failed pin wrote the pin file")
	}
}

func TestProductStartPullNeverNeedsLocalImages(t *testing.T) {
	f := &fakeDocker{}
	state, _ := startFixture(t, f)
	r := productStartCLI(t, f, state, "--pull", "never")
	if r.code == 0 || !strings.Contains(r.stderr, "not present locally") || f.did("pull") {
		t.Fatalf("--pull never must not pull and must fail clearly: code %d, %q", r.code, r.stderr)
	}
	if r := productStartCLI(t, f, state, "--pull", "sometimes"); r.code != exitUsage {
		t.Fatalf("invalid --pull exit %d", r.code)
	}
}

func TestVersionCommand(t *testing.T) {
	r := runCLI(t, context.Background(), "", nil, "version")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "lensdock "+version+" (") {
		t.Fatalf("version: %d %q", r.code, r.stdout)
	}
}
