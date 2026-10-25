package product

import (
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

func writeFile(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

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

// releaseOptions returns the options of a release deployment in a fresh state
// directory.
func releaseOptions(t *testing.T, origin string) InitOptions {
	t.Helper()
	b, err := OpenBundle(writeBundle(t, "1.2.3"))
	if err != nil {
		t.Fatal(err)
	}
	if origin == "" {
		origin = "http://127.0.0.1:18581"
	}
	runtime, gateway := DefaultImages(b)
	return InitOptions{
		StateDir: filepath.Join(t.TempDir(), "state"), Origin: origin, WorkspaceID: DefaultWorkspace,
		Project: "lensdock-smoke", RuntimeImage: runtime, GatewayImage: gateway, Bundle: b,
	}
}

// fakeDocker answers the docker commands Start issues and records every one
// with the binary name and environment it was started with.
type fakeDocker struct {
	mu       sync.Mutex
	cmds     []update.Cmd
	pulled   map[string]bool
	project  bool   // the Compose project already has volumes
	proof    string // what the credential volume's ca.crt holds
	failUp   bool
	noDigest bool // images carry no registry digest
}

func (f *fakeDocker) lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.cmds))
	for i, c := range f.cmds {
		out[i] = strings.Join(c.Args, " ")
	}
	return out
}

func (f *fakeDocker) did(prefix string) bool {
	for _, l := range f.lines() {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeDocker) count(substr string) int {
	n := 0
	for _, l := range f.lines() {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

func (f *fakeDocker) text() string { return strings.Join(f.lines(), "\n") }

func (f *fakeDocker) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = nil
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
	f.cmds = append(f.cmds, c)
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
			out("lensdock-smoke_gateway-credentials\nlensdock-smoke_runtime-credentials\n")
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

// startFixture initializes a release deployment whose Gateway readiness URL
// is answered by a local server, and returns it with the Docker fake.
func startFixture(t *testing.T) (*update.Deployment, *fakeDocker) {
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
	dep, err := Initialize(releaseOptions(t, "http://127.0.0.1:"+port))
	if err != nil {
		t.Fatal(err)
	}
	return dep, &fakeDocker{pulled: map[string]bool{}}
}
