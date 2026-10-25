package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiclient"
	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/auth"
)

type result struct {
	code           int
	stdout, stderr string
}

func runCLI(t *testing.T, ctx context.Context, stdin string, env map[string]string, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	c := &cli{
		stdin: strings.NewReader(stdin), stdout: &out, stderr: &errb,
		getenv: func(k string) string { return env[k] },
	}
	code := c.run(ctx, args)
	return result{code, out.String(), errb.String()}
}

func writeFile(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

func writeLease(t *testing.T, dir, base, mode, token string) string {
	t.Helper()
	path := filepath.Join(dir, "lease-"+strings.ToLower(mode)+".json")
	err := apiclient.WriteLeaseFile(path, apiclient.LeaseFile{
		APIBaseURL: base, WorkspaceID: "default",
		Lease: apiwire.Lease{
			LeaseID: "lse_1", WorkspaceID: "default", HolderID: "operator", Mode: mode, Epoch: 4,
			RuntimeGeneration: "gen-1", ExpiresAt: time.Now().Add(30 * time.Second),
		},
		CapabilityToken: auth.Secret(token),
	})
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func apiJSONError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiwire.Error{Code: code, Message: "test", RequestID: "r1"})
}

func TestUsageErrors(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "tok")
	writeFile(t, tokenFile, "ldo_x\n", 0o600)
	for name, args := range map[string][]string{
		"no args":               {},
		"unknown command":       {"frobnicate"},
		"missing subcommand":    {"lease"},
		"unknown subcommand":    {"lease", "frobnicate"},
		"unknown flag":          {"workspace", "status", "default", "--nope"},
		"missing workspace":     {"workspace", "status", "--operator-token-file", tokenFile},
		"missing token file":    {"workspace", "status", "default"},
		"bad mode":              {"lease", "acquire", "default", "--mode", "root", "--operator-token-file", tokenFile, "--output", filepath.Join(dir, "o")},
		"bad ttl":               {"lease", "acquire", "default", "--mode", "agent", "--ttl", "0", "--operator-token-file", tokenFile, "--output", filepath.Join(dir, "o")},
		"ttl below minimum":     {"lease", "acquire", "default", "--mode", "agent", "--ttl", "9", "--operator-token-file", tokenFile, "--output", filepath.Join(dir, "o")},
		"ttl above maximum":     {"lease", "acquire", "default", "--mode", "agent", "--ttl", "121", "--operator-token-file", tokenFile, "--output", filepath.Join(dir, "o")},
		"ttl far above maximum": {"lease", "acquire", "default", "--mode", "agent", "--ttl", "300", "--operator-token-file", tokenFile, "--output", filepath.Join(dir, "o")},
		"output dir missing":    {"lease", "acquire", "default", "--mode", "agent", "--operator-token-file", tokenFile, "--output", filepath.Join(dir, "missing", "o")},
		"missing output":        {"lease", "acquire", "default", "--mode", "agent", "--operator-token-file", tokenFile},
		"release no target":     {"lease", "release"},
		"release file + id":     {"lease", "release", "--lease-file", "x", "--lease-id", "y"},
		"insecure api":          {"workspace", "status", "default", "--api", "http://example.com:8080", "--operator-token-file", tokenFile},
		"init without dir":      {"admin", "init"},
		"bridge without lease":  {"cdp", "bridge"},
	} {
		if r := runCLI(t, context.Background(), "", nil, args...); r.code != exitUsage {
			t.Errorf("%s: exit %d, want %d (stderr %q)", name, r.code, exitUsage, r.stderr)
		}
	}
}

func TestAdminInit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	r := runCLI(t, context.Background(), "", nil, "admin", "init", "--state-dir", dir)
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	tokenPath, envPath := filepath.Join(dir, "operator-token"), filepath.Join(dir, "runtime.env")
	for _, p := range []string{tokenPath, envPath} {
		fi, err := os.Stat(p)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v mode %v", p, err, fi)
		}
	}
	tok, _ := os.ReadFile(tokenPath)
	token := strings.TrimSpace(string(tok))
	if !strings.HasPrefix(token, "ldo_") {
		t.Errorf("operator token prefix: %q", token[:min(4, len(token))])
	}
	env, _ := os.ReadFile(envPath)
	if want := "LENSDOCK_OPERATOR_TOKEN_SHA256=" + auth.DigestOf(token).Hex() + "\n"; string(env) != want {
		t.Errorf("runtime.env = %q, want digest line", env)
	}
	if strings.Contains(r.stdout+r.stderr, token) {
		t.Error("admin init printed the operator token")
	}

	again := runCLI(t, context.Background(), "", nil, "admin", "init", "--state-dir", dir)
	if again.code == 0 {
		t.Error("second admin init overwrote existing credentials")
	}
	if after, _ := os.ReadFile(tokenPath); string(after) != string(tok) {
		t.Error("existing operator token was modified")
	}
}

func TestExitCodesFollowAPIErrors(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "tok")
	writeFile(t, tokenFile, "ldo_x\n", 0o600)
	var status atomic.Int32
	var code atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiJSONError(w, int(status.Load()), code.Load().(string))
	}))
	cases := []struct {
		status int
		code   string
		want   int
	}{
		{401, apiwire.CodeUnauthenticated, exitLease},
		{404, apiwire.CodeNotFound, exitLease},
		{412, apiwire.CodeEpochMismatch, exitLease},
		{409, apiwire.CodeLeaseEnded, exitLease},
		{409, apiwire.CodeLeaseConflict, exitFailure},
		{500, apiwire.CodeInternal, exitFailure},
	}
	for _, c := range cases {
		status.Store(int32(c.status))
		code.Store(c.code)
		r := runCLI(t, context.Background(), "", nil, "workspace", "status", "default", "--api", srv.URL, "--operator-token-file", tokenFile)
		if r.code != c.want {
			t.Errorf("%d %s: exit %d, want %d", c.status, c.code, r.code, c.want)
		}
		if strings.Contains(r.stderr, "ldo_x") {
			t.Error("stderr contains the operator token")
		}
	}
	srv.Close()
	r := runCLI(t, context.Background(), "", nil, "workspace", "status", "default", "--api", srv.URL, "--operator-token-file", tokenFile)
	if r.code != exitUnreachable {
		t.Errorf("closed server: exit %d, want %d", r.code, exitUnreachable)
	}
}

func TestOperatorTokenFileMustBePrivate(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "tok")
	writeFile(t, tokenFile, "ldo_x\n", 0o644)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer srv.Close()
	r := runCLI(t, context.Background(), "", nil, "workspace", "status", "default", "--api", srv.URL, "--operator-token-file", tokenFile)
	if r.code == 0 || calls.Load() != 0 {
		t.Errorf("exit %d, %d calls: group-readable token file was used", r.code, calls.Load())
	}
}

func TestLeaseAcquireReleaseFlow(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "tok")
	writeFile(t, tokenFile, "ldo_operator\n", 0o600)

	var mu sync.Mutex
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/workspaces/default/leases":
			if r.Header.Get("Idempotency-Key") == "" {
				apiJSONError(w, 400, apiwire.CodeInvalidRequest)
				return
			}
			var body apiwire.AcquireLease
			if err := apiwire.Decode(r.Body, 1024, &body); err != nil || body.Mode != "AGENT" || body.TTLSeconds == nil || *body.TTLSeconds != 45 {
				apiJSONError(w, 400, apiwire.CodeInvalidRequest)
				return
			}
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(apiwire.LeaseGrant{
				Lease:           apiwire.Lease{LeaseID: "lse_9", WorkspaceID: "default", HolderID: "operator", Mode: "AGENT", Epoch: 2, RuntimeGeneration: "g", ExpiresAt: time.Now().Add(45 * time.Second)},
				CapabilityToken: "ldl_secret-lease-token",
			})
		case r.Method == http.MethodDelete:
			w.WriteHeader(204)
		default:
			apiJSONError(w, 404, apiwire.CodeNotFound)
		}
	}))
	defer srv.Close()
	leasePath := filepath.Join(dir, "lease.json")

	r := runCLI(t, context.Background(), "", map[string]string{"LENSDOCK_API": srv.URL},
		"lease", "acquire", "default", "--mode", "agent", "--ttl", "45", "--operator-token-file", tokenFile, "--output", leasePath)
	if r.code != 0 {
		t.Fatalf("acquire exit %d: %s", r.code, r.stderr)
	}
	if strings.Contains(r.stdout+r.stderr, "ldl_secret-lease-token") || strings.Contains(r.stdout+r.stderr, "ldo_operator") {
		t.Error("acquire printed a token")
	}
	lf, err := apiclient.ReadLeaseFile(leasePath)
	if err != nil {
		t.Fatalf("lease file: %v", err)
	}
	if lf.CapabilityToken != "ldl_secret-lease-token" || lf.Lease.LeaseID != "lse_9" || lf.WorkspaceID != "default" || lf.APIBaseURL != srv.URL {
		t.Errorf("lease file = %+v", lf)
	}

	// A second acquire to the same output must fail without contacting the runtime.
	mu.Lock()
	before := len(requests)
	mu.Unlock()
	if r := runCLI(t, context.Background(), "", map[string]string{"LENSDOCK_API": srv.URL},
		"lease", "acquire", "default", "--mode", "agent", "--operator-token-file", tokenFile, "--output", leasePath); r.code == 0 {
		t.Error("acquire overwrote an existing lease file")
	}
	mu.Lock()
	if len(requests) != before {
		t.Error("acquire contacted the runtime although the output file exists")
	}
	mu.Unlock()

	if r := runCLI(t, context.Background(), "", nil, "lease", "release", "--lease-file", leasePath); r.code != 0 {
		t.Fatalf("release by lease file: exit %d: %s", r.code, r.stderr)
	}
	if r := runCLI(t, context.Background(), "", map[string]string{"LENSDOCK_API": srv.URL},
		"lease", "release", "default", "--lease-id", "lse_9", "--operator-token-file", tokenFile); r.code != 0 {
		t.Fatalf("operator release: exit %d: %s", r.code, r.stderr)
	}
	mu.Lock()
	defer mu.Unlock()
	got := requests[len(requests)-2:]
	if got[0] != "DELETE /api/v1/workspaces/default/leases/lse_9 Bearer ldl_secret-lease-token" ||
		got[1] != "DELETE /api/v1/workspaces/default/leases/lse_9 Bearer ldo_operator" {
		t.Errorf("release requests = %q", got)
	}
}

func TestLeaseAcquireRejectsMissingOutputDirectoryBeforeAcquiring(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "tok")
	writeFile(t, tokenFile, "ldo_operator\n", 0o600)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer srv.Close()

	missing := filepath.Join(dir, "no-such-dir", "lease.json")
	r := runCLI(t, context.Background(), "", nil, "lease", "acquire", "default", "--mode", "agent",
		"--api", srv.URL, "--operator-token-file", tokenFile, "--output", missing)
	if r.code != exitUsage {
		t.Errorf("exit %d, want %d (stderr %q)", r.code, exitUsage, r.stderr)
	}
	if calls.Load() != 0 {
		t.Errorf("runtime was contacted %d times although the output directory is missing", calls.Load())
	}
}

// TestLeaseAcquireReleasesTheLeaseWhenTheFileCannotBeWritten covers the race
// the pre-check cannot close: the output appears between the pre-check and
// the write. The token would be lost, so the lease must not stay held.
func TestLeaseAcquireReleasesTheLeaseWhenTheFileCannotBeWritten(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "tok")
	writeFile(t, tokenFile, "ldo_operator\n", 0o600)
	leasePath := filepath.Join(dir, "lease.json")

	var mu sync.Mutex
	var deletes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/workspaces/default/leases":
			// A competing writer creates the output after the CLI's pre-check.
			if err := os.WriteFile(leasePath, []byte("someone else's file"), 0o600); err != nil {
				apiJSONError(w, 500, apiwire.CodeInternal)
				return
			}
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(apiwire.LeaseGrant{
				Lease:           apiwire.Lease{LeaseID: "lse_9", WorkspaceID: "default", HolderID: "operator", Mode: "AGENT", Epoch: 2, RuntimeGeneration: "g", ExpiresAt: time.Now().Add(30 * time.Second)},
				CapabilityToken: "ldl_secret-lease-token",
			})
		case r.Method == http.MethodDelete:
			mu.Lock()
			deletes = append(deletes, r.URL.Path+" "+r.Header.Get("Authorization"))
			mu.Unlock()
			w.WriteHeader(204)
		default:
			apiJSONError(w, 404, apiwire.CodeNotFound)
		}
	}))
	defer srv.Close()

	r := runCLI(t, context.Background(), "", nil, "lease", "acquire", "default", "--mode", "agent",
		"--api", srv.URL, "--operator-token-file", tokenFile, "--output", leasePath)
	if r.code == 0 {
		t.Fatalf("acquire succeeded although the lease file could not be written; stdout %q", r.stdout)
	}
	if r.stdout != "" || strings.Contains(r.stderr, "ldl_secret-lease-token") {
		t.Errorf("stdout %q stderr %q: want no output and no token", r.stdout, r.stderr)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := "/api/v1/workspaces/default/leases/lse_9 Bearer ldl_secret-lease-token"; len(deletes) != 1 || deletes[0] != want {
		t.Errorf("release requests = %q, want exactly %q", deletes, want)
	}
	if got, _ := os.ReadFile(leasePath); string(got) != "someone else's file" {
		t.Errorf("existing output was modified: %q", got)
	}
}

func TestLeaseReleaseByFileUsesAPIOverride(t *testing.T) {
	type hit struct{ method, path, auth string }
	newServer := func(t *testing.T) (*httptest.Server, func() []hit) {
		var mu sync.Mutex
		var hits []hit
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits = append(hits, hit{r.Method, r.URL.Path, r.Header.Get("Authorization")})
			mu.Unlock()
			w.WriteHeader(204)
		}))
		t.Cleanup(srv.Close)
		return srv, func() []hit {
			mu.Lock()
			defer mu.Unlock()
			return append([]hit(nil), hits...)
		}
	}
	want := hit{"DELETE", "/api/v1/workspaces/default/leases/lse_1", "Bearer ldl_agent"}

	t.Run("flag overrides the lease file's URL", func(t *testing.T) {
		fileSrv, fileHits := newServer(t)
		overrideSrv, overrideHits := newServer(t)
		leasePath := writeLease(t, t.TempDir(), fileSrv.URL, "AGENT", "ldl_agent")
		r := runCLI(t, context.Background(), "", nil, "lease", "release", "--lease-file", leasePath, "--api", overrideSrv.URL)
		if r.code != 0 {
			t.Fatalf("exit %d: %s", r.code, r.stderr)
		}
		if got := overrideHits(); len(got) != 1 || got[0] != want {
			t.Errorf("override server hits = %v, want [%v]", got, want)
		}
		if got := fileHits(); len(got) != 0 {
			t.Errorf("lease file's server was contacted despite --api: %v", got)
		}
	})
	t.Run("LENSDOCK_API does not override the lease file's URL", func(t *testing.T) {
		fileSrv, fileHits := newServer(t)
		envSrv, envHits := newServer(t)
		leasePath := writeLease(t, t.TempDir(), fileSrv.URL, "AGENT", "ldl_agent")
		r := runCLI(t, context.Background(), "", map[string]string{"LENSDOCK_API": envSrv.URL}, "lease", "release", "--lease-file", leasePath)
		if r.code != 0 {
			t.Fatalf("exit %d: %s", r.code, r.stderr)
		}
		if got := fileHits(); len(got) != 1 || got[0] != want {
			t.Errorf("lease file's server hits = %v, want [%v]", got, want)
		}
		if got := envHits(); len(got) != 0 {
			t.Errorf("$LENSDOCK_API server was contacted: %v", got)
		}
	})
}

func TestClipboardSetSendsStdinVerbatim(t *testing.T) {
	text := "e\u0301 \U0001F468\u200D\U0001F469 \u7e41\u9ad4\r\n  tail \n"
	var gotBody []byte
	var gotAuth, gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotAuth, gotType = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		w.WriteHeader(204)
	}))
	defer srv.Close()
	leasePath := writeLease(t, t.TempDir(), srv.URL, "HUMAN", "ldl_human")
	r := runCLI(t, context.Background(), text, nil, "clipboard", "set", "--lease-file", leasePath)
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if string(gotBody) != text || gotAuth != "Bearer ldl_human" || gotType != "text/plain; charset=utf-8" {
		t.Errorf("request: body %q auth %q type %q", gotBody, gotAuth, gotType)
	}

	big := strings.Repeat("a", 64*1024+1)
	if r := runCLI(t, context.Background(), big, nil, "clipboard", "set", "--lease-file", leasePath); r.code == 0 {
		t.Error("oversized clipboard text was sent")
	}
}

func TestBridgeRefusesUnsafeInputsBeforeNetwork(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer srv.Close()
	dir := t.TempDir()
	agent := writeLease(t, dir, srv.URL, "AGENT", "ldl_agent")
	human := writeLease(t, dir, srv.URL, "HUMAN", "ldl_human")

	for _, listen := range []string{"0.0.0.0:0", ":0", "192.168.1.5:9222", "[::]:0", "[::1]:0", "example.com:9222", "127.0.0.2:0", "127.0.0.1", "127.0.0.1:99999", "127.0.0.1.evil.test:80"} {
		r := runCLI(t, context.Background(), "", nil, "cdp", "bridge", "--lease-file", agent, "--listen", listen)
		if r.code != exitUsage {
			t.Errorf("--listen %q: exit %d, want %d", listen, r.code, exitUsage)
		}
	}
	if r := runCLI(t, context.Background(), "", nil, "cdp", "bridge", "--lease-file", human); r.code != exitUsage {
		t.Errorf("HUMAN lease: exit %d, want %d", r.code, exitUsage)
	}

	open := filepath.Join(dir, "open.json")
	data, _ := os.ReadFile(agent)
	writeFile(t, open, string(data), 0o644)
	if r := runCLI(t, context.Background(), "", nil, "cdp", "bridge", "--lease-file", open); r.code != exitLease {
		t.Errorf("group-readable lease file: exit %d, want %d", r.code, exitLease)
	}
	if calls.Load() != 0 {
		t.Errorf("runtime was contacted %d times before validation finished", calls.Load())
	}
}

// fakeRuntime serves the endpoints the bridge command needs.
type fakeRuntime struct {
	renewStatus atomic.Int32 // 0 = ok
	renews      atomic.Int32
	srv         *httptest.Server
}

func newFakeRuntime(t *testing.T) *fakeRuntime {
	f := &fakeRuntime{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/workspaces/default/leases/lse_1/renew":
			if r.Header.Get("Authorization") != "Bearer ldl_agent" {
				apiJSONError(w, 401, apiwire.CodeUnauthenticated)
				return
			}
			f.renews.Add(1)
			if st := f.renewStatus.Load(); st != 0 {
				apiJSONError(w, int(st), apiwire.CodeLeaseEnded)
				return
			}
			_ = json.NewEncoder(w).Encode(apiwire.Lease{LeaseID: "lse_1", ExpiresAt: time.Now().Add(10 * time.Second)})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/workspaces/default/cdp/json/version":
			if r.Header.Get("Authorization") != "Bearer ldl_agent" {
				apiJSONError(w, 401, apiwire.CodeUnauthenticated)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"Browser":"Chrome/1","webSocketDebuggerUrl":"ws://runtime.invalid/api/v1/workspaces/default/cdp/devtools/browser/abc-123"}`)
		default:
			apiJSONError(w, 404, apiwire.CodeNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func TestBridgeStartsPrintsEndpointsAndStopsOnSignal(t *testing.T) {
	const wsID = "/devtools/browser/abc-123"
	for _, tt := range []struct {
		name       string
		args       []string
		secretPath bool
	}{
		{"root mode", nil, false},
		{"path secret", []string{"--path-secret"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRuntime(t)
			dir := t.TempDir()
			leasePath := writeLease(t, dir, f.srv.URL, "AGENT", "ldl_agent")
			urlFile := filepath.Join(dir, "bridge.json")

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan result, 1)
			args := append([]string{"cdp", "bridge", "--lease-file", leasePath, "--url-file", urlFile}, tt.args...)
			go func() { done <- runCLI(t, ctx, "", nil, args...) }()

			var line []byte
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				line, _ = os.ReadFile(urlFile)
				if bytes.HasSuffix(line, []byte("\n")) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !bytes.HasSuffix(line, []byte("\n")) {
				cancel()
				r := <-done
				t.Fatalf("bridge did not publish endpoints; exit %d stderr %q", r.code, r.stderr)
			}
			var out bridgeOutput
			if err := json.Unmarshal(line, &out); err != nil {
				t.Fatalf("output is not one JSON object: %v", err)
			}
			disc, err := url.Parse(out.DiscoveryURL)
			if err != nil {
				t.Fatal(err)
			}
			ws, err := url.Parse(out.BrowserWebSocketURL)
			if err != nil {
				t.Fatal(err)
			}
			if out.LeaseID != "lse_1" || disc.Scheme != "http" || ws.Scheme != "ws" ||
				disc.Host != ws.Host || !strings.HasPrefix(disc.Host, "127.0.0.1:") {
				t.Errorf("output = %+v", out)
			}
			if tt.secretPath {
				secret := strings.TrimPrefix(disc.Path, "/")
				if len(secret) != 64 || strings.Trim(secret, "0123456789abcdef") != "" || ws.Path != disc.Path+wsID {
					t.Errorf("path-secret output = %+v", out)
				}
			} else if disc.Path != "" || ws.Path != wsID {
				t.Errorf("root-mode output has a path prefix: %+v", out)
			}

			// The published discovery URL answers on the running bridge.
			resp, err := http.Get(out.DiscoveryURL + "/json/version")
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || !strings.Contains(string(body), out.BrowserWebSocketURL) {
				t.Errorf("GET /json/version = %d %s, want 200 naming %s", resp.StatusCode, body, out.BrowserWebSocketURL)
			}

			cancel()
			var r result
			select {
			case r = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("bridge did not stop after cancellation")
			}
			if r.code != 0 {
				t.Fatalf("exit %d, want 0: %s", r.code, r.stderr)
			}
			if r.stdout != string(line) {
				t.Errorf("stdout %q differs from url file %q", r.stdout, line)
			}
			fi, err := os.Stat(urlFile)
			if err != nil || fi.Mode().Perm() != 0o600 {
				t.Errorf("url file: %v %v", err, fi)
			}
			if strings.Contains(r.stdout+r.stderr, "ldl_agent") {
				t.Error("bridge printed the lease token")
			}
			if f.renews.Load() < 1 {
				t.Error("bridge never renewed the lease")
			}
		})
	}
}

func TestBridgeExitsWithLeaseCodeWhenLeaseEnds(t *testing.T) {
	t.Run("ended before start", func(t *testing.T) {
		f := newFakeRuntime(t)
		f.renewStatus.Store(409)
		leasePath := writeLease(t, t.TempDir(), f.srv.URL, "AGENT", "ldl_agent")
		r := runCLI(t, context.Background(), "", nil, "cdp", "bridge", "--lease-file", leasePath)
		if r.code != exitLease || r.stdout != "" {
			t.Errorf("exit %d stdout %q, want %d and no output", r.code, r.stdout, exitLease)
		}
	})
	t.Run("ended while running", func(t *testing.T) {
		if testing.Short() {
			t.Skip("waits for the first renew tick (~3s)")
		}
		f := newFakeRuntime(t)
		leasePath := writeLease(t, t.TempDir(), f.srv.URL, "AGENT", "ldl_agent")
		done := make(chan result, 1)
		go func() { done <- runCLI(t, context.Background(), "", nil, "cdp", "bridge", "--lease-file", leasePath) }()
		deadline := time.Now().Add(10 * time.Second)
		for f.renews.Load() < 1 {
			if time.Now().After(deadline) {
				t.Fatal("bridge did not renew the lease within 10s")
			}
			time.Sleep(5 * time.Millisecond)
		}
		f.renewStatus.Store(409)
		select {
		case r := <-done:
			if r.code != exitLease {
				t.Errorf("exit %d, want %d (stderr %q)", r.code, exitLease, r.stderr)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("bridge kept running after the lease ended")
		}
	})
}
