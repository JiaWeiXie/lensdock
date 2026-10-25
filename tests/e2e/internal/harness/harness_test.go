package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTB records failures instead of aborting, so Require can be tested.
type fakeTB struct {
	failed bool
	msg    string
}

func (f *fakeTB) Helper()                   {}
func (f *fakeTB) Logf(string, ...any)       {}
func (f *fakeTB) Cleanup(func())            {}
func (f *fakeTB) Fatalf(s string, a ...any) { f.failed, f.msg = true, fmt.Sprintf(s, a...) }

func TestRequireFailsOnMissingVariable(t *testing.T) {
	t.Setenv("LENSDOCK_E2E_TEST_PRESENT", "x")
	t.Setenv("LENSDOCK_E2E_TEST_EMPTY", "")
	f := &fakeTB{}
	if v := Require(f, "LENSDOCK_E2E_TEST_PRESENT"); v != "x" || f.failed {
		t.Fatalf("Require present = %q, failed=%v", v, f.failed)
	}
	Require(f, "LENSDOCK_E2E_TEST_EMPTY")
	if !f.failed || !strings.Contains(f.msg, "LENSDOCK_E2E_TEST_EMPTY") {
		t.Fatalf("empty variable did not fail the test: failed=%v msg=%q", f.failed, f.msg)
	}
}

func TestEvidenceRecordsSha256AndRefusesEscapesAndOverwrites(t *testing.T) {
	dir := t.TempDir()
	e, err := NewEvidenceIn(dir)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("hello 繁體")
	digest, err := e.Write("TestX/a.txt", data)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	sum := sha256.Sum256(data)
	if digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("digest = %s", digest)
	}
	got, err := os.ReadFile(filepath.Join(dir, "TestX", "a.txt"))
	if err != nil || string(got) != string(data) {
		t.Fatalf("stored file = %q, %v", got, err)
	}
	manifest, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil || string(manifest) != digest+"  TestX/a.txt\n" {
		t.Fatalf("manifest = %q, %v", manifest, err)
	}
	for _, bad := range []string{"", "../x", "/abs", "a/../../x", "a//b", ".", ManifestName} {
		if _, err := e.Write(bad, data); err == nil {
			t.Errorf("Write(%q) succeeded", bad)
		}
	}
	if _, err := e.Write("TestX/a.txt", []byte("other")); err == nil {
		t.Error("evidence was overwritten")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "TestX", "a.txt")); string(got) != string(data) {
		t.Error("overwrite attempt changed the stored evidence")
	}
}

func TestParseProcSnapshot(t *testing.T) {
	snap := "=== 1\ncmdline: /usr/bin/tini -g -- /usr/local/bin/lensdock-runtime supervise \nName:\ttini\nUid:\t0\t0\t0\t0\n" +
		"=== 42\ncmdline: /usr/lib/chromium/chromium --user-data-dir=/data/profile \nName:\tchromium\nUid:\t10001\t10001\t10001\t10001\nSeccomp:\t0\nNSpid:\t42\n" +
		"=== 77\ncmdline: /usr/lib/chromium/chromium --type=renderer --lang=zh-TW \nUid:\t10001\t10001\t10001\t10001\nSeccomp:\t2\nNSpid:\t77\t5\n"
	procs, err := ParseProcSnapshot(snap)
	if err != nil || len(procs) != 3 {
		t.Fatalf("ParseProcSnapshot = %d procs, %v", len(procs), err)
	}
	main, ok := ChromiumMain(procs)
	if !ok || main.PID != 42 || main.RealUID() != 10001 {
		t.Fatalf("ChromiumMain = %+v, %v", main, ok)
	}
	if procs[2].ChromiumType() != "renderer" || procs[2].Fields["Seccomp"] != "2" || procs[2].NSpidDepth() != 2 {
		t.Fatalf("renderer parsed as %+v", procs[2])
	}
	if procs[0].IsChromiumMain() || procs[0].RealUID() != 0 {
		t.Fatalf("tini parsed as %+v", procs[0])
	}
	if _, err := ParseProcSnapshot(""); err == nil {
		t.Fatal("empty snapshot accepted")
	}
	if _, err := ParseProcSnapshot("=== x\n"); err == nil {
		t.Fatal("non-numeric pid accepted")
	}
}

func fakeRuntime(t *testing.T, h http.HandlerFunc) *API {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &API{Base: srv.URL, Workspace: "default", Operator: "ldo_operator", HC: srv.Client()}
}

func TestAcquireSendsOperatorTokenIdempotencyKeyAndDecodesGrant(t *testing.T) {
	var gotAuth, gotKey, gotBody string
	a := fakeRuntime(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotKey = r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key")
		b := new(strings.Builder)
		_, _ = fmt.Fprint(b, r.URL.Path, " ")
		dec := json.NewDecoder(r.Body)
		var m map[string]any
		_ = dec.Decode(&m)
		j, _ := json.Marshal(m)
		b.Write(j)
		gotBody = b.String()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"lease":{"leaseId":"l1","workspaceId":"default","holderId":"operator","mode":"AGENT","epoch":3,"runtimeGeneration":"g","expiresAt":"2026-01-01T00:00:00Z"},"capabilityToken":"ldl_tok"}`))
	})
	g, err := a.Acquire(context.Background(), "AGENT", 10)
	if err != nil || g.Token != "ldl_tok" || g.Lease.Epoch != 3 {
		t.Fatalf("Acquire = %+v, %v", g, err)
	}
	if gotAuth != "Bearer ldo_operator" || !strings.HasPrefix(gotKey, "e2e-") {
		t.Fatalf("auth %q key %q", gotAuth, gotKey)
	}
	if gotBody != `/api/v1/workspaces/default/leases {"mode":"AGENT","ttlSeconds":10}` {
		t.Fatalf("request = %s", gotBody)
	}
}

func TestAcquireWhenFreeRetriesOnlyRetryableCodes(t *testing.T) {
	var calls atomic.Int32
	a := fakeRuntime(t, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		switch {
		case n < 3:
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"LEASE_CONFLICT","message":"held","requestId":"r"}`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"UNAUTHENTICATED","message":"no","requestId":"r"}`))
		}
	})
	_, err := a.AcquireWhenFree(context.Background(), &fakeTB{}, "AGENT", 0, 30*time.Second)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 401 || ae.Code != "UNAUTHENTICATED" {
		t.Fatalf("error = %v, want the non-retryable 401", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3 (two conflicts then the 401)", calls.Load())
	}
}

func TestPutClipboardSendsExactBytesAsUTF8Plain(t *testing.T) {
	var gotType string
	var gotBody []byte
	a := fakeRuntime(t, func(w http.ResponseWriter, r *http.Request) {
		gotType = r.Header.Get("Content-Type")
		gotBody = make([]byte, 64)
		n, _ := r.Body.Read(gotBody)
		gotBody = gotBody[:n]
		w.WriteHeader(http.StatusNoContent)
	})
	text := []byte("e\u0301 👩‍💻\n")
	if err := a.PutClipboard(context.Background(), "ldl_h", text); err != nil {
		t.Fatalf("PutClipboard: %v", err)
	}
	if gotType != "text/plain; charset=utf-8" || string(gotBody) != string(text) {
		t.Fatalf("content type %q body %q", gotType, gotBody)
	}
}

func TestStartBridgeParsesEndpointLineAndReportsExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake")
	}
	script := filepath.Join(t.TempDir(), "lensdock")
	body := "#!/bin/sh\n" +
		`printf '{"discoveryUrl":"http://127.0.0.1:1/p","browserWebSocketUrl":"ws://127.0.0.1:1/p/devtools/browser/b","leaseId":"l"}\n'` + "\n" +
		"sleep 0.2\nexit 3\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	c := CLI{Bin: script}
	b, err := c.StartBridge(context.Background(), "unused", 5*time.Second, false)
	if err != nil {
		t.Fatalf("StartBridge: %v", err)
	}
	if b.Endpoints.LeaseID != "l" || !strings.HasPrefix(b.Endpoints.DiscoveryURL, "http://127.0.0.1:1/") {
		t.Fatalf("endpoints = %+v", b.Endpoints)
	}
	code, err := b.WaitExit(5 * time.Second)
	if err != nil || code != 3 {
		t.Fatalf("WaitExit = %d, %v; want 3", code, err)
	}
}

func TestStartBridgePassesPathSecretFlagOnlyWhenAsked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake")
	}
	script := filepath.Join(t.TempDir(), "lensdock")
	// The fake reports its own argument vector as the lease id.
	body := "#!/bin/sh\n" +
		`printf '{"discoveryUrl":"http://127.0.0.1:1","browserWebSocketUrl":"ws://127.0.0.1:1/devtools/browser/b","leaseId":"%s"}\n' "$*"` + "\n" +
		"sleep 0.2\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, withSecret := range []bool{false, true} {
		b, err := CLI{Bin: script}.StartBridge(context.Background(), "lease.json", 5*time.Second, withSecret)
		if err != nil {
			t.Fatalf("StartBridge(pathSecret=%v): %v", withSecret, err)
		}
		if got := strings.Contains(b.Endpoints.LeaseID, "--path-secret"); got != withSecret {
			t.Errorf("pathSecret=%v: argv %q, --path-secret present = %v", withSecret, b.Endpoints.LeaseID, got)
		}
		b.Kill()
	}
}

func TestStartBridgeFailsWhenProcessExitsWithoutEndpoints(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake")
	}
	script := filepath.Join(t.TempDir(), "lensdock")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho refused >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := CLI{Bin: script}.StartBridge(context.Background(), "unused", 5*time.Second, false)
	if err == nil || !strings.Contains(err.Error(), "exited 2") {
		t.Fatalf("error = %v, want exit code 2 reported", err)
	}
}
