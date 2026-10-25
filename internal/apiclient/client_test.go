package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/auth"
)

func TestNewBaseURLValidation(t *testing.T) {
	good := []string{
		"http://127.0.0.1:18080", "http://localhost:8080", "http://[::1]:8080",
		"https://lensdock.example.com", "https://10.0.0.5:8443", "http://127.0.0.1:18080/",
	}
	for _, base := range good {
		if _, err := New(base, nil); err != nil {
			t.Errorf("New(%q) = %v, want ok", base, err)
		}
	}
	bad := []string{
		"http://example.com:8080", "http://10.0.0.5:8080", "http://127.0.0.1",
		"http://127.0.0.1.evil.test:8080", "ftp://127.0.0.1:8080", "http://user:pw@127.0.0.1:8080",
		"http://127.0.0.1:8080/api", "http://127.0.0.1:8080?x=1", "127.0.0.1:8080", "",
	}
	for _, base := range bad {
		if _, err := New(base, nil); err == nil {
			t.Errorf("New(%q) accepted an unsafe base URL", base)
		}
	}
}

func TestTerminalClassification(t *testing.T) {
	cases := []struct {
		status int
		code   string
		want   bool
	}{
		{401, apiwire.CodeUnauthenticated, true},
		{403, apiwire.CodeWrongLeaseMode, true},
		{404, apiwire.CodeNotFound, true},
		{409, apiwire.CodeLeaseEnded, true},
		{409, apiwire.CodeAgentModeDisabled, true},
		{409, apiwire.CodeAgentPaused, true},
		{412, apiwire.CodeEpochMismatch, true},
		{412, apiwire.CodeGenerationMismatch, true},
		{409, apiwire.CodeLeaseConflict, false},
		{409, apiwire.CodeRuntimeNotReady, false},
		{400, apiwire.CodeInvalidRequest, false},
		{429, apiwire.CodeTooManyConnections, false},
		{502, apiwire.CodeUpstreamUnavailable, false},
		{500, apiwire.CodeInternal, false},
	}
	for _, c := range cases {
		e := &APIError{Status: c.status, Code: c.code}
		if got := e.Terminal(); got != c.want {
			t.Errorf("Terminal(%d %s) = %v, want %v", c.status, c.code, got, c.want)
		}
	}
}

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestErrorDecoding(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "hdr-id")
		w.WriteHeader(http.StatusPreconditionFailed)
		_ = json.NewEncoder(w).Encode(apiwire.Error{Code: apiwire.CodeEpochMismatch, Message: "epoch mismatch", RequestID: "body-id"})
	})
	_, err := c.RenewLease(ctx, "default", "lse_1", "ldl_token", 1, "gen")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v is not *APIError", err)
	}
	if apiErr.Status != 412 || apiErr.Code != apiwire.CodeEpochMismatch || apiErr.RequestID != "body-id" || !apiErr.Terminal() {
		t.Errorf("decoded error = %+v", apiErr)
	}
	if strings.Contains(err.Error(), "ldl_token") {
		t.Error("error text contains the bearer token")
	}
}

func TestNonJSONErrorBodyIsNotEchoed(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "<html>upstream secret detail</html>")
	})
	err := c.ReleaseLease(context.Background(), "default", "lse_1", "ldo_op")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 502 || apiErr.Code != "" {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), "secret detail") {
		t.Error("non-JSON error body leaked into the error text")
	}
}

func TestUnreachableRuntime(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close()
	c, err := New(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Workspace(context.Background(), "default", "ldo_op")
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("error = %v, want ErrUnreachable", err)
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	var followed bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed = true }))
	defer target.Close()
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	})
	err := c.ReleaseLease(context.Background(), "default", "lse_1", "ldo_op")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusTemporaryRedirect {
		t.Fatalf("error = %v, want redirect APIError", err)
	}
	if followed {
		t.Error("client followed a redirect and would have forwarded the token")
	}
}

func TestLeaseFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lease.json")
	in := LeaseFile{
		APIBaseURL:  "http://127.0.0.1:18080",
		WorkspaceID: "default",
		Lease: apiwire.Lease{
			LeaseID: "lse_1", WorkspaceID: "default", HolderID: "operator", Mode: "AGENT",
			Epoch: 7, RuntimeGeneration: "gen-1", ExpiresAt: time.Date(2026, 10, 24, 12, 0, 30, 0, time.UTC),
		},
		CapabilityToken: "ldl_secret",
	}
	if err := WriteLeaseFile(path, in); err != nil {
		t.Fatalf("WriteLeaseFile: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	out, err := ReadLeaseFile(path)
	if err != nil {
		t.Fatalf("ReadLeaseFile: %v", err)
	}
	in.Version = LeaseFileVersion
	if out.Version != in.Version || out.APIBaseURL != in.APIBaseURL || out.WorkspaceID != in.WorkspaceID ||
		out.Lease != in.Lease || out.CapabilityToken != in.CapabilityToken {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", out, in)
	}
	if err := WriteLeaseFile(path, in); err == nil {
		t.Error("WriteLeaseFile overwrote an existing file")
	}
}

func TestReadLeaseFileRejectsUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	valid := `{"version":1,"apiBaseUrl":"http://127.0.0.1:18080","workspaceId":"default","lease":{"leaseId":"lse_1","workspaceId":"default","holderId":"operator","mode":"AGENT","epoch":1,"runtimeGeneration":"g","expiresAt":"2026-10-24T12:00:00Z"},"capabilityToken":"ldl_x"}`

	t.Run("group readable", func(t *testing.T) {
		p := filepath.Join(dir, "open.json")
		if err := os.WriteFile(p, []byte(valid), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadLeaseFile(p); !errors.Is(err, auth.ErrNotPrivate) {
			t.Errorf("error = %v, want ErrNotPrivate", err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		real := filepath.Join(dir, "real.json")
		if err := os.WriteFile(real, []byte(valid), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link.json")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadLeaseFile(link); err == nil {
			t.Error("symlinked lease file accepted")
		}
	})
	bodies := map[string]string{
		"unknown version": strings.Replace(valid, `"version":1`, `"version":2`, 1),
		"unknown field":   strings.Replace(valid, `"version":1`, `"version":1,"extra":true`, 1),
		"missing token":   strings.Replace(valid, `"capabilityToken":"ldl_x"`, `"capabilityToken":""`, 1),
		"not json":        "ldl_x",
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(dir, strings.ReplaceAll(name, " ", "_")+".json")
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadLeaseFile(p); err == nil {
				t.Error("invalid lease file accepted")
			}
		})
	}
}
